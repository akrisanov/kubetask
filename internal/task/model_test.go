package task

import (
	"testing"
	"time"
)

func TestRestoreRejectsInconsistentRecordedFacts(t *testing.T) {
	task := newTask(t)
	task.State = Running
	if _, err := Restore(task); err == nil {
		t.Fatal("Restore accepted running task without authorization")
	}

	task = running(t)
	task = accept(t, task, epoch.Add(3*time.Minute))
	task.Receipt.AcceptedAt = task.Authorization.AuthorizedAt.Add(-time.Second)
	if _, err := Restore(task); err == nil {
		t.Fatal("Restore accepted receipt before authorization")
	}
}
