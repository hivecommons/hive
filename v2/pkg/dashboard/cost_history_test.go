package dashboard

import "testing"

// TestAppendCostHistory verifies a single append lands and the throttle
// suppresses a rapid second append (same 5-min cadence as fact history).
func TestAppendCostHistory(t *testing.T) {
	s := newTestServer()

	if got := s.CostHistory(); len(got) != 0 {
		t.Fatalf("fresh server cost history len = %d, want 0", len(got))
	}

	s.AppendCostHistory(100.50)
	got := s.CostHistory()
	if len(got) != 1 {
		t.Fatalf("after one append len = %d, want 1", len(got))
	}
	if got[0].USD != 100.50 {
		t.Errorf("entry USD = %v, want 100.50", got[0].USD)
	}
	if got[0].Timestamp == 0 {
		t.Error("entry Timestamp not set")
	}

	// A second immediate append is throttled (< costHistoryMinIntervalMs apart).
	s.AppendCostHistory(200.75)
	if got := s.CostHistory(); len(got) != 1 {
		t.Fatalf("after throttled append len = %d, want 1 (throttle should suppress)", len(got))
	}
}

// TestCostHistoryReturnsCopy verifies CostHistory hands back a copy so callers
// can't mutate the server's slice.
func TestCostHistoryReturnsCopy(t *testing.T) {
	s := newTestServer()
	s.AppendCostHistory(42.0)

	got := s.CostHistory()
	got[0].USD = 999.0

	again := s.CostHistory()
	if again[0].USD != 42.0 {
		t.Errorf("mutating returned slice leaked into server state: got %v", again[0].USD)
	}
}

// TestSeedCostHistoryCapAndOrdering verifies the seed path trims to the cap
// (keeping the most-recent tail) and preserves ordering.
func TestSeedCostHistoryCapAndOrdering(t *testing.T) {
	s := newTestServer()

	// Build an over-cap ordered slice.
	over := costHistoryMaxEntries + 10
	entries := make([]CostHistoryEntry, over)
	for i := range entries {
		entries[i] = CostHistoryEntry{Timestamp: int64(i), USD: float64(i)}
	}
	s.SeedCostHistory(entries)

	got := s.CostHistory()
	if len(got) != costHistoryMaxEntries {
		t.Fatalf("seeded len = %d, want cap %d", len(got), costHistoryMaxEntries)
	}
	// The oldest (over - cap) entries should have been dropped; the tail kept.
	wantFirst := int64(over - costHistoryMaxEntries)
	if got[0].Timestamp != wantFirst {
		t.Errorf("first kept timestamp = %d, want %d (tail should be retained)", got[0].Timestamp, wantFirst)
	}
	if got[len(got)-1].Timestamp != int64(over-1) {
		t.Errorf("last timestamp = %d, want %d", got[len(got)-1].Timestamp, over-1)
	}
	// Ordering preserved (monotonic).
	for i := 1; i < len(got); i++ {
		if got[i].Timestamp <= got[i-1].Timestamp {
			t.Fatalf("ordering broken at %d: %d <= %d", i, got[i].Timestamp, got[i-1].Timestamp)
		}
	}
}

// TestAppendCostHistoryPerAgent verifies the optional per-agent map is stored
// on the entry and omitted when empty.
func TestAppendCostHistoryPerAgent(t *testing.T) {
	s := &Server{}
	s.AppendCostHistory(10.0, map[string]float64{"scanner": 7.5, "quality": 2.5})

	got := s.CostHistory()
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Agents["scanner"] != 7.5 || got[0].Agents["quality"] != 2.5 {
		t.Errorf("agents map = %v, want scanner=7.5 quality=2.5", got[0].Agents)
	}

	// Empty map should be dropped, not stored as {}.
	s2 := &Server{}
	s2.AppendCostHistory(1.0, map[string]float64{})
	if got := s2.CostHistory(); len(got) != 1 || got[0].Agents != nil {
		t.Errorf("empty agents map should be nil on the entry, got %v", got[0].Agents)
	}
}
