package governor

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// ---------------------------------------------------------------------------
// SeedLastKicks
// ---------------------------------------------------------------------------

func TestSeedLastKicks(t *testing.T) {
	g := testGovernor()

	now := time.Now()
	kicks := map[string]time.Time{
		"scanner":    now.Add(-5 * time.Minute),
		"supervisor": now.Add(-10 * time.Minute),
	}
	g.SeedLastKicks(kicks)

	state := g.GetState()
	if state.LastKick["scanner"].IsZero() {
		t.Error("expected scanner last kick to be set")
	}
	if state.LastKick["supervisor"].IsZero() {
		t.Error("expected supervisor last kick to be set")
	}
}

func TestSeedLastKicks_Empty(t *testing.T) {
	g := testGovernor()
	g.SeedLastKicks(map[string]time.Time{})
	state := g.GetState()
	if len(state.LastKick) != 0 {
		t.Errorf("expected empty last kick map, got %d", len(state.LastKick))
	}
}

// ---------------------------------------------------------------------------
// SeedKickHistory
// ---------------------------------------------------------------------------

func TestSeedKickHistory(t *testing.T) {
	g := testGovernor()

	records := []KickRecord{
		{Timestamp: time.Now().Add(-2 * time.Hour), Agent: "scanner"},
		{Timestamp: time.Now().Add(-1 * time.Hour), Agent: "supervisor"},
	}
	g.SeedKickHistory(records)

	history := g.KickHistory()
	if len(history) != 2 {
		t.Fatalf("expected 2 kick records, got %d", len(history))
	}
	if history[0].Agent != "scanner" {
		t.Errorf("first agent = %q", history[0].Agent)
	}
}

func TestSeedKickHistory_OverCapacity(t *testing.T) {
	g := testGovernor()

	records := make([]KickRecord, kickHistoryCapacity+10)
	for i := range records {
		records[i] = KickRecord{Timestamp: time.Now(), Agent: "scanner"}
	}
	g.SeedKickHistory(records)

	history := g.KickHistory()
	if len(history) != kickHistoryCapacity {
		t.Errorf("expected capped at %d, got %d", kickHistoryCapacity, len(history))
	}
}

func TestSeedKickHistory_Empty(t *testing.T) {
	g := testGovernor()
	g.SeedKickHistory(nil)
	history := g.KickHistory()
	if len(history) != 0 {
		t.Errorf("expected empty, got %d", len(history))
	}
}

// ---------------------------------------------------------------------------
// SeedBudget
// ---------------------------------------------------------------------------

func TestSeedBudget(t *testing.T) {
	g := testGovernor()

	resetAt := time.Now().Add(7 * 24 * time.Hour)
	byAgent := map[string]int64{"scanner": 5000, "supervisor": 3000}
	byModel := map[string]int64{"sonnet": 4000, "opus": 4000}

	g.SeedBudget(8000, byAgent, byModel, resetAt)

	budget := g.GetBudget()
	if budget.CurrentSpend != 8000 {
		t.Errorf("spend = %d, want 8000", budget.CurrentSpend)
	}
	if budget.ByAgent["scanner"] != 5000 {
		t.Errorf("scanner = %d, want 5000", budget.ByAgent["scanner"])
	}
	if budget.ByModel["sonnet"] != 4000 {
		t.Errorf("sonnet = %d, want 4000", budget.ByModel["sonnet"])
	}
	if !budget.ResetAt.Equal(resetAt) {
		t.Errorf("resetAt mismatch")
	}
}

func TestSeedBudget_EmptyMaps(t *testing.T) {
	g := testGovernor()
	g.SeedBudget(100, nil, nil, time.Time{})

	budget := g.GetBudget()
	if budget.CurrentSpend != 100 {
		t.Errorf("spend = %d, want 100", budget.CurrentSpend)
	}
}

// ---------------------------------------------------------------------------
// SetMode
// ---------------------------------------------------------------------------

func TestSetMode(t *testing.T) {
	g := testGovernor()

	g.SetMode(ModeSurge)
	state := g.GetState()
	if state.Mode != ModeSurge {
		t.Errorf("mode = %v, want SURGE", state.Mode)
	}

	g.SetMode(ModeIdle)
	state = g.GetState()
	if state.Mode != ModeIdle {
		t.Errorf("mode = %v, want IDLE", state.Mode)
	}
}

// ---------------------------------------------------------------------------
// SeedLastEval
// ---------------------------------------------------------------------------

func TestSeedLastEval(t *testing.T) {
	g := testGovernor()

	ts := time.Now().Add(-30 * time.Minute)
	g.SeedLastEval(ts)

	state := g.GetState()
	if !state.LastEval.Equal(ts) {
		t.Errorf("lastEval mismatch")
	}
}

// ---------------------------------------------------------------------------
// SeedQueueState
// ---------------------------------------------------------------------------

func TestSeedQueueState(t *testing.T) {
	g := testGovernor()

	g.SeedQueueState(10, 5, 3, 2)

	state := g.GetState()
	if state.QueueIssues != 10 {
		t.Errorf("issues = %d, want 10", state.QueueIssues)
	}
	if state.QueuePRs != 5 {
		t.Errorf("prs = %d, want 5", state.QueuePRs)
	}
	if state.QueueHold != 3 {
		t.Errorf("hold = %d, want 3", state.QueueHold)
	}
	if state.SLAViolations != 2 {
		t.Errorf("sla = %d, want 2", state.SLAViolations)
	}
}

// ---------------------------------------------------------------------------
// GetBudget returns copies
// ---------------------------------------------------------------------------

func TestGetBudget_ReturnsCopy(t *testing.T) {
	g := testGovernor()
	g.UpdateBudgetFromTotals(1000, map[string]int64{"scanner": 500}, map[string]int64{"sonnet": 500})

	budget := g.GetBudget()
	budget.ByAgent["scanner"] = 9999 // mutate copy

	budget2 := g.GetBudget()
	if budget2.ByAgent["scanner"] != 500 {
		t.Error("GetBudget should return a copy")
	}
}

// ---------------------------------------------------------------------------
// thresholdFor defaults
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// AttachRepoSnapshots
// ---------------------------------------------------------------------------

func TestAttachRepoSnapshots(t *testing.T) {
	g := testGovernor()
	g.Evaluate(5, 0, 0, 0)

	repos := map[string]RepoSnapshot{
		"repo1": {Issues: 10, PRs: 3},
	}
	g.AttachRepoSnapshots(repos)

	history := g.EvalHistory()
	last := history[len(history)-1]
	if last.Repos == nil {
		t.Error("expected repos attached")
	}
	if last.Repos["repo1"].Issues != 10 {
		t.Errorf("repo1 issues = %d, want 10", last.Repos["repo1"].Issues)
	}
}

func TestAttachRepoSnapshots_EmptyHistory(t *testing.T) {
	g := testGovernor()
	// Should not panic with empty history
	g.AttachRepoSnapshots(map[string]RepoSnapshot{})
}

func TestThresholdFor_AllDefaults(t *testing.T) {
	cfg := config.GovernorConfig{Modes: map[string]config.ModeConfig{}}
	g := New(cfg, nil, testLogger())

	// Test all default thresholds by driving evaluation
	g.Evaluate(25, 0, 0, 0)
	if g.GetState().Mode != ModeSurge {
		t.Error("expected SURGE for 25 issues with default threshold=20")
	}

	g.Evaluate(15, 0, 0, 0)
	if g.GetState().Mode != ModeBusy {
		t.Error("expected BUSY for 15 issues with default threshold=10")
	}

	g.Evaluate(3, 0, 0, 0)
	if g.GetState().Mode != ModeQuiet {
		t.Error("expected QUIET for 3 issues with default threshold=2")
	}

	g.Evaluate(1, 0, 0, 0)
	if g.GetState().Mode != ModeIdle {
		t.Error("expected IDLE for 1 issue")
	}
}
