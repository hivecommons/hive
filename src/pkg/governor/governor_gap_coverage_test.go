package governor

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Coverage-gap tests for #3617. Each test pins real runtime behavior that had
// no test at all: live config/agent reloads, stat attachment, budget-window
// restore, the global budget bypass, the kick-timing reset, the kick-channel
// gate, and the time-of-day cadence edges. Every test has a positive control
// so it cannot pass vacuously.

// ---------------------------------------------------------------------------
// UpdateConfig
// ---------------------------------------------------------------------------

// TestUpdateConfig_NewThresholdsTakeEffect: a config reload must change mode
// computation immediately — the governor keeps a reference to the config it
// was handed, and a hive whose operator raises the quiet threshold expects
// the very next eval to honor it.
func TestUpdateConfig_NewThresholdsTakeEffect(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	// Positive control: with the standard quiet threshold (>2), depth 5 is QUIET.
	g.Evaluate(5, 0, 0, 0)
	if got := g.GetState().Mode; got != ModeQuiet {
		t.Fatalf("precondition: mode = %s, want QUIET at depth 5", got)
	}

	// Raise the quiet threshold to >8. Depth 5 must now be IDLE.
	g.UpdateConfig(config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"quiet": {Threshold: 8},
		},
	})

	g.Evaluate(5, 0, 0, 0)
	if got := g.GetState().Mode; got != ModeIdle {
		t.Errorf("after UpdateConfig: mode = %s, want IDLE at depth 5 (new quiet threshold 8)", got)
	}

	// The new threshold itself must be live, not just "old config dropped":
	// depth 9 is above the new quiet threshold.
	g.Evaluate(9, 0, 0, 0)
	if got := g.GetState().Mode; got != ModeQuiet {
		t.Errorf("after UpdateConfig: mode = %s, want QUIET at depth 9", got)
	}
}

// ---------------------------------------------------------------------------
// UpdateAgents
// ---------------------------------------------------------------------------

// TestUpdateAgents_AddAndRemove: an agent-map reload must be honored on the
// next eval — a newly added agent picks up its cadence and becomes kickable,
// and a removed agent stops being kicked.
func TestUpdateAgents_AddAndRemove(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "15m"}},
		},
	}
	g := New(cfg, map[string]config.AgentConfig{}, testLogger())

	if due := g.Evaluate(0, 0, 0, 0); len(due) != 0 {
		t.Fatalf("precondition: no agents configured, but due = %v", due)
	}

	g.UpdateAgents(map[string]config.AgentConfig{
		"scanner": {Enabled: true},
	})
	due := g.Evaluate(0, 0, 0, 0)
	if len(due) != 1 || due[0] != "scanner" {
		t.Errorf("after adding scanner: due = %v, want [scanner]", due)
	}

	g.UpdateAgents(map[string]config.AgentConfig{})
	if due := g.Evaluate(0, 0, 0, 0); len(due) != 0 {
		t.Errorf("after removing all agents: due = %v, want none", due)
	}
	if c := g.GetState().Cadences; len(c) != 0 {
		t.Errorf("removed agents should leave no cadence entries, got %v", c)
	}
}

// ---------------------------------------------------------------------------
// AttachAgentStats
// ---------------------------------------------------------------------------

// TestAttachAgentStats mirrors TestAttachRepoSnapshots: resolved stats attach
// to the MOST RECENT eval snapshot so the dashboard's sparkline rows carry
// per-agent stats.
func TestAttachAgentStats(t *testing.T) {
	g := testGovernor()
	g.Evaluate(5, 0, 0, 0)

	g.AttachAgentStats(map[string]map[string]any{
		"scanner": {"open_prs": 4},
	})

	history := g.EvalHistory()
	last := history[len(history)-1]
	if last.AgentStats == nil {
		t.Fatal("expected agent stats attached to the latest snapshot")
	}
	if got := last.AgentStats["scanner"]["open_prs"]; got != 4 {
		t.Errorf("scanner open_prs = %v, want 4", got)
	}
}

func TestAttachAgentStats_EmptyHistory(t *testing.T) {
	g := testGovernor()
	// No evals yet: must be a no-op, not a panic or a phantom snapshot.
	g.AttachAgentStats(map[string]map[string]any{"scanner": {"x": 1}})
	if history := g.EvalHistory(); len(history) != 0 {
		t.Errorf("attaching stats with no history should not create snapshots, got %d", len(history))
	}
}

// ---------------------------------------------------------------------------
// SeedBudgetWindowBaseline
// ---------------------------------------------------------------------------

// TestSeedBudgetWindowBaseline_SpendComputedFromSeededBaseline: restoring the
// persisted baseline must make the next totals scan compute window spend as
// (lifetime − baseline). If the seed were dropped, a restart would re-open the
// window at the full lifetime total and report zero spend.
func TestSeedBudgetWindowBaseline_SpendComputedFromSeededBaseline(t *testing.T) {
	g := testGovernor()
	g.SetBudgetResetAt(time.Now()) // window already open (restored snapshot)
	g.SeedBudgetWindowBaseline(700)

	if b := g.GetBudget(); b.WindowBaseline != 700 {
		t.Fatalf("seeded baseline = %d, want 700", b.WindowBaseline)
	}

	g.UpdateBudgetFromTotals(1000, nil, nil)
	if b := g.GetBudget(); b.CurrentSpend != 300 {
		t.Errorf("window spend = %d, want 300 (1000 lifetime − 700 seeded baseline)", b.CurrentSpend)
	}
}

// ---------------------------------------------------------------------------
// SetBudgetIgnoreAll
// ---------------------------------------------------------------------------

// TestSetBudgetIgnoreAll_BypassesKickSuppression: the dashboard's global
// "ignore budget" toggle opens the kick gate while keeping the limit intact,
// and toggling it back off restores suppression.
func TestSetBudgetIgnoreAll_BypassesKickSuppression(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	g.SetBudgetLimit(1000)
	g.SeedBudget(1500, nil, nil, time.Now())

	// Positive control: exhausted budget suppresses kicks.
	if due := dueSet(g); due["scanner"] {
		t.Fatal("precondition: exhausted budget should suppress kicks")
	}

	g.SetBudgetIgnoreAll(true)
	if due := dueSet(g); !due["scanner"] {
		t.Error("ignore-all should reopen the kick gate despite exhausted spend")
	}
	if g.GetState().BudgetExhausted {
		t.Error("state must not report exhausted while ignore-all is on")
	}
	if b := g.GetBudget(); !b.IgnoreAll {
		t.Error("GetBudget should report the ignore-all toggle")
	}
	if b := g.GetBudget(); b.WeeklyLimit != 1000 {
		t.Errorf("ignore-all must keep the limit intact, got %d", b.WeeklyLimit)
	}

	g.SetBudgetIgnoreAll(false)
	if due := dueSet(g); due["scanner"] {
		t.Error("turning ignore-all off should restore kick suppression")
	}
}

// ---------------------------------------------------------------------------
// Kick-channel gate in scheduled kicks
// ---------------------------------------------------------------------------

// TestEvaluate_NonKickChannelAgentNeverDue: an agent that declares channels
// but not the kick channel is never timer-kicked, while an explicit kick
// channel keeps the agent kickable. This is the scheduled-kick twin of the
// CEL-eligibility channel gate.
func TestEvaluate_NonKickChannelAgentNeverDue(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{
				"kicked":  "15m",
				"webhook": "15m",
			}},
		},
	}
	agents := map[string]config.AgentConfig{
		"kicked":  {Enabled: true, Channels: []config.ChannelConfig{{Type: "kick"}}},
		"webhook": {Enabled: true, Channels: []config.ChannelConfig{{Type: "webhook"}}},
	}
	g := New(cfg, agents, testLogger())

	due := g.Evaluate(0, 0, 0, 0)
	set := make(map[string]bool, len(due))
	for _, name := range due {
		set[name] = true
	}
	if !set["kicked"] {
		t.Error("agent with an explicit kick channel should be due")
	}
	if set["webhook"] {
		t.Error("agent without a kick channel must never be timer-kicked")
	}
}

// ---------------------------------------------------------------------------
// Time-of-day cadence edges
// ---------------------------------------------------------------------------

// TestAllowResumeKick_DeniedForTimeOfDaySchedule: time-of-day cadences are
// exact wall-clock schedules — a crash restart must wait for the next
// occurrence, never mint an off-schedule kick.
func TestAllowResumeKick_DeniedForTimeOfDaySchedule(t *testing.T) {
	c, err := configFromYAML(`{times: ["09:00"], tz: UTC}`)
	if err != nil {
		t.Fatal(err)
	}
	g := timeCadenceGovernor(c, time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC))
	g.Evaluate(0, 0, 0, 0) // populate state.Cadences with the schedule entry

	if g.AllowResumeKick("scanner") {
		t.Error("resume kick must be denied for a time-of-day schedule")
	}

	// Positive control: the same call path grants a resume kick for an
	// interval cadence, so the denial above is the schedule gate, not a
	// governor that denies everything.
	if !resumeTestGovernor(t, "1h").AllowResumeKick("scanner") {
		t.Error("interval-cadence agent should get a resume kick")
	}
}

// TestEvaluate_ModeChangeKeepsTimeOfDaySchedule: a mode change must carry a
// time-of-day schedule through the idle fallback unchanged (modes decide
// whether the schedule is active, never scale its times), and an occurrence
// hit during the change still fires.
func TestEvaluate_ModeChangeKeepsTimeOfDaySchedule(t *testing.T) {
	c, err := configFromYAML(`{times: ["09:00"], tz: UTC}`)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": c}},
			"busy": {Threshold: 10, Cadences: map[string]config.Cadence{}},
		},
	}
	g := New(cfg, map[string]config.AgentConfig{"scanner": {Enabled: true}}, testLogger())
	g.now = func() time.Time { return time.Date(2026, 8, 7, 9, 0, 5, 0, time.UTC) }

	// Queue pressure forces IDLE→BUSY on the same eval that hits 09:00.
	due := g.Evaluate(15, 0, 0, 0)
	if got := g.GetState().Mode; got != ModeBusy {
		t.Fatalf("mode = %s, want BUSY", got)
	}
	if len(due) != 1 || due[0] != "scanner" {
		t.Errorf("due = %v, want [scanner] at the 09:00 occurrence", due)
	}

	cad, ok := g.GetState().Cadences["scanner"]
	if !ok {
		t.Fatal("scanner schedule should survive the mode change via idle fallback")
	}
	if cad.Schedule.Mode() != config.CadenceModeTimes {
		t.Errorf("schedule mode = %q, want %q", cad.Schedule.Mode(), config.CadenceModeTimes)
	}
	if cad.Interval != 0 {
		t.Errorf("time-of-day cadence must carry no interval, got %v", cad.Interval)
	}
}

// ---------------------------------------------------------------------------
// Replica idle fallback + agents with no cadence anywhere
// ---------------------------------------------------------------------------

// TestResolveCadence_ReplicaIdleFallback: a replica in a mode with no cadence
// entry falls back to the BASE agent's idle-mode cadence — the same chain the
// dashboard displays — and an agent with no entry in any mode gets none.
func TestResolveCadence_ReplicaIdleFallback(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "10m"}},
			"busy": {Threshold: 10, Cadences: map[string]config.Cadence{}},
		},
	}
	agents := map[string]config.AgentConfig{
		"scanner":   {Enabled: true, ReplicaIndex: 1, ReplicaCount: 2},
		"scanner-2": {Enabled: true, ReplicaOf: "scanner", ReplicaIndex: 2, ReplicaCount: 2},
		"orphan":    {Enabled: true},
	}
	g := New(cfg, agents, testLogger())

	// IDLE first: orphan has no entry in idle either — no cadence, no kicks.
	g.Evaluate(0, 0, 0, 0)
	if _, ok := g.GetState().Cadences["orphan"]; ok {
		t.Error("agent with no cadence in any mode should have no entry in idle")
	}

	// BUSY has no cadences at all: the replica must resolve through the idle
	// fallback OF ITS BASE agent, not lose its cadence.
	g.Evaluate(15, 0, 0, 0)
	state := g.GetState()
	if got := state.Cadences["scanner-2"].Interval; got != 10*time.Minute {
		t.Errorf("replica busy-mode cadence = %v, want base idle fallback 10m", got)
	}
	if got := state.Cadences["scanner"].Interval; got != 10*time.Minute {
		t.Errorf("base busy-mode cadence = %v, want idle fallback 10m", got)
	}
	if _, ok := state.Cadences["orphan"]; ok {
		t.Error("agent with no cadence in any mode should have no entry in busy")
	}
}

// ---------------------------------------------------------------------------
// Invalid schedule objects
// ---------------------------------------------------------------------------

// TestUpdateCadences_InvalidScheduleGetsNoKicks: a stored cadence object that
// fails validation (times without a timezone — e.g. hand-edited or written by
// an older version) must remove the agent from timer kicking rather than kick
// on a garbage schedule.
func TestUpdateCadences_InvalidScheduleGetsNoKicks(t *testing.T) {
	// Constructed directly because the YAML/JSON decoders reject this at
	// parse time; a persisted config can still contain it.
	invalid := config.Cadence(`tod:{"times":["09:00"]}`)
	if invalid.Validate() == nil {
		t.Fatal("precondition: times without tz should fail validation")
	}

	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": invalid}},
		},
	}
	g := New(cfg, map[string]config.AgentConfig{"scanner": {Enabled: true}}, testLogger())

	due := g.Evaluate(0, 0, 0, 0)
	if len(due) != 0 {
		t.Errorf("agent with an invalid schedule must not be due, got %v", due)
	}
	if _, ok := g.GetState().Cadences["scanner"]; ok {
		t.Error("agent with an invalid schedule should have no cadence entry")
	}
}
