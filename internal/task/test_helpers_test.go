package task

import (
	"testing"
	"time"
)

// These are domain-rule tests. They cannot establish database atomicity,
// durable acceptance, global admission limits, or process termination.

var epoch = time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC)

func newTask(t *testing.T) Task {
	t.Helper()
	task, err := New("task-1", epoch, Timing{
		PendingDeadline:  epoch.Add(10 * time.Minute),
		AllocationWindow: 2 * time.Minute,
		ExecutionWindow:  10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func expected(task Task) Expected {
	return Expected{State: task.State, Version: task.Version}
}

type decisionResult struct {
	decision Decision
	err      error
}

func result(decision Decision, err error) decisionResult {
	return decisionResult{decision: decision, err: err}
}

func apply(t *testing.T, result decisionResult) Task {
	t.Helper()
	if result.err != nil {
		t.Fatal(result.err)
	}
	return result.decision.Task
}

func allocating(t *testing.T) Task {
	t.Helper()
	task := newTask(t)
	decision, err := BeginAllocation(task, expected(task), epoch.Add(time.Minute), "execution-1")
	return apply(t, result(decision, err))
}

func running(t *testing.T) Task {
	t.Helper()
	task := allocating(t)
	decision, err := Authorize(task, expected(task), epoch.Add(2*time.Minute), "execution-1", "boot-1")
	return apply(t, result(decision, err))
}

func receipt(at time.Time) CompletionReceipt {
	return CompletionReceipt{
		ExecutionIdentity: "execution-1",
		RuntimeBootID:     "boot-1",
		ManifestReference: "results/task-1/manifest.json",
		ManifestDigest:    "sha256:abc",
		AcceptedAt:        at,
	}
}

func accept(t *testing.T, task Task, at time.Time) Task {
	t.Helper()
	decision, err := AcceptReceipt(task, expected(task), at, receipt(at))
	return apply(t, result(decision, err))
}
