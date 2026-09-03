package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// writeExpiredClaudeCreds writes a credentials.json whose OAuth token expired
// in the past — the shape the login detector MUST still pause on.
func writeExpiredClaudeCreds(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken": "stale-token",
			"expiresAt":   time.Now().Add(-1 * time.Hour).UnixMilli(),
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestCredentialFileProves_ClaudeExpiryIsHonoured is the core of the #5291 gate:
// the detector may only skip a pause on PROOF of a working credential, and for
// claude that means an unexpired token — the one backend where expiry is
// actually knowable from the file.
func TestCredentialFileProves_ClaudeExpiryIsHonoured(t *testing.T) {
	emptySharedPaths(t)
	m := &Manager{}
	home := t.TempDir()
	t.Setenv("HOME", home)
	cred := filepath.Join(home, ".claude", ".credentials.json")

	if m.credentialFileProves("supervisor", 0, "claude") {
		t.Fatal("no credential file anywhere must not read as proof")
	}

	writeExpiredClaudeCreds(t, cred)
	if m.credentialFileProves("supervisor", 0, "claude") {
		t.Fatal("an EXPIRED token must not read as proof — that agent genuinely needs a login")
	}

	writeClaudeCreds(t, cred)
	if !m.credentialFileProves("supervisor", 0, "claude") {
		t.Fatal("a fresh token is exactly the proof the detector must stand down for")
	}
}

// TestCredentialFileProves_SharedPathCountsForTheAgent reproduces the incident's
// own shape: the operator's /login refreshed the SHARED credential, and the
// agent whose pane still showed login chrome must be recognised as
// authenticated through that shared file.
func TestCredentialFileProves_SharedPathCountsForTheAgent(t *testing.T) {
	emptySharedPaths(t)
	m := &Manager{}
	t.Setenv("HOME", t.TempDir()) // the agent's own home is empty

	if m.credentialFileProves("supervisor", 0, "claude") {
		t.Fatal("precondition: nothing should be proven yet")
	}
	writeClaudeCreds(t, sharedClaudeCredentialPath)
	if !m.credentialFileProves("supervisor", 0, "claude") {
		t.Fatal("the shared credential the operator just refreshed must count for the agent")
	}
}

// TestCredentialFileProves_UncheckableBackendsStayUnproven pins the deliberate
// limit: a backend with no credential file this process can read yields no
// proof, so the detector keeps its existing behaviour for it rather than
// silently never pausing.
func TestCredentialFileProves_UncheckableBackendsStayUnproven(t *testing.T) {
	emptySharedPaths(t)
	emptyCodexSharedPath(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	m := &Manager{}

	for _, backend := range append([]string{"gemini", "bob", "agy", ""}, config.InferenceBackends...) {
		if m.credentialFileProves("supervisor", 0, backend) {
			t.Errorf("backend %q has no checkable credential file — it must not claim proof", backend)
		}
	}
}

// TestCredentialFileProves_CopilotAndCodexPresence documents the honest
// asymmetry: these two are checked for the PRESENCE of tokens, not for expiry,
// which is the same trade configHasTokens() already makes for the heal.
func TestCredentialFileProves_CopilotAndCodexPresence(t *testing.T) {
	emptySharedPaths(t)
	emptyCodexSharedPath(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	m := &Manager{}

	if m.credentialFileProves("supervisor", 0, "copilot") {
		t.Fatal("precondition: no copilot tokens yet")
	}
	if err := os.MkdirAll(filepath.Dir(sharedCopilotConfigPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(sharedCopilotConfigPath,
		[]byte(`{"copilotTokens":{"github.com":"tok"}}`), 0o600); err != nil {
		t.Fatalf("write copilot config: %v", err)
	}
	if !m.credentialFileProves("supervisor", 0, "copilot") {
		t.Fatal("a copilot config carrying tokens must read as proof")
	}

	if m.credentialFileProves("supervisor", 0, "codex") {
		t.Fatal("precondition: no codex auth yet")
	}
	writeCodexAuth(t, codexSharedAuthFile, `{"tokens":{"access_token":"tok"}}`)
	if !m.credentialFileProves("supervisor", 0, "codex") {
		t.Fatal("a codex auth.json carrying tokens must read as proof")
	}
}

// TestAgentHasValidCredential_ResolvesBackendAndNilSafety covers the public
// entry point the detector calls: it must resolve the agent's own backend
// (including a runtime override) and must never panic on an unknown agent or a
// nil manager.
func TestAgentHasValidCredential_ResolvesBackendAndNilSafety(t *testing.T) {
	emptySharedPaths(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeClaudeCreds(t, filepath.Join(home, ".claude", ".credentials.json"))

	var nilMgr *Manager
	if nilMgr.AgentHasValidCredential("supervisor") {
		t.Fatal("a nil manager cannot prove anything")
	}

	m := &Manager{agents: map[string]*AgentProcess{}}
	if m.AgentHasValidCredential("supervisor") {
		t.Fatal("an agent the manager does not know cannot be proven authenticated")
	}

	m.agents["supervisor"] = &AgentProcess{Config: config.AgentConfig{Backend: "claude"}}
	if !m.AgentHasValidCredential("supervisor") {
		t.Fatal("a claude agent with a fresh token must be proven authenticated")
	}

	// A runtime backend override must be what gets probed — otherwise an agent
	// switched to gemini would keep answering from claude's credential.
	m.agents["supervisor"].BackendOverride = "gemini"
	if m.AgentHasValidCredential("supervisor") {
		t.Fatal("the override backend must be probed, not the configured one")
	}
}

// TestAgentAuthState_UnchangedByTheCredentialExtraction is the refactor guard:
// AgentAuthState's answers must not have moved when its positive file probe was
// split out for the detector to share.
func TestAgentAuthState_UnchangedByTheCredentialExtraction(t *testing.T) {
	emptySharedPaths(t)
	emptyCodexSharedPath(t)
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := &Manager{}

	// claude, nothing on disk: UNKNOWN (absence is not proof of logged-out).
	if avail, known := m.AgentAuthState("a", 0, "claude", false, false); avail || known {
		t.Fatalf("claude with no file: got (%v,%v), want (false,false)", avail, known)
	}
	// copilot, nothing on disk: KNOWN needs-login.
	if avail, known := m.AgentAuthState("a", 0, "copilot", false, false); avail || !known {
		t.Fatalf("copilot with no file: got (%v,%v), want (false,true)", avail, known)
	}
	// codex, nothing on disk: KNOWN needs-login.
	if avail, known := m.AgentAuthState("a", 0, "codex", false, false); avail || !known {
		t.Fatalf("codex with no file: got (%v,%v), want (false,true)", avail, known)
	}
	// claude with a fresh token: authenticated and known.
	writeClaudeCreds(t, filepath.Join(home, ".claude", ".credentials.json"))
	if avail, known := m.AgentAuthState("a", 0, "claude", false, false); !avail || !known {
		t.Fatalf("claude with a fresh token: got (%v,%v), want (true,true)", avail, known)
	}
	// The pane's own login signal still outranks the file (rule 3).
	if avail, known := m.AgentAuthState("a", 0, "claude", false, true); avail || !known {
		t.Fatalf("needsLogin must still win: got (%v,%v), want (false,true)", avail, known)
	}
}
