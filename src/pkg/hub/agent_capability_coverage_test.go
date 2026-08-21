package hub

import (
	"testing"
	"time"
)

func TestAgentRunState_StringAllCases(t *testing.T) {
	cases := map[agentRunState]string{
		runUnknown:        "unknown",
		runWorking:        "working",
		runIdleAtPrompt:   "idle-at-prompt",
		runStuckAtLogin:   "stuck-at-login",
		runSessionGone:    "session-gone",
		runDead:           "dead",
		runQuietByDesign:  "quiet-by-design",
		agentRunState(99): "unknown",
	}
	for st, want := range cases {
		if got := st.String(); got != want {
			t.Errorf("String(%d) = %q, want %q", st, got, want)
		}
	}
}

func TestHiveBlockers_ReasonPrecedence(t *testing.T) {
	cases := []struct {
		name string
		b    hiveBlockers
		want string
	}{
		{"none", hiveBlockers{}, ""},
		{"perm issue wins", hiveBlockers{GitHubAppPermIssue: "need contents:write", GitHubAppRequired: true}, "need contents:write"},
		{"app required", hiveBlockers{GitHubAppRequired: true}, "GitHub App not installed or not authorized"},
		{"app state", hiveBlockers{GitHubAppState: "suspended"}, "GitHub App state: suspended"},
		{"repo target issue", hiveBlockers{RepoTargetIssue: "wrong org"}, "wrong org"},
		{"repo misconfigured", hiveBlockers{RepoTargetMisconfigured: true}, "repo target misconfigured"},
		{"inference auth", hiveBlockers{InferenceAuthError: "gateway key expired"}, "gateway key expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.b.reason(); got != tc.want {
				t.Errorf("reason() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHiveBlockers_Any(t *testing.T) {
	if (hiveBlockers{}).any() {
		t.Error("empty blockers must report none")
	}
	each := []hiveBlockers{
		{GitHubAppRequired: true},
		{GitHubAppPermIssue: "x"},
		{GitHubAppState: "suspended"},
		{RepoTargetMisconfigured: true},
		{RepoTargetIssue: "x"},
		{InferenceAuthError: "x"},
	}
	for i, b := range each {
		if !b.any() {
			t.Errorf("case %d: any() = false, want true", i)
		}
	}
}

func TestBlockingGitHubAppState(t *testing.T) {
	healthy := []string{"", "ok", "OK", "active", " installed ", "healthy"}
	for _, s := range healthy {
		if blockingGitHubAppState(s) {
			t.Errorf("%q must not block", s)
		}
	}
	blocking := []string{"suspended", "perm_mismatch", "revoked", "unknown-thing"}
	for _, s := range blocking {
		if !blockingGitHubAppState(s) {
			t.Errorf("%q must block", s)
		}
	}
}

// Exercise the session-gone and idle-with-work run-states end to end.
func TestVerdict_SessionGoneAndIdle(t *testing.T) {
	now := time.Now()

	zombie := AgentSummary{
		Name: "z", State: "running", Backend: "claude",
		Enabled: true, ExpectedActive: true, SessionMissing: true,
		CanOpenIssue: true, CanOpenPR: true, CanMerge: true,
		StartedAt: settled(now),
	}
	v := deriveAgentVerdict(zombie, hiveBlockers{}, 5, now)
	if v.RunState != runSessionGone {
		t.Errorf("zombie run-state = %v, want session-gone", v.RunState)
	}
	if !v.Stuck {
		t.Error("expected-active zombie must be STUCK")
	}

	idle := AgentSummary{
		Name: "i", State: "running", Backend: "claude",
		Enabled: true, ExpectedActive: true,
		CanOpenIssue: true, CanOpenPR: true, CanMerge: true,
		StartedAt: settled(now), LastActivityAt: activeAt(now, 90*time.Minute),
	}
	// queuedWork > 0 so the idle rule fires.
	vi := deriveAgentVerdict(idle, hiveBlockers{}, 5, now)
	if vi.RunState != runIdleAtPrompt {
		t.Errorf("idle-with-work run-state = %v, want idle-at-prompt", vi.RunState)
	}
	if !vi.Stuck {
		t.Error("expected-active idle-with-work must be STUCK")
	}
}

// no-capability agent (advisory below issues) → red tier + reason.
func TestVerdict_NoCapabilityIsRed(t *testing.T) {
	now := time.Now()
	a := AgentSummary{
		Name: "adv", State: "running", Backend: "claude",
		Enabled: true, ExpectedActive: true,
		CanOpenIssue: false, CanOpenPR: false, CanMerge: false,
		StartedAt: settled(now), LastActivityAt: activeAt(now, 1*time.Minute),
	}
	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)
	if v.CapabilityTier != tierRed {
		t.Errorf("no-capability tier = %s, want red", v.CapabilityTier)
	}
	if v.BlockedReason == "" {
		t.Error("no-capability agent must carry a reason")
	}
	// Running but not capable → impotent.
	if !v.Impotent {
		t.Error("running no-capability agent must be IMPOTENT")
	}
}

// Tier arms: green (unblocked, full mission), amber (issues still work but a
// blocker gates a mode-granted write), red (cannot even open an issue).
func TestVerdict_TierArms(t *testing.T) {
	now := time.Now()
	writer := AgentSummary{
		Name: "a", State: "running", Backend: "claude",
		Enabled: true, ExpectedActive: true,
		CanOpenIssue: true, CanOpenPR: true, CanMerge: true,
		StartedAt: settled(now), LastActivityAt: activeAt(now, 1*time.Minute),
	}
	// green — unblocked, full mission.
	if v := deriveAgentVerdict(writer, hiveBlockers{}, 5, now); v.CapabilityTier != tierGreen {
		t.Errorf("unblocked writer = %s, want green", v.CapabilityTier)
	}
	// amber — can still open issues, but a hive blocker gates its writes.
	if v := deriveAgentVerdict(writer, hiveBlockers{RepoTargetMisconfigured: true}, 5, now); v.CapabilityTier != tierAmber {
		t.Errorf("write-blocked writer = %s, want amber", v.CapabilityTier)
	}
	// amber — a login block on an interactive backend also gates writes while
	// issues remain conceptually open.
	loginBlk := writer
	loginBlk.NeedsLogin = true
	if v := deriveAgentVerdict(loginBlk, hiveBlockers{}, 5, now); v.CapabilityTier != tierAmber {
		t.Errorf("login-blocked writer = %s, want amber", v.CapabilityTier)
	}
	// red — issues-only agent (no write to gate) that is ALSO blocked at the
	// floor: model an agent with no capability at all.
	none := writer
	none.CanOpenIssue, none.CanOpenPR, none.CanMerge = false, false, false
	if v := deriveAgentVerdict(none, hiveBlockers{}, 5, now); v.CapabilityTier != tierRed {
		t.Errorf("no-capability agent = %s, want red", v.CapabilityTier)
	}
}

func TestMarkUnknown(t *testing.T) {
	v := agentVerdict{Able: true, Impotent: true, CapabilityTier: tierGreen, BlockedReason: "x"}
	v.markUnknown()
	if v.Able || v.Impotent || v.CapabilityTier != tierGray || v.BlockedReason != "" {
		t.Errorf("markUnknown did not reset: %+v", v)
	}
}

func TestInteractiveLoginBackend(t *testing.T) {
	inference := []string{"vllm", "llm-d", "litellm", "watsonx"}
	for _, b := range inference {
		if interactiveLoginBackend(b) {
			t.Errorf("%q must not be interactive-login", b)
		}
	}
	interactive := []string{"claude", "copilot", "gemini", "", "codex"}
	for _, b := range interactive {
		if !interactiveLoginBackend(b) {
			t.Errorf("%q must be interactive-login (empty defaults to interactive)", b)
		}
	}
}
