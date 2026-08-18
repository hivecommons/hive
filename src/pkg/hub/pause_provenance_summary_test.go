package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// #4041 hub-side provenance: the per-agent summaries riding the heartbeat
// into the My Hives payload must carry WHO/WHY/WHEN for a paused agent, so
// the fleet view can tell a deliberate owner quiesce from a malfunction.

func TestNewAgentSummary_CarriesPauseProvenance(t *testing.T) {
	pausedAt := time.Date(2026, 8, 14, 16, 10, 10, 0, time.UTC)
	as := NewAgentSummary("scanner", "paused", "", AgentActivity{
		Paused:        true,
		PausedTrigger: "dashboard-api",
		PausedReason:  "manual pause",
		PausedBy:      "bketelsen",
		PausedAt:      pausedAt,
	})
	if !as.Paused {
		t.Fatal("Paused not set")
	}
	if as.PausedTrigger != "dashboard-api" {
		t.Errorf("PausedTrigger = %q, want %q", as.PausedTrigger, "dashboard-api")
	}
	if as.PausedReason != "manual pause" {
		t.Errorf("PausedReason = %q, want %q", as.PausedReason, "manual pause")
	}
	if as.PausedBy != "bketelsen" {
		t.Errorf("PausedBy = %q, want %q", as.PausedBy, "bketelsen")
	}
	if as.PausedAt != "2026-08-14T16:10:10Z" {
		t.Errorf("PausedAt = %q, want RFC3339 %q", as.PausedAt, "2026-08-14T16:10:10Z")
	}
}

// A zero PausedAt must serialize as ABSENT — never as a bogus year-1 string
// the fleet view would render as a 2000-year-old pause.
func TestNewAgentSummary_ZeroPausedAtOmitted(t *testing.T) {
	as := NewAgentSummary("scanner", "running", "", AgentActivity{})
	if as.PausedAt != "" {
		t.Fatalf("PausedAt = %q, want empty for a zero time", as.PausedAt)
	}
	raw, err := json.Marshal(as)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, field := range []string{"pausedAt", "pausedTrigger", "pausedReason", "pausedBy"} {
		if strings.Contains(string(raw), field) {
			t.Errorf("non-paused agent summary leaks %q: %s", field, raw)
		}
	}
}
