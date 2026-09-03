package governor

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func testGovernor() *Governor {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle":  {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "15m", "supervisor": "pause"}},
			"quiet": {Threshold: 2, Cadences: map[string]config.Cadence{"scanner": "10m"}},
			"busy":  {Threshold: 10, Cadences: map[string]config.Cadence{"scanner": "5m"}},
			"surge": {Threshold: 20, Cadences: map[string]config.Cadence{"scanner": "2m"}},
		},
	}
	agents := map[string]config.AgentConfig{
		"scanner":    {Backend: "claude", Model: "sonnet", Enabled: true},
		"supervisor": {Backend: "claude", Model: "opus", Enabled: true},
	}
	return New(cfg, agents, testLogger())
}

func TestModeHistory(t *testing.T) {
	g := testGovernor()
	history := g.ModeHistory()
	// New() records an initial startup ModeChange so the timeline is never empty.
	if len(history) != 1 {
		t.Errorf("expected 1 initial mode history entry (startup), got %d", len(history))
	}

	// Trigger mode change
	g.Evaluate(15, 0, 0, 0)
	history = g.ModeHistory()
	if len(history) != 2 {
		t.Errorf("expected 2 mode changes (startup + eval), got %d", len(history))
	}
}

func TestEvalHistory(t *testing.T) {
	g := testGovernor()
	history := g.EvalHistory()
	if len(history) != 0 {
		t.Errorf("expected empty eval history, got %d", len(history))
	}

	g.Evaluate(5, 2, 1, 0)
	history = g.EvalHistory()
	if len(history) != 1 {
		t.Fatalf("expected 1 eval, got %d", len(history))
	}
	if history[0].QueueIssues != 5 {
		t.Errorf("issues = %d, want 5", history[0].QueueIssues)
	}
}

func TestKickHistory(t *testing.T) {
	g := testGovernor()
	history := g.KickHistory()
	if len(history) != 0 {
		t.Errorf("expected empty kick history, got %d", len(history))
	}

	g.RecordKick("scanner")
	history = g.KickHistory()
	if len(history) != 1 {
		t.Fatalf("expected 1 kick, got %d", len(history))
	}
	if history[0].Agent != "scanner" {
		t.Errorf("agent = %q", history[0].Agent)
	}
}

func TestGetBudget(t *testing.T) {
	g := testGovernor()
	budget := g.GetBudget()
	if budget.WeeklyLimit != 0 {
		t.Errorf("weekly limit = %d", budget.WeeklyLimit)
	}
}

func TestSetBudgetLimit(t *testing.T) {
	g := testGovernor()
	g.SetBudgetLimit(500000)
	budget := g.GetBudget()
	if budget.WeeklyLimit != 500000 {
		t.Errorf("weekly limit = %d, want 500000", budget.WeeklyLimit)
	}
}

func TestSetBudgetIgnored(t *testing.T) {
	g := testGovernor()
	g.SetBudgetIgnored([]string{"outreach", "architect"})
	budget := g.GetBudget()
	if len(budget.IgnoredAgents) != 2 {
		t.Errorf("ignored agents len = %d", len(budget.IgnoredAgents))
	}
}

func TestSetBudgetResetAt(t *testing.T) {
	g := testGovernor()
	now := time.Now()
	g.SetBudgetResetAt(now)
	budget := g.GetBudget()
	if !budget.ResetAt.Equal(now) {
		t.Errorf("reset_at mismatch")
	}
}

func TestUpdateBudgetFromTotals_InitializesWindow(t *testing.T) {
	g := testGovernor()
	byAgent := map[string]int64{"scanner": 1000, "supervisor": 500}
	byModel := map[string]int64{"sonnet": 1200, "opus": 300}
	g.UpdateBudgetFromTotals(1500, byAgent, byModel)

	budget := g.GetBudget()
	if budget.ResetAt.IsZero() {
		t.Error("first update should open a budget window")
	}
	if budget.WindowBaseline != 1500 {
		t.Errorf("baseline = %d, want 1500", budget.WindowBaseline)
	}
	if budget.CurrentSpend != 0 {
		t.Errorf("current spend = %d, want 0 at window open", budget.CurrentSpend)
	}
	if budget.ByAgent["scanner"] != 1000 {
		t.Errorf("scanner spend = %d", budget.ByAgent["scanner"])
	}
}

func TestUpdateBudgetFromTotals_SpendIsWindowRelative(t *testing.T) {
	g := testGovernor()
	g.UpdateBudgetFromTotals(1000, nil, nil)
	g.UpdateBudgetFromTotals(1500, nil, nil)

	budget := g.GetBudget()
	if budget.CurrentSpend != 500 {
		t.Errorf("current spend = %d, want 500", budget.CurrentSpend)
	}
	if budget.WindowBaseline != 1000 {
		t.Errorf("baseline = %d, want 1000", budget.WindowBaseline)
	}
}

func TestUpdateBudgetFromTotals_RollsExpiredWindow(t *testing.T) {
	g := testGovernor()
	g.SeedBudget(900, nil, nil, time.Now().Add(-BudgetWindowDuration-time.Hour))
	g.UpdateBudgetFromTotals(2000, nil, nil)

	budget := g.GetBudget()
	if budget.WindowBaseline != 2000 {
		t.Errorf("baseline = %d, want 2000 after roll", budget.WindowBaseline)
	}
	if budget.CurrentSpend != 0 {
		t.Errorf("current spend = %d, want 0 after roll", budget.CurrentSpend)
	}
	if time.Since(budget.ResetAt) > time.Minute {
		t.Errorf("reset_at not advanced on roll: %v", budget.ResetAt)
	}
}

func TestUpdateBudgetFromTotals_RebaselinesOnShrink(t *testing.T) {
	g := testGovernor()
	g.UpdateBudgetFromTotals(1000, nil, nil)
	// Session files pruned: lifetime totals drop below the baseline.
	g.UpdateBudgetFromTotals(400, nil, nil)

	budget := g.GetBudget()
	if budget.WindowBaseline != 400 {
		t.Errorf("baseline = %d, want 400 after shrink", budget.WindowBaseline)
	}
	if budget.CurrentSpend != 0 {
		t.Errorf("current spend = %d, want 0 after shrink", budget.CurrentSpend)
	}
}

func TestModeToConfigKey(t *testing.T) {
	tests := []struct {
		mode Mode
		want string
	}{
		{ModeSurge, "surge"},
		{ModeBusy, "busy"},
		{ModeQuiet, "quiet"},
		{ModeIdle, "idle"},
		{Mode("UNKNOWN"), "idle"},
	}
	for _, tt := range tests {
		got := modeToConfigKey(tt.mode)
		if got != tt.want {
			t.Errorf("modeToConfigKey(%v) = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

func TestFormatStatus(t *testing.T) {
	g := testGovernor()
	g.Evaluate(5, 3, 1, 2)
	status := g.FormatStatus()
	if status == "" {
		t.Error("expected non-empty status")
	}
}

func TestHistoryCapacity(t *testing.T) {
	g := testGovernor()
	// Fill eval history beyond capacity
	for i := 0; i < evalHistoryCapacity+10; i++ {
		g.Evaluate(i%30, 0, 0, 0)
	}
	history := g.EvalHistory()
	if len(history) > evalHistoryCapacity {
		t.Errorf("eval history len = %d, max = %d", len(history), evalHistoryCapacity)
	}
}

func TestKickHistoryCapacity(t *testing.T) {
	g := testGovernor()
	for i := 0; i < kickHistoryCapacity+10; i++ {
		g.RecordKick("scanner")
	}
	history := g.KickHistory()
	if len(history) > kickHistoryCapacity {
		t.Errorf("kick history len = %d, max = %d", len(history), kickHistoryCapacity)
	}
}

func TestThresholdFor_DefaultValues(t *testing.T) {
	cfg := config.GovernorConfig{Modes: map[string]config.ModeConfig{}}
	agents := map[string]config.AgentConfig{}
	g := New(cfg, agents, testLogger())

	// Access the private method via Evaluate behavior
	// The default thresholds are surge=20, busy=10, quiet=2
	g.Evaluate(25, 0, 0, 0)
	state := g.GetState()
	if state.Mode != ModeSurge {
		t.Errorf("mode = %v, want SURGE for 25 issues", state.Mode)
	}

	g.Evaluate(15, 0, 0, 0)
	state = g.GetState()
	if state.Mode != ModeBusy {
		t.Errorf("mode = %v, want BUSY for 15 issues", state.Mode)
	}

	g.Evaluate(5, 0, 0, 0)
	state = g.GetState()
	if state.Mode != ModeQuiet {
		t.Errorf("mode = %v, want QUIET for 5 issues", state.Mode)
	}

	g.Evaluate(0, 0, 0, 0)
	state = g.GetState()
	if state.Mode != ModeIdle {
		t.Errorf("mode = %v, want IDLE for 0 issues", state.Mode)
	}
}

func TestAgentsDueForKick(t *testing.T) {
	g := testGovernor()

	// Evaluate to set cadences
	g.Evaluate(5, 0, 0, 0)

	// Record a recent kick — should NOT be due again
	g.RecordKick("scanner")

	g.mu.Lock()
	due := g.agentsDueForKick()
	g.mu.Unlock()

	for _, name := range due {
		if name == "scanner" {
			t.Error("scanner should not be due immediately after kick")
		}
	}
}

func TestUpdateCadences_InvalidDuration(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "not-a-duration"}},
		},
	}
	agents := map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}
	g := New(cfg, agents, testLogger())

	// Should not panic, just log warning
	g.Evaluate(0, 0, 0, 0)
	state := g.GetState()
	// scanner cadence should not be set since the duration was invalid
	if c, ok := state.Cadences["scanner"]; ok && c.Interval != 0 {
		t.Errorf("expected zero interval for invalid duration, got %v", c.Interval)
	}
}

func TestSeedEvalHistory_Empty(t *testing.T) {
	g := testGovernor()
	g.SeedEvalHistory(nil)
	history := g.EvalHistory()
	if len(history) != 0 {
		t.Errorf("expected empty after nil seed, got %d", len(history))
	}
}

func TestSeedEvalHistory_Normal(t *testing.T) {
	g := testGovernor()
	snapshots := []EvalSnapshot{
		{Timestamp: 1, QueueIssues: 5, Mode: ModeQuiet},
		{Timestamp: 2, QueueIssues: 15, Mode: ModeBusy},
	}
	g.SeedEvalHistory(snapshots)
	history := g.EvalHistory()
	if len(history) != 2 {
		t.Fatalf("expected 2, got %d", len(history))
	}
	if history[0].QueueIssues != 5 {
		t.Errorf("first entry issues = %d", history[0].QueueIssues)
	}
}

func TestSeedEvalHistory_OverCapacity(t *testing.T) {
	g := testGovernor()
	big := make([]EvalSnapshot, evalHistoryCapacity+50)
	for i := range big {
		big[i] = EvalSnapshot{Timestamp: int64(i), QueueIssues: i}
	}
	g.SeedEvalHistory(big)
	history := g.EvalHistory()
	if len(history) != evalHistoryCapacity {
		t.Errorf("expected capped at %d, got %d", evalHistoryCapacity, len(history))
	}
	// Should keep the last N entries
	if history[0].QueueIssues != 50 {
		t.Errorf("first entry issues = %d, want 50", history[0].QueueIssues)
	}
}

func TestModeHistoryCapacity(t *testing.T) {
	g := testGovernor()
	// Fill mode history beyond capacity by toggling between modes
	for i := 0; i < modeHistoryCapacity+10; i++ {
		if i%2 == 0 {
			g.Evaluate(25, 0, 0, 0) // surge
		} else {
			g.Evaluate(0, 0, 0, 0) // idle
		}
	}
	history := g.ModeHistory()
	if len(history) > modeHistoryCapacity {
		t.Errorf("mode history len = %d, max = %d", len(history), modeHistoryCapacity)
	}
}

func TestAgentsDueForKick_ZeroInterval(t *testing.T) {
	// Test that agents with zero cadence interval are skipped
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "0"}},
		},
	}
	agents := map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Enabled: true},
	}
	g := New(cfg, agents, testLogger())
	g.Evaluate(0, 0, 0, 0)

	g.mu.Lock()
	due := g.agentsDueForKick()
	g.mu.Unlock()

	for _, name := range due {
		if name == "scanner" {
			t.Error("scanner with zero interval should not be due")
		}
	}
}

func TestSeedModeHistory_Empty(t *testing.T) {
	g := testGovernor()
	g.SeedModeHistory(nil)
	history := g.ModeHistory()
	if len(history) != 0 {
		t.Errorf("expected empty after nil seed, got %d", len(history))
	}
}

func TestSeedModeHistory_Normal(t *testing.T) {
	g := testGovernor()
	changes := []ModeChange{
		{Timestamp: time.Now().Add(-2 * time.Hour), From: ModeIdle, To: ModeQuiet, Reason: "test"},
		{Timestamp: time.Now().Add(-1 * time.Hour), From: ModeQuiet, To: ModeBusy, Reason: "test"},
	}
	g.SeedModeHistory(changes)
	history := g.ModeHistory()
	if len(history) != 2 {
		t.Fatalf("expected 2, got %d", len(history))
	}
	if history[0].To != ModeQuiet {
		t.Errorf("first entry to = %v", history[0].To)
	}
	if history[1].To != ModeBusy {
		t.Errorf("second entry to = %v", history[1].To)
	}
}

func TestSeedModeHistory_OverCapacity(t *testing.T) {
	g := testGovernor()
	big := make([]ModeChange, modeHistoryCapacity+50)
	for i := range big {
		big[i] = ModeChange{
			Timestamp: time.Now().Add(time.Duration(-i) * time.Minute),
			From:      ModeIdle,
			To:        ModeQuiet,
			Reason:    "test",
		}
	}
	g.SeedModeHistory(big)
	history := g.ModeHistory()
	if len(history) != modeHistoryCapacity {
		t.Errorf("expected capped at %d, got %d", modeHistoryCapacity, len(history))
	}
}

func TestUpdateCadences_PausedAgent(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "paused"}},
		},
	}
	agents := map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}
	g := New(cfg, agents, testLogger())
	g.Evaluate(0, 0, 0, 0)

	state := g.GetState()
	if c, ok := state.Cadences["scanner"]; ok {
		if !c.Paused {
			t.Error("scanner should be paused")
		}
	}
}
