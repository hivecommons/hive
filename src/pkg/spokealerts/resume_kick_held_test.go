package spokealerts

import (
	"strings"
	"testing"
	"time"
)

type heldSink struct {
	added   map[string]string
	cleared []string
}

func (s *heldSink) AddSystemAlert(id, severity, message string) {
	if s.added == nil {
		s.added = map[string]string{}
	}
	s.added[id] = severity + "|" + message
}

func (s *heldSink) ClearSystemAlert(id string) { s.cleared = append(s.cleared, id) }

// TestResumeKickHeld_RaisesPerAgentAndClearsOnLaterKick: a held agent gets a
// warning naming it; a kick recorded AFTER the hold clears it, an older kick
// does not, and an agent that was never held is untouched.
func TestResumeKickHeld_RaisesPerAgentAndClearsOnLaterKick(t *testing.T) {
	now := time.Date(2026, 9, 28, 21, 9, 50, 0, time.UTC)
	r := NewResumeKickHeld()
	r.now = func() time.Time { return now }
	sink := &heldSink{}

	r.Apply(sink, []string{"scanner", "reviewer"}, func(a string) bool { return a == "scanner" })
	if got := sink.added[ResumeKickHeldAlertID("scanner")]; !strings.HasPrefix(got, "warning|") || !strings.Contains(got, "OOM-killed") || !strings.Contains(got, "memory limit") {
		t.Fatalf("scanner alert = %q", got)
	}
	if got := sink.added[ResumeKickHeldAlertID("reviewer")]; strings.Contains(got, "OOM") || !strings.Contains(got, "agent reviewer is idle") {
		t.Fatalf("reviewer alert = %q", got)
	}

	kicks := map[string]time.Time{"scanner": now.Add(-time.Minute), "reviewer": now.Add(2 * time.Minute)}
	r.ClearKicked(sink, func(a string) (time.Time, bool) { t, ok := kicks[a]; return t, ok })
	if len(sink.cleared) != 1 || sink.cleared[0] != ResumeKickHeldAlertID("reviewer") {
		t.Fatalf("cleared = %v, want only reviewer", sink.cleared)
	}
	if _, still := r.heldAt["scanner"]; !still {
		t.Fatal("scanner should remain held: its last kick predates the hold")
	}

	kicks["scanner"] = now.Add(time.Second)
	r.ClearKicked(sink, func(a string) (time.Time, bool) { t, ok := kicks[a]; return t, ok })
	if len(sink.cleared) != 2 || len(r.heldAt) != 0 {
		t.Fatalf("after later kick cleared=%v heldAt=%v", sink.cleared, r.heldAt)
	}
}

// TestResumeKickHeld_ReapplyKeepsOriginalHoldTime: a second refusal on the
// next eval cycle must not move the hold forward, or a kick that landed in
// between would never clear the alert.
func TestResumeKickHeld_ReapplyKeepsOriginalHoldTime(t *testing.T) {
	now := time.Date(2026, 9, 28, 21, 0, 0, 0, time.UTC)
	r := NewResumeKickHeld()
	r.now = func() time.Time { return now }
	sink := &heldSink{}
	r.Apply(sink, []string{"scanner"}, nil)
	r.now = func() time.Time { return now.Add(5 * time.Minute) }
	r.Apply(sink, []string{"scanner"}, nil)
	if got := r.heldAt["scanner"]; !got.Equal(now) {
		t.Fatalf("heldAt = %v, want original %v", got, now)
	}
	r.ClearKicked(sink, func(string) (time.Time, bool) { return now.Add(time.Minute), true })
	if len(sink.cleared) != 1 {
		t.Fatalf("kick between refusals should clear; cleared=%v", sink.cleared)
	}
}
