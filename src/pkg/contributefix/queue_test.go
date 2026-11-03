package contributefix

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func feedback(n int) Feedback {
	return Feedback{Repo: "org/repo", Number: n, HeadSHA: "head-a", CreatedAt: epoch.Add(time.Duration(n) * time.Minute), FailingChecks: []string{"unit tests"}}
}

func enqueue(t *testing.T, q *Queue, f Feedback) {
	t.Helper()
	if err := q.Enqueue(f); err != nil {
		t.Fatal(err)
	}
}

func counts(t *testing.T, q *Queue, queued, active int, canClaim bool) {
	t.Helper()
	a, b, c := q.Counts()
	if a != queued || b != active || c != canClaim {
		t.Fatalf("Counts = (%d,%d,%v), want (%d,%d,%v)", a, b, c, queued, active, canClaim)
	}
}

func TestParallelCapacityAndRefill(t *testing.T) {
	var q Queue
	counts(t, &q, 0, 0, true)
	for n := 5; n > 0; n-- {
		enqueue(t, &q, feedback(n))
	}
	counts(t, &q, 5, 0, false)
	if got := q.Dispatch(epoch, false, 3, 10); len(got) != 0 {
		t.Fatal("offline dispatch")
	}
	tasks := q.Dispatch(epoch, true, 3, 10)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %v", tasks)
	}
	for i, task := range tasks {
		if task.Action != Spawn || task.PRs[0].Number != i+1 || task.Kind != Kind || task.Priority != Priority {
			t.Fatalf("task = %+v", task)
		}
	}
	counts(t, &q, 3, 2, false)
	if got := q.Dispatch(epoch, true, 3, 10); len(got) != 0 {
		t.Fatal("over capacity")
	}
	if !q.Finish(tasks[0].ID) || q.Finish(tasks[0].ID) {
		t.Fatal("completion not idempotent")
	}
	next := q.Dispatch(epoch, true, 3, 10)
	if len(next) != 1 || next[0].PRs[0].Number != 3 || next[0].Action != Spawn {
		t.Fatalf("refill = %v", next)
	}
	counts(t, &q, 2, 2, false)
}

func TestPreemptWaitsForActualResumeAndFocusWindow(t *testing.T) {
	for _, capacity := range []int{0, 1} {
		var q Queue
		enqueue(t, &q, feedback(1))
		enqueue(t, &q, feedback(2))
		tasks := q.Dispatch(epoch, true, capacity, 5)
		if len(tasks) != 1 || tasks[0].Action != Preempt {
			t.Fatalf("tasks = %v", tasks)
		}
		if q.ResumePrimary(epoch, 30*time.Minute) {
			t.Fatal("resumed while fix active")
		}
		q.Finish(tasks[0].ID)
		if got := q.Dispatch(epoch.Add(time.Hour), true, capacity, 5); len(got) != 0 {
			t.Fatal("preempted before resume")
		}
		resume := epoch.Add(2 * time.Hour)
		if !q.ResumePrimary(resume, 30*time.Minute) || q.ResumePrimary(resume.Add(time.Hour), 30*time.Minute) {
			t.Fatal("resume acknowledgement")
		}
		if got := q.Dispatch(resume.Add(29*time.Minute), true, capacity, 5); len(got) != 0 {
			t.Fatal("focus window ignored")
		}
		if got := q.Dispatch(resume.Add(30*time.Minute), true, capacity, 5); len(got) != 1 {
			t.Fatal("focus window never expired")
		}
	}
}

func TestReviewPriorityAndCauseCoalescing(t *testing.T) {
	var q Queue
	for n := 1; n <= 4; n++ {
		f := feedback(n)
		f.Check, f.ErrorSignature = "unit", "panic: missing fixture"
		if n == 4 {
			f.ChangesRequested = true
			f.ReviewAsks = []string{"handle the error"}
		}
		enqueue(t, &q, f)
	}
	tasks := q.Dispatch(epoch, true, 3, 2)
	if len(tasks) != 2 || tasks[0].PRs[0].Number != 4 || len(tasks[0].PRs) != 1 || len(tasks[1].PRs) != 3 {
		t.Fatalf("tasks = %+v", tasks)
	}
	for i, f := range tasks[1].PRs {
		if f.Number != i+1 {
			t.Fatal("coalesced PRs not FIFO")
		}
	}
	counts(t, &q, 0, 2, false)
	body, err := json.Marshal(tasks[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kind":"fix-pr"`, `"priority":"interrupt"`, `"head_sha":"head-a"`, `"review_asks":["handle the error"]`, "existing PR branch", "Signed-off-by"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("payload missing %q: %s", want, body)
		}
	}
}

func TestCauseRequiresCheckAndSignatureAndSameRepo(t *testing.T) {
	var q Queue
	for n := 1; n <= 4; n++ {
		f := feedback(n)
		f.Check = "unit"
		if n >= 3 {
			f.ErrorSignature = "signature"
		}
		if n == 4 {
			f.Repo = "other/repo"
		}
		enqueue(t, &q, f)
	}
	if tasks := q.Dispatch(epoch, true, 10, 10); len(tasks) != 4 {
		t.Fatalf("unrelated feedback coalesced: %v", tasks)
	}
}

func TestSameCauseWaitsForInFlightBatch(t *testing.T) {
	var q Queue
	f := feedback(1)
	f.Check, f.ErrorSignature = "unit", "signature"
	enqueue(t, &q, f)
	first := q.Dispatch(epoch, true, 3, 2)[0]
	f.Number = 2
	enqueue(t, &q, f)
	if tasks := q.Dispatch(epoch, true, 3, 2); len(tasks) != 0 {
		t.Fatal("same cause ran concurrently")
	}
	q.Finish(first.ID)
	if tasks := q.Dispatch(epoch, true, 3, 2); len(tasks) != 1 {
		t.Fatal("cause queue not refilled")
	}
}

func TestHeadFencingAndNewWorkGate(t *testing.T) {
	var q Queue
	f := feedback(1)
	enqueue(t, &q, f)
	first := q.Dispatch(epoch, true, 2, 1)[0]
	enqueue(t, &q, f) // same in-flight head: no duplicate
	counts(t, &q, 0, 1, false)
	f.HeadSHA = "head-b"
	enqueue(t, &q, f)
	q.Remove(f.Repo, f.Number, "head-a") // stale green observation
	counts(t, &q, 1, 1, false)
	if tasks := q.Dispatch(epoch, true, 3, 2); len(tasks) != 0 {
		t.Fatal("same PR ran twice")
	}
	q.Finish(first.ID)
	next := q.Dispatch(epoch, true, 2, 1)[0]
	if next.ID == first.ID || next.PRs[0].HeadSHA != "head-b" {
		t.Fatal("head/generation not advanced")
	}
	if q.Finish(first.ID) {
		t.Fatal("stale completion accepted")
	}
	counts(t, &q, 0, 1, false)
	q.Finish(next.ID)
	counts(t, &q, 0, 0, true)
}

func TestRejectDeliveryPreservesNewHead(t *testing.T) {
	var q Queue
	f := feedback(1)
	enqueue(t, &q, f)
	first := q.Dispatch(epoch, true, 1, 1)[0]
	f.HeadSHA = "head-b"
	enqueue(t, &q, f)
	if !q.Reject(first.ID) || q.Reject(first.ID) {
		t.Fatal("reject acknowledgement")
	}
	next := q.Dispatch(epoch, true, 1, 1)
	if len(next) != 1 || next[0].PRs[0].HeadSHA != "head-b" {
		t.Fatalf("retry = %+v", next)
	}
}

func TestServerCapAndSnapshotIsolation(t *testing.T) {
	var q Queue
	f := feedback(1)
	enqueue(t, &q, f)
	f.FailingChecks[0] = "mutated input"
	enqueue(t, &q, feedback(2))
	tasks := q.Dispatch(epoch, true, 1000, 1)
	if len(tasks) != 1 || tasks[0].PRs[0].FailingChecks[0] != "unit tests" {
		t.Fatalf("tasks = %v", tasks)
	}
	tasks[0].PRs[0].FailingChecks[0] = "mutated output"
	q.Reject(tasks[0].ID)
	next := q.Dispatch(epoch, true, 1000, 1)
	if next[0].PRs[0].FailingChecks[0] != "unit tests" {
		t.Fatal("caller mutated reservation")
	}
}

func TestSingleFixMustResumeBeforeNewWork(t *testing.T) {
	var q Queue
	enqueue(t, &q, feedback(1))
	task := q.Dispatch(epoch, true, 1, 1)[0]
	q.Finish(task.ID)
	counts(t, &q, 0, 0, false)
	if !q.ResumePrimary(epoch, 30*time.Minute) {
		t.Fatal("resume refused")
	}
	counts(t, &q, 0, 0, true)
}

func TestRemoveCurrentHeadAndCapacityReduction(t *testing.T) {
	var q Queue
	for n := 1; n <= 4; n++ {
		enqueue(t, &q, feedback(n))
	}
	tasks := q.Dispatch(epoch, true, 3, 2)
	q.Remove("org/repo", 4, "head-a")
	counts(t, &q, 1, 2, false)
	q.Finish(tasks[0].ID)
	if next := q.Dispatch(epoch, true, 2, 2); len(next) != 0 {
		t.Fatal("reduced capacity ignored")
	}
	q.Finish(tasks[1].ID)
	next := q.Dispatch(epoch, true, 2, 2)
	if len(next) != 1 || next[0].PRs[0].Number != 3 {
		t.Fatalf("next = %v", next)
	}
}

func TestInvalidFeedbackAndConcurrentEnqueue(t *testing.T) {
	var q Queue
	if err := q.Enqueue(Feedback{}); err == nil {
		t.Fatal("invalid feedback accepted")
	}
	var wg sync.WaitGroup
	for n := 1; n <= 20; n++ {
		wg.Add(1)
		go func(n int) { defer wg.Done(); _ = q.Enqueue(feedback(n)) }(n)
	}
	wg.Wait()
	counts(t, &q, 20, 0, false)
}
