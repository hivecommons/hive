package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// extractCopilotToken must handle both value shapes the CLI writes: a bare
// string and a {"token":…} object; and return "" when there is none.
func TestExtractCopilotToken(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "config.json")
		if err := os.WriteFile(p, []byte(copilotConfigHeader+body), 0o660); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// bare-string shape (in-agent /login on 1.0.78)
	if got := extractCopilotToken(write(`{"copilotTokens":{"https://github.com:me":"gho_bare"}}`)); got != "gho_bare" {
		t.Errorf("string shape: got %q, want gho_bare", got)
	}
	// object shape (restoreCopilotTokens / other CLI versions)
	if got := extractCopilotToken(write(`{"copilotTokens":{"github.com":{"token":"gho_obj"}}}`)); got != "gho_obj" {
		t.Errorf("object shape: got %q, want gho_obj", got)
	}
	// empty map
	if got := extractCopilotToken(write(`{"copilotTokens":{}}`)); got != "" {
		t.Errorf("empty map: got %q, want \"\"", got)
	}
	// missing file
	if got := extractCopilotToken(filepath.Join(dir, "nope.json")); got != "" {
		t.Errorf("missing file: got %q, want \"\"", got)
	}
}

func TestWriteDurableCopilotToken(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "copilot-user-token")
	if err := writeDurableCopilotToken(p, "ghu_dur"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "ghu_dur" {
		t.Errorf("durable file = %q, want ghu_dur", string(b))
	}
	// blank is a no-op (must not create/overwrite)
	if err := writeDurableCopilotToken(p, "   "); err != nil {
		t.Fatalf("blank should no-op, got %v", err)
	}
	b, _ = os.ReadFile(p)
	if string(b) != "ghu_dur" {
		t.Error("blank write must not overwrite an existing token")
	}
}

// syncCopilotToken PROMOTE: a token in config but none held by the hive → the
// token is mirrored to the durable file AND SetCopilotToken updates memory.
// This is the "logged in inside the agent" case the operator hit.
func TestSyncCopilotToken_Promote(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	dur := filepath.Join(dir, "durable")
	if err := os.WriteFile(cfg, []byte(copilotConfigHeader+`{"copilotTokens":{"https://github.com:me":"gho_fromcli"}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	m := testManager(5)
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.copilotAuthToken = "" // hive holds nothing (in-agent login bypassed the hive)

	if act := m.syncCopilotToken(cfg, dur); act != copilotSyncPromote {
		t.Fatalf("action = %v, want promote", act)
	}
	b, _ := os.ReadFile(dur)
	if string(b) != "gho_fromcli" {
		t.Errorf("durable file = %q, want gho_fromcli (promoted from config)", string(b))
	}
	if m.CopilotToken() != "gho_fromcli" {
		t.Errorf("in-memory token = %q, want gho_fromcli (SetCopilotToken)", m.CopilotToken())
	}
}

// PROMOTE no-op when the hive already holds exactly the CLI's token.
func TestSyncCopilotToken_PromoteNoopWhenAlreadyHeld(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	dur := filepath.Join(dir, "durable")
	if err := os.WriteFile(cfg, []byte(copilotConfigHeader+`{"copilotTokens":{"github.com":{"token":"gho_same"}}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	m := testManager(5)
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.copilotAuthToken = "gho_same"
	if act := m.syncCopilotToken(cfg, dur); act != copilotSyncNoop {
		t.Fatalf("action = %v, want noop (already held)", act)
	}
	if _, err := os.Stat(dur); !os.IsNotExist(err) {
		t.Error("durable file must not be written when nothing changed")
	}
}

// SEED: config empty but hive holds a token → config re-populated (the #4494
// direction still works through the merged path).
func TestSyncCopilotToken_Seed(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	dur := filepath.Join(dir, "durable")
	if err := os.WriteFile(cfg, []byte(copilotConfigHeader+`{"copilotTokens":{}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	m := testManager(5)
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.copilotAuthToken = "ghu_held"
	if act := m.syncCopilotToken(cfg, dur); act != copilotSyncSeed {
		t.Fatalf("action = %v, want seed", act)
	}
	if !copilotCredentialFileHasTokens(cfg) {
		t.Error("config must be re-seeded")
	}
}

// Both empty → noop (genuine logout; watchdog alert + manual login covers it).
func TestSyncCopilotToken_BothEmptyNoop(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	dur := filepath.Join(dir, "durable")
	if err := os.WriteFile(cfg, []byte(copilotConfigHeader+`{"copilotTokens":{}}`), 0o660); err != nil {
		t.Fatal(err)
	}
	m := testManager(5)
	m.agents["scanner"] = &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "copilot"}}
	m.copilotAuthToken = ""
	if act := m.syncCopilotToken(cfg, dur); act != copilotSyncNoop {
		t.Fatalf("action = %v, want noop", act)
	}
}

// refreshCopilotSessionToken no-ops entirely without a copilot backend.
func TestRefreshCopilotSessionToken_NoCopilotBackend(t *testing.T) {
	m := testManager(5)
	m.agents["a"] = &AgentProcess{Name: "a", Config: config.AgentConfig{Backend: "claude"}}
	m.copilotAuthToken = "gho_held"
	// Should return without panicking or touching the shared/durable paths.
	m.refreshCopilotSessionToken()
}
