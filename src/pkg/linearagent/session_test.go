package linearagent

import (
	"fmt"
	"testing"
	"time"
)

func TestTracker_ObserveAndSnapshotOrder(t *testing.T) {
	tr := NewTracker()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	tr.SetClock(func() time.Time { return now })

	ev1 := createdEvent()
	tr.Observe(ev1)
	now = now.Add(time.Minute)
	ev2 := createdEvent()
	ev2.AgentSession.ID = "sess-2"
	ev2.AgentSession.Issue.Identifier = "ENG-43"
	tr.Observe(ev2)

	snap := tr.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot = %d", len(snap))
	}
	if snap[0].ID != "sess-2" || snap[1].ID != "sess-1" {
		t.Errorf("order = %s, %s (want newest first)", snap[0].ID, snap[1].ID)
	}
	if snap[1].IssueIdentifier != "ENG-42" || snap[1].State != SessionStateAcked {
		t.Errorf("session = %+v", snap[1])
	}

	// Re-observing the same session updates, not duplicates.
	tr.Observe(ev1)
	if len(tr.Snapshot()) != 2 {
		t.Error("re-observe duplicated the session")
	}
}

func TestTracker_AgentBindingLifecycle(t *testing.T) {
	tr := NewTracker()
	tr.Observe(createdEvent())

	if _, ok := tr.ActiveSessionForAgent("scanner"); ok {
		t.Fatal("binding exists before SetAgent")
	}
	tr.SetAgent("sess-1", "scanner")
	id, ok := tr.ActiveSessionForAgent("scanner")
	if !ok || id != "sess-1" {
		t.Fatalf("binding = %q, %v", id, ok)
	}
	if tr.Snapshot()[0].State != SessionStateWorking {
		t.Errorf("state = %q", tr.Snapshot()[0].State)
	}

	tr.SetState("sess-1", SessionStateFinished, ActivityResponse)
	if _, ok := tr.ActiveSessionForAgent("scanner"); ok {
		t.Error("binding survives finish")
	}
	s := tr.Snapshot()[0]
	if s.State != SessionStateFinished || s.LastActivity != ActivityResponse {
		t.Errorf("session = %+v", s)
	}

	// SetAgent / SetState on an unknown session are safe no-ops.
	tr.SetAgent("nope", "scanner")
	tr.SetState("nope", SessionStateFailed, "")
}

func TestTracker_Eviction(t *testing.T) {
	tr := NewTracker()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	tr.SetClock(func() time.Time { return now })
	for i := 0; i < trackerCapacity+5; i++ {
		var ev SessionEvent
		ev.AgentSession.ID = fmt.Sprintf("sess-%03d", i)
		now = now.Add(time.Second)
		tr.Observe(ev)
		tr.SetAgent(ev.AgentSession.ID, fmt.Sprintf("agent-%03d", i))
	}
	snap := tr.Snapshot()
	if len(snap) != trackerCapacity {
		t.Fatalf("retained = %d, want %d", len(snap), trackerCapacity)
	}
	for _, s := range snap {
		if s.ID == "sess-000" {
			t.Error("oldest session not evicted")
		}
	}
	// Evicted sessions release their agent bindings too.
	if _, ok := tr.ActiveSessionForAgent("agent-000"); ok {
		t.Error("evicted session's binding retained")
	}
}
