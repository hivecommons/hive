package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

func TestCopilotSessionRefreshInterval(t *testing.T) {
	t.Setenv(CopilotSessionRefreshIntervalEnv, "")
	if got := copilotSessionRefreshInterval(); got != defaultCopilotSessionRefreshInterval {
		t.Errorf("default = %v, want %v", got, defaultCopilotSessionRefreshInterval)
	}
	t.Setenv(CopilotSessionRefreshIntervalEnv, "2m")
	if got := copilotSessionRefreshInterval(); got != 2*time.Minute {
		t.Errorf("override = %v, want 2m", got)
	}
	t.Setenv(CopilotSessionRefreshIntervalEnv, "garbage")
	if got := copilotSessionRefreshInterval(); got != defaultCopilotSessionRefreshInterval {
		t.Errorf("invalid override should fall back to default, got %v", got)
	}
}

func TestStartCopilotSessionRefreshSeedsAndStops(t *testing.T) {
	t.Setenv(CopilotSessionRefreshStartDelayEnv, "0s")
	t.Setenv(CopilotSessionRefreshIntervalEnv, "10ms")

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := sharedCopilotConfigPath
	sharedCopilotConfigPath = path
	defer func() { sharedCopilotConfigPath = orig }()
	if err := os.WriteFile(path, []byte(copilotConfigHeader+`{"copilotTokens":{}}`), 0o660); err != nil {
		t.Fatal(err)
	}

	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "copilot"}}, discardLogger(), ProjectContext{})
	m.SetCopilotToken("ghu_held")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.StartCopilotSessionRefresh(ctx)
		close(done)
	}()

	// Generous deadlines: under a full-suite run the scheduler can starve this
	// goroutine well past the 10ms interval (observed >2s on loaded CI hosts),
	// and the deadline guards liveness, not latency.
	deadline := time.After(15 * time.Second)
	for !copilotCredentialFileHasTokens(path) {
		select {
		case <-deadline:
			cancel()
			t.Fatal("session refresh loop did not seed copilotTokens")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("StartCopilotSessionRefresh did not stop after context cancellation")
	}
}

// restoreCopilotTokens must populate copilotTokens (which the credential reader
// then sees as present), preserve the JSONC header, and no-op on a blank token.
func TestRestoreCopilotTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Seed an emptied config, as clearExpiredTokens / a roll would leave it.
	if err := os.WriteFile(path, []byte(copilotConfigHeader+"{\n  \"copilotTokens\": {}\n}\n"), 0o660); err != nil {
		t.Fatal(err)
	}
	if copilotCredentialFileHasTokens(path) {
		t.Fatal("precondition: emptied config must have no tokens")
	}

	if err := restoreCopilotTokens(path, "ghu_validtoken"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !copilotCredentialFileHasTokens(path) {
		t.Error("after restore, config must report a token present")
	}
	raw, _ := os.ReadFile(path)
	if len(raw) < len(copilotConfigHeader) || string(raw[:len(copilotConfigHeader)]) != copilotConfigHeader {
		t.Error("JSONC header must be preserved on rewrite")
	}

	// Blank token is a no-op and must not error or wipe an existing token.
	if err := restoreCopilotTokens(path, "   "); err != nil {
		t.Fatalf("blank restore should be a no-op, got %v", err)
	}
	if !copilotCredentialFileHasTokens(path) {
		t.Error("blank restore must not clear an existing token")
	}

	// A MISSING config file is seeded fresh (not an error) — a fresh agent pod
	// whose CLI never wrote config.json still gets a usable credential.
	fresh := filepath.Join(dir, "sub", "config.json")
	if err := os.MkdirAll(filepath.Dir(fresh), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := restoreCopilotTokens(fresh, "ghu_fresh"); err != nil {
		t.Fatalf("restore into a missing file should seed it, got %v", err)
	}
	if !copilotCredentialFileHasTokens(fresh) {
		t.Error("restore must create + populate a missing config")
	}
}

// The round-trip: clear empties, restore re-populates.
func TestClearThenRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := sharedCopilotConfigPath
	sharedCopilotConfigPath = path
	defer func() { sharedCopilotConfigPath = orig }()

	if err := os.WriteFile(path, []byte(copilotConfigHeader+`{"copilotTokens":{"github.com":{"token":"gho_old"}}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := clearExpiredTokens(); err != nil {
		t.Fatal(err)
	}
	if copilotConfigHasTokens() {
		t.Error("clearExpiredTokens must empty the token map")
	}
	if err := restoreCopilotTokens(path, "ghu_new"); err != nil {
		t.Fatal(err)
	}
	if !copilotConfigHasTokens() {
		t.Error("restore must re-populate the token map")
	}
}

// refreshCopilotSessionToken must: no-op without a copilot backend; no-op when
// tokens are already present; and re-seed an empty store when a copilot agent
// exists and the manager holds a user token.
func TestRefreshCopilotSessionToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := sharedCopilotConfigPath
	sharedCopilotConfigPath = path
	defer func() { sharedCopilotConfigPath = orig }()
	writeEmpty := func() {
		if err := os.WriteFile(path, []byte(copilotConfigHeader+`{"copilotTokens":{}}`), 0o660); err != nil {
			t.Fatal(err)
		}
	}

	// (a) No copilot backend → no-op even with a token held.
	writeEmpty()
	m := testManager(5)
	m.agents["a"] = &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "claude"}}
	m.copilotAuthToken = "ghu_held"
	m.refreshCopilotSessionToken()
	if copilotConfigHasTokens() {
		t.Error("no copilot backend: must not seed tokens")
	}

	// (b) Copilot backend + held token + empty store → re-seed.
	m2 := testManager(5)
	m2.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m2.copilotAuthToken = "ghu_held"
	writeEmpty()
	m2.refreshCopilotSessionToken()
	if !copilotConfigHasTokens() {
		t.Error("copilot backend + held token + empty store: must re-seed")
	}

	// (c) Copilot backend but NO held token → no-op (genuine logout, manual login).
	m3 := testManager(5)
	m3.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m3.copilotAuthToken = ""
	writeEmpty()
	m3.refreshCopilotSessionToken()
	if copilotConfigHasTokens() {
		t.Error("no held token: must not fabricate a credential")
	}

	// (d) Config already populated with a token the hive does NOT hold → the
	// config token is PRESERVED (never clobbered) AND promoted to the durable
	// store (bidirectional sync). Uses syncCopilotToken with a temp durable path
	// so the promote write does not target the production /data path.
	m4 := testManager(5)
	m4.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m4.copilotAuthToken = "ghu_held"
	dur := filepath.Join(dir, "durable")
	if err := os.WriteFile(path, []byte(copilotConfigHeader+`{"copilotTokens":{"github.com":{"token":"gho_cli_owned"}}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	if act := m4.syncCopilotToken(path, dur); act != copilotSyncPromote {
		t.Errorf("differing config token must promote, got action %v", act)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "gho_cli_owned") {
		t.Error("config token must not be overwritten by a promote")
	}
	if b, _ := os.ReadFile(dur); string(b) != "gho_cli_owned" {
		t.Errorf("durable file = %q, want gho_cli_owned (promoted)", string(b))
	}
}
