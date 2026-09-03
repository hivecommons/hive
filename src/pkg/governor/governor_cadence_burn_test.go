package governor

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Regression tests for #2573: the hive burned backend tokens ("Bob coins")
// faster than any configured cadence because (a) stale cadences from a
// previous mode were silently retained when the current mode had no entry,
// and (b) crash-restarted agents were force-kicked unconditionally on every
// eval cycle.

// queue depths chosen to land the test governor in a specific mode
// (thresholds: quiet=2, busy=10, surge=20).
const (
	queueDepthIdle  = 0
	queueDepthQuiet = 3
	queueDepthBusy  = 15
	queueDepthSurge = 25
)

// TestUpdateCadences_NoStaleCarryOverAcrossModes is the core #2573
// regression: an agent with an entry in one mode but not another must NOT
// keep the old mode's (shorter) interval after the mode changes — it must
// fall back to the idle-mode entry, exactly like the dashboard displays.
func TestUpdateCadences_NoStaleCarryOverAcrossModes(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle":  {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "6h"}},
			"quiet": {Threshold: 2, Cadences: map[string]config.Cadence{"scanner": "6h"}},
			// busy defines a short scanner cadence; quiet/idle are 6h.
			"busy":  {Threshold: 10, Cadences: map[string]config.Cadence{"scanner": "5m"}},
			"surge": {Threshold: 20, Cadences: map[string]config.Cadence{}},
		},
	}
	agents := map[string]config.AgentConfig{
		"scanner": {Backend: "bob", Enabled: true},
	}
	g := New(cfg, agents, testLogger())

	// Enter BUSY: scanner runs at 5m.
	g.Evaluate(queueDepthBusy, 0, 0, 0)
	if c := g.GetState().Cadences["scanner"]; c.Interval != 5*time.Minute {
		t.Fatalf("busy cadence = %v, want 5m", c.Interval)
	}

	// Drop back to QUIET: scanner must run at quiet's 6h, not busy's stale 5m.
	g.Evaluate(queueDepthQuiet, 0, 0, 0)
	if c := g.GetState().Cadences["scanner"]; c.Interval != 6*time.Hour {
		t.Errorf("quiet cadence = %v, want 6h (stale busy cadence retained)", c.Interval)
	}

	// Enter SURGE (no scanner entry): falls back to IDLE's 6h — the value the
	// dashboard shows — not busy's stale 5m.
	g.Evaluate(queueDepthSurge, 0, 0, 0)
	if c := g.GetState().Cadences["scanner"]; c.Interval != 6*time.Hour {
		t.Errorf("surge cadence = %v, want idle fallback 6h", c.Interval)
	}
}

// TestUpdateCadences_MissingModeBlockFallsBackToIdle covers the whole-mode
// gap: before the fix a missing mode block returned early and kept EVERY
// agent's previous cadence.
func TestUpdateCadences_MissingModeBlockFallsBackToIdle(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "4h"}},
			"busy": {Threshold: 10, Cadences: map[string]config.Cadence{"scanner": "2m"}},
			// no "quiet" or "surge" blocks at all
		},
	}
	agents := map[string]config.AgentConfig{
		"scanner": {Backend: "bob", Enabled: true},
	}
	g := New(cfg, agents, testLogger())

	g.Evaluate(queueDepthBusy, 0, 0, 0) // busy: 2m
	// quiet has no block (threshold defaults apply: quiet>2) — idle fallback.
	g.Evaluate(queueDepthQuiet, 0, 0, 0)
	if c := g.GetState().Cadences["scanner"]; c.Interval != 4*time.Hour {
		t.Errorf("cadence with missing mode block = %v, want idle fallback 4h", c.Interval)
	}
}

// TestUpdateCadences_IdleFallbackRespectsPause: an idle-mode "pause" must
// carry into modes with no entry, keeping the agent un-kicked.
func TestUpdateCadences_IdleFallbackRespectsPause(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"supervisor": "pause"}},
			"busy": {Threshold: 10, Cadences: map[string]config.Cadence{}},
		},
	}
	agents := map[string]config.AgentConfig{
		"supervisor": {Backend: "claude", Enabled: true},
	}
	g := New(cfg, agents, testLogger())

	g.Evaluate(queueDepthBusy, 0, 0, 0)
	c, ok := g.GetState().Cadences["supervisor"]
	if !ok || !c.Paused {
		t.Errorf("supervisor cadence = %+v (ok=%v), want Paused via idle fallback", c, ok)
	}
	if due := g.agentsDueForKick(); len(due) != 0 {
		t.Errorf("paused-by-fallback agent listed as due: %v", due)
	}
}

// TestUpdateCadences_OffMeansNoKicks: the "off" sentinel removes the agent
// from timer kicking entirely rather than warning about an unparsable value.
func TestUpdateCadences_OffMeansNoKicks(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "off"}},
		},
	}
	agents := map[string]config.AgentConfig{
		"scanner": {Backend: "bob", Enabled: true},
	}
	g := New(cfg, agents, testLogger())

	g.Evaluate(queueDepthIdle, 0, 0, 0)
	if _, ok := g.GetState().Cadences["scanner"]; ok {
		t.Error("agent with cadence 'off' should have no cadence entry")
	}
	if due := g.agentsDueForKick(); len(due) != 0 {
		t.Errorf("agent with cadence 'off' listed as due: %v", due)
	}
}

func resumeTestGovernor(t *testing.T, cadence string) *Governor {
	t.Helper()
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": config.Cadence(cadence)}},
		},
	}
	agents := map[string]config.AgentConfig{
		"scanner": {Backend: "bob", Enabled: true},
	}
	g := New(cfg, agents, testLogger())
	g.Evaluate(queueDepthIdle, 0, 0, 0) // populate state.Cadences
	return g
}

// TestAllowResumeKick_OncePerInterval: the first crash restart earns a
// resume kick; a crash loop inside the same cadence interval does not.
func TestAllowResumeKick_OncePerInterval(t *testing.T) {
	g := resumeTestGovernor(t, "6h")

	if !g.AllowResumeKick("scanner") {
		t.Fatal("first resume kick should be allowed")
	}
	if g.AllowResumeKick("scanner") {
		t.Error("second resume kick within the cadence interval should be denied")
	}

	// Simulate the interval elapsing: backdate the recorded resume kick.
	g.mu.Lock()
	g.resumeKicks["scanner"] = time.Now().Add(-7 * time.Hour)
	g.mu.Unlock()
	if !g.AllowResumeKick("scanner") {
		t.Error("resume kick should be allowed again after the interval elapses")
	}
}

// TestAllowResumeKick_DeniedWhenModePaused: a mode-level "pause" means the
// operator wants no work — a crash restart must not override it.
func TestAllowResumeKick_DeniedWhenModePaused(t *testing.T) {
	g := resumeTestGovernor(t, "pause")
	if g.AllowResumeKick("scanner") {
		t.Error("resume kick should be denied when the mode cadence is 'pause'")
	}
}

// TestAllowResumeKick_DeniedWithoutCadence: an agent with no cadence entry
// (unknown agent, or cadence "off") never gets resume kicks.
func TestAllowResumeKick_DeniedWithoutCadence(t *testing.T) {
	g := resumeTestGovernor(t, "6h")
	if g.AllowResumeKick("no-such-agent") {
		t.Error("resume kick should be denied for an agent with no cadence entry")
	}
}

// TestAllowResumeKick_DeniedForOnDemand: on-demand agents are only ever
// triggered explicitly.
func TestAllowResumeKick_DeniedForOnDemand(t *testing.T) {
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "1h"}},
		},
	}
	agents := map[string]config.AgentConfig{
		"scanner": {Backend: "bob", Enabled: true, OnDemand: true},
	}
	g := New(cfg, agents, testLogger())
	g.Evaluate(queueDepthIdle, 0, 0, 0)
	if g.AllowResumeKick("scanner") {
		t.Error("resume kick should be denied for an on-demand agent")
	}
}

// TestAllowResumeKick_RespectsBudget: an exhausted budget suppresses resume
// kicks exactly like scheduled kicks, honoring the exemption list.
func TestAllowResumeKick_RespectsBudget(t *testing.T) {
	g := resumeTestGovernor(t, "1h")
	const (
		weeklyLimit = 1000
		overspend   = 2000
	)
	g.SetBudgetLimit(weeklyLimit)
	g.UpdateBudgetFromTotals(0, nil, nil) // open the window at baseline 0
	g.UpdateBudgetFromTotals(overspend, nil, nil)

	if g.AllowResumeKick("scanner") {
		t.Error("resume kick should be denied when the budget is exhausted")
	}

	g.SetBudgetIgnored([]string{"scanner"})
	if !g.AllowResumeKick("scanner") {
		t.Error("budget-exempt agent should still get a resume kick")
	}
}
