package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
)

// quotaExhaustedProcessCount must count ONLY running, unpaused processes whose
// pane showed quota exhaustion — a paused or stopped agent out of quota is not
// an actionable heartbeat signal.
func TestQuotaExhaustedProcessCount(t *testing.T) {
	statuses := map[string]*agent.AgentProcess{
		"counted":    {State: agent.StateRunning, QuotaExhausted: true},
		"paused":     {State: agent.StateRunning, QuotaExhausted: true, Paused: true},
		"stopped":    {State: agent.StateStopped, QuotaExhausted: true},
		"has-quota":  {State: agent.StateRunning, QuotaExhausted: false},
		"nil-status": nil,
	}
	if got := quotaExhaustedProcessCount(statuses); got != 1 {
		t.Errorf("quotaExhaustedProcessCount = %d, want 1", got)
	}
	if got := quotaExhaustedProcessCount(nil); got != 0 {
		t.Errorf("quotaExhaustedProcessCount(nil) = %d, want 0", got)
	}
}

// advisoryIssueNumber must treat a recorded 0 as "not resolved" — 0 is the
// zero value a failed ensure leaves behind, and posting to issue 0 is not a
// thing. This is the read-side twin of advisoryIssueUnresolved.
func TestAdvisoryIssueNumber(t *testing.T) {
	issues := map[string]int{"org/ok": 42, "org/zero": 0}

	if num, ok := advisoryIssueNumber(issues, "org/ok"); !ok || num != 42 {
		t.Errorf("advisoryIssueNumber(org/ok) = %d, %v; want 42, true", num, ok)
	}
	if _, ok := advisoryIssueNumber(issues, "org/zero"); ok {
		t.Error("advisoryIssueNumber(org/zero) reported ok for a recorded 0")
	}
	if _, ok := advisoryIssueNumber(issues, "org/missing"); ok {
		t.Error("advisoryIssueNumber(org/missing) reported ok for an absent repo")
	}
	if _, ok := advisoryIssueNumber(nil, "org/ok"); ok {
		t.Error("advisoryIssueNumber(nil map) reported ok")
	}
}

// convergenceModeEffect is operator-facing notification text: each enrolled
// mode must produce a distinct, non-empty explanation, and every unknown mode
// must fall back to the inert-baseline wording.
func TestConvergenceModeEffect(t *testing.T) {
	shadow := convergenceModeEffect(config.ConvergenceModeShadow)
	enforce := convergenceModeEffect(config.ConvergenceModeEnforce)
	off := convergenceModeEffect("off")
	unknown := convergenceModeEffect("bogus-mode")

	for name, s := range map[string]string{"shadow": shadow, "enforce": enforce, "default": off} {
		if s == "" {
			t.Errorf("convergenceModeEffect(%s) returned empty text", name)
		}
	}
	if shadow == enforce {
		t.Error("shadow and enforce modes share the same explanation text")
	}
	if off != unknown {
		t.Errorf("unknown mode %q should get the default explanation %q", unknown, off)
	}
}

// githubAppTokenHeartbeatFields must report nothing at all for a hive that is
// not App-authenticated — a PAT hive has no installation-token cache to age.
func TestGitHubAppTokenHeartbeatFieldsNoApp(t *testing.T) {
	if s, at, e := githubAppTokenHeartbeatFields(nil, "detail"); s != "" || at != "" || e != "" {
		t.Errorf("nil config: got (%q, %q, %q), want all empty", s, at, e)
	}
	cfg := &config.Config{}
	if s, at, e := githubAppTokenHeartbeatFields(cfg, "detail"); s != "" || at != "" || e != "" {
		t.Errorf("no-App config: got (%q, %q, %q), want all empty", s, at, e)
	}
}

// quotaExhaustedAgentReason must be empty for a zero/negative count so the
// heartbeat never carries a vacuous provider-limit reason.
func TestQuotaExhaustedAgentReason(t *testing.T) {
	if got := quotaExhaustedAgentReason(0); got != "" {
		t.Errorf("quotaExhaustedAgentReason(0) = %q, want empty", got)
	}
	if got := quotaExhaustedAgentReason(-1); got != "" {
		t.Errorf("quotaExhaustedAgentReason(-1) = %q, want empty", got)
	}
	if got := quotaExhaustedAgentReason(3); got != "3 agent(s) out of provider quota" {
		t.Errorf("quotaExhaustedAgentReason(3) = %q", got)
	}
}
