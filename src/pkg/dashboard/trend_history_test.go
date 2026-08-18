package dashboard

import "testing"

// TestAppendTrendHistory verifies a single append lands and the throttle
// suppresses a rapid second append (same 5-min cadence as fact/cost history).
func TestAppendTrendHistory(t *testing.T) {
	s := newTestServer()

	if got := s.TrendHistory(); len(got) != 0 {
		t.Fatalf("fresh server trend history len = %d, want 0", len(got))
	}

	status := &StatusPayload{
		Governor: FrontendGovernor{Issues: 3, PRs: 2},
		Hold:     FrontendHold{Total: 4},
		Beads:    FrontendBeads{Workers: 5, Supervisor: 1},
		Repos: []FrontendRepo{
			{Name: "console", Issues: 2, PRs: 1},
		},
		SystemResources: &SystemResources{DiskPct: 40, MemPct: 55, CpuPct: 12},
	}
	s.AppendTrendHistory(status)

	got := s.TrendHistory()
	if len(got) != 1 {
		t.Fatalf("after one append len = %d, want 1", len(got))
	}
	e := got[0]
	if e.GovIssues != 3 || e.GovPrs != 2 || e.GovTotal != 5 {
		t.Errorf("governor counts = %d/%d/%d, want 3/2/5", e.GovIssues, e.GovPrs, e.GovTotal)
	}
	if e.GovHold != 4 {
		t.Errorf("hold = %d, want 4", e.GovHold)
	}
	if e.BeadsWorkers != 5 || e.BeadsSupervisor != 1 {
		t.Errorf("beads = %d/%d, want 5/1", e.BeadsWorkers, e.BeadsSupervisor)
	}
	if e.Repos["console"].Issues != 2 || e.Repos["console"].PRs != 1 {
		t.Errorf("repo snap = %+v, want {2 1}", e.Repos["console"])
	}
	if e.System == nil || e.System.DiskPct != 40 || e.System.MemPct != 55 || e.System.CpuPct != 12 {
		t.Errorf("system snap = %+v, want {40 55 12}", e.System)
	}
	if e.Timestamp == 0 {
		t.Error("entry Timestamp not set")
	}

	// A second immediate append is throttled (< trendHistoryMinIntervalMs apart).
	s.AppendTrendHistory(status)
	if got := s.TrendHistory(); len(got) != 1 {
		t.Fatalf("after throttled append len = %d, want 1 (throttle should suppress)", len(got))
	}
}

// TestAppendTrendHistoryNilStatus verifies a nil status is a no-op.
func TestAppendTrendHistoryNilStatus(t *testing.T) {
	s := newTestServer()
	s.AppendTrendHistory(nil)
	if got := s.TrendHistory(); len(got) != 0 {
		t.Fatalf("nil status appended an entry: len = %d, want 0", len(got))
	}
}

// TestAppendTrendHistoryOmitsEmptyOptionals verifies the repos map and system
// pointer are omitted (nil) when the status carries no repos / no resources.
func TestAppendTrendHistoryOmitsEmptyOptionals(t *testing.T) {
	s := &Server{}
	s.AppendTrendHistory(&StatusPayload{
		Governor: FrontendGovernor{Issues: 1},
	})
	got := s.TrendHistory()
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Repos != nil {
		t.Errorf("empty repos should be nil, got %v", got[0].Repos)
	}
	if got[0].System != nil {
		t.Errorf("absent system resources should be nil, got %v", got[0].System)
	}
}

// TestTrendHistoryReturnsCopy verifies TrendHistory hands back a copy so callers
// can't mutate the server's slice.
func TestTrendHistoryReturnsCopy(t *testing.T) {
	s := &Server{}
	s.AppendTrendHistory(&StatusPayload{Governor: FrontendGovernor{Issues: 7}})

	got := s.TrendHistory()
	got[0].GovIssues = 999

	again := s.TrendHistory()
	if again[0].GovIssues != 7 {
		t.Errorf("mutating returned slice leaked into server state: got %d", again[0].GovIssues)
	}
}

// TestSeedTrendHistoryCapAndOrdering verifies the seed path trims to the cap
// (keeping the most-recent tail) and preserves ordering.
func TestSeedTrendHistoryCapAndOrdering(t *testing.T) {
	s := newTestServer()

	over := trendHistoryMaxEntries + 10
	entries := make([]TrendHistoryEntry, over)
	for i := range entries {
		entries[i] = TrendHistoryEntry{Timestamp: int64(i), GovTotal: i}
	}
	s.SeedTrendHistory(entries)

	got := s.TrendHistory()
	if len(got) != trendHistoryMaxEntries {
		t.Fatalf("seeded len = %d, want cap %d", len(got), trendHistoryMaxEntries)
	}
	wantFirst := int64(over - trendHistoryMaxEntries)
	if got[0].Timestamp != wantFirst {
		t.Errorf("first kept timestamp = %d, want %d (tail should be retained)", got[0].Timestamp, wantFirst)
	}
	if got[len(got)-1].Timestamp != int64(over-1) {
		t.Errorf("last timestamp = %d, want %d", got[len(got)-1].Timestamp, over-1)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Timestamp <= got[i-1].Timestamp {
			t.Fatalf("ordering broken at %d: %d <= %d", i, got[i].Timestamp, got[i-1].Timestamp)
		}
	}
}
