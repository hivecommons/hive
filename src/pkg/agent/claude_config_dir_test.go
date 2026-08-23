package agent

// Tests for the per-agent CLAUDE_CONFIG_DIR layout (kubestellar/hive#4596).
//
// The bug: every interactive claude agent shared HOME=/data/home, so N
// concurrently running CLIs raced on the single ~/.claude.json state file, and
// an agent still sitting at the login menu flushed its fresh-boot skeleton over
// the logged-in state another agent had just established. These tests pin the
// three halves of the fix: the env the CLI is launched with, the auth probe's
// search order, and setupClaudeConfigDir's seed/migrate/bridge behavior.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// stubSuExec installs a fake su-exec on PATH that drops the user spec and
// execs the remaining argv as the current user, so setupClaudeConfigDir's
// mkdir/ln/sh calls operate on the test's temp tree.
func stubSuExec(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "su-exec"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing su-exec stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// claudeConfigSeams points every package-level path seam into a temp tree and
// restores the production values on cleanup. Returns the tree root.
func claudeConfigSeams(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	prevPrefix := claudeConfigDirPrefix
	prevState := claudeLegacyStateFile
	prevSettings := claudeLegacySettingsFile
	prevCreds := claudeSharedCredentialsFile
	claudeConfigDirPrefix = filepath.Join(root, "cfg-")
	claudeLegacyStateFile = filepath.Join(root, "legacy.claude.json")
	claudeLegacySettingsFile = filepath.Join(root, "legacy.settings.json")
	claudeSharedCredentialsFile = filepath.Join(root, "shared.credentials.json")
	t.Cleanup(func() {
		claudeConfigDirPrefix = prevPrefix
		claudeLegacyStateFile = prevState
		claudeLegacySettingsFile = prevSettings
		claudeSharedCredentialsFile = prevCreds
	})
	return root
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return m
}

func TestClaudeConfigDirLayout(t *testing.T) {
	if got, want := claudeConfigDirPath("scanner"), "/data/home/.claude-scanner"; got != want {
		t.Errorf("claudeConfigDirPath = %q, want %q", got, want)
	}
	if got, want := ClaudeConfigProjectsGlob(), "/data/home/.claude-*/projects"; got != want {
		t.Errorf("ClaudeConfigProjectsGlob = %q, want %q", got, want)
	}
}

// TestAgentEnvPairs_ClaudeConfigDir: the env var is emitted for interactive
// claude agents with a per-agent UID, and for NOTHING else — inference
// backends have their own per-agent HOME, copilot/codex have their own dirs,
// and UID<=0 agents run out of the process HOME exactly as before.
func TestAgentEnvPairs_ClaudeConfigDir(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		uid     int
		want    bool
	}{
		{"claude with per-agent uid", "claude", 2001, true},
		{"claude without uid", "claude", 0, false},
		{"copilot", "copilot", 2001, false},
		{"codex", "codex", 2001, false},
		{"inference vllm", "vllm", 2001, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(map[string]config.AgentConfig{
				"a": {Backend: tc.backend},
			}, discardLogger(), ProjectContext{})
			m.mu.Lock()
			agent := m.agents["a"]
			agent.UID = tc.uid
			m.mu.Unlock()

			var got string
			for _, p := range m.agentEnvPairs(agent) {
				if p.Key == "CLAUDE_CONFIG_DIR" {
					got = p.Value
				}
			}
			if tc.want && got != claudeConfigDirPath("a") {
				t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q", got, claudeConfigDirPath("a"))
			}
			if !tc.want && got != "" {
				t.Errorf("CLAUDE_CONFIG_DIR unexpectedly set to %q for backend=%s uid=%d", got, tc.backend, tc.uid)
			}
		})
	}
}

// TestAgentClaudeCredentialPaths_PerAgentDirFirst: under the per-agent layout
// the probe must look where the CLI actually writes — the config dir's
// TOP-LEVEL .credentials.json (CLAUDE_CONFIG_DIR replaces ~/.claude, so there
// is no .claude/ subdirectory) — and keep the shared legacy path as fallback.
func TestAgentClaudeCredentialPaths_PerAgentDirFirst(t *testing.T) {
	paths := agentClaudeCredentialPaths("scanner", 2007, "claude")
	if len(paths) < 2 {
		t.Fatalf("paths = %v, want at least per-agent + shared", paths)
	}
	if want := filepath.Join(claudeConfigDirPath("scanner"), ".credentials.json"); paths[0] != want {
		t.Errorf("paths[0] = %q, want %q", paths[0], want)
	}
	if !containsPath(paths, sharedClaudeCredentialPath) {
		t.Errorf("paths %v missing shared fallback %q", paths, sharedClaudeCredentialPath)
	}

	// UID<=0 (single-user layout) and inference backends keep their existing
	// resolution and gain no config-dir candidate.
	for _, tc := range []struct {
		uid     int
		backend string
	}{{0, "claude"}, {2007, "vllm"}} {
		got := agentClaudeCredentialPaths("scanner", tc.uid, tc.backend)
		for _, p := range got {
			if p == filepath.Join(claudeConfigDirPath("scanner"), ".credentials.json") {
				t.Errorf("uid=%d backend=%s: unexpected config-dir candidate in %v", tc.uid, tc.backend, got)
			}
		}
	}
}

// TestSetupClaudeConfigDir_SeedsAndBridges: a fresh per-agent dir is created,
// state and settings are seeded FROM THE LEGACY SHARED FILES (migration: a
// pre-upgrade login and operator settings carry forward) merged with the
// required seed keys, and the credential is a symlink to the shared file.
func TestSetupClaudeConfigDir_SeedsAndBridges(t *testing.T) {
	stubSuExec(t)
	claudeConfigSeams(t)

	// Legacy shared files as a working pre-upgrade hive would have them.
	if err := os.WriteFile(claudeLegacyStateFile, []byte(`{"userID":"u1","oauthAccount":{"emailAddress":"op@example.com"},"hasCompletedOnboarding":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeLegacySettingsFile, []byte(`{"effortLevel":"high"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeSharedCredentialsFile, []byte(`{"claudeAiOauth":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["scanner"]
	agent.UID = 2007
	m.mu.Unlock()

	m.setupClaudeConfigDir(agent)

	dir := claudeConfigDirPath("scanner")

	state := readJSONFile(t, filepath.Join(dir, ".claude.json"))
	if got, _ := state["userID"].(string); got != "u1" {
		t.Errorf("legacy userID not carried forward: %v", state["userID"])
	}
	if _, ok := state["oauthAccount"]; !ok {
		t.Error("legacy oauthAccount not carried forward — the upgrade would log the fleet out")
	}
	if v, _ := state["hasCompletedOnboarding"].(bool); !v {
		t.Error("hasCompletedOnboarding not true — the CLI would show the login menu")
	}
	if v, _ := state["bypassPermissionsModeAccepted"].(bool); !v {
		t.Error("bypassPermissionsModeAccepted seed missing")
	}

	settings := readJSONFile(t, filepath.Join(dir, "settings.json"))
	if got, _ := settings["effortLevel"].(string); got != "high" {
		t.Errorf("operator effortLevel not carried from legacy settings: %v", settings["effortLevel"])
	}
	if v, _ := settings["skipDangerousModePermissionPrompt"].(bool); !v {
		t.Error("skipDangerousModePermissionPrompt seed missing — agents would hit the consent dialog")
	}

	fi, err := os.Lstat(filepath.Join(dir, ".credentials.json"))
	if err != nil {
		t.Fatalf("credential link missing: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("credential is not a symlink")
	}
	target, _ := os.Readlink(filepath.Join(dir, ".credentials.json"))
	if target != claudeSharedCredentialsFile {
		t.Errorf("credential link -> %q, want %q", target, claudeSharedCredentialsFile)
	}
}

// TestSetupClaudeConfigDir_NeverClobbersAgentState: an existing per-agent
// state file is repaired (missing seed keys filled) but its own content is
// never replaced — one writer per state file is the entire point of #4596 —
// and a REGULAR credential file (a refresh that replaced the symlink) is left
// exactly as the agent's CLI wrote it.
func TestSetupClaudeConfigDir_NeverClobbersAgentState(t *testing.T) {
	stubSuExec(t)
	claudeConfigSeams(t)
	if err := os.WriteFile(claudeSharedCredentialsFile, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(map[string]config.AgentConfig{
		"quality": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["quality"]
	agent.UID = 2006
	m.mu.Unlock()

	// Simulate a dir the agent's CLI already owns.
	dir := claudeConfigDirPath("quality")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"marker":"cli-owned"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`PRIVATE-REFRESHED`), 0o600); err != nil {
		t.Fatal(err)
	}

	m.setupClaudeConfigDir(agent)

	state := readJSONFile(t, filepath.Join(dir, ".claude.json"))
	if got, _ := state["marker"].(string); got != "cli-owned" {
		t.Errorf("agent-owned state clobbered: marker = %v", state["marker"])
	}
	if v, _ := state["hasCompletedOnboarding"].(bool); !v {
		t.Error("missing seed key not repaired into existing state")
	}

	b, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if err != nil || string(b) != "PRIVATE-REFRESHED" {
		t.Errorf("regular credential file was disturbed: %q %v", b, err)
	}
	if fi, _ := os.Lstat(filepath.Join(dir, ".credentials.json")); fi.Mode()&os.ModeSymlink != 0 {
		t.Error("regular credential file was replaced with a symlink")
	}
}

// TestSetupClaudeConfigDir_UIDZeroNoop: root agents run out of the process
// HOME exactly as before — nothing is created for them.
func TestSetupClaudeConfigDir_UIDZeroNoop(t *testing.T) {
	stubSuExec(t)
	claudeConfigSeams(t)

	m := NewManager(map[string]config.AgentConfig{
		"solo": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["solo"]
	agent.UID = 0
	m.mu.Unlock()

	m.setupClaudeConfigDir(agent)

	if _, err := os.Stat(claudeConfigDirPath("solo")); !os.IsNotExist(err) {
		t.Errorf("config dir created for UID 0 agent (err=%v)", err)
	}
}
