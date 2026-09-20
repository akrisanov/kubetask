// Package task contains pure lifecycle decisions for an accepted KubeTask task.
//
// A Decision is a proposed replacement for a persisted task row. Callers must
// commit it with the supplied expected state and version before performing any
// external action. In particular, Authorize records authorization but does not
// create or return a start permit.
package task

import (
	"errors"
	"slices"
	"time"
)

// ValidationWindow is the fixed period allowed for receipt validation retries.
const ValidationWindow = 60 * time.Second

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

// State is the task's authoritative lifecycle state.
type State string

const (
	Pending    State = "Pending"
	Allocating State = "Allocating"
	Running    State = "Running"
	Succeeded  State = "Succeeded"
	Failed     State = "Failed"
	Cancelled  State = "Cancelled"
	TimedOut   State = "TimedOut"
	Expired    State = "Expired"
)

// Terminal reports whether no further lifecycle-state transition is allowed.
func (s State) Terminal() bool {
	switch s {
	case Succeeded, Failed, Cancelled, TimedOut, Expired:
		return true
	default:
		return false
	}
}

// OutcomeKind distinguishes immutable terminal execution outcomes.
type OutcomeKind string

const (
	OutcomeSucceeded             OutcomeKind = "succeeded"
	OutcomeExecutionFailure      OutcomeKind = "execution_failure"
	OutcomeInfrastructureFailure OutcomeKind = "infrastructure_failure"
	OutcomeCancelled             OutcomeKind = "cancelled"
	OutcomeTimedOut              OutcomeKind = "timed_out"
	OutcomeExpired               OutcomeKind = "expired"
)

// Outcome records an immutable terminal result and its authoritative time.
type Outcome struct {
	Kind OutcomeKind
	Code string
	At   time.Time
}

// FailureKind identifies whether a failure arose from user execution or infrastructure.
type FailureKind string

const (
	ExecutionFailure      FailureKind = "execution_failure"
	InfrastructureFailure FailureKind = "infrastructure_failure"
)

// Expected is the state and version a decision must replace atomically.
type Expected struct {
	State   State
	Version uint64
}

// Timing contains the persisted deadline policy for one accepted task.
type Timing struct {
	PendingDeadline  time.Time
	AllocationWindow time.Duration
	ExecutionWindow  time.Duration
}

// CancellationIntent records the first durable cancellation request.
type CancellationIntent struct {
	At time.Time
}

// Allocation is an opaque, deterministic identity recorded before a runtime is
// created. It deliberately contains no provider-specific type.
type Allocation struct {
	ExecutionIdentity string
	RecordedAt        time.Time
}

// Authorization binds the only possible execution attempt to one allocation
// identity and one runtime boot. It is evidence of a committed decision, not a
// usable credential or permit.
type Authorization struct {
	ExecutionIdentity string
	RuntimeBootID     string
	AuthorizedAt      time.Time
}

// CompletionReceipt identifies a runtime's immutable published result manifest.
type CompletionReceipt struct {
	ExecutionIdentity string
	RuntimeBootID     string
	ManifestReference string
	ManifestDigest    string
	AcceptedAt        time.Time
}

// Validation records manifest-validation retry facts for an accepted receipt.
type Validation struct {
	RetryDeadline time.Time
	Attempts      uint32
	LastAttemptAt time.Time
}

// ValidationResult is the result of an external manifest-validation read.
type ValidationResult string

// Supported manifest-validation results.
const (
	ValidationValid       ValidationResult = "valid"
	ValidationInvalid     ValidationResult = "invalid"
	ValidationUnavailable ValidationResult = "unavailable"
)

// CapacityReservation remains independent from the terminal execution outcome.
// Each observation is recorded by the application after it has observed the
// provider. ReleaseCapacity only accepts all three required observations.
type CapacityReservation struct {
	Held                      bool
	AllocationRequestResolved bool
	RuntimeStopped            bool
	ProviderCannotRecreate    bool
	ReleasedAt                time.Time
}

// HistoryEntry records a version-ordered lifecycle fact or state transition.
type HistoryEntry struct {
	Version uint64
	At      time.Time
	Kind    string
	From    State
	To      State
}

// Task contains only facts required for lifecycle decisions. It is suitable for
// mapping to a persistence record but it is not a database schema.
type Task struct {
	ID string

	State   State
	Version uint64

	AcceptedAt         time.Time
	PendingDeadline    time.Time
	AllocationWindow   time.Duration
	ExecutionWindow    time.Duration
	AllocationDeadline time.Time
	ExecutionDeadline  time.Time

	Cancellation  *CancellationIntent
	Allocation    *Allocation
	Authorization *Authorization
	Receipt       *CompletionReceipt
	Validation    *Validation
	Capacity      CapacityReservation
	Outcome       *Outcome
	History       []HistoryEntry
}

// Decision is a candidate replacement record. Persist Task atomically using the
// expected state and version before asking a runtime to act on it.
type Decision struct {
	Task                   Task
	Changed                bool
	AuthorizationRecorded  bool
	ReceiptAlreadyAccepted bool
}

// New creates the initial Pending record for a durably accepted task.
func New(id string, acceptedAt time.Time, timing Timing) (Task, error) {
	if id == "" ||
		acceptedAt.IsZero() ||
		!timing.PendingDeadline.After(acceptedAt) ||
		timing.AllocationWindow <= 0 ||
		timing.ExecutionWindow <= 0 {
		return Task{}, errors.New("invalid accepted task facts")
	}
	t := Task{
		ID:               id,
		State:            Pending,
		Version:          1,
		AcceptedAt:       acceptedAt,
		PendingDeadline:  timing.PendingDeadline,
		AllocationWindow: timing.AllocationWindow,
		ExecutionWindow:  timing.ExecutionWindow,
		History: []HistoryEntry{{
			Version: 1, At: acceptedAt, Kind: "accepted", To: Pending,
		}},
	}
	return t, nil
}

// Restore validates a task reconstructed from recorded facts. It never creates
// a permit. A restored authorized task remains authorized and cannot authorize
// another runtime.
func Restore(record Task) (Task, error) {
	if record.ID == "" ||
		record.Version == 0 ||
		!knownState(record.State) ||
		record.AcceptedAt.IsZero() ||
		record.PendingDeadline.IsZero() {
		return Task{}, errors.New("invalid task record")
	}
	if record.State.Terminal() != (record.Outcome != nil) {
		return Task{}, errors.New("terminal state and outcome disagree")
	}
	if record.Outcome != nil && !outcomeMatchesState(record.Outcome.Kind, record.State) {
		return Task{}, errors.New("outcome does not match state")
	}
	if record.Authorization != nil && record.State == Pending {
		return Task{}, errors.New("pending task has authorization")
	}
	if record.State == Allocating && (record.Allocation == nil || record.AllocationDeadline.IsZero()) {
		return Task{}, errors.New("allocating task lacks allocation facts")
	}
	if record.Cancellation != nil && !validRecordedTime(record.AcceptedAt, record.Cancellation.At) {
		return Task{}, errors.New("cancellation precedes acceptance")
	}
	if record.Allocation != nil &&
		!validRecordedTime(record.AcceptedAt, record.Allocation.RecordedAt) {
		return Task{}, errors.New("allocation precedes acceptance")
	}
	if record.Authorization != nil &&
		(record.Allocation == nil ||
			record.Authorization.ExecutionIdentity != record.Allocation.ExecutionIdentity) {
		return Task{}, errors.New("authorization does not match allocation")
	}
	if record.Authorization != nil &&
		!validRecordedTime(record.Allocation.RecordedAt, record.Authorization.AuthorizedAt) {
		return Task{}, errors.New("authorization precedes allocation")
	}
	if record.Receipt != nil &&
		(record.Authorization == nil ||
			!matchesAuthorization(record.Authorization, *record.Receipt) ||
			record.Validation == nil) {
		return Task{}, errors.New("receipt lacks matching authorization or validation facts")
	}
	if record.Receipt != nil &&
		(!validRecordedTime(record.Authorization.AuthorizedAt, record.Receipt.AcceptedAt) ||
			!record.Validation.RetryDeadline.Equal(record.Receipt.AcceptedAt.Add(ValidationWindow))) {
		return Task{}, errors.New("receipt has inconsistent timing")
	}
	if record.State == Running && (record.Authorization == nil || record.ExecutionDeadline.IsZero()) {
		return Task{}, errors.New("running task lacks authorization facts")
	}
	if record.Outcome != nil && !validRecordedTime(record.AcceptedAt, record.Outcome.At) {
		return Task{}, errors.New("outcome precedes acceptance")
	}
	if !record.Capacity.ReleasedAt.IsZero() &&
		!validRecordedTime(record.AcceptedAt, record.Capacity.ReleasedAt) {
		return Task{}, errors.New("capacity release precedes acceptance")
	}
	return clone(record), nil
}

func validRecordedTime(previous, at time.Time) bool {
	return !at.IsZero() && !at.Before(previous)
}

func clone(t Task) Task {
	next := t
	next.History = slices.Clone(t.History)
	if t.Cancellation != nil {
		v := *t.Cancellation
		next.Cancellation = &v
	}
	if t.Allocation != nil {
		v := *t.Allocation
		next.Allocation = &v
	}
	if t.Authorization != nil {
		v := *t.Authorization
		next.Authorization = &v
	}
	if t.Receipt != nil {
		next.Receipt = copyReceipt(*t.Receipt)
	}
	if t.Validation != nil {
		v := *t.Validation
		next.Validation = &v
	}
	if t.Outcome != nil {
		v := *t.Outcome
		next.Outcome = &v
	}
	return next
}

func copyReceipt(r CompletionReceipt) *CompletionReceipt {
	v := r
	return &v
}

func knownState(s State) bool {
	switch s {
	case Pending, Allocating, Running, Succeeded, Failed, Cancelled, TimedOut, Expired:
		return true
	default:
		return false
	}
}

func outcomeMatchesState(outcome OutcomeKind, state State) bool {
	switch state {
	case Succeeded:
		return outcome == OutcomeSucceeded
	case Failed:
		return outcome == OutcomeExecutionFailure || outcome == OutcomeInfrastructureFailure
	case Cancelled:
		return outcome == OutcomeCancelled
	case TimedOut:
		return outcome == OutcomeTimedOut
	case Expired:
		return outcome == OutcomeExpired
	default:
		return false
	}
}
