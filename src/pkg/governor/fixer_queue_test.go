package governor

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFixerQueueFirstDeferralDeliveryAndClosure(t *testing.T) {
	g := &Governor{}
	at := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	g.RecordFixKickDeferred("org/repo#2", at.Add(time.Minute))
	g.RecordFixKickDeferred("ORG/REPO#1", at)
	g.RecordFixKickDeferred("org/other#1", at.Add(time.Minute))
	// A retry must preserve its original position, including equal-time ties.
	g.RecordFixKickDeferred("org/repo#1", at.Add(time.Hour))
	assertFixerQueue(t, g.FixerQueue(), "org/repo#1", "org/repo#2", "org/other#1")

	// Returned maps and ahead pointers cannot mutate governor state.
	snapshot := g.FixerQueue()
	*snapshot["org/repo#2"].FixerQueueAhead = 99
	delete(snapshot, "org/repo#1")
	assertFixerQueue(t, g.FixerQueue(), "org/repo#1", "org/repo#2", "org/other#1")

	g.RecordFixKickDelivered("ORG/REPO#1")
	assertFixerQueue(t, g.FixerQueue(), "org/repo#2", "org/other#1")
	g.ReconcileFixerQueue(map[string]bool{"org/other#1": true})
	assertFixerQueue(t, g.FixerQueue(), "org/other#1")
	g.RecordFixKickDelivered("org/other#1")
	if got := g.FixerQueue(); len(got) != 0 {
		t.Fatalf("delivered PRs still queued: %+v", got)
	}
	// A newly deferred kick after delivery starts a fresh wait.
	g.RecordFixKickDeferred("org/repo#2", at.Add(2*time.Hour))
	g.RecordFixKickDeferred("org/other#1", at.Add(3*time.Hour))
	assertFixerQueue(t, g.FixerQueue(), "org/repo#2", "org/other#1")
	g.ReconcileFixerQueue(nil)
	assertFixerQueue(t, g.FixerQueue())
}

func assertFixerQueue(t *testing.T, got map[string]FixerQueueStatus, refs ...string) {
	t.Helper()
	if len(got) != len(refs) {
		t.Fatalf("queue = %+v, want %v", got, refs)
	}
	for i, ref := range refs {
		entry := got[ref]
		if entry.FixerQueuePosition != i+1 || entry.FixerQueueAhead == nil || *entry.FixerQueueAhead != i {
			t.Fatalf("%s = %+v, want position %d / ahead %d", ref, entry, i+1, i)
		}
	}
}

func TestFixerQueueJSONOptionalFields(t *testing.T) {
	for _, tc := range []struct {
		status FixerQueueStatus
		want   string
	}{
		{FixerQueueStatus{}, `{}`},
		{FixerQueueStatus{FixerQueuePosition: 1, FixerQueueAhead: new(int)}, `{"fixer_queue_position":1,"fixer_queue_ahead":0}`},
	} {
		got, err := json.Marshal(tc.status)
		if err != nil || string(got) != tc.want {
			t.Fatalf("json = %s (%v), want %s", got, err, tc.want)
		}
	}
	var g *Governor
	g.RecordFixKickDeferred("org/repo#1", time.Now())
	g.RecordFixKickDelivered("org/repo#1")
	g.ReconcileFixerQueue(nil)
	assertFixerQueue(t, g.FixerQueue())
}
