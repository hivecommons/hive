package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// ---------------------------------------------------------------------------
// reapAgentCLI / killAgentProcesses — /proc-based; on non-Linux /proc is
// absent so these exercise the ReadDir-error branch. containsCLIMarker and
// environHasMarker are pure and always testable.
// ---------------------------------------------------------------------------

func TestReapAgentCLI_NoProc(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	// On darwin /proc doesn't exist -> returns 0; on linux it scans and finds
	// no matching HIVE_AGENT marker for this test agent -> also 0.
	if n := m.reapAgentCLI(agent); n != 0 {
		t.Errorf("expected 0 reaped for a fresh test agent, got %d", n)
	}
}

func TestKillAgentProcesses_NoMatch(t *testing.T) {
	// A UID that owns no processes (or /proc absent) -> 0 killed, no panic.
	if n := killAgentProcesses(999999, discardLogger()); n != 0 {
		t.Errorf("expected 0 killed, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// installCavemanForAgent — fast branches only (no npx exec).
// ---------------------------------------------------------------------------

func TestInstallCavemanForAgent_EmptyMode(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	// CavemanMode is "" -> early return, no exec.
	m.installCavemanForAgent(agent, "claude")
}

func TestInstallCavemanForAgent_UnsupportedBackend(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "bob", CavemanMode: "loud"},
	}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	// "bob" hits the default (unsupported) switch branch -> returns before exec.
	m.installCavemanForAgent(agent, "bob")
}

// ---------------------------------------------------------------------------
// sanitizeGitRemotes
// ---------------------------------------------------------------------------

func TestSanitizeGitRemotes_CanPushSkips(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude", Mode: "ISSUES_AND_PRS"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	// CanPush() true -> early return, no walk.
	m.sanitizeGitRemotes(agent)
}

func TestSanitizeGitRemotes_AdvisoryWalksEmptyDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_WORK_DIR", dir)
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude", Mode: "ADVISORY"},
	}, discardLogger(), ProjectContext{ACMMLevel: 1})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	// Advisory (no push) -> walks the (empty/nonexistent) agent dir without error.
	m.sanitizeGitRemotes(agent)
}

// ---------------------------------------------------------------------------
// UIDMap.Save error branch
// ---------------------------------------------------------------------------

func TestUIDMapSave_MkdirError(t *testing.T) {
	u := NewUIDMap()
	u.AllocateUIDs([]string{"a", "b"})

	// Create a regular file, then try to Save into a path under it — MkdirAll
	// on the parent fails because a file is in the way.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := u.Save(filepath.Join(blocker, "sub", "uid-map.json")); err == nil {
		t.Error("expected MkdirAll error when parent is a file")
	}
}

func TestUIDMapSave_Success(t *testing.T) {
	u := NewUIDMap()
	u.AllocateUIDs([]string{"z", "a"})
	path := filepath.Join(t.TempDir(), "nested", "uid-map.json")
	if err := u.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := LoadUIDMap(path)
	if err != nil {
		t.Fatalf("LoadUIDMap: %v", err)
	}
	// Alphabetical allocation: a=2001, z=2002.
	if loaded.LookupByName("a") != 2001 {
		t.Errorf("a uid = %d", loaded.LookupByName("a"))
	}
}

// ---------------------------------------------------------------------------
// permissions_watcher: ensureWatchedDirs / fixPermissions / fixEntry
// ---------------------------------------------------------------------------

func TestEnsureWatchedDirs_NoPanic(t *testing.T) {
	// TestMain points WatchedHomeDirs into the test temp tree, so this
	// exercises the real mkdir path hermetically instead of warning against
	// unwritable /data roots.
	ensureWatchedDirs(discardLogger())
}

// TestFixPermissions_RepairsFixture used to be TestFixPermissions_NoPanic,
// which called fixPermissions against whatever the HOST had: on a live hive it
// walked (and could chown/chmod) real /data agent trees for 10+ minutes from a
// unit test (#4685). TestMain now points every walk root into the test temp
// tree, and this test upgrades "doesn't panic" to asserting the repair
// behavior on a fixture it builds itself.
func TestFixPermissions_RepairsFixture(t *testing.T) {
	origWatched := WatchedHomeDirs
	origShared := SharedRepoParent
	origGoose := GooseLogsDir
	origDevUID, origNodeGID := DevUID, NodeGID
	t.Cleanup(func() {
		WatchedHomeDirs = origWatched
		SharedRepoParent = origShared
		GooseLogsDir = origGoose
		DevUID, NodeGID = origDevUID, origNodeGID
	})

	// fixEntry only chmods entries owned by DevUID (it refuses to touch other
	// users' files) and skips the chown when uid/gid already match. Point the
	// guard at the uid/gid this test process actually runs as, so the repair
	// path is exercised rather than skipped.
	DevUID = os.Getuid()
	NodeGID = os.Getgid()

	root := t.TempDir()
	watched := filepath.Join(root, "watched")
	SharedRepoParent = filepath.Join(root, "home")
	GooseLogsDir = filepath.Join(root, "goose-logs")
	WatchedHomeDirs = []string{watched}

	// A too-narrow directory and file inside a watched root, and a repo clone
	// under SharedRepoParent created the way the entrypoint does (0755 → group
	// has no write, the exact ISSUES_AND_PRS lockout fixPermissions exists to
	// heal).
	narrowDir := filepath.Join(watched, "narrow")
	if err := os.MkdirAll(narrowDir, 0o700); err != nil {
		t.Fatal(err)
	}
	narrowFile := filepath.Join(narrowDir, "settings.json")
	if err := os.WriteFile(narrowFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(SharedRepoParent, "api-server")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	repoFile := filepath.Join(repo, "main.go")
	if err := os.WriteFile(repoFile, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink pointing outside the tree must never be followed (CWE-59 guard
	// in fixEntry): its target's mode must be left alone.
	outside := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(narrowDir, "planted-link")); err != nil {
		t.Fatal(err)
	}

	fixPermissions(discardLogger())

	assertMode := func(path string, wantBits os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm()&wantBits != wantBits {
			t.Errorf("%s mode = %v, missing required bits %v", path, info.Mode().Perm(), wantBits)
		}
	}
	assertMode(narrowDir, DirPerms)   // 0700 dir → at least 0770
	assertMode(narrowFile, FilePerms) // 0600 file → at least 0660
	assertMode(repo, DirPerms)        // 0755 clone → group write restored
	assertMode(repoFile, FilePerms)

	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("symlink target outside the tree was touched: mode=%v err=%v — the CWE-59 guard regressed",
			info.Mode().Perm(), err)
	}

	// Idempotent: a second run must change nothing (modes already satisfied).
	before, _ := os.Stat(narrowDir)
	fixPermissions(discardLogger())
	after, _ := os.Stat(narrowDir)
	if before.Mode() != after.Mode() {
		t.Errorf("second fixPermissions run changed %s: %v -> %v", narrowDir, before.Mode(), after.Mode())
	}
}

// ---------------------------------------------------------------------------
// TrajectoryAgents
// ---------------------------------------------------------------------------

func TestTrajectoryAgents(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"running":  {Backend: "claude"},
		"paused":   {Backend: "claude"},
		"stopped":  {Backend: "claude"},
		"nooutput": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.Lock()
	r := m.agents["running"]
	r.State = StateRunning
	r.LastKickMessage = "fix issue #1"
	r.paneMu.Lock()
	r.lastPaneCapture = []string{"working on it", "line two"}
	r.paneMu.Unlock()

	p := m.agents["paused"]
	p.State = StateRunning
	p.Paused = true

	m.agents["nooutput"].State = StateRunning // running but no pane -> omitted
	m.mu.Unlock()

	views := m.TrajectoryAgents()
	if len(views) != 1 {
		t.Fatalf("expected 1 trajectory view, got %d", len(views))
	}
	if views[0].Name != "running" || views[0].Intent != "fix issue #1" {
		t.Errorf("unexpected view: %+v", views[0])
	}
	if views[0].Transcript == "" {
		t.Error("transcript should be non-empty")
	}
}

// ---------------------------------------------------------------------------
// configHasTokens / copilotConfigHasTokens / clearExpiredTokens — shared config
// paths under /data are absent in tests, so these exercise the not-found /
// error branches (returns false / err), which is the safe default.
// ---------------------------------------------------------------------------

func TestConfigHasTokens_NoFiles(t *testing.T) {
	// Redirect both shared credential paths to absent temp files so the
	// negative assertion holds even on live hosts where /data/home/.claude
	// and /data/home/.copilot exist.
	emptySharedPaths(t)
	if configHasTokens() {
		t.Error("configHasTokens should return false when neither shared file exists")
	}
}

func TestCopilotConfigHasTokens_NoFile(t *testing.T) {
	emptySharedPaths(t)
	if copilotConfigHasTokens() {
		t.Error("copilotConfigHasTokens should return false when the shared config is absent")
	}
}

func TestClearExpiredTokens_NoFile(t *testing.T) {
	// Missing config.json -> ReadFile error is returned.
	emptySharedPaths(t)
	if err := clearExpiredTokens(); err == nil {
		t.Error("clearExpiredTokens should return the ReadFile error when config.json is absent")
	}
}

// ---------------------------------------------------------------------------
// BackendAuthAvailable
// ---------------------------------------------------------------------------

func TestBackendAuthAvailable(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})

	// Unknown backend -> (false, false).
	if avail, known := m.BackendAuthAvailable("gemini"); avail || known {
		t.Errorf("gemini auth = (%v,%v), want (false,false)", avail, known)
	}

	// claude -> always known (value depends on credentials file presence).
	if _, known := m.BackendAuthAvailable("claude"); !known {
		t.Error("claude auth should be known")
	}

	// copilot with a cached token -> (true, true).
	m.SetCopilotToken("gho_test")
	if avail, known := m.BackendAuthAvailable("copilot"); !avail || !known {
		t.Errorf("copilot with token = (%v,%v), want (true,true)", avail, known)
	}
	if m.CopilotToken() != "gho_test" {
		t.Error("CopilotToken should return the cached token")
	}

	// copilot with no cached token -> falls through to configHasTokens.
	m.SetCopilotToken("")
	if _, known := m.BackendAuthAvailable("copilot"); !known {
		t.Error("copilot auth should be known even without token")
	}
}

// ---------------------------------------------------------------------------
// ReloadClaudeToken — smoke (reads credentials file, may be empty)
// ---------------------------------------------------------------------------

func TestReloadClaudeToken(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	m.ReloadClaudeToken() // must not panic
}

// ---------------------------------------------------------------------------
// buildEnvPrefix — secrets excluded, non-secrets quoted
// ---------------------------------------------------------------------------

func TestBuildEnvPrefix_SecretExcluded(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	m.SetCopilotToken("gho_secret")
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	prefix := m.buildEnvPrefix(agent)
	if !containsBoot(prefix, "HIVE_AGENT=") {
		t.Errorf("prefix missing HIVE_AGENT: %q", prefix)
	}
	if containsBoot(prefix, "gho_secret") {
		t.Error("secret token must not appear in env prefix")
	}
}
