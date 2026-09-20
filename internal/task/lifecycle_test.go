package task

import (
	"errors"
	"testing"
	"time"
)

func TestAllowedTransitionSequences(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T) Task
		want State
		kind OutcomeKind
	}{
		{
			name: "pending to allocating to running to succeeded",
			run: func(t *testing.T) Task {
				task := running(t)
				task = accept(t, task, epoch.Add(3*time.Minute))
				return apply(t, result(RecordValidation(task, expected(task), epoch.Add(3*time.Minute+30*time.Second), ValidationValid, false)))
			}, want: Succeeded, kind: OutcomeSucceeded,
		},
		{
			name: "pending cancellation",
			run: func(t *testing.T) Task {
				task := newTask(t)
				task = apply(t, result(RequestCancellation(task, expected(task), epoch.Add(time.Minute))))
				return apply(t, result(Evaluate(task, expected(task), epoch.Add(2*time.Minute))))
			}, want: Cancelled, kind: OutcomeCancelled,
		},
		{
			name: "pending deadline expires",
			run: func(t *testing.T) Task {
				task := newTask(t)
				return apply(t, result(Evaluate(task, expected(task), task.PendingDeadline)))
			}, want: Expired, kind: OutcomeExpired,
		},
		{
			name: "pending permanent infrastructure failure",
			run: func(t *testing.T) Task {
				task := newTask(t)
				return apply(t, result(Fail(task, expected(task), epoch.Add(time.Minute), InfrastructureFailure, "admission_broken")))
			}, want: Failed, kind: OutcomeInfrastructureFailure,
		},
		{
			name: "allocating cancellation",
			run: func(t *testing.T) Task {
				task := allocating(t)
				task = apply(t, result(RequestCancellation(task, expected(task), epoch.Add(2*time.Minute))))
				return apply(t, result(Evaluate(task, expected(task), epoch.Add(3*time.Minute))))
			}, want: Cancelled, kind: OutcomeCancelled,
		},
		{
			name: "allocating deadline is infrastructure failure",
			run: func(t *testing.T) Task {
				task := allocating(t)
				return apply(t, result(Evaluate(task, expected(task), task.AllocationDeadline)))
			}, want: Failed, kind: OutcomeInfrastructureFailure,
		},
		{
			name: "allocating infrastructure failure",
			run: func(t *testing.T) Task {
				task := allocating(t)
				return apply(t, result(Fail(task, expected(task), epoch.Add(2*time.Minute), InfrastructureFailure, "claim_rejected")))
			}, want: Failed, kind: OutcomeInfrastructureFailure,
		},
		{
			name: "running execution failure",
			run: func(t *testing.T) Task {
				task := running(t)
				return apply(t, result(Fail(task, expected(task), epoch.Add(3*time.Minute), ExecutionFailure, "exit_nonzero")))
			}, want: Failed, kind: OutcomeExecutionFailure,
		},
		{
			name: "running cancellation",
			run: func(t *testing.T) Task {
				task := running(t)
				task = apply(t, result(RequestCancellation(task, expected(task), epoch.Add(3*time.Minute))))
				return apply(t, result(Evaluate(task, expected(task), epoch.Add(4*time.Minute))))
			}, want: Cancelled, kind: OutcomeCancelled,
		},
		{
			name: "running deadline times out",
			run: func(t *testing.T) Task {
				task := running(t)
				return apply(t, result(Evaluate(task, expected(task), task.ExecutionDeadline)))
			}, want: TimedOut, kind: OutcomeTimedOut,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task := test.run(t)
			if task.State != test.want || task.Outcome == nil || task.Outcome.Kind != test.kind {
				t.Fatalf("state=%s outcome=%+v, want state=%s outcome=%s", task.State, task.Outcome, test.want, test.kind)
			}
		})
	}
}

func TestGuardsAndImmutableOutcomes(t *testing.T) {
	task := newTask(t)
	stale := expected(task)
	task = apply(t, result(BeginAllocation(task, expected(task), epoch.Add(time.Minute), "execution-1")))
	if _, err := Authorize(task, stale, epoch.Add(2*time.Minute), "execution-1", "boot-1"); !errors.Is(err, ErrStale) {
		t.Fatalf("stale authorization error = %v", err)
	}
	if _, err := Fail(task, expected(task), epoch.Add(2*time.Minute), ExecutionFailure, "bad"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("pending or allocating execution failure error = %v", err)
	}

	task = apply(t, result(Fail(task, expected(task), epoch.Add(2*time.Minute), InfrastructureFailure, "claim_failed")))
	original := *task.Outcome
	if _, err := Fail(task, expected(task), epoch.Add(3*time.Minute), InfrastructureFailure, "other"); !errors.Is(err, ErrTerminal) {
		t.Fatalf("terminal failure error = %v", err)
	}
	decision, err := Evaluate(task, expected(task), epoch.Add(4*time.Minute))
	if err != nil || decision.Changed || *decision.Task.Outcome != original {
		t.Fatalf("terminal outcome changed: decision=%+v err=%v", decision, err)
	}
}

func TestAuthorizationIsOneUseAndReconstructionDoesNotPermitAgain(t *testing.T) {
	task := allocating(t)
	if _, err := Authorize(task, expected(task), epoch.Add(2*time.Minute), "different", "boot-1"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("changed execution identity error = %v", err)
	}
	task = apply(t, result(Authorize(task, expected(task), epoch.Add(2*time.Minute), "execution-1", "boot-1")))
	if _, err := Authorize(task, expected(task), epoch.Add(3*time.Minute), "execution-1", "boot-2"); !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("repeated authorization error = %v", err)
	}
	restored, err := Restore(task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Authorize(restored, expected(restored), epoch.Add(3*time.Minute), "execution-1", "boot-1"); !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("restored task authorization error = %v", err)
	}
}

func TestCancellationReceiptAndDeadlinePrecedence(t *testing.T) {
	t.Run("receipt before cancellation keeps priority", func(t *testing.T) {
		task := running(t)
		receiptAt := epoch.Add(3 * time.Minute)
		task = accept(t, task, receiptAt)
		task = apply(t, result(RequestCancellation(task, expected(task), receiptAt.Add(time.Minute))))
		task = apply(t, result(RecordValidation(task, expected(task), task.ExecutionDeadline.Add(time.Minute), ValidationValid, true)))
		if task.State != Succeeded {
			t.Fatalf("state=%s, want succeeded", task.State)
		}
	})

	t.Run("cancellation before receipt rejects receipt", func(t *testing.T) {
		task := running(t)
		task = apply(t, result(RequestCancellation(task, expected(task), epoch.Add(3*time.Minute))))
		if _, err := AcceptReceipt(task, expected(task), epoch.Add(4*time.Minute), receipt(epoch.Add(4*time.Minute))); !errors.Is(err, ErrReceiptIneligible) {
			t.Fatalf("receipt error = %v", err)
		}
		task = apply(t, result(Evaluate(task, expected(task), epoch.Add(4*time.Minute))))
		if task.State != Cancelled {
			t.Fatalf("state=%s, want cancelled", task.State)
		}
	})

	t.Run("equal timestamp uses committed version order", func(t *testing.T) {
		at := epoch.Add(3 * time.Minute)
		firstReceipt := running(t)
		firstReceipt = accept(t, firstReceipt, at)
		firstReceipt = apply(t, result(RequestCancellation(firstReceipt, expected(firstReceipt), at)))
		firstReceipt = apply(t, result(RecordValidation(firstReceipt, expected(firstReceipt), at.Add(30*time.Second), ValidationValid, false)))
		if firstReceipt.State != Succeeded {
			t.Fatalf("receipt-first state=%s", firstReceipt.State)
		}

		firstCancel := running(t)
		firstCancel = apply(t, result(RequestCancellation(firstCancel, expected(firstCancel), at)))
		if _, err := AcceptReceipt(firstCancel, expected(firstCancel), at, receipt(at)); !errors.Is(err, ErrReceiptIneligible) {
			t.Fatalf("cancel-first receipt error=%v", err)
		}
		firstCancel = apply(t, result(Evaluate(firstCancel, expected(firstCancel), at.Add(time.Minute))))
		if firstCancel.State != Cancelled {
			t.Fatalf("cancel-first state=%s", firstCancel.State)
		}
	})

	t.Run("deadlines win at their exact boundary", func(t *testing.T) {
		pending := newTask(t)
		pending = apply(t, result(RequestCancellation(pending, expected(pending), pending.PendingDeadline)))
		pending = apply(t, result(Evaluate(pending, expected(pending), pending.PendingDeadline)))
		if pending.State != Expired {
			t.Fatalf("pending state=%s", pending.State)
		}

		run := running(t)
		if _, err := AcceptReceipt(run, expected(run), run.ExecutionDeadline, receipt(run.ExecutionDeadline)); !errors.Is(err, ErrReceiptIneligible) {
			t.Fatalf("deadline receipt error=%v", err)
		}
		run = apply(t, result(Evaluate(run, expected(run), run.ExecutionDeadline)))
		if run.State != TimedOut {
			t.Fatalf("running state=%s", run.State)
		}
	})
}

func TestReceiptRetriesAndValidationWindow(t *testing.T) {
	task := running(t)
	accepted := receipt(epoch.Add(3 * time.Minute))
	task = accept(t, task, epoch.Add(3*time.Minute))
	originalDeadline := task.Validation.RetryDeadline

	// This retry represents the runtime repeating the same receipt after both
	// the execution deadline and a control-plane restart.
	retry, err := AcceptReceipt(task, Expected{}, task.ExecutionDeadline.Add(time.Minute), accepted)
	if err != nil || !retry.ReceiptAlreadyAccepted || retry.Task.Version != task.Version {
		t.Fatalf("identical retry=%+v err=%v", retry, err)
	}
	conflicting := accepted
	conflicting.ManifestDigest = "sha256:different"
	if _, err := AcceptReceipt(task, expected(task), task.ExecutionDeadline.Add(time.Minute), conflicting); !errors.Is(err, ErrReceiptConflict) {
		t.Fatalf("conflicting retry error=%v", err)
	}

	// A timely receipt remains eligible after the execution deadline while the
	// reconciler validates it.
	task = apply(t, result(RecordValidation(task, expected(task), task.ExecutionDeadline.Add(time.Second), ValidationValid, true)))
	if task.State != Succeeded {
		t.Fatalf("late validation state=%s", task.State)
	}

	// Start a second accepted receipt to exercise retry-window exhaustion.
	task = running(t)
	accepted = receipt(epoch.Add(3 * time.Minute))
	task = accept(t, task, epoch.Add(3*time.Minute))
	originalDeadline = task.Validation.RetryDeadline
	task = apply(t, result(RecordValidation(task, expected(task), epoch.Add(3*time.Minute+30*time.Second), ValidationUnavailable, false)))
	if !task.Validation.RetryDeadline.Equal(originalDeadline) {
		t.Fatalf("retry deadline reset from %s to %s", originalDeadline, task.Validation.RetryDeadline)
	}
	if _, err := RecordValidation(task, expected(task), originalDeadline, ValidationUnavailable, false); !errors.Is(err, ErrFinalReadRequired) {
		t.Fatalf("retry exhaustion error=%v", err)
	}
	task = apply(t, result(RecordValidation(task, expected(task), originalDeadline, ValidationUnavailable, true)))
	if task.State != Failed || task.Outcome.Kind != OutcomeInfrastructureFailure {
		t.Fatalf("final validation outcome=%+v", task.Outcome)
	}
}

func TestValidationFailureAndCapacityReleaseAreSeparate(t *testing.T) {
	task := running(t)
	task = accept(t, task, epoch.Add(3*time.Minute))
	task = apply(t, result(RecordValidation(task, expected(task), epoch.Add(3*time.Minute+30*time.Second), ValidationInvalid, false)))
	if task.State != Failed || task.Outcome.Kind != OutcomeInfrastructureFailure || !task.Capacity.Held {
		t.Fatalf("terminal task should retain capacity: %+v", task)
	}
	if _, err := ReleaseCapacity(task, expected(task), epoch.Add(5*time.Minute)); !errors.Is(err, ErrCapacityHeld) {
		t.Fatalf("premature release error=%v", err)
	}
	task = apply(t, result(ResolveAllocation(task, expected(task), epoch.Add(5*time.Minute))))
	task = apply(t, result(ConfirmRuntimeStopped(task, expected(task), epoch.Add(6*time.Minute))))
	task = apply(t, result(ConfirmProviderCannotRecreate(task, expected(task), epoch.Add(7*time.Minute))))
	task = apply(t, result(ReleaseCapacity(task, expected(task), epoch.Add(8*time.Minute))))
	if task.Capacity.Held || task.Capacity.ReleasedAt.IsZero() || task.State != Failed {
		t.Fatalf("capacity=%+v state=%s", task.Capacity, task.State)
	}
}

func TestMutatingDecisionsRejectInvalidEventTimes(t *testing.T) {
	beforeAcceptance := epoch.Add(-time.Second)

	tests := []struct {
		name string
		run  func(*testing.T) error
	}{
		{
			name: "cancellation before acceptance",
			run: func(t *testing.T) error {
				task := newTask(t)
				_, err := RequestCancellation(task, expected(task), beforeAcceptance)
				return err
			},
		},
		{
			name: "allocation at zero time",
			run: func(t *testing.T) error {
				task := newTask(t)
				_, err := BeginAllocation(task, expected(task), time.Time{}, "execution-1")
				return err
			},
		},
		{
			name: "authorization before acceptance",
			run: func(t *testing.T) error {
				task := allocating(t)
				_, err := Authorize(task, expected(task), beforeAcceptance, "execution-1", "boot-1")
				return err
			},
		},
		{
			name: "receipt at zero time",
			run: func(t *testing.T) error {
				task := running(t)
				_, err := AcceptReceipt(task, expected(task), time.Time{}, receipt(epoch.Add(3*time.Minute)))
				return err
			},
		},
		{
			name: "evaluation before acceptance",
			run: func(t *testing.T) error {
				task := newTask(t)
				_, err := Evaluate(task, expected(task), beforeAcceptance)
				return err
			},
		},
		{
			name: "failure before acceptance",
			run: func(t *testing.T) error {
				task := newTask(t)
				_, err := Fail(task, expected(task), beforeAcceptance, InfrastructureFailure, "storage_failed")
				return err
			},
		},
		{
			name: "capacity observation at zero time",
			run: func(t *testing.T) error {
				task := allocating(t)
				_, err := ResolveAllocation(task, expected(task), time.Time{})
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(t); !errors.Is(err, ErrInvalidEventTime) {
				t.Fatalf("error=%v, want %v", err, ErrInvalidEventTime)
			}
		})
	}
}

func TestReceiptAndValidationEnforceCausalTimestampOrder(t *testing.T) {
	task := running(t)
	receiptAt := task.Authorization.AuthorizedAt.Add(-time.Second)
	if _, err := AcceptReceipt(task, expected(task), receiptAt, receipt(receiptAt)); !errors.Is(err, ErrCausalOrder) {
		t.Fatalf("receipt before authorization error=%v", err)
	}

	task = accept(t, task, epoch.Add(3*time.Minute))
	validationAt := task.Receipt.AcceptedAt.Add(-time.Second)
	if _, err := RecordValidation(task, expected(task), validationAt, ValidationValid, false); !errors.Is(err, ErrCausalOrder) {
		t.Fatalf("validation before receipt error=%v", err)
	}
}
