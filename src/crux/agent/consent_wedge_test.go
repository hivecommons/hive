package agent

import (
	"reflect"
	"testing"
	"time"
)

// The consent-wedge tracker (#5577) answers one question for the heartbeat:
// which agents hit a consent-screen restart within the last hour? A wedge
// loop refreshes its stamp on every kick attempt, so "recent restart" IS the
// live-loop signal; an old stamp means the operator completed the flow.
func TestConsentWedgeTracker_WindowAndOrdering(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var tr consentWedgeTracker

	tr.note("quality", now.Add(-5*time.Minute))  // live wedge
	tr.note("scanner", now.Add(-59*time.Minute)) // still inside the window
	tr.note("outreach", now.Add(-2*time.Hour))   // resolved long ago
	tr.note("", now)                             // never recorded

	got := tr.wedged(now)
	if want := []string{"quality", "scanner"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("wedged = %v, want %v", got, want)
	}
}

// Empty must be a non-nil measurement: the heartbeat serializes [] so the hub
// clears a carried-forward wedge once it resolves, instead of reading null as
// "not measured" and keeping the stale alarm alive.
func TestConsentWedgeTracker_EmptyIsNonNil(t *testing.T) {
	var tr consentWedgeTracker
	got := tr.wedged(time.Now())
	if got == nil {
		t.Fatal("wedged returned nil; must be a non-nil measurement")
	}
	if len(got) != 0 {
		t.Fatalf("empty tracker reported %v", got)
	}
}

// A repeat sighting refreshes the stamp — the loop shape — and the Manager
// facade records through the tracker without touching m.mu (the recording
// sites hold it).
func TestManagerConsentWedge_RecordAndExpire(t *testing.T) {
	m := &Manager{}
	m.noteConsentWedge("quality")
	got := m.ConsentWedgedAgents()
	if want := []string{"quality"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ConsentWedgedAgents = %v, want %v", got, want)
	}

	// Age the stamp past the window: the wedge report must clear itself.
	m.consentWedges.mu.Lock()
	m.consentWedges.lastByAgent["quality"] = time.Now().Add(-consentWedgeWindow - time.Minute)
	m.consentWedges.mu.Unlock()
	if got := m.ConsentWedgedAgents(); len(got) != 0 {
		t.Fatalf("expired wedge still reported: %v", got)
	}
}
