package fixqueue

import (
	"testing"
	"time"
)

// Every event that takes a PR out of a queue or a dispatch carries a reason
// on its log line: a queued fix once left the queue with a bare "remove"
// line and nothing else, and no one could tell which path had taken it.

type loggedEvent struct {
	event, reason string
	prs           []string
}

func captureEvents(s *Scheduler) *[]loggedEvent {
	var got []loggedEvent
	s.SetLogger(func(event, _ string, d Dispatch, _ int) {
		var prs []string
		for _, it := range d.Items {
			prs = append(prs, it.Key())
		}
		got = append(got, loggedEvent{event: event, reason: d.Reason, prs: prs})
	})
	return &got
}

func lastEvent(t *testing.T, got []loggedEvent, event string) loggedEvent {
	t.Helper()
	for i := len(got) - 1; i >= 0; i-- {
		if got[i].event == event {
			return got[i]
		}
	}
	t.Fatalf("no %q event in %+v", event, got)
	return loggedEvent{}
}

func TestRemoveForLogsTheCallersReason(t *testing.T) {
	s := New(Options{})
	got := captureEvents(s)
	s.Enqueue("ann", Item{Repo: "acme/widgets", Number: 1})
	s.Enqueue("ann", Item{Repo: "acme/widgets", Number: 2})

	if s.RemoveFor("ann", "acme/widgets", 1, "left-stage-072") {
		t.Fatal("removing a queued item frees no slot")
	}
	if ev := lastEvent(t, *got, "remove"); ev.reason != "left-stage-072" || len(ev.prs) != 1 || ev.prs[0] != "acme/widgets#1" {
		t.Fatalf("remove event = %+v, want acme/widgets#1 with reason left-stage-072", ev)
	}
	s.Remove("ann", "acme/widgets", 2)
	if ev := lastEvent(t, *got, "remove"); ev.reason != ReasonUnspecified {
		t.Fatalf("plain Remove logged reason %q, want %q", ev.reason, ReasonUnspecified)
	}
}

func TestRemoveForInFlightLogsRemoveAndDequeue(t *testing.T) {
	s := New(Options{})
	got := captureEvents(s)
	s.Enqueue("bo", Item{Repo: "acme/widgets", Number: 3})
	if d := s.Dispatch("bo", Capacity{Max: 1}); d == nil {
		t.Fatal("expected a dispatch")
	}
	if !s.RemoveFor("bo", "acme/widgets", 3, "pr-closed") {
		t.Fatal("removing the only in-flight item frees its slot")
	}
	// Before, an in-flight removal logged nothing unless it emptied the
	// dispatch, and then only a bare "dequeue".
	if ev := lastEvent(t, *got, "remove"); ev.reason != "pr-closed" || len(ev.prs) != 1 || ev.prs[0] != "acme/widgets#3" {
		t.Fatalf("remove event = %+v", ev)
	}
	if ev := lastEvent(t, *got, "dequeue"); ev.reason != "pr-closed" {
		t.Fatalf("dequeue event = %+v, want the removal's reason", ev)
	}
}

func TestUnwindAndOverflowLogTheirReasons(t *testing.T) {
	now := time.Now()
	s := New(Options{Now: func() time.Time { return now }})
	got := captureEvents(s)
	cap1 := Capacity{Max: 1}

	s.Enqueue("cy", Item{Repo: "acme/widgets", Number: 4})
	d := s.Dispatch("cy", cap1)
	s.Return("cy", d.ID, time.Minute)
	if ev := lastEvent(t, *got, "return"); ev.reason != ReasonRelayNotFree {
		t.Fatalf("return event = %+v", ev)
	}
	d = s.DispatchAtBoundary("cy", cap1)
	s.Requeue("cy", d.ID, time.Minute)
	if ev := lastEvent(t, *got, "requeue"); ev.reason != ReasonRelayFailed {
		t.Fatalf("requeue event = %+v", ev)
	}
	d = s.DispatchAtBoundary("cy", cap1)
	s.Release("cy", d.ID)
	if ev := lastEvent(t, *got, "release"); ev.reason != ReasonRelayNeverReplied {
		t.Fatalf("release event = %+v", ev)
	}
	d = s.DispatchAtBoundary("cy", cap1)
	s.CompleteDispatch("cy", d.ID)
	if ev := lastEvent(t, *got, "dequeue"); ev.reason != ReasonFixFinished {
		t.Fatalf("dequeue event = %+v", ev)
	}

	s.Enqueue("cy", Item{Repo: "acme/widgets", Number: 5})
	now = now.Add(time.Hour)
	if out := s.Overflow("cy", time.Minute); len(out) != 1 {
		t.Fatalf("overflow = %+v", out)
	}
	if ev := lastEvent(t, *got, "overflow"); ev.reason != ReasonOverflow {
		t.Fatalf("overflow event = %+v", ev)
	}
	if ev := lastEvent(t, *got, "dispatch"); ev.reason != "" {
		t.Fatalf("a dispatch handed out carries no reason: %+v", ev)
	}
}
