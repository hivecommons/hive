package agent

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// firstRunTestAgent sets up a per-agent interactive HOME and a work dir with
// one git repo below it, and returns the manager, the agent, its HOME and the
// directories the seed must mark trusted.
//
// The agent UID is the test's own UID where that is non-zero, so the seed's
// chown through the descriptor succeeds unprivileged and ownership can be
// asserted exactly; as root any UID works and 1001 is used.
func firstRunTestAgent(t *testing.T) (*Manager, *AgentProcess, string, []string) {
	t.Helper()
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	m.workDir = t.TempDir()
	agentDir := filepath.Join(m.workDir, "scanner")
	repoDir := filepath.Join(agentDir, "repo")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := interactiveHomePath("scanner")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	uid := os.Getuid()
	if uid == 0 {
		uid = 1001
	}
	return m, &AgentProcess{Name: "scanner", UID: uid}, home, []string{agentDir, repoDir}
}

func readJSONObject(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("seeded file is not a JSON object: %v\n%s", err, data)
	}
	return out
}

// assertFirstRunSeeded checks onboarding and every dir's trust answer.
func assertFirstRunSeeded(t *testing.T, path string, dirs []string) map[string]json.RawMessage {
	t.Helper()
	top := readJSONObject(t, path)
	if !jsonValueIsTrue(top[claudeOnboardingKey]) {
		t.Errorf("%s = %s, want true", claudeOnboardingKey, top[claudeOnboardingKey])
	}
	projects := map[string]map[string]json.RawMessage{}
	if err := json.Unmarshal(top[claudeProjectsKey], &projects); err != nil {
		t.Fatalf("projects: %v", err)
	}
	for _, dir := range dirs {
		if !jsonValueIsTrue(projects[dir][claudeTrustAcceptedKey]) {
			t.Errorf("projects[%q].%s = %s, want true", dir, claudeTrustAcceptedKey, projects[dir][claudeTrustAcceptedKey])
		}
	}
	return top
}

func fileOwnerAndMode(t *testing.T, path string) (int, os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(info.Sys().(*syscall.Stat_t).Uid), info.Mode().Perm()
}

// assertNoSeedLeftovers fails when a staging file was left in the HOME.
func assertNoSeedLeftovers(t *testing.T, home string) {
	t.Helper()
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), claudeFirstRunSeedTempInfix) {
			t.Errorf("staging file left behind: %s", e.Name())
		}
	}
}

func TestClaudeFirstRunSeed_MissingFileIsCreatedForTheAgent(t *testing.T) {
	m, ap, home, dirs := firstRunTestAgent(t)
	path := claudeSessionFile(home)

	m.seedClaudeFirstRunForAgent(ap)

	assertFirstRunSeeded(t, path, dirs)
	uid, mode := fileOwnerAndMode(t, path)
	if uid != ap.UID {
		t.Errorf("owner = %d, want the agent UID %d", uid, ap.UID)
	}
	if mode != claudeSessionSeedFileMode {
		t.Errorf("mode = %v, want %v", mode, os.FileMode(claudeSessionSeedFileMode))
	}
	assertNoSeedLeftovers(t, home)
}

// TestClaudeFirstRunSeed_PartialFileKeepsEverythingElse: only the two answers
// are added; every other key and value, the owner and the mode stay.
func TestClaudeFirstRunSeed_PartialFileKeepsEverythingElse(t *testing.T) {
	m, ap, home, dirs := firstRunTestAgent(t)
	agentDir := dirs[0]
	existing := `{"numStartups":7,"userID":"abc","firstStartTime":"2026-11-16T00:00:00.000Z",` +
		`"big":12345678901234567890,` +
		`"projects":{"/elsewhere":{"allowedTools":["Bash"],"hasTrustDialogAccepted":false},` +
		`"` + agentDir + `":{"history":[{"display":"fix a<b && c>d"}],"hasTrustDialogAccepted":false}}}`
	path := writeSession(t, home, existing)
	wantUID, _ := fileOwnerAndMode(t, path)

	m.seedClaudeFirstRunForAgent(ap)

	top := assertFirstRunSeeded(t, path, dirs)
	for key, want := range map[string]string{
		"numStartups":    `7`,
		"userID":         `"abc"`,
		"firstStartTime": `"2026-11-16T00:00:00.000Z"`,
		// Larger than float64 holds exactly: carried as raw JSON, not rounded.
		"big": `12345678901234567890`,
	} {
		if string(top[key]) != want {
			t.Errorf("%s = %s, want %s", key, top[key], want)
		}
	}
	projects := map[string]map[string]json.RawMessage{}
	if err := json.Unmarshal(top[claudeProjectsKey], &projects); err != nil {
		t.Fatal(err)
	}
	if got := string(projects["/elsewhere"][claudeTrustAcceptedKey]); got != "false" {
		t.Errorf("a project hive does not own was changed: hasTrustDialogAccepted = %s", got)
	}
	if got := string(projects["/elsewhere"]["allowedTools"]); got != `["Bash"]` {
		t.Errorf("/elsewhere allowedTools = %s", got)
	}
	if got := string(projects[agentDir]["history"]); got != `[{"display":"fix a<b && c>d"}]` {
		t.Errorf("agent project history = %s, want it byte-equal (no HTML escaping)", got)
	}
	uid, mode := fileOwnerAndMode(t, path)
	if uid != wantUID || mode != 0o600 {
		t.Errorf("owner, mode = %d, %v; want %d, 0600 kept", uid, mode, wantUID)
	}
	assertNoSeedLeftovers(t, home)
}

// TestClaudeFirstRunSeed_KeepsSignedInIdentity: the file holds the agent's
// sign-in; it must survive the seed unchanged.
func TestClaudeFirstRunSeed_KeepsSignedInIdentity(t *testing.T) {
	m, ap, home, dirs := firstRunTestAgent(t)
	path := writeSession(t, home, signedInClaudeSession)
	before := readJSONObject(t, path)[claudeOAuthAccountKey]

	m.seedClaudeFirstRunForAgent(ap)

	top := assertFirstRunSeeded(t, path, dirs)
	if !bytes.Equal(top[claudeOAuthAccountKey], before) {
		t.Errorf("oauthAccount changed: %s, want %s", top[claudeOAuthAccountKey], before)
	}
	if got := inspectClaudeSession(path).State; got != claudeSessionSignedIn {
		t.Errorf("session state after seed = %v, want signed-in", got)
	}
}

// TestClaudeFirstRunSeed_AlreadySeededIsNotRewritten: a file that already has
// both answers is not touched (same inode, same bytes).
func TestClaudeFirstRunSeed_AlreadySeededIsNotRewritten(t *testing.T) {
	m, ap, home, _ := firstRunTestAgent(t)
	path := claudeSessionFile(home)
	m.seedClaudeFirstRunForAgent(ap)
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)

	m.seedClaudeFirstRunForAgent(ap)

	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("an already-seeded file was replaced")
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, data) {
		t.Errorf("content changed on a no-op seed:\n%s\n%s", data, again)
	}
}

func TestClaudeFirstRunSeed_SymlinkIsRefused(t *testing.T) {
	m, ap, home, _ := firstRunTestAgent(t)
	victim := filepath.Join(t.TempDir(), "victim.json")
	const victimContent = `{"untouched":true}`
	if err := os.WriteFile(victim, []byte(victimContent), 0o600); err != nil {
		t.Fatal(err)
	}
	path := claudeSessionFile(home)
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}

	m.seedClaudeFirstRunForAgent(ap)

	if got, _ := os.ReadFile(victim); string(got) != victimContent {
		t.Errorf("seed wrote through a symlink: victim = %s", got)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink itself was replaced (err=%v)", err)
	}
	assertNoSeedLeftovers(t, home)
}

// TestClaudeFirstRunSeed_UnreadableFileIsLeftAlone: an unreadable file is most
// likely the agent's own 0600 sign-in. It must be skipped, never treated as
// absent and recreated; and the skip is logged once, not on every launch.
func TestClaudeFirstRunSeed_UnreadableFileIsLeftAlone(t *testing.T) {
	requireUnprivileged(t)
	m, ap, home, _ := firstRunTestAgent(t)
	var logs bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&logs, nil))
	path := writeSession(t, home, signedInClaudeSession)
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	m.seedClaudeFirstRunForAgent(ap)
	m.seedClaudeFirstRunForAgent(ap)

	if _, mode := fileOwnerAndMode(t, path); mode != 0o200 {
		t.Errorf("mode = %v, want 0200 untouched", mode)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != signedInClaudeSession {
		t.Errorf("unreadable file was rewritten: %s", got)
	}
	if n := strings.Count(logs.String(), "level=WARN msg=\"claude first-run seed skipped"); n != 1 {
		t.Errorf("WARN count = %d, want 1 across two launches; logs:\n%s", n, logs.String())
	}
	assertNoSeedLeftovers(t, home)
}

// TestClaudeFirstRunSeed_UnwritableHomeIsLeftAlone: when the replacement
// cannot be staged, the original stays exactly as it was.
func TestClaudeFirstRunSeed_UnwritableHomeIsLeftAlone(t *testing.T) {
	requireUnprivileged(t)
	m, ap, home, _ := firstRunTestAgent(t)
	path := writeSession(t, home, skeletonClaudeSession)
	if err := os.Chmod(home, 0o555); err != nil {
		t.Fatal(err)
	}
	restoreMode(t, home)

	m.seedClaudeFirstRunForAgent(ap)

	if got, _ := os.ReadFile(path); string(got) != skeletonClaudeSession {
		t.Errorf("file changed although the seed could not be staged: %s", got)
	}
	assertNoSeedLeftovers(t, home)
}

func TestClaudeFirstRunSeed_UnparseableFileIsLeftAlone(t *testing.T) {
	m, ap, home, _ := firstRunTestAgent(t)
	for _, content := range []string{"{not json", "null", `[1,2]`, `{"projects":"oops"}`} {
		path := writeSession(t, home, content)
		m.seedClaudeFirstRunForAgent(ap)
		if got, _ := os.ReadFile(path); string(got) != content {
			t.Errorf("file %q was rewritten to %s", content, got)
		}
	}
	assertNoSeedLeftovers(t, home)
}

// TestClaudeFirstRunSeed_OnlyPerAgentHomes: with no agent UID the HOME is the
// hive process's own, and the shared-home layout is one file for the whole
// fleet; neither is written.
func TestClaudeFirstRunSeed_OnlyPerAgentHomes(t *testing.T) {
	m, ap, _, _ := firstRunTestAgent(t)
	processHome := t.TempDir()
	t.Setenv("HOME", processHome)
	m.seedClaudeFirstRunForAgent(&AgentProcess{Name: ap.Name, UID: 0})
	if _, err := os.Lstat(claudeSessionFile(processHome)); !os.IsNotExist(err) {
		t.Errorf("uid 0: process HOME's .claude.json was written (err=%v)", err)
	}

	t.Setenv("HIVE_SHARED_AGENT_HOME", "1")
	m.seedClaudeFirstRunForAgent(ap)
	if _, err := os.Lstat(claudeSessionFile(sharedAgentHome)); !os.IsNotExist(err) {
		t.Errorf("shared layout: shared .claude.json was written (err=%v)", err)
	}
}

// TestInferenceDismisserOwnsPrompt: for inference agents the lifetime watcher
// leaves the security notes to dismissInferencePrompts (which already presses
// Enter there), and answers everything else as before.
func TestInferenceDismisserOwnsPrompt(t *testing.T) {
	if !inferenceDismisserOwnsPrompt(true, "claude security notes") {
		t.Error("inference: the watcher must not send a second Enter to the security notes")
	}
	if inferenceDismisserOwnsPrompt(false, "claude security notes") {
		t.Error("subscription claude: the watcher must answer the security notes")
	}
	if inferenceDismisserOwnsPrompt(true, "claude workspace trust") {
		t.Error("inference: the workspace trust answer must stay with the watcher")
	}
}
