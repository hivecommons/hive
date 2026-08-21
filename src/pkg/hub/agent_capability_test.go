package hub

import (
	"testing"
	"time"
)

// A capable, expected-active, working agent on a modern spoke: the green case.
func modernWorking(now time.Time) AgentSummary {
	return AgentSummary{
		Name: "scanner", State: "running", Backend: "claude",
		Enabled: true, ExpectedActive: true,
		CanOpenIssue: true, CanOpenPR: true, CanMerge: true,
		StartedAt: settled(now), LastActivityAt: activeAt(now, 1*time.Minute),
	}
}

func TestVerdict_WorkingAndAble(t *testing.T) {
	now := time.Now()
	v := deriveAgentVerdict(modernWorking(now), hiveBlockers{}, 5, now)
	if v.RunState != runWorking {
		t.Errorf("runState = %v, want working", v.RunState)
	}
	if !v.Able || v.CapabilityTier != tierGreen {
		t.Errorf("want able/green, got able=%v tier=%s", v.Able, v.CapabilityTier)
	}
	if v.Stuck || v.Impotent || v.QuietByDesign {
		t.Errorf("healthy agent must carry no delta: %+v", v)
	}
}

// Expected active + enabled but the spoke does not report it running and it is
// not paused => dead => STUCK (the governor expects it, it isn't there).
func TestVerdict_StuckWhenExpectedButDown(t *testing.T) {
	now := time.Now()
	a := modernWorking(now)
	a.State = "stopped"
	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)
	if v.RunState != runDead {
		t.Errorf("runState = %v, want dead", v.RunState)
	}
	if !v.Stuck {
		t.Error("expected-active but down agent must be STUCK")
	}
	if v.Impotent {
		t.Error("a down agent is stuck, not impotent")
	}
}

// Running + login-stuck on an interactive backend => IMPOTENT and STUCK (it is
// expected active, and it is running but cannot work).
func TestVerdict_StuckAtLoginIsImpotentAndStuck(t *testing.T) {
	now := time.Now()
	a := modernWorking(now)
	a.NeedsLogin = true
	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)
	if v.RunState != runStuckAtLogin {
		t.Errorf("runState = %v, want stuck-at-login", v.RunState)
	}
	if !v.Stuck {
		t.Error("login-stuck + expected active must be STUCK")
	}
	if !v.Impotent {
		t.Error("running but login-blocked must be IMPOTENT")
	}
	if v.Able {
		t.Error("login-blocked agent must not be Able")
	}
	if v.BlockedReason == "" {
		t.Error("login block must carry a reason")
	}
}

// A hive-level blocker (App perm) makes a running, capable agent IMPOTENT even
// though its ACMM gates say it can push/merge.
func TestVerdict_HiveBlockerMakesImpotent(t *testing.T) {
	now := time.Now()
	a := modernWorking(now)
	b := hiveBlockers{GitHubAppPermIssue: "App missing contents:write"}
	v := deriveAgentVerdict(a, b, 5, now)
	if v.Able {
		t.Error("App-perm-blocked agent must not be Able")
	}
	if !v.Impotent {
		t.Error("running + hive-blocked must be IMPOTENT")
	}
	if v.BlockedReason != "App missing contents:write" {
		t.Errorf("reason = %q, want the App perm issue", v.BlockedReason)
	}
	if v.RunState != runWorking {
		t.Errorf("hive blocker does not change run-state; got %v", v.RunState)
	}
}

// An inference backend that reports NeedsLogin is NOT treated as login-blocked
// (those backends never sit on a CLI login prompt).
func TestVerdict_InferenceBackendIgnoresNeedsLogin(t *testing.T) {
	now := time.Now()
	a := modernWorking(now)
	a.Backend = "llm-d"
	a.NeedsLogin = true
	a.CanOpenIssue, a.CanOpenPR, a.CanMerge = true, true, true
	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)
	if v.RunState == runStuckAtLogin {
		// classifyInactiveAgent still keys off NeedsLogin regardless of backend;
		// that is a pre-existing behavior. What matters for capability is that
		// the inference backend is not counted as login-blocked for Able.
		t.Log("note: run-state still login per shared classifier")
	}
	if !v.Able {
		t.Error("inference backend must not be login-blocked for capability")
	}
}

// Paused agent: quiet-by-design, never stuck or impotent, whatever else is set.
func TestVerdict_PausedIsQuietByDesign(t *testing.T) {
	now := time.Now()
	a := modernWorking(now)
	a.Paused = true
	a.State = "paused"
	a.NeedsLogin = true // even with a fault signal, paused wins
	v := deriveAgentVerdict(a, hiveBlockers{GitHubAppRequired: true}, 5, now)
	if v.RunState != runQuietByDesign || !v.QuietByDesign {
		t.Errorf("paused must be quiet-by-design, got %v", v.RunState)
	}
	if v.Stuck || v.Impotent {
		t.Errorf("paused agent must carry no fault delta: %+v", v)
	}
}

// Not expected active in the current mode (governor pauses it there) and not
// running: quiet-by-design, not stuck.
func TestVerdict_ExpectedOffIsQuiet(t *testing.T) {
	now := time.Now()
	a := AgentSummary{
		Name: "brainstorm", State: "stopped", Backend: "claude",
		Enabled: true, ExpectedActive: false,
		CanOpenIssue: true, CanOpenPR: true,
	}
	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)
	if v.RunState != runQuietByDesign {
		t.Errorf("expected-off agent = %v, want quiet-by-design", v.RunState)
	}
	if v.Stuck {
		t.Error("expected-off agent must never be STUCK")
	}
}

// Legacy spoke: all new fields zero. Must read as UNKNOWN — never stuck, never
// impotent, capability gray.
func TestVerdict_LegacySpokeIsUnknownNeverAlarms(t *testing.T) {
	now := time.Now()
	// A legacy running agent (no capability fields, no backend, not expected).
	a := AgentSummary{Name: "old", State: "running", StartedAt: settled(now), LastActivityAt: activeAt(now, 1*time.Minute)}
	v := deriveAgentVerdict(a, hiveBlockers{GitHubAppRequired: true}, 5, now)
	if v.CapabilityTier != tierGray {
		t.Errorf("legacy agent tier = %s, want gray", v.CapabilityTier)
	}
	if v.Able || v.Stuck || v.Impotent {
		t.Errorf("legacy agent must not be able/stuck/impotent: %+v", v)
	}
	// A legacy stopped agent must not be called dead/stuck.
	a2 := AgentSummary{Name: "old2", State: "stopped"}
	v2 := deriveAgentVerdict(a2, hiveBlockers{}, 5, now)
	if v2.Stuck || v2.RunState == runDead {
		t.Errorf("legacy stopped agent must not be dead/stuck: %+v", v2)
	}
}

// An advisory (issues-only) agent that cannot merge is NOT impotent — merging
// was never its mission. Able means "can do what its mode grants".
func TestVerdict_IssuesOnlyAgentIsAble(t *testing.T) {
	now := time.Now()
	a := AgentSummary{
		Name: "advisor", State: "running", Backend: "claude",
		Enabled: true, ExpectedActive: true,
		CanOpenIssue: true, CanOpenPR: false, CanMerge: false,
		StartedAt: settled(now), LastActivityAt: activeAt(now, 1*time.Minute),
	}
	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)
	if !v.Able {
		t.Error("issues-only agent that can open issues must be Able")
	}
	if v.Impotent {
		t.Error("issues-only agent is not impotent for lacking merge")
	}
}

func TestRollup_Counts(t *testing.T) {
	now := time.Now()
	working := modernWorking(now)
	loginStuck := modernWorking(now)
	loginStuck.Name, loginStuck.NeedsLogin = "quality", true
	down := modernWorking(now)
	down.Name, down.State = "guide", "stopped"
	paused := modernWorking(now)
	paused.Name, paused.Paused, paused.State = "brainstorm", true, "paused"

	agents := []AgentSummary{working, loginStuck, down, paused}
	r := rollupAgents(agents, hiveBlockers{}, 5, now)

	// expected: working, loginStuck, down are ExpectedActive (paused copy is too,
	// but its expected count still increments since ExpectedActive is set).
	if r.Expected != 4 {
		t.Errorf("expected = %d, want 4 (all carry ExpectedActive)", r.Expected)
	}
	if r.Running != 1 {
		t.Errorf("running = %d, want 1 (only the healthy one)", r.Running)
	}
	if r.Able != 1 {
		t.Errorf("able = %d, want 1", r.Able)
	}
	// stuck: loginStuck (running+login) and down (dead). paused is not stuck.
	if r.Stuck != 2 {
		t.Errorf("stuck = %d, want 2 (login-stuck + down)", r.Stuck)
	}
	// impotent: only loginStuck (running but blocked). down is not running.
	if r.Impotent != 1 {
		t.Errorf("impotent = %d, want 1 (login-stuck)", r.Impotent)
	}
}

func TestBuildAgentVerdicts_ShapeAndSkipBlank(t *testing.T) {
	now := time.Now()
	agents := []AgentSummary{modernWorking(now), {Name: "  "}}
	out := buildAgentVerdicts(agents, hiveBlockers{}, 5, now)
	if len(out) != 1 {
		t.Fatalf("blank-named agent must be skipped; got %d rows", len(out))
	}
	if out[0].RunState != "working" {
		t.Errorf("runState serialized = %q, want working", out[0].RunState)
	}
	if !out[0].CanMerge || out[0].CapabilityTier != tierGreen {
		t.Errorf("verdict JSON missing derived fields: %+v", out[0])
	}
}
