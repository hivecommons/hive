package agent

import (
	"context"
	"log/slog"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// breakerTestManager builds a Manager and forces each named agent into a known
// live state, since NewManager starts every agent StateStopped. running=true
// puts the agent in StateRunning; paused=true puts it in StatePaused with the
// given trigger (to simulate a prior operator pause). onDemand marks the agent
// as OnDemand in config.
type breakerAgentSpec struct {
	running      bool
	paused       bool
	pausedByWhom string
	onDemand     bool
}

func newBreakerTestManager(t *testing.T, specs map[string]breakerAgentSpec) *Manager {
	t.Helper()
	cfgs := make(map[string]config.AgentConfig, len(specs))
	for name, s := range specs {
		// A backend with no installed binary makes Resume's relaunch path a
		// fast no-op (launchInTmux marks StateFailed and returns) so the test
		// never spawns a real agent CLI. The paused-state assertions below read
		// fields Resume sets synchronously, independent of the relaunch outcome.
		cfgs[name] = config.AgentConfig{Backend: "no-such-backend", OnDemand: s.onDemand}
	}
	m := NewManager(cfgs, slog.Default(), ProjectContext{})
	m.mu.Lock()
	for name, s := range specs {
		a := m.agents[name]
		switch {
		case s.paused:
			a.State = StatePaused
			a.Paused = true
			a.Config.Paused = true
			a.PausedTrigger = s.pausedByWhom
		case s.running:
			a.State = StateRunning
			a.Paused = false
		default:
			a.State = StateStopped
		}
	}
	m.mu.Unlock()
	return m
}

func (m *Manager) breakerAgentPaused(name string) (paused bool, trigger string, state ProcessState) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.agents[name]
	return a.Paused, a.PausedTrigger, a.State
}

// TestEngageBreaker_PausesRunningNonOnDemandOnly verifies engage pauses only
// running, non-on-demand agents and SKIPS on-demand and already-paused agents,
// never adding the skipped ones to the recorded set.
func TestEngageBreaker_PausesRunningNonOnDemandOnly(t *testing.T) {
	m := newBreakerTestManager(t, map[string]breakerAgentSpec{
		"scanner":    {running: true},
		"reviewer":   {running: true},
		"brainstorm": {running: true, onDemand: true},               // on-demand: skip
		"prepaused":  {paused: true, pausedByWhom: "dashboard-api"}, // prior manual pause: skip
		"idle":       {},                                            // stopped: not running, skip
	})

	paused := m.EngageBreaker()

	got := map[string]bool{}
	for _, n := range paused {
		got[n] = true
	}
	if !got["scanner"] || !got["reviewer"] {
		t.Fatalf("expected scanner+reviewer paused, got %v", paused)
	}
	if got["brainstorm"] {
		t.Error("on-demand agent must NOT be in the breaker set")
	}
	if got["prepaused"] {
		t.Error("already-paused agent must NOT be in the breaker set")
	}
	if got["idle"] {
		t.Error("stopped agent must NOT be in the breaker set")
	}
	if len(paused) != 2 {
		t.Fatalf("expected exactly 2 paused, got %d (%v)", len(paused), paused)
	}

	// The breaker-paused agents carry the distinct trigger; the pre-paused one
	// keeps its own trigger untouched.
	if p, trig, st := m.breakerAgentPaused("scanner"); !p || trig != BreakerTrigger || st != StatePaused {
		t.Errorf("scanner: paused=%v trigger=%q state=%q", p, trig, st)
	}
	if p, trig, _ := m.breakerAgentPaused("prepaused"); !p || trig != "dashboard-api" {
		t.Errorf("prepaused trigger clobbered: paused=%v trigger=%q", p, trig)
	}
	if p, _, _ := m.breakerAgentPaused("brainstorm"); p {
		t.Error("on-demand agent must remain un-paused")
	}

	engaged, set := m.BreakerState()
	if !engaged {
		t.Error("breaker should report engaged")
	}
	if len(set) != 2 {
		t.Errorf("BreakerState set = %v, want 2", set)
	}
}

// TestReleaseBreaker_ResumesOnlyBreakerSet verifies release resumes ONLY the
// agents the breaker paused and never touches on-demand, pre-paused, or
// operator-re-paused agents. This is the core restore invariant.
func TestReleaseBreaker_ResumesOnlyBreakerSet(t *testing.T) {
	m := newBreakerTestManager(t, map[string]breakerAgentSpec{
		"scanner":    {running: true},
		"reviewer":   {running: true},
		"brainstorm": {running: true, onDemand: true},
		"prepaused":  {paused: true, pausedByWhom: "dashboard-api"},
	})

	m.EngageBreaker()

	// Operator re-pauses "reviewer" during the breaker window with a DIFFERENT
	// trigger — this must NOT be auto-resumed by release.
	if err := m.Pause("reviewer", "dashboard-api", "operator re-pause"); err != nil {
		t.Fatalf("re-pause reviewer: %v", err)
	}

	m.ReleaseBreaker(context.TODO())

	// scanner: breaker paused it, operator never touched it → resumed.
	if p, _, _ := m.breakerAgentPaused("scanner"); p {
		t.Error("scanner should be resumed after release")
	}
	// reviewer: operator re-paused (trigger != fleet-breaker) → stays paused.
	if p, trig, _ := m.breakerAgentPaused("reviewer"); !p || trig != "dashboard-api" {
		t.Errorf("reviewer must stay operator-paused: paused=%v trigger=%q", p, trig)
	}
	// prepaused: never in the set → still paused with its own trigger.
	if p, trig, _ := m.breakerAgentPaused("prepaused"); !p || trig != "dashboard-api" {
		t.Errorf("prepaused must stay paused: paused=%v trigger=%q", p, trig)
	}
	// brainstorm: on-demand, never paused → still un-paused.
	if p, _, _ := m.breakerAgentPaused("brainstorm"); p {
		t.Error("on-demand brainstorm must never be paused/resumed by the breaker")
	}

	engaged, _ := m.BreakerState()
	if engaged {
		t.Error("breaker should report disengaged after release")
	}
}

// TestEngageBreaker_Idempotent verifies a double-engage does not re-pause and
// returns the same set.
func TestEngageBreaker_Idempotent(t *testing.T) {
	m := newBreakerTestManager(t, map[string]breakerAgentSpec{
		"scanner":  {running: true},
		"reviewer": {running: true},
	})

	first := m.EngageBreaker()
	if len(first) != 2 {
		t.Fatalf("first engage set = %v, want 2", first)
	}
	second := m.EngageBreaker()
	if len(second) != 2 {
		t.Fatalf("second engage set = %v, want 2 (idempotent)", second)
	}
	engaged, set := m.BreakerState()
	if !engaged || len(set) != 2 {
		t.Errorf("state after double-engage: engaged=%v set=%v", engaged, set)
	}
}

// TestReleaseBreaker_Idempotent verifies a double-release (and a release with
// no breaker engaged) is a harmless no-op.
func TestReleaseBreaker_Idempotent(t *testing.T) {
	m := newBreakerTestManager(t, map[string]breakerAgentSpec{
		"scanner": {running: true},
	})

	// Release with nothing engaged: no-op, nil result.
	if resumed := m.ReleaseBreaker(context.TODO()); resumed != nil {
		t.Errorf("release with no breaker engaged should be nil, got %v", resumed)
	}

	m.EngageBreaker()
	m.ReleaseBreaker(context.TODO())
	// Second release: breaker already disengaged → no-op.
	if resumed := m.ReleaseBreaker(context.TODO()); resumed != nil {
		t.Errorf("double-release should be nil, got %v", resumed)
	}
	if engaged, _ := m.BreakerState(); engaged {
		t.Error("breaker should stay disengaged after double-release")
	}
}

// TestRestoreBreaker_SurvivesReload simulates a snapshot round-trip: the
// breaker was engaged, the process restarted, the agents restored paused with
// the breaker trigger (as cmd/hive does), and RestoreBreaker re-attaches the
// breaker so a later release resumes exactly them — without auto-releasing on
// boot.
func TestRestoreBreaker_SurvivesReload(t *testing.T) {
	// Simulate post-restart state: scanner+reviewer restored paused BY the
	// breaker (trigger fleet-breaker), prepaused restored paused by operator,
	// brainstorm on-demand and running.
	m := newBreakerTestManager(t, map[string]breakerAgentSpec{
		"scanner":    {paused: true, pausedByWhom: BreakerTrigger},
		"reviewer":   {paused: true, pausedByWhom: BreakerTrigger},
		"prepaused":  {paused: true, pausedByWhom: "dashboard-api"},
		"brainstorm": {running: true, onDemand: true},
	})

	// The snapshot recorded the breaker as engaged holding scanner+reviewer.
	m.RestoreBreaker(true, []string{"scanner", "reviewer"})

	engaged, set := m.BreakerState()
	if !engaged {
		t.Fatal("breaker must remain engaged after restore (no auto-release on boot)")
	}
	if len(set) != 2 {
		t.Fatalf("restored set = %v, want scanner+reviewer", set)
	}

	// Boot restore must not have changed any agent's paused state.
	for _, n := range []string{"scanner", "reviewer", "prepaused"} {
		if p, _, _ := m.breakerAgentPaused(n); !p {
			t.Errorf("%s should still be paused after restore", n)
		}
	}

	// A later release resumes only the restored breaker set; prepaused stays.
	m.ReleaseBreaker(context.TODO())
	if p, _, _ := m.breakerAgentPaused("scanner"); p {
		t.Error("scanner should resume on post-restore release")
	}
	if p, trig, _ := m.breakerAgentPaused("prepaused"); !p || trig != "dashboard-api" {
		t.Errorf("prepaused must stay operator-paused: paused=%v trigger=%q", p, trig)
	}
}

// TestRestoreBreaker_DisengagedNoop verifies restoring a disengaged breaker
// changes nothing.
func TestRestoreBreaker_DisengagedNoop(t *testing.T) {
	m := newBreakerTestManager(t, map[string]breakerAgentSpec{
		"scanner": {running: true},
	})
	m.RestoreBreaker(false, nil)
	if engaged, _ := m.BreakerState(); engaged {
		t.Error("restoring a disengaged breaker must not engage it")
	}
}
