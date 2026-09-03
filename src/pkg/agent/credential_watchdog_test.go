package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestCredentialWatchdogInterval_Default(t *testing.T) {
	t.Setenv(CredentialWatchdogIntervalEnv, "")
	if got := credentialWatchdogInterval(); got != defaultCredentialWatchdogInterval {
		t.Errorf("expected default %v, got %v", defaultCredentialWatchdogInterval, got)
	}
}

func TestCredentialWatchdogInterval_EnvOverride(t *testing.T) {
	t.Setenv(CredentialWatchdogIntervalEnv, "30s")
	if got := credentialWatchdogInterval(); got != 30*time.Second {
		t.Errorf("expected 30s from env override, got %v", got)
	}
}

func TestCredentialWatchdogInterval_InvalidFallsBack(t *testing.T) {
	for _, bad := range []string{"not-a-duration", "-5m", "0s"} {
		t.Setenv(CredentialWatchdogIntervalEnv, bad)
		if got := credentialWatchdogInterval(); got != defaultCredentialWatchdogInterval {
			t.Errorf("env %q: expected default %v, got %v", bad, defaultCredentialWatchdogInterval, got)
		}
	}
}

// TestBackendInUse proves the per-backend gate honors both Config.Backend and a
// runtime BackendOverride.
func TestBackendInUse(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude"},
		"b": {Backend: "llm-d"},
	}, discardLogger(), ProjectContext{})

	if !m.backendInUse("claude") {
		t.Error("claude agent present but backendInUse(claude)=false")
	}
	if m.backendInUse("copilot") {
		t.Error("no copilot agent but backendInUse(copilot)=true")
	}

	m.mu.Lock()
	m.agents["b"].BackendOverride = "copilot"
	m.mu.Unlock()
	if !m.backendInUse("copilot") {
		t.Error("agent overridden to copilot but backendInUse(copilot)=false")
	}
}

// TestCopilotTokenUsable proves the Copilot probe: absent/empty → unusable,
// non-empty → usable.
func TestCopilotTokenUsable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copilot-user-token")

	if ok, reason := copilotTokenUsable(path); ok || reason != "missing" {
		t.Fatalf("absent file: expected (false,\"missing\"), got (%v,%q)", ok, reason)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, reason := copilotTokenUsable(path); ok || reason != "missing" {
		t.Fatalf("empty file: expected (false,\"missing\"), got (%v,%q)", ok, reason)
	}
	if err := os.WriteFile(path, []byte("ghu_exampletoken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := copilotTokenUsable(path); !ok {
		t.Fatal("populated file: expected usable")
	}
}

// writeClaudeCredsWithExpiry writes a Claude credentials file with the given access token
// and expiry, mirroring the shape claude.ReadAccessToken parses.
func writeClaudeCredsWithExpiry(t *testing.T, path, token string, expiresAtMillis int64) {
	t.Helper()
	body := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken": token,
			"expiresAt":   expiresAtMillis,
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeClaudeCredsRefreshable writes a credentials file whose access token has
// expired but whose refresh grant has not — the state a Claude fleet enters
// roughly once a day, since access tokens live 8h.
func writeClaudeCredsRefreshable(t *testing.T, path string) {
	t.Helper()
	body := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":           "sk-ant-oat-old",
			"expiresAt":             time.Now().Add(-2 * time.Hour).UnixMilli(),
			"refreshToken":          "sk-ant-ort-live",
			"refreshTokenExpiresAt": time.Now().Add(28 * 24 * time.Hour).UnixMilli(),
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClaudeTokenUsable proves the Claude probe distinguishes absent
// ("missing") from a spent login from a credential that still works.
func TestClaudeTokenUsable(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")

	if ok, reason := claudeTokenUsable(path); ok || reason != "missing" {
		t.Fatalf("absent file: expected (false,\"missing\"), got (%v,%q)", ok, reason)
	}

	// Expired access token, no refresh grant: genuinely spent, only a human
	// can fix it, and the watchdog's operator alert is correct.
	past := time.Now().Add(-time.Hour).UnixMilli()
	writeClaudeCredsWithExpiry(t, path, "sk-ant-oat-old", past)
	if ok, reason := claudeTokenUsable(path); ok || reason != "login expired (no usable refresh grant)" {
		t.Fatalf("expired file: expected (false,\"login expired (no usable refresh grant)\"), got (%v,%q)", ok, reason)
	}

	// Present and valid (expiresAt in the future).
	future := time.Now().Add(time.Hour).UnixMilli()
	writeClaudeCredsWithExpiry(t, path, "sk-ant-oat-fresh", future)
	if ok, _ := claudeTokenUsable(path); !ok {
		t.Fatal("valid file: expected usable")
	}

	// The regression this probe carried until 2026-09-01: an access token that
	// merely aged out is NOT an unusable credential. The refresh grant beside
	// it mints a new one on the next CLI start, so alerting here prescribed an
	// interactive login for a fleet that only needed a restart.
	writeClaudeCredsRefreshable(t, path)
	if ok, reason := claudeTokenUsable(path); !ok {
		t.Fatalf("expired-but-refreshable file: expected usable, got (%v,%q)", ok, reason)
	}
}

// TestEvalCredentialWatch_NotInUseIsSkipped proves a backend with no agent is
// never probed and never alerts, even when its file is absent.
func TestEvalCredentialWatch_NotInUseIsSkipped(t *testing.T) {
	m, sink := testManagerWithSink(t)
	m.mu.Lock()
	m.agents["a"] = &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "claude"}}
	m.mu.Unlock()

	// Copilot watch, but no copilot agent → skipped despite an absent path.
	w := credentialWatch{backend: "copilot", path: filepath.Join(t.TempDir(), "nope"),
		auditAction: AuditCopilotTokenMissing, probe: copilotTokenUsable}
	last := map[string]bool{}
	m.evalCredentialWatch(w, last)
	if sink.count() != 0 {
		t.Fatalf("non-in-use backend must not audit; got %d entries", sink.count())
	}
	if _, tracked := last["copilot"]; tracked {
		t.Error("skipped backend must not be recorded in the transition tracker")
	}
}

// TestEvalCredentialWatch_TransitionOnlyAudits proves an in-use backend audits
// once on usable→unusable and once more only after it recovers and breaks again
// (i.e. no re-audit while a standing condition persists).
func TestEvalCredentialWatch_TransitionOnlyAudits(t *testing.T) {
	m, sink := testManagerWithSink(t)
	m.mu.Lock()
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.mu.Unlock()

	path := filepath.Join(t.TempDir(), "copilot-user-token")
	w := credentialWatch{backend: "copilot", path: path,
		auditAction: AuditCopilotTokenMissing, probe: copilotTokenUsable}
	last := map[string]bool{}

	// Absent from the start → one audit.
	m.evalCredentialWatch(w, last)
	m.evalCredentialWatch(w, last) // still absent → no second audit
	if got := sink.count(); got != 1 {
		t.Fatalf("standing missing condition must audit once, got %d", got)
	}

	// Restore → recovery (no new audit), then break again → second audit.
	if err := os.WriteFile(path, []byte("ghu_x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.evalCredentialWatch(w, last)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	m.evalCredentialWatch(w, last)
	if got := sink.count(); got != 2 {
		t.Fatalf("re-broken condition must audit again, got %d", got)
	}

	e := sink.find(AuditCopilotTokenMissing)
	if e == nil {
		t.Fatal("expected a copilot_token_missing audit entry")
	}
	if e.Fields["backend"] != "copilot" || e.Fields["trigger"] != "watchdog" {
		t.Errorf("unexpected audit fields: %+v", e.Fields)
	}
}

// TestStartCredentialWatchdog_AuditsOnMissing proves the running loop emits a
// copilot_token_missing audit for an in-use copilot backend whose durable file
// is absent (the production path does not exist under the test sandbox).
func TestStartCredentialWatchdog_AuditsOnMissing(t *testing.T) {
	t.Setenv(CredentialWatchdogIntervalEnv, "10ms")
	orig := copilotUserTokenWatchPath
	copilotUserTokenWatchPath = filepath.Join(t.TempDir(), "missing-copilot-user-token")
	t.Cleanup(func() { copilotUserTokenWatchPath = orig })

	m, sink := testManagerWithSink(t)
	m.mu.Lock()
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		m.StartCredentialWatchdog(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for sink.find(AuditCopilotTokenMissing) == nil {
		select {
		case <-deadline:
			cancel()
			t.Fatal("watchdog never emitted a copilot_token_missing audit within 2s")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}
