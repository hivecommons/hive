package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestSanitizeGitRemotes_StripsEmbeddedToken creates a real git repo under the
// agent's work dir whose origin remote embeds a token, and verifies the
// advisory-mode sanitizer strips it.
func TestSanitizeGitRemotes_StripsEmbeddedToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	work := t.TempDir()
	t.Setenv("HIVE_WORK_DIR", work)

	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude", Mode: "ADVISORY"}, // CanPush() false -> sanitizes
	}, discardLogger(), ProjectContext{ACMMLevel: 1})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()

	// Create a repo at <work>/a/repo with a token-embedded origin.
	repo := filepath.Join(work, "a", "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init")
	run("remote", "add", "origin", "https://x-access-token:SECRETTOKEN@github.com/org/repo.git")

	m.sanitizeGitRemotes(agent)

	out, err := exec.Command("git", "-C", repo, "remote", "get-url", "origin").Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(out))
	if strings.Contains(got, "SECRETTOKEN") {
		t.Errorf("token should be stripped, got %q", got)
	}
	if got != "https://github.com/org/repo.git" {
		t.Errorf("clean url = %q", got)
	}
}

// TestNewManager_CopilotTokenFromEnv covers the COPILOT_GITHUB_TOKEN env branch.
func TestNewManager_CopilotTokenFromEnv(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "gho_from_env")
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "copilot"}}, discardLogger(), ProjectContext{})
	if m.CopilotToken() != "gho_from_env" {
		t.Errorf("copilot token from env = %q", m.CopilotToken())
	}
}
