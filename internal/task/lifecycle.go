package task

import (
	"errors"
	"time"
)

// Lifecycle errors identify rejected domain decisions.
var (
	ErrStale             = errors.New("task state or version is stale")
	ErrInvalidTransition = errors.New("invalid task transition")
	ErrTerminal          = errors.New("task has an immutable terminal outcome")
	ErrAuthorizationUsed = errors.New("execution authorization was already used")
	ErrReceiptConflict   = errors.New("completion receipt conflicts with the accepted receipt")
	ErrReceiptIneligible = errors.New("completion receipt is not eligible")
	ErrFinalReadRequired = errors.New("final validation read is required")
	ErrCapacityHeld      = errors.New("capacity release conditions are not satisfied")
	ErrInvalidEventTime  = errors.New("event time is invalid")
	ErrCausalOrder       = errors.New("event time violates lifecycle causal order")
)

// RequestCancellation records the first cancellation intent without stopping a runtime.
func RequestCancellation(t Task, expected Expected, at time.Time) (Decision, error) {
	if t.Outcome != nil || t.Cancellation != nil {
		return Decision{Task: clone(t)}, nil
	}
	if err := checkExpected(t, expected); err != nil {
		return Decision{}, err
	}
	if err := validateEventTime(t, at); err != nil {
		return Decision{}, err
	}
	next := change(t, at, "cancellation_requested")
	next.Cancellation = &CancellationIntent{At: at}
	return Decision{Task: next, Changed: true}, nil
}

// BeginAllocation reserves active capacity and records the exact allocation
// identity before the application creates a runtime.
func BeginAllocation(
	t Task,
	expected Expected,
	at time.Time,
	executionIdentity string,
) (Decision, error) {
	if err := checkExpected(t, expected); err != nil {
		return Decision{}, err
	}
	if t.Outcome != nil {
		return Decision{}, ErrTerminal
	}
	if t.State != Pending || executionIdentity == "" {
		return Decision{}, ErrInvalidTransition
	}
	if err := validateEventTime(t, at); err != nil {
		return Decision{}, err
	}
	if decision, settled := settle(t, at); settled {
		return decision, nil
	}
	next := transition(t, at, Allocating, "capacity_reserved")
	next.Allocation = &Allocation{ExecutionIdentity: executionIdentity, RecordedAt: at}
	next.AllocationDeadline = at.Add(t.AllocationWindow)
	next.Capacity.Held = true
	return Decision{Task: next, Changed: true}, nil
}

// Authorize records the one irreversible execution authorization. The caller
// may grant an actual runtime start permit only after this decision is committed.
func Authorize(
	t Task,
	expected Expected,
	at time.Time,
	executionIdentity, runtimeBootID string,
) (Decision, error) {
	if t.Authorization != nil {
		return Decision{}, ErrAuthorizationUsed
	}
	if err := checkExpected(t, expected); err != nil {
		return Decision{}, err
	}
	if err := validateEventTime(t, at); err != nil {
		return Decision{}, err
	}
	if t.Outcome != nil {
		return Decision{}, ErrTerminal
	}
	if t.State != Allocating || t.Allocation == nil {
		return Decision{}, ErrInvalidTransition
	}
	if t.Allocation.ExecutionIdentity != executionIdentity || runtimeBootID == "" {
		return Decision{}, ErrInvalidTransition
	}
	if decision, settled := settle(t, at); settled {
		return decision, nil
	}
	next := transition(t, at, Running, "execution_authorized")
	next.Authorization = &Authorization{
		ExecutionIdentity: executionIdentity,
		RuntimeBootID:     runtimeBootID,
		AuthorizedAt:      at,
	}
	next.ExecutionDeadline = at.Add(t.ExecutionWindow)
	return Decision{Task: next, Changed: true, AuthorizationRecorded: true}, nil
}

// AcceptReceipt accepts one timely receipt at the supplied authoritative time.
// An identical retry returns the original recorded task even after its execution
// deadline or terminal outcome.
func AcceptReceipt(
	t Task,
	expected Expected,
	at time.Time,
	receipt CompletionReceipt,
) (Decision, error) {
	if t.Receipt != nil {
		if sameReceipt(*t.Receipt, receipt) {
			return Decision{Task: clone(t), ReceiptAlreadyAccepted: true}, nil
		}
		return Decision{}, ErrReceiptConflict
	}
	if err := checkExpected(t, expected); err != nil {
		return Decision{}, err
	}
	if t.Outcome != nil {
		return Decision{}, ErrTerminal
	}
	if t.State != Running || t.Authorization == nil {
		return Decision{}, ErrReceiptIneligible
	}
	if !matchesAuthorization(t.Authorization, receipt) {
		return Decision{}, ErrReceiptIneligible
	}
	if receipt.ManifestReference == "" || receipt.ManifestDigest == "" {
		return Decision{}, ErrReceiptIneligible
	}
	if err := validateEventTime(t, at); err != nil {
		return Decision{}, err
	}
	if at.Before(t.Authorization.AuthorizedAt) {
		return Decision{}, ErrCausalOrder
	}
	if !at.Before(t.ExecutionDeadline) || (t.Cancellation != nil && !at.Before(t.Cancellation.At)) {
		return Decision{}, ErrReceiptIneligible
	}
	next := change(t, at, "completion_receipt_accepted")
	next.Receipt = copyReceipt(receipt)
	next.Receipt.AcceptedAt = at
	next.Validation = &Validation{RetryDeadline: at.Add(ValidationWindow)}
	return Decision{Task: next, Changed: true}, nil
}

// RecordValidation records an external validation observation. At the retry
// deadline the application must pass finalRead=true after one final bounded
// read. That final read is represented here, but is never performed here.
func RecordValidation(
	t Task,
	expected Expected,
	at time.Time,
	result ValidationResult,
	finalRead bool,
) (Decision, error) {
	if err := checkExpected(t, expected); err != nil {
		return Decision{}, err
	}
	if t.Outcome != nil {
		return Decision{}, ErrTerminal
	}
	if t.State != Running || t.Receipt == nil || t.Validation == nil {
		return Decision{}, ErrInvalidTransition
	}
	if err := validateEventTime(t, at); err != nil {
		return Decision{}, err
	}
	if at.Before(t.Receipt.AcceptedAt) {
		return Decision{}, ErrCausalOrder
	}
	switch result {
	case ValidationValid, ValidationInvalid, ValidationUnavailable:
	default:
		return Decision{}, ErrInvalidTransition
	}
	if finalRead && at.Before(t.Validation.RetryDeadline) {
		return Decision{}, ErrInvalidTransition
	}
	if !finalRead && !at.Before(t.Validation.RetryDeadline) {
		return Decision{}, ErrFinalReadRequired
	}
	if result == ValidationValid {
		next := terminal(t, at, Succeeded, OutcomeSucceeded, "", "receipt_validated")
		return Decision{Task: next, Changed: true}, nil
	}
	if result == ValidationInvalid || finalRead {
		next := terminal(
			t,
			at,
			Failed,
			OutcomeInfrastructureFailure,
			"result_validation_failed",
			"receipt_validation_failed",
		)
		return Decision{Task: next, Changed: true}, nil
	}
	next := change(t, at, "receipt_validation_retry")
	next.Validation.Attempts++
	next.Validation.LastAttemptAt = at
	return Decision{Task: next, Changed: true}, nil
}

// Evaluate applies durable cancellation and phase deadline facts. It does not
// infer completion from workload status or a manifest. An eligible receipt holds
// precedence while validation remains pending.
func Evaluate(t Task, expected Expected, at time.Time) (Decision, error) {
	if err := checkExpected(t, expected); err != nil {
		return Decision{}, err
	}
	if t.Outcome != nil {
		return Decision{Task: clone(t)}, nil
	}
	if err := validateEventTime(t, at); err != nil {
		return Decision{}, err
	}
	if decision, settled := settle(t, at); settled {
		return decision, nil
	}
	return Decision{Task: clone(t)}, nil
}

// Fail records an established execution or infrastructure failure when
// precedence permits it.
func Fail(
	t Task,
	expected Expected,
	at time.Time,
	kind FailureKind,
	code string,
) (Decision, error) {
	if err := checkExpected(t, expected); err != nil {
		return Decision{}, err
	}
	if t.Outcome != nil {
		return Decision{}, ErrTerminal
	}
	if err := validateEventTime(t, at); err != nil {
		return Decision{}, err
	}
	if decision, settled := settle(t, at); settled {
		return decision, nil
	}
	if t.Receipt != nil {
		return Decision{}, ErrInvalidTransition
	}
	if kind != ExecutionFailure && kind != InfrastructureFailure {
		return Decision{}, ErrInvalidTransition
	}
	if (t.State == Pending || t.State == Allocating) && kind != InfrastructureFailure {
		return Decision{}, ErrInvalidTransition
	}
	if t.State != Pending && t.State != Allocating && t.State != Running {
		return Decision{}, ErrInvalidTransition
	}
	outcome := OutcomeInfrastructureFailure
	if kind == ExecutionFailure {
		outcome = OutcomeExecutionFailure
	}
	next := terminal(t, at, Failed, outcome, code, "failure_established")
	return Decision{Task: next, Changed: true}, nil
}

// ResolveAllocation records that no allocation request remains unresolved.
func ResolveAllocation(t Task, expected Expected, at time.Time) (Decision, error) {
	return recordCapacityFact(
		t,
		expected,
		at,
		"allocation_request_resolved",
		func(c *CapacityReservation) { c.AllocationRequestResolved = true },
	)
}

// ConfirmRuntimeStopped records that all allocated runtime processes have stopped.
func ConfirmRuntimeStopped(t Task, expected Expected, at time.Time) (Decision, error) {
	return recordCapacityFact(
		t,
		expected,
		at,
		"runtime_stopped",
		func(c *CapacityReservation) { c.RuntimeStopped = true },
	)
}

// ConfirmProviderCannotRecreate records that the provider cannot recreate the runtime.
func ConfirmProviderCannotRecreate(t Task, expected Expected, at time.Time) (Decision, error) {
	return recordCapacityFact(
		t,
		expected,
		at,
		"provider_cannot_recreate",
		func(c *CapacityReservation) { c.ProviderCannotRecreate = true },
	)
}

// ReleaseCapacity releases a reservation only after every required stop observation.
func ReleaseCapacity(t Task, expected Expected, at time.Time) (Decision, error) {
	if err := checkExpected(t, expected); err != nil {
		return Decision{}, err
	}
	if !t.Capacity.Held {
		return Decision{Task: clone(t)}, nil
	}
	if err := validateEventTime(t, at); err != nil {
		return Decision{}, err
	}
	if !t.Capacity.AllocationRequestResolved ||
		!t.Capacity.RuntimeStopped ||
		!t.Capacity.ProviderCannotRecreate {
		return Decision{}, ErrCapacityHeld
	}
	next := change(t, at, "capacity_released")
	next.Capacity.Held = false
	next.Capacity.ReleasedAt = at
	return Decision{Task: next, Changed: true}, nil
}

func recordCapacityFact(
	t Task,
	expected Expected,
	at time.Time,
	kind string,
	set func(*CapacityReservation),
) (Decision, error) {
	if err := checkExpected(t, expected); err != nil {
		return Decision{}, err
	}
	if !t.Capacity.Held {
		return Decision{}, ErrInvalidTransition
	}
	if err := validateEventTime(t, at); err != nil {
		return Decision{}, err
	}
	next := change(t, at, kind)
	set(&next.Capacity)
	return Decision{Task: next, Changed: true}, nil
}

func settle(t Task, at time.Time) (Decision, bool) {
	if t.State == Running && t.Receipt != nil {
		return Decision{Task: clone(t)}, false
	}
	rule := phaseRuleFor(t)
	if t.Cancellation != nil && t.Cancellation.At.Before(rule.deadline) {
		next := terminal(t, at, Cancelled, OutcomeCancelled, "", "cancellation_honored")
		return Decision{Task: next, Changed: true}, true
	}
	if !at.Before(rule.deadline) {
		next := terminal(
			t,
			at,
			rule.terminalState,
			rule.outcome,
			rule.code,
			"phase_deadline_reached",
		)
		return Decision{Task: next, Changed: true}, true
	}
	return Decision{}, false
}

type phaseRule struct {
	deadline      time.Time
	terminalState State
	outcome       OutcomeKind
	code          string
}

func phaseRuleFor(t Task) phaseRule {
	switch t.State {
	case Pending:
		return phaseRule{
			deadline:      t.PendingDeadline,
			terminalState: Expired,
			outcome:       OutcomeExpired,
			code:          "pending_deadline_exceeded",
		}
	case Allocating:
		return phaseRule{
			deadline:      t.AllocationDeadline,
			terminalState: Failed,
			outcome:       OutcomeInfrastructureFailure,
			code:          "allocation_deadline_exceeded",
		}
	case Running:
		return phaseRule{
			deadline:      t.ExecutionDeadline,
			terminalState: TimedOut,
			outcome:       OutcomeTimedOut,
			code:          "execution_deadline_exceeded",
		}
	default:
		return phaseRule{}
	}
}

func checkExpected(t Task, expected Expected) error {
	if t.State != expected.State || t.Version != expected.Version {
		return ErrStale
	}
	return nil
}

func validateEventTime(t Task, at time.Time) error {
	if at.IsZero() || at.Before(t.AcceptedAt) {
		return ErrInvalidEventTime
	}
	return nil
}

func transition(t Task, at time.Time, to State, kind string) Task {
	next := change(t, at, kind)
	from := next.State
	next.State = to
	next.History[len(next.History)-1].From = from
	next.History[len(next.History)-1].To = to
	return next
}

func terminal(t Task, at time.Time, state State, outcome OutcomeKind, code, kind string) Task {
	next := transition(t, at, state, kind)
	next.Outcome = &Outcome{Kind: outcome, Code: code, At: at}
	return next
}

func change(t Task, at time.Time, kind string) Task {
	next := clone(t)
	next.Version++
	next.History = append(next.History, HistoryEntry{Version: next.Version, At: at, Kind: kind})
	return next
}

func sameReceipt(left, right CompletionReceipt) bool {
	return left.ExecutionIdentity == right.ExecutionIdentity &&
		left.RuntimeBootID == right.RuntimeBootID &&
		left.ManifestReference == right.ManifestReference &&
		left.ManifestDigest == right.ManifestDigest
}

func matchesAuthorization(a *Authorization, r CompletionReceipt) bool {
	return a.ExecutionIdentity == r.ExecutionIdentity &&
		a.RuntimeBootID == r.RuntimeBootID
}
