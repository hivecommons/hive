package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
)

// writeClaudeCreds writes a credentials.json with a non-expired OAuth token at
// path, creating parent dirs.
func writeClaudeCreds(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken": "test-token",
			"expiresAt":   time.Now().Add(24 * time.Hour).UnixMilli(),
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

// emptySharedPaths redirects the shared/legacy credential locations to a
// nonexistent temp dir — the exact condition observed on a per-agent-UID spoke,
// where /data/home/.claude/.credentials.json is absent and
// /data/home/.config/github-copilot is empty.
func emptySharedPaths(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	origClaude, origCopilot := sharedClaudeCredentialPath, sharedCopilotConfigPath
	sharedClaudeCredentialPath = filepath.Join(dir, "absent-claude.json")
	sharedCopilotConfigPath = filepath.Join(dir, "absent-copilot.json")
	t.Cleanup(func() {
		sharedClaudeCredentialPath = origClaude
		sharedCopilotConfigPath = origCopilot
	})
}

// TestAgentAuthState_InferenceBackendNeverNeedsLogin is the primary guard: a
// method that has no interactive login (litellm/vllm/llm-d — the spoke's
// scanner runs METHOD=litellm) must NEVER report "needs login", no matter what
// the credential files say or what the process state is.
func TestAgentAuthState_InferenceBackendNeverNeedsLogin(t *testing.T) {
	emptySharedPaths(t)
	m := &Manager{}

	for _, backend := range config.InferenceBackends {
		for _, running := range []bool{true, false} {
			// Even a pane that looks like a login prompt cannot make an
			// API-key backend "need login" — there is no login to perform.
			for _, needsLogin := range []bool{true, false} {
				avail, known := m.AgentAuthState("scanner", 2007, backend, running, needsLogin)
				if !known || !avail {
					t.Fatalf("backend=%s running=%v needsLogin=%v: got (avail=%v, known=%v), want authenticated+known",
						backend, running, needsLogin, avail, known)
				}
				if known && !avail {
					t.Fatalf("backend=%s: would render a login badge", backend)
				}
			}
		}
	}

	if BackendRequiresInteractiveAuth("litellm") {
		t.Fatal("litellm must not be classified as an interactive-auth backend")
	}
}

// TestAgentAuthState_PerUIDCredentialsFound is the exact regression: the shared
// legacy path is empty but the agent's own per-UID home holds valid
// credentials. No badge may be raised.
func TestAgentAuthState_PerUIDCredentialsFound(t *testing.T) {
	emptySharedPaths(t)
	m := &Manager{}

	// Agents with a UID whose backend is an inference backend get a per-agent
	// HOME; point that prefix at a temp dir and populate it.
	home := t.TempDir()
	origPrefix := claudeInferenceHomePrefixForTest(t, home+"/h-")
	_ = origPrefix
	writeClaudeCreds(t, filepath.Join(home+"/h-scanner", ".claude", ".credentials.json"))

	paths := agentClaudeCredentialPaths("scanner", 2007, "vllm")
	if len(paths) < 2 {
		t.Fatalf("want per-UID path then shared fallback, got %v", paths)
	}
	if paths[0] == sharedClaudeCredentialPath {
		t.Fatalf("per-UID home must be probed FIRST, got %v", paths)
	}

	// A non-running claude agent (so the probe actually runs) whose per-UID
	// home holds credentials must report authenticated.
	claudeHome := t.TempDir()
	t.Setenv("HOME", claudeHome)
	writeClaudeCreds(t, filepath.Join(claudeHome, ".claude", ".credentials.json"))
	avail, known := m.AgentAuthState("scanner", 0, "claude", false, false)
	if !known || !avail {
		t.Fatalf("per-UID credentials present: got (avail=%v, known=%v), want authenticated", avail, known)
	}
}

// TestAgentAuthState_RunningAgentNoBadgeOnMissingFile encodes the precedence
// rule: positive evidence of health (running, not at a login prompt) beats
// absence-of-credential-file. This is the case the operator saw — scanner and
// quality running and passing deep health while the shared path was empty.
func TestAgentAuthState_RunningAgentNoBadgeOnMissingFile(t *testing.T) {
	emptySharedPaths(t)
	t.Setenv("HOME", t.TempDir()) // no credentials anywhere
	m := &Manager{}

	avail, known := m.AgentAuthState("scanner", 2007, "claude", true /*running*/, false /*needsLogin*/)
	if known && !avail {
		t.Fatalf("running agent with empty shared path must not raise a login badge; got (avail=%v, known=%v)", avail, known)
	}
}

// TestAgentAuthState_TruePositivePreserved: an interactive backend with no
// credentials anywhere and an agent that is NOT running must still report
// "needs login" — the signal has to keep working.
func TestAgentAuthState_TruePositivePreserved(t *testing.T) {
	emptySharedPaths(t)
	t.Setenv("HOME", t.TempDir())
	m := &Manager{}

	avail, known := m.AgentAuthState("scanner", 2007, "claude", false /*running*/, false)
	if !known || avail {
		t.Fatalf("no credentials + not running must report needs-login; got (avail=%v, known=%v)", avail, known)
	}
}

// TestAgentAuthState_PaneLoginPromptWins: an agent genuinely parked at an
// interactive login prompt reports needs-login even while its process is
// running — rule 3 must not be masked by rule 2.
func TestAgentAuthState_PaneLoginPromptWins(t *testing.T) {
	emptySharedPaths(t)
	m := &Manager{}

	avail, known := m.AgentAuthState("scanner", 2007, "claude", true /*running*/, true /*needsLogin*/)
	if !known || avail {
		t.Fatalf("pane at login prompt must report needs-login; got (avail=%v, known=%v)", avail, known)
	}
}

// TestLoginPromptPatterns_NoBareSlashLogin: a bare "/login" substring appears
// in ordinary agent output (an agent reviewing an auth route, a CLI printing
// its slash-command list) and must not trip the login detector.
func TestLoginPromptPatterns_NoBareSlashLogin(t *testing.T) {
	benign := []string{
		"the handler for POST /login returns 302 on success",
		"  /help  /login  /model  /status",
		"added a test for /login rate limiting",
	}
	for _, line := range benign {
		if paneShowsLoginPrompt([]string{line}) {
			t.Fatalf("benign agent output falsely detected as a login prompt: %q", line)
		}
	}

	// True positives must still match: "/login" plus an imperative verb is a
	// directive to the operator, which is what a real login screen renders.
	truePositives := []string{
		"Please /login to continue",
		"Please run /login to authenticate",
		"Run /login",
		"Type /login to sign in",
		"You need to /login first",
		"You must sign in to use Copilot",
		"Use the url below to sign in",
		"Enter one-time code",
		"github.com/login/device",
	}
	for _, line := range truePositives {
		if !paneShowsLoginPrompt([]string{line}) {
			t.Fatalf("genuine login prompt not detected: %q", line)
		}
	}
}

// claudeInferenceHomePrefixForTest redirects the per-agent inference home
// prefix and restores it on cleanup.
func claudeInferenceHomePrefixForTest(t *testing.T, prefix string) string {
	t.Helper()
	orig := inferenceHomePrefixOverride
	inferenceHomePrefixOverride = prefix
	t.Cleanup(func() { inferenceHomePrefixOverride = orig })
	return orig
}
