package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hub"
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

func TestGitHubAppTokenHeartbeatFields_UsesInjectableCachePath(t *testing.T) {
	oldPath := githubAppTokenCachePath
	t.Cleanup(func() { githubAppTokenCachePath = oldPath })
	cfg := &config.Config{GitHub: config.GitHubConfig{AppID: 123}}
	detail := "mint failed"

	githubAppTokenCachePath = filepath.Join(t.TempDir(), "missing.cache")
	status, minted, lastErr := githubAppTokenHeartbeatFields(cfg, detail)
	if status != hub.GitHubAppTokenStatusMissing || minted != "" || lastErr != detail {
		t.Fatalf("missing cache = %q/%q/%q, want missing/empty/detail", status, minted, lastErr)
	}

	nowPath := filepath.Join(t.TempDir(), "token.cache")
	if err := os.WriteFile(nowPath, []byte("token"), 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	githubAppTokenCachePath = nowPath
	status, minted, lastErr = githubAppTokenHeartbeatFields(cfg, detail)
	if status != hub.GitHubAppTokenStatusOK || minted == "" || lastErr != "" {
		t.Fatalf("fresh cache = %q/%q/%q, want ok/minted/empty", status, minted, lastErr)
	}

	staleAt := time.Now().Add(-hub.GitHubAppTokenStaleAfter - time.Minute)
	if err := os.Chtimes(nowPath, staleAt, staleAt); err != nil {
		t.Fatalf("stale cache: %v", err)
	}
	status, minted, lastErr = githubAppTokenHeartbeatFields(cfg, detail)
	if status != hub.GitHubAppTokenStatusStale || minted == "" || lastErr != detail {
		t.Fatalf("stale cache = %q/%q/%q, want stale/minted/detail", status, minted, lastErr)
	}

	parentFile := filepath.Join(t.TempDir(), "not-dir")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write parent file: %v", err)
	}
	githubAppTokenCachePath = filepath.Join(parentFile, "token.cache")
	status, minted, lastErr = githubAppTokenHeartbeatFields(cfg, detail)
	if status != hub.GitHubAppTokenStatusError || minted != "" || lastErr == "" {
		t.Fatalf("stat error = %q/%q/%q, want error/empty/error", status, minted, lastErr)
	}
}
