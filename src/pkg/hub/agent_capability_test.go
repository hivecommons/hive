package hub

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/inferencehealth"
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

func TestVerdict_BoundGatewayFaultBlocksCapabilityAndAbleCount(t *testing.T) {
	now := time.Now()
	blocked := modernWorking(now)
	blocked.Name = "scanner"
	blocked.Backend = "litellm"
	healthy := modernWorking(now)
	healthy.Name = "guide"
	healthy.Backend = "claude"
	blockers := hiveBlockers{GatewayHealth: []inferencehealth.GatewayStatus{{
		Name:        "litellm",
		ErrorClass:  inferencehealth.ClassAuth,
		HTTPStatus:  401,
		LastErrorAt: now.UTC().Format(time.RFC3339),
	}}}
	v := deriveAgentVerdict(blocked, blockers, 5, now)
	if v.Able || v.CapabilityTier != tierRed || !v.Impotent {
		t.Fatalf("gateway-bound agent = %+v, want not able/red/impotent", v)
	}
	if v.BlockedReason != "inference gateway 'litellm' rejected key (401)" {
		t.Fatalf("blocked reason = %q", v.BlockedReason)
	}
	r := rollupAgents([]AgentSummary{blocked, healthy}, blockers, 5, now)
	if r.Able != 1 || r.Problems != 1 {
		t.Fatalf("rollup able=%d problems=%d, want able=1 problems=1", r.Able, r.Problems)
	}
}

func TestVerdict_ProviderLimitOnlyBlocksNamedQuotaAgents(t *testing.T) {
	now := time.Now()
	guide := modernWorking(now)
	guide.Name = "guide"
	guide.QuotaExhausted = true
	quality := modernWorking(now)
	quality.Name = "quality"
	scanner := modernWorking(now)
	scanner.Name = "scanner"

	blockers := hiveBlockers{
		ProviderLimitReason: "1 agent(s) out of provider quota",
		ProviderLimitAgents: []string{"guide"},
	}
	r := rollupAgents([]AgentSummary{guide, quality, scanner}, blockers, 5, now)
	if r.Problems != 1 || r.QuotaExhausted != 1 || r.Able != 2 {
		t.Fatalf("rollup = %+v, want one quota problem and two able agents", r)
	}
	v := deriveAgentVerdict(guide, blockers, 5, now)
	if v.BlockedReason != "1 agent(s) out of provider quota (affected: guide)" {
		t.Fatalf("blocked reason = %q", v.BlockedReason)
	}
	if v := deriveAgentVerdict(quality, blockers, 5, now); v.Problem {
		t.Fatalf("unnamed quota agent was blocked: %+v", v)
	}
}

func TestVerdict_ProviderLimitHiveWideBlocksAllAgents(t *testing.T) {
	now := time.Now()
	guide := modernWorking(now)
	guide.Name = "guide"
	scanner := modernWorking(now)
	scanner.Name = "scanner"
	blockers := hiveBlockers{
		ProviderLimitReason:   "provider spending limit reached",
		ProviderLimitHiveWide: true,
	}
	r := rollupAgents([]AgentSummary{guide, scanner}, blockers, 5, now)
	if r.Problems != 2 || r.QuotaExhausted != 2 {
		t.Fatalf("rollup = %+v, want hive-wide quota to block both agents", r)
	}
}

func TestVerdict_ProviderLimitReasonWithoutQuotaNamesBlocksNoAgents(t *testing.T) {
	now := time.Now()
	blockers := hiveBlockers{ProviderLimitReason: "1 agent(s) out of provider quota"}
	r := rollupAgents([]AgentSummary{modernWorking(now)}, blockers, 5, now)
	if r.Problems != 0 || r.Able != 1 {
		t.Fatalf("rollup = %+v, want aggregate non-hive-wide reason ignored for agent blockers", r)
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

func TestVerdict_QuotaExhaustedOutranksIdle(t *testing.T) {
	now := time.Now()
	a := modernWorking(now)
	a.LastActivityAt = activeAt(now, 2*time.Hour)
	a.QuotaExhausted = true
	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)
	if v.RunState != runQuotaExhausted {
		t.Fatalf("runState = %v, want quota-exhausted", v.RunState)
	}
	if !v.Problem || !v.Stuck || !v.Impotent {
		t.Fatalf("quota-exhausted expected-active agent must be a stuck/impotent problem: %+v", v)
	}
	if v.BlockedReason != "provider quota exhausted" {
		t.Fatalf("blockedReason = %q, want provider quota exhausted", v.BlockedReason)
	}
	r := rollupAgents([]AgentSummary{a}, hiveBlockers{}, 5, now)
	if r.QuotaExhausted != 1 || r.IdleWithWork != 0 {
		t.Fatalf("rollup quota=%d idle=%d, want quota=1 idle=0", r.QuotaExhausted, r.IdleWithWork)
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
// for CAPABILITY (those backends never sit on a CLI login prompt), so its
// badge tier stays green — even though the shared run-state classifier still
// keys off NeedsLogin regardless of backend (a pre-existing behavior we do not
// change here). Capability (would it work) is distinct from run-state (is the
// pane live).
func TestVerdict_InferenceBackendNotLoginBlockedForCapability(t *testing.T) {
	now := time.Now()
	a := modernWorking(now)
	a.Backend = "llm-d"
	a.NeedsLogin = true
	a.CanOpenIssue, a.CanOpenPR, a.CanMerge = true, true, true
	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)
	if v.CapabilityTier != tierGreen {
		t.Errorf("inference backend must not be login-blocked for capability; tier=%s reason=%q",
			v.CapabilityTier, v.BlockedReason)
	}
	if v.BlockedReason != "" {
		t.Errorf("inference backend must carry no blocked reason, got %q", v.BlockedReason)
	}
	// It must NOT be flagged impotent, because it is not capability-blocked.
	if v.Impotent {
		t.Error("inference backend with a stray needsLogin must not be IMPOTENT")
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
	// Legacy agent must be run-state UNKNOWN (not "working" off a bare state
	// field) and never a Problem — this is the Bluefin "off+working+✗✗✗" fix.
	if v.RunState != runUnknown {
		t.Errorf("legacy agent run-state = %v, want unknown", v.RunState)
	}
	if v.Problem {
		t.Error("legacy/unknown agent must never be a PROBLEM (can't-verify ≠ broken)")
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

// The live EPM/alchemy-logging shape: the spoke reports enabled+running+
// needsLogin with capabilities but OMITS expectedActive. The wedged credential
// is a hive-wide fault; the off-schedule quiet branch must not swallow it.
func TestVerdict_LoginStuckWithoutExpectedActive(t *testing.T) {
	now := time.Now()
	a := modernWorking(now)
	a.Name, a.Backend = "quality", "copilot"
	a.ExpectedActive = false
	a.CanMerge = false
	a.NeedsLogin = true
	v := deriveAgentVerdict(a, hiveBlockers{}, 40, now)
	if v.RunState != runStuckAtLogin {
		t.Errorf("runState = %v, want stuck-at-login (not quiet-by-design)", v.RunState)
	}
	if v.QuietByDesign {
		t.Error("a login-stuck agent is never quiet by design")
	}
	if !v.Stuck {
		t.Error("login-stuck must be STUCK even when expectedActive is absent")
	}
	if !v.Problem {
		t.Error("login-stuck must be a PROBLEM even when expectedActive is absent")
	}
}

// A genuinely off-schedule agent without a login prompt keeps the quiet branch:
// the login carve-out above must not resurrect the surge-mode false alarm.
func TestVerdict_OffScheduleWithoutLoginStaysQuiet(t *testing.T) {
	now := time.Now()
	a := modernWorking(now)
	a.ExpectedActive = false
	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)
	if v.RunState != runQuietByDesign || v.Problem {
		t.Errorf("off-schedule running agent must stay quiet-by-design; got %v problem=%v", v.RunState, v.Problem)
	}
}

func TestVerdict_CadenceIdleIsWorkingUntilCadenceMissed(t *testing.T) {
	now := time.Now()
	a := modernWorking(now)
	a.Name = "quality"
	a.LastActivityAt = activeAt(now, time.Hour)
	a.KickIntervalSec = int64(2 * time.Hour / time.Second)

	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)
	if v.RunState != runWorking || v.Problem {
		t.Fatalf("cadence-idle agent before next run must stay working/non-problem, got %+v", v)
	}

	a.LastActivityAt = activeAt(now, 2*time.Hour+inactiveAgentCadenceSlack+time.Minute)
	v = deriveAgentVerdict(a, hiveBlockers{}, 5, now)
	if v.RunState != runIdleAtPrompt || !v.Problem {
		t.Fatalf("agent past cadence+slack must be idle-at-prompt problem, got %+v", v)
	}

	r := rollupAgents([]AgentSummary{a}, hiveBlockers{}, 5, now)
	if r.IdleWithWork != 1 || r.Problems != 1 {
		t.Fatalf("rollup must count missed-cadence idle-with-work, got %+v", r)
	}
}

func TestVerdict_WriteIncapableAdvisoryAgentNotIdleWithWork(t *testing.T) {
	now := time.Now()
	a := AgentSummary{
		Name: "guide", State: "running", Backend: "bob",
		Enabled: true, ExpectedActive: true,
		StartedAt: settled(now), LastActivityAt: activeAt(now, 3*time.Hour),
	}

	v := deriveAgentVerdict(a, hiveBlockers{}, 9, now)
	if v.RunState != runWorking || !v.Able || v.Problem {
		t.Fatalf("write-incapable advisory agent must be working/able/non-problem, got %+v", v)
	}
}

func TestRollup_LoginStuckCount(t *testing.T) {
	now := time.Now()
	stuck := modernWorking(now)
	stuck.Name, stuck.NeedsLogin = "quality", true
	down := modernWorking(now)
	down.Name, down.State = "guide", "stopped"
	r := rollupAgents([]AgentSummary{modernWorking(now), stuck, down}, hiveBlockers{}, 5, now)
	if r.Problems != 2 {
		t.Fatalf("problems = %d, want 2", r.Problems)
	}
	if r.LoginStuck != 1 {
		t.Errorf("loginStuck = %d, want 1 (only the login-wedged agent)", r.LoginStuck)
	}
	if r.DeadOrGone != 1 {
		t.Errorf("deadOrGone = %d, want 1 (the stopped agent)", r.DeadOrGone)
	}
}

func TestRollup_LoginPromptWithinGraceStillBucketsHiveCause(t *testing.T) {
	now := time.Now()
	guide := modernWorking(now)
	guide.Name, guide.Backend = "guide", "copilot"
	guide.NeedsLogin = true
	guide.StartedAt = now.Add(-(inactiveAgentStartupGrace + 2*time.Minute)).UTC().Format(time.RFC3339)

	v := deriveAgentVerdict(guide, hiveBlockers{}, 17, now)
	if v.RunState != runWorking {
		t.Fatalf("login prompt inside 20m grace should still be runWorking, got %+v", v)
	}
	if !v.Problem || v.BlockedReason != "sitting at login prompt" {
		t.Fatalf("ABLE leg must mark the login prompt as the problem cause, got %+v", v)
	}

	r := rollupAgents([]AgentSummary{guide}, hiveBlockers{}, 17, now)
	if r.Problems != 1 || r.LoginStuck != 1 {
		t.Fatalf("login-in-grace pool-hive shape must bucket as login-stuck cause, got %+v", r)
	}
}

// TestParseAgentTime_CompactWireFormat pins the colonless timestamp variant
// spokes emit live ("2026-08-22T024118Z"). RFC3339-only parsing returned !ok
// for every such value, which silently disabled the needs-login grace and the
// idle rule fleet-wide — login-wedged agents read as healthy.
func TestParseAgentTime_CompactWireFormat(t *testing.T) {
	got, ok := parseAgentTime("2026-08-22T024118Z")
	if !ok {
		t.Fatal("compact wire timestamp did not parse")
	}
	want := time.Date(2026, 8, 22, 2, 41, 18, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parsed %v, want %v", got, want)
	}
	if _, ok := parseAgentTime("garbage"); ok {
		t.Error("garbage parsed as a time")
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
	if r.Paused != 1 {
		t.Errorf("paused = %d, want 1 (expected-active paused agent)", r.Paused)
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

func TestRollup_AllExpectedAgentsPaused(t *testing.T) {
	now := time.Now()
	agent := func(name string) AgentSummary {
		a := modernWorking(now)
		a.Name = name
		a.State = agentStatePaused
		a.Paused = true
		return a
	}

	r := rollupAgents([]AgentSummary{
		agent("quality"),
		agent("scanner"),
		agent("supervisor"),
	}, hiveBlockers{}, 11, now)
	if r.Expected != 3 || r.Paused != 3 || r.Running != 0 || r.Problems != 0 {
		t.Fatalf("all-paused live shape should be expected+paused, not dead/problematic: %+v", r)
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

func TestBuildAgentVerdicts_SortedByAgentName(t *testing.T) {
	now := time.Now()
	z := modernWorking(now)
	z.Name = "zeta"
	a := modernWorking(now)
	a.Name = "alpha"
	m := modernWorking(now)
	m.Name = "middle"

	out := buildAgentVerdicts([]AgentSummary{z, m, a}, hiveBlockers{}, 5, now)
	got := []string{out[0].Name, out[1].Name, out[2].Name}
	want := []string{"alpha", "middle", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("agent verdict order = %v, want %v", got, want)
		}
	}
}

func TestComputeAgentRosterMismatch_WarningReason(t *testing.T) {
	agents := []AgentSummary{
		{Name: "scanner"},
		{Name: "quality"},
		{Name: "guide"},
		{Name: "supervisor"},
		{Name: "outreach"},
	}
	got := computeAgentRosterMismatch(2, agents)
	if got == nil {
		t.Fatal("expected mismatch warning")
	}
	if got.Reason != "agent roster mismatch for L2: missing brainstorm; unexpected: outreach" {
		t.Fatalf("reason = %q", got.Reason)
	}
	if len(got.Missing) != 1 || got.Missing[0] != "brainstorm" {
		t.Fatalf("missing = %v, want [brainstorm]", got.Missing)
	}
	if len(got.Unexpected) != 1 || got.Unexpected[0] != "outreach" {
		t.Fatalf("unexpected = %v, want [outreach]", got.Unexpected)
	}
}

func TestComputeAgentRosterMismatch_NoWarningForMatchingOrUnknown(t *testing.T) {
	matching := []AgentSummary{
		{Name: "brainstorm"},
		{Name: "guide"},
		{Name: "quality"},
		{Name: "scanner"},
		{Name: "supervisor"},
	}
	if got := computeAgentRosterMismatch(2, matching); got != nil {
		t.Fatalf("matching roster got warning %+v", got)
	}
	if got := computeAgentRosterMismatch(99, []AgentSummary{{Name: "scanner"}}); got != nil {
		t.Fatalf("unknown level got warning %+v", got)
	}
	if got := computeAgentRosterMismatch(2, nil); got != nil {
		t.Fatalf("missing legacy agent data got warning %+v", got)
	}
}

// PROBLEM = governor expects it on AND it can't deliver, for any reason. It
// unifies stuck/impotent/blocked and never fires on paused/off/legacy agents.
func TestVerdict_ProblemFlag(t *testing.T) {
	now := time.Now()
	// Expected + login-stuck → problem.
	stuck := modernWorking(now)
	stuck.NeedsLogin = true
	if v := deriveAgentVerdict(stuck, hiveBlockers{}, 5, now); !v.Problem {
		t.Error("expected + login-stuck must be a PROBLEM")
	}
	// Expected + hive-blocked (running but impotent) → problem.
	blk := modernWorking(now)
	if v := deriveAgentVerdict(blk, hiveBlockers{RepoTargetMisconfigured: true}, 5, now); !v.Problem {
		t.Error("expected + blocked must be a PROBLEM")
	}
	// Expected + working + able → NOT a problem.
	if v := deriveAgentVerdict(modernWorking(now), hiveBlockers{}, 5, now); v.Problem {
		t.Error("healthy agent must not be a PROBLEM")
	}
	// Paused (even if it carries ExpectedActive) → NOT a problem.
	paused := modernWorking(now)
	paused.Paused, paused.State = true, "paused"
	if v := deriveAgentVerdict(paused, hiveBlockers{}, 5, now); v.Problem {
		t.Error("paused agent must never be a PROBLEM")
	}
	// Not expected active → NOT a problem even if it can't work.
	off := modernWorking(now)
	off.ExpectedActive, off.State = false, "stopped"
	if v := deriveAgentVerdict(off, hiveBlockers{}, 5, now); v.Problem {
		t.Error("off-schedule agent must never be a PROBLEM")
	}
}

func TestRollup_ProblemsAndKnown(t *testing.T) {
	now := time.Now()
	healthy := modernWorking(now)
	stuck := modernWorking(now)
	stuck.Name, stuck.NeedsLogin = "quality", true
	legacy := AgentSummary{Name: "old", State: "running", StartedAt: settled(now)}

	r := rollupAgents([]AgentSummary{healthy, stuck, legacy}, hiveBlockers{}, 5, now)
	if r.Problems != 1 {
		t.Errorf("problems = %d, want 1 (the login-stuck one)", r.Problems)
	}
	if r.Known != 2 {
		t.Errorf("known = %d, want 2 (legacy agent excluded)", r.Known)
	}

	// All-legacy hive: Known==0 so the frontend renders it UNKNOWN, not "0 able".
	allLegacy := rollupAgents([]AgentSummary{legacy}, hiveBlockers{}, 5, now)
	if allLegacy.Known != 0 || allLegacy.Problems != 0 {
		t.Errorf("all-legacy hive must be known=0 problems=0, got %+v", allLegacy)
	}
}

func TestVerdict_RestartStormThresholdBoundary(t *testing.T) {
	t.Setenv(EnvAgentRestartProblemThreshold, "5")
	now := time.Now()
	a := modernWorking(now)
	a.Restarts = AgentRestartTelemetry{Total: 10, Last24h: 4, LastReason: "crash"}
	if v := deriveAgentVerdict(a, hiveBlockers{}, 0, now); v.Problem || v.RunState == runRestartStorm {
		t.Fatalf("below threshold verdict = %+v, want no restart problem", v)
	}
	a.Restarts.Last24h = 5
	v := deriveAgentVerdict(a, hiveBlockers{}, 0, now)
	if !v.Problem || v.RunState != runRestartStorm || v.BlockedReason != "agent restarts: scanner ×5/24h (crash)" {
		t.Fatalf("at threshold verdict = %+v", v)
	}
	r := rollupAgents([]AgentSummary{a}, hiveBlockers{}, 0, now)
	if r.Problems != 1 || r.RestartStorms != 1 {
		t.Fatalf("rollup problems=%d restartStorms=%d, want 1/1", r.Problems, r.RestartStorms)
	}
}

func TestApplyAgentRestartResetBaselines(t *testing.T) {
	now := time.Date(2026, 9, 4, 21, 10, 0, 0, time.UTC)
	agents := []AgentSummary{{Name: "scanner", Restarts: AgentRestartTelemetry{Total: 12, Last24h: 9}}}
	applyAgentRestartResetBaselines(agents, map[string]AgentRestartReset{
		"scanner": {ResetAt: now.Add(-time.Hour).Format(time.RFC3339), By: "andy", TotalBaseline: 10},
	}, now)
	if agents[0].Restarts.Last24h != 2 || agents[0].Restarts.ResetBy != "andy" {
		t.Fatalf("reset-adjusted restarts = %+v, want delta 2 by andy", agents[0].Restarts)
	}
}

func TestPendingAgentRestartResetsForHeartbeatDrains(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	if err := saveSaaSHive(&SaaSHive{ID: "h1", AgentRestartResets: map[string]AgentRestartReset{
		"scanner": {ResetAt: time.Now().UTC().Format(time.RFC3339), TotalBaseline: 3, Pending: true},
	}}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	got := pendingAgentRestartResetsForHeartbeat("h1")
	if len(got) != 1 || got[0] != "scanner" {
		t.Fatalf("pending resets = %#v", got)
	}
	if again := pendingAgentRestartResetsForHeartbeat("h1"); len(again) != 0 {
		t.Fatalf("pending reset was not drained: %#v", again)
	}
	if h := loadSaaSHive("h1"); h.AgentRestartResets["scanner"].TotalBaseline != 0 {
		t.Fatalf("delivered reset must switch to spoke-zero baseline: %+v", h.AgentRestartResets["scanner"])
	}
}
