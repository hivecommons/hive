package governor

import (
	"log/slog"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// testLogger returns a discard logger suitable for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// standardConfig returns a GovernorConfig with explicit thresholds:
//
//	SURGE  > 20
//	BUSY   > 10
//	QUIET  > 2
//	IDLE   otherwise
func standardConfig(agents ...string) (config.GovernorConfig, map[string]config.AgentConfig) {
	cadences := make(map[string]config.Cadence, len(agents))
	for _, a := range agents {
		cadences[a] = "15m"
	}

	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"surge": {Threshold: 20, Cadences: cadences},
			"busy":  {Threshold: 10, Cadences: cadences},
			"quiet": {Threshold: 2, Cadences: cadences},
			"idle":  {Threshold: 0, Cadences: cadences},
		},
	}

	agentMap := make(map[string]config.AgentConfig, len(agents))
	for _, a := range agents {
		agentMap[a] = config.AgentConfig{Enabled: true}
	}

	return cfg, agentMap
}

// ----------------------------------------------------------------------------
// computeMode tests
// ----------------------------------------------------------------------------

func TestComputeMode_BelowAllThresholds(t *testing.T) {
	cfg, agents := standardConfig()
	g := New(cfg, agents, testLogger())

	if got := g.computeMode(0); got != ModeIdle {
		t.Errorf("depth=0: expected IDLE, got %s", got)
	}
	if got := g.computeMode(1); got != ModeIdle {
		t.Errorf("depth=1: expected IDLE, got %s", got)
	}
	if got := g.computeMode(2); got != ModeIdle {
		t.Errorf("depth=2: expected IDLE (threshold is >, not >=), got %s", got)
	}
}

func TestComputeMode_QuietBand(t *testing.T) {
	cfg, agents := standardConfig()
	g := New(cfg, agents, testLogger())

	// depth=3 is above quiet threshold (2) but not above busy (10)
	if got := g.computeMode(3); got != ModeQuiet {
		t.Errorf("depth=3: expected QUIET, got %s", got)
	}
	if got := g.computeMode(10); got != ModeQuiet {
		t.Errorf("depth=10: expected QUIET (threshold is >, not >=), got %s", got)
	}
}

func TestComputeMode_BusyBand(t *testing.T) {
	cfg, agents := standardConfig()
	g := New(cfg, agents, testLogger())

	if got := g.computeMode(11); got != ModeBusy {
		t.Errorf("depth=11: expected BUSY, got %s", got)
	}
	if got := g.computeMode(20); got != ModeBusy {
		t.Errorf("depth=20: expected BUSY (threshold is >, not >=), got %s", got)
	}
}

func TestComputeMode_Surge(t *testing.T) {
	cfg, agents := standardConfig()
	g := New(cfg, agents, testLogger())

	if got := g.computeMode(21); got != ModeSurge {
		t.Errorf("depth=21: expected SURGE, got %s", got)
	}
	if got := g.computeMode(999); got != ModeSurge {
		t.Errorf("depth=999: expected SURGE, got %s", got)
	}
}

func TestComputeMode_DefaultThresholds(t *testing.T) {
	// GovernorConfig with no Modes map — should fall back to hard-coded defaults
	// (surge>20, busy>10, quiet>2)
	g := New(config.GovernorConfig{}, map[string]config.AgentConfig{}, testLogger())

	cases := []struct {
		depth int
		want  Mode
	}{
		{0, ModeIdle},
		{2, ModeIdle},
		{3, ModeQuiet},
		{10, ModeQuiet},
		{11, ModeBusy},
		{20, ModeBusy},
		{21, ModeSurge},
	}
	for _, tc := range cases {
		if got := g.computeMode(tc.depth); got != tc.want {
			t.Errorf("default thresholds: depth=%d expected %s, got %s", tc.depth, tc.want, got)
		}
	}
}

// ----------------------------------------------------------------------------
// Evaluate tests
// ----------------------------------------------------------------------------

func TestEvaluate_ReturnsCorrectMode(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	g.Evaluate(25, 0, 0, 0)
	if s := g.GetState(); s.Mode != ModeSurge {
		t.Errorf("expected SURGE after depth=25, got %s", s.Mode)
	}

	g.Evaluate(5, 0, 0, 0)
	if s := g.GetState(); s.Mode != ModeQuiet {
		t.Errorf("expected QUIET after depth=5, got %s", s.Mode)
	}

	g.Evaluate(0, 0, 0, 0)
	if s := g.GetState(); s.Mode != ModeIdle {
		t.Errorf("expected IDLE after depth=0, got %s", s.Mode)
	}
}

func TestEvaluate_OpenPRsCountTowardModePressure(t *testing.T) {
	// Mode pressure is issues + open PRs. Issues alone let the governor idle
	// while a large PR backlog sat unmerged (2026-08-03: 23 open PRs, 1
	// actionable issue → idle cadence → merge sweeps too rare to drain).
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	g.Evaluate(1, 15, 0, 0)
	if s := g.GetState(); s.Mode != ModeBusy {
		t.Errorf("expected BUSY with issues=1 prs=15 (pressure 16), got %s", s.Mode)
	}

	g.Evaluate(0, 25, 0, 0)
	if s := g.GetState(); s.Mode != ModeSurge {
		t.Errorf("expected SURGE with issues=0 prs=25 (pressure 25), got %s", s.Mode)
	}

	// Held items must NOT add pressure — they are waiting on a human.
	g.Evaluate(0, 0, 30, 0)
	if s := g.GetState(); s.Mode != ModeIdle {
		t.Errorf("expected IDLE with only held items queued, got %s", s.Mode)
	}
}

func TestEvaluate_UpdatesQueueFields(t *testing.T) {
	cfg, agents := standardConfig()
	g := New(cfg, agents, testLogger())

	g.Evaluate(7, 3, 1, 2)
	s := g.GetState()
	if s.QueueIssues != 7 {
		t.Errorf("QueueIssues: got %d, want 7", s.QueueIssues)
	}
	if s.QueuePRs != 3 {
		t.Errorf("QueuePRs: got %d, want 3", s.QueuePRs)
	}
	if s.QueueHold != 1 {
		t.Errorf("QueueHold: got %d, want 1", s.QueueHold)
	}
	if s.SLAViolations != 2 {
		t.Errorf("SLAViolations: got %d, want 2", s.SLAViolations)
	}
}

func TestEvaluate_LastEvalIsRecent(t *testing.T) {
	cfg, agents := standardConfig()
	g := New(cfg, agents, testLogger())

	before := time.Now()
	g.Evaluate(0, 0, 0, 0)
	after := time.Now()

	s := g.GetState()
	if s.LastEval.Before(before) || s.LastEval.After(after) {
		t.Errorf("LastEval %v not in range [%v, %v]", s.LastEval, before, after)
	}
}

func TestEvaluate_NeverKickedAgentIsDue(t *testing.T) {
	cfg, agents := standardConfig("alpha", "beta")
	g := New(cfg, agents, testLogger())

	due := g.Evaluate(15, 0, 0, 0) // BUSY mode, cadence 15m
	sort.Strings(due)

	if len(due) != 2 {
		t.Fatalf("expected 2 agents due, got %v", due)
	}
	if due[0] != "alpha" || due[1] != "beta" {
		t.Errorf("unexpected agents due: %v", due)
	}
}

func TestEvaluate_RecentlyKickedAgentNotDue(t *testing.T) {
	cfg, agents := standardConfig("scanner")
	g := New(cfg, agents, testLogger())

	// First evaluate — scanner is due (never kicked).
	g.Evaluate(15, 0, 0, 0)
	g.RecordKick("scanner")

	// Immediately evaluate again — scanner was just kicked, should NOT be due.
	due := g.Evaluate(15, 0, 0, 0)
	for _, a := range due {
		if a == "scanner" {
			t.Error("scanner should not be due immediately after being kicked")
		}
	}
}

func TestEvaluate_ExpiredCadenceIsDue(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"busy": {
				Threshold: 10,
				Cadences:  map[string]config.Cadence{"worker": "1ms"}, // extremely short cadence
			},
		},
	}
	agents := map[string]config.AgentConfig{"worker": {Enabled: true}}
	g := New(cfg, agents, testLogger())

	g.Evaluate(15, 0, 0, 0)
	g.RecordKick("worker")

	// Wait longer than the 1ms cadence.
	time.Sleep(5 * time.Millisecond)

	due := g.Evaluate(15, 0, 0, 0)
	found := false
	for _, a := range due {
		if a == "worker" {
			found = true
		}
	}
	if !found {
		t.Error("worker should be due after cadence interval has elapsed")
	}
}

func TestEvaluate_PausedAgentNeverDue(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"busy": {
				Threshold: 10,
				Cadences: map[string]config.Cadence{
					"active": "1ms",
					"paused": "pause",
				},
			},
		},
	}
	agents := map[string]config.AgentConfig{
		"active": {Enabled: true},
		"paused": {Enabled: true},
	}
	g := New(cfg, agents, testLogger())

	time.Sleep(5 * time.Millisecond)
	due := g.Evaluate(15, 0, 0, 0)

	for _, a := range due {
		if a == "paused" {
			t.Error("paused agent must never appear in due list")
		}
	}

	// Verify the active one IS returned.
	found := false
	for _, a := range due {
		if a == "active" {
			found = true
		}
	}
	if !found {
		t.Error("active agent should be due after cadence elapsed")
	}
}

func TestEvaluate_PausedVariantSpelling(t *testing.T) {
	// "paused" (not just "pause") should also cause the agent to be skipped.
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"quiet": {
				Threshold: 2,
				Cadences:  map[string]config.Cadence{"robot": "paused"},
			},
		},
	}
	agents := map[string]config.AgentConfig{"robot": {Enabled: true}}
	g := New(cfg, agents, testLogger())

	due := g.Evaluate(5, 0, 0, 0)
	for _, a := range due {
		if a == "robot" {
			t.Error("agent with cadence='paused' must not appear in due list")
		}
	}

	s := g.GetState()
	cadence, ok := s.Cadences["robot"]
	if !ok {
		t.Fatal("expected cadence entry for robot")
	}
	if !cadence.Paused {
		t.Error("cadence.Paused should be true for cadence='paused'")
	}
}

func TestEvaluate_AgentWithNoCadenceInModeNotDue(t *testing.T) {
	// Agent exists in the agent map but has no cadence entry for the current mode.
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"busy": {
				Threshold: 10,
				Cadences:  map[string]config.Cadence{}, // no cadences at all
			},
		},
	}
	agents := map[string]config.AgentConfig{"lonely": {Enabled: true}}
	g := New(cfg, agents, testLogger())

	due := g.Evaluate(15, 0, 0, 0)
	if len(due) != 0 {
		t.Errorf("expected no agents due when mode has no cadences, got %v", due)
	}
}

// ----------------------------------------------------------------------------
// RecordKick tests
// ----------------------------------------------------------------------------

func TestRecordKick_SetsLastKickTime(t *testing.T) {
	cfg, agents := standardConfig("agent-a")
	g := New(cfg, agents, testLogger())

	before := time.Now()
	g.RecordKick("agent-a")
	after := time.Now()

	s := g.GetState()
	kicked, ok := s.LastKick["agent-a"]
	if !ok {
		t.Fatal("expected LastKick entry for agent-a")
	}
	if kicked.Before(before) || kicked.After(after) {
		t.Errorf("LastKick %v not in expected range [%v, %v]", kicked, before, after)
	}
}

func TestRecordKick_UnknownAgentIsStored(t *testing.T) {
	cfg, agents := standardConfig()
	g := New(cfg, agents, testLogger())

	g.RecordKick("mystery-agent")
	s := g.GetState()
	if _, ok := s.LastKick["mystery-agent"]; !ok {
		t.Error("RecordKick should store an entry even for unknown agents")
	}
}

func TestRecordKick_OverwritesPreviousKick(t *testing.T) {
	cfg, agents := standardConfig("agent-b")
	g := New(cfg, agents, testLogger())

	g.RecordKick("agent-b")
	first := g.GetState().LastKick["agent-b"]

	time.Sleep(2 * time.Millisecond)
	g.RecordKick("agent-b")
	second := g.GetState().LastKick["agent-b"]

	if !second.After(first) {
		t.Errorf("second kick time %v should be after first %v", second, first)
	}
}

// ----------------------------------------------------------------------------
// GetState (copy / thread safety) tests
// ----------------------------------------------------------------------------

func TestGetState_ReturnsCopy(t *testing.T) {
	cfg, agents := standardConfig("agt")
	g := New(cfg, agents, testLogger())

	g.Evaluate(15, 0, 0, 0)
	g.RecordKick("agt")

	s1 := g.GetState()
	// Mutate the returned copy.
	s1.Cadences["injected"] = AgentCadence{Agent: "injected"}
	s1.LastKick["injected"] = time.Now()
	s1.Mode = ModeSurge

	s2 := g.GetState()
	if _, found := s2.Cadences["injected"]; found {
		t.Error("mutating returned State.Cadences should not affect internal state")
	}
	if _, found := s2.LastKick["injected"]; found {
		t.Error("mutating returned State.LastKick should not affect internal state")
	}
}

func TestGetState_ConcurrentAccess(t *testing.T) {
	cfg, agents := standardConfig("worker")
	g := New(cfg, agents, testLogger())

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines * 3)

	// Concurrent Evaluate, RecordKick, and GetState calls.
	for i := range goroutines {
		go func(depth int) {
			defer wg.Done()
			g.Evaluate(depth%30, 0, 0, 0)
		}(i)

		go func(i int) {
			defer wg.Done()
			if i%5 == 0 {
				g.RecordKick("worker")
			}
		}(i)

		go func() {
			defer wg.Done()
			_ = g.GetState()
		}()
	}

	wg.Wait() // passes with -race if locking is correct
}

// ----------------------------------------------------------------------------
// Mode transition logging tests
// ----------------------------------------------------------------------------

func TestModeTransition_LoggedOnChange(t *testing.T) {
	// We verify the transition occurred by checking the state, not by capturing
	// log output (which would require a custom handler and adds brittleness).
	cfg, agents := standardConfig("a")
	g := New(cfg, agents, testLogger())

	g.Evaluate(0, 0, 0, 0) // IDLE
	if g.GetState().Mode != ModeIdle {
		t.Fatal("should start in IDLE")
	}

	g.Evaluate(15, 0, 0, 0) // → BUSY
	if g.GetState().Mode != ModeBusy {
		t.Errorf("expected BUSY, got %s", g.GetState().Mode)
	}

	g.Evaluate(25, 0, 0, 0) // → SURGE
	if g.GetState().Mode != ModeSurge {
		t.Errorf("expected SURGE, got %s", g.GetState().Mode)
	}

	g.Evaluate(5, 0, 0, 0) // → QUIET
	if g.GetState().Mode != ModeQuiet {
		t.Errorf("expected QUIET, got %s", g.GetState().Mode)
	}

	g.Evaluate(0, 0, 0, 0) // → IDLE
	if g.GetState().Mode != ModeIdle {
		t.Errorf("expected IDLE, got %s", g.GetState().Mode)
	}
}

func TestModeTransition_NoTransitionWhenSameMode(t *testing.T) {
	cfg, agents := standardConfig("a")
	g := New(cfg, agents, testLogger())

	g.Evaluate(15, 0, 0, 0) // → BUSY
	modeBefore := g.GetState().Mode

	g.Evaluate(12, 0, 0, 0) // still BUSY — no transition
	modeAfter := g.GetState().Mode

	if modeBefore != ModeBusy || modeAfter != ModeBusy {
		t.Errorf("expected BUSY→BUSY, got %s→%s", modeBefore, modeAfter)
	}
}

// ----------------------------------------------------------------------------
// FormatStatus smoke test
// ----------------------------------------------------------------------------

func TestFormatStatus_ContainsMode(t *testing.T) {
	cfg, agents := standardConfig()
	g := New(cfg, agents, testLogger())

	g.Evaluate(25, 3, 1, 2)
	status := g.FormatStatus()

	if status == "" {
		t.Fatal("FormatStatus returned empty string")
	}
	// Verify it contains key fields (exact format is an implementation detail).
	for _, want := range []string{"SURGE", "25", "3", "1", "2"} {
		found := false
		for i := range len(status) - len(want) + 1 {
			if status[i:i+len(want)] == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("FormatStatus %q missing expected substring %q", status, want)
		}
	}
}
