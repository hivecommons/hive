package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLateQueuedAndPartialCompletionCannotEraseObservedRunnerExecution(t *testing.T) {
	store := testStore(t)
	job := Job{Repo: "o/r", Base: "v5", ID: 1, RunID: 2, Attempt: 1, Head: strings.Repeat("a", 40), RunnerID: 3,
		Status: "in_progress", StartedAt: epoch, ObservedAt: epoch.Add(time.Minute)}
	queued := job
	queued.Status, queued.RunnerID, queued.ObservedAt = "queued", 0, epoch.Add(2*time.Minute)
	completed := queued
	completed.Status, completed.Conclusion, completed.CompletedAt, completed.ObservedAt = "completed", "cancelled", epoch.Add(3*time.Minute), epoch.Add(4*time.Minute)
	completed.StartedAt = time.Time{}
	partial := completed
	partial.CompletedAt, partial.ObservedAt = time.Time{}, epoch.Add(5*time.Minute)
	for n, observation := range []Job{job, queued, completed, partial} {
		if err := store.Append(Event{Version: 1, ID: fmt.Sprintf("job/%d", n), At: observation.ObservedAt, Job: &observation}); err != nil {
			t.Fatal(err)
		}
		actual := store.jobs[job.Key()]
		if n == 1 && actual.Status != "in_progress" {
			t.Fatal("late queued delivery erased assigned execution")
		}
	}
	actual := store.jobs[job.Key()]
	if actual.RunnerID != 3 || actual.StartedAt != epoch || actual.CompletedAt != completed.CompletedAt {
		t.Fatalf("partial terminal delivery erased known physical execution: %+v", actual)
	}
	report := cost(store.jobs, Coverage{From: epoch, Through: completed.ObservedAt, Complete: true}, "o/r", "v5", epoch, completed.ObservedAt)
	if report.RunnerMinutes == nil || *report.RunnerMinutes != 3 {
		t.Fatalf("canceled execution lost: %+v", report)
	}
	restarted, err := Open(store.path)
	if err != nil || restarted.jobs[job.Key()].RunnerID != 3 {
		t.Fatalf("restart lost assigned execution: %v", err)
	}
}
