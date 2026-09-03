package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
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

func writeCodexAuth(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir codex auth dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write codex auth: %v", err)
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
	origUserToken := copilotUserTokenProbePath
	sharedClaudeCredentialPath = filepath.Join(dir, "absent-claude.json")
	sharedCopilotConfigPath = filepath.Join(dir, "absent-copilot.json")
	// The dashboard device-flow token at /data/copilot-user-token exists on a
	// live hive host and short-circuits the copilot probe to "authenticated",
	// so "no shared credentials" must redirect it too (#4585).
	copilotUserTokenProbePath = filepath.Join(dir, "absent-copilot-user-token")
	t.Cleanup(func() {
		sharedClaudeCredentialPath = origClaude
		sharedCopilotConfigPath = origCopilot
		copilotUserTokenProbePath = origUserToken
	})
}

func emptyCodexSharedPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	orig := codexSharedAuthFile
	codexSharedAuthFile = filepath.Join(dir, "absent-codex-auth.json")
	t.Cleanup(func() {
		codexSharedAuthFile = orig
	})
}

func overrideCodexHomePrefix(t *testing.T, prefix string) {
	t.Helper()
	orig := codexHomePrefix
	codexHomePrefix = prefix
	t.Cleanup(func() {
		codexHomePrefix = orig
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

func TestAgentAuthState_ClaudeCredentialFileMissingThenPresent(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	shared := filepath.Join(dir, "shared", ".claude", ".credentials.json")
	t.Setenv("HOME", home)

	origClaude, origCopilot := sharedClaudeCredentialPath, sharedCopilotConfigPath
	sharedClaudeCredentialPath = shared
	sharedCopilotConfigPath = filepath.Join(dir, "shared", ".copilot", "config.json")
	t.Cleanup(func() {
		sharedClaudeCredentialPath = origClaude
		sharedCopilotConfigPath = origCopilot
	})

	m := &Manager{}
	// A MISSING Claude credential file is INCONCLUSIVE, not proof of needs-login:
	// Claude can be authenticated with no file on disk (in-memory session /
	// keychain / a per-UID home this probe cannot read). Report unknown → no
	// badge. (A real Claude login prompt is caught by the pane-scan needsLogin
	// signal, covered by TestAgentAuthState_PaneLoginPromptWins.) This is the fix
	// for the false 🔑 badge on authenticated-but-restarting Claude agents.
	if avail, known := m.AgentAuthState("writer", 0, "claude", false, false); known || avail {
		t.Fatalf("missing claude credentials: got (avail=%v, known=%v), want unknown (no badge)", avail, known)
	}

	// A PRESENT+valid Claude credential file is still positive proof.
	writeClaudeCreds(t, shared)
	if avail, known := m.AgentAuthState("writer", 0, "claude", false, false); !known || !avail {
		t.Fatalf("shared claude credentials present: got (avail=%v, known=%v), want authenticated", avail, known)
	}
}

func TestAgentAuthPathConstructionUsesIsolatedHomes(t *testing.T) {
	emptySharedPaths(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	prefix := filepath.Join(t.TempDir(), "agent-home-")
	claudeInferenceHomePrefixForTest(t, prefix)

	if got := AgentHome("writer", 0, "claude"); got != home {
		t.Fatalf("uid 0 AgentHome = %q, want HOME %q", got, home)
	}
	if got := AgentHome("writer", 2001, "claude"); got != interactiveHomePath("writer") {
		t.Fatalf("claude agent home = %q, want per-agent home %q (#4596)", got, interactiveHomePath("writer"))
	}
	if got := AgentHome("writer", 2001, "litellm"); got != prefix+"writer" {
		t.Fatalf("inference agent home = %q, want %q", got, prefix+"writer")
	}

	claudePaths := agentClaudeCredentialPaths("writer", 0, "claude")
	wantClaude := filepath.Join(home, ".claude", ".credentials.json")
	if len(claudePaths) < 2 || claudePaths[0] != wantClaude || claudePaths[len(claudePaths)-1] != sharedClaudeCredentialPath {
		t.Fatalf("claude credential paths = %v, want first %q and last shared %q",
			claudePaths, wantClaude, sharedClaudeCredentialPath)
	}

	copilotPaths := agentCopilotConfigPaths("writer", 0, "copilot")
	wantCopilot := []string{
		filepath.Join(home, ".copilot", "config.json"),
		filepath.Join(home, ".config", "github-copilot", "apps.json"),
		filepath.Join(home, ".config", "github-copilot", "hosts.json"),
	}
	if len(copilotPaths) < len(wantCopilot)+1 {
		t.Fatalf("copilot credential paths = %v, want home paths plus shared fallback", copilotPaths)
	}
	for i, want := range wantCopilot {
		if copilotPaths[i] != want {
			t.Fatalf("copilotPaths[%d] = %q, want %q (all paths %v)", i, copilotPaths[i], want, copilotPaths)
		}
	}
	if copilotPaths[len(copilotPaths)-1] != sharedCopilotConfigPath {
		t.Fatalf("copilot credential paths = %v, want shared fallback last %q", copilotPaths, sharedCopilotConfigPath)
	}
}

// TestAgentAuthState_TruePositivePreserved: an interactive backend whose missing
// credential file IS conclusive (copilot — no in-memory-session path like
// Claude's) and an agent that is NOT running must still report "needs login" —
// the file-probe signal has to keep working for those backends. (Claude is the
// exception: a missing file is inconclusive → no badge; see
// TestAgentAuthState_ClaudeCredentialFileMissingThenPresent.)
func TestAgentAuthState_TruePositivePreserved(t *testing.T) {
	emptySharedPaths(t)
	// uid>0 resolves HOME to <sharedAgentHome>/agents/scanner — on a live hive
	// host that is a REAL agent home holding real copilot credentials, so the
	// per-agent probe must be pointed at a temp tree too (#4585).
	withSharedAgentHome(t)
	t.Setenv("HOME", t.TempDir())
	m := &Manager{}

	avail, known := m.AgentAuthState("scanner", 2007, "copilot", false /*running*/, false)
	if !known || avail {
		t.Fatalf("no copilot credentials + not running must report needs-login; got (avail=%v, known=%v)", avail, known)
	}
}

// TestAgentAuthState_ClaudeMissingFileIsInconclusive: the specific fix — a Claude
// agent that is NOT running with NO credentials file must report UNKNOWN (no
// badge), because a missing Claude file does not prove needs-login (in-memory
// session / unreadable per-UID home). A false 🔑 badge on such an agent — the
// bug this fixes — was worse than showing nothing.
func TestAgentAuthState_ClaudeMissingFileIsInconclusive(t *testing.T) {
	emptySharedPaths(t)
	t.Setenv("HOME", t.TempDir())
	m := &Manager{}

	avail, known := m.AgentAuthState("quality", 2006, "claude", false /*running*/, false /*needsLogin*/)
	if known || avail {
		t.Fatalf("claude, not running, no file: want unknown (no badge); got (avail=%v, known=%v)", avail, known)
	}
	// But a real pane login prompt STILL badges, even for Claude.
	if avail, known := m.AgentAuthState("quality", 2006, "claude", false, true /*needsLogin*/); !known || avail {
		t.Fatalf("claude at a real login prompt: want needs-login; got (avail=%v, known=%v)", avail, known)
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

func TestAgentAuthState_CopilotCredentials(t *testing.T) {
	emptySharedPaths(t)
	t.Setenv("HOME", t.TempDir())

	m := &Manager{copilotAuthToken: "cached-token"}
	if avail, known := m.AgentAuthState("planner", 0, "copilot", false, false); !known || !avail {
		t.Fatalf("cached token: got (avail=%v, known=%v), want authenticated", avail, known)
	}

	m.copilotAuthToken = ""
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := filepath.Join(home, ".copilot", "config.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatalf("mkdir copilot config dir: %v", err)
	}
	if err := os.WriteFile(cfg, []byte("// generated\n{\"copilotTokens\":{\"user\":\"gho_test\"}}\n"), 0o600); err != nil {
		t.Fatalf("write copilot config: %v", err)
	}
	if avail, known := m.AgentAuthState("planner", 0, "copilot", false, false); !known || !avail {
		t.Fatalf("home config token: got (avail=%v, known=%v), want authenticated", avail, known)
	}

	paths := agentCopilotConfigPaths("planner", 0, "copilot")
	if len(paths) < 4 || paths[0] != cfg {
		t.Fatalf("agentCopilotConfigPaths should prefer HOME config; got %v, want first %q", paths, cfg)
	}
	if !containsPath(paths, sharedCopilotConfigPath) {
		t.Fatalf("agentCopilotConfigPaths should retain shared fallback; got %v", paths)
	}

	if avail, known := m.AgentAuthState("planner", 0, "copilot", false, false); !known || !avail {
		t.Fatalf("copilot config should still authenticate after path checks: got (avail=%v, known=%v)", avail, known)
	}
	m.copilotAuthToken = ""
	t.Setenv("HOME", t.TempDir())
	emptySharedPaths(t)
	if avail, known := m.AgentAuthState("planner", 0, "copilot", false, false); !known || avail {
		t.Fatalf("copilot without tokens: got (avail=%v, known=%v), want known unauthenticated", avail, known)
	}
}

func TestAgentAuthState_CodexCredentials(t *testing.T) {
	emptyCodexSharedPath(t)
	t.Setenv("HOME", t.TempDir())
	// codexEnvHasCredentials reads the developer's real shell: an
	// OPENAI_API_KEY / CODEX_API_KEY exported locally made the "no
	// credentials" arm pass as authenticated. Pin both empty first.
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CODEX_API_KEY", "")
	m := &Manager{}

	if !BackendRequiresInteractiveAuth("codex") {
		t.Fatal("codex must be classified as an interactive OAuth/device-login backend")
	}
	if avail, known := m.AgentAuthState("coder", 0, "codex", false, false); !known || avail {
		t.Fatalf("codex without auth.json: got (avail=%v, known=%v), want known unauthenticated", avail, known)
	}

	t.Setenv("CODEX_API_KEY", "env-api-key")
	if avail, known := m.AgentAuthState("coder", 0, "codex", false, false); !known || !avail {
		t.Fatalf("codex CODEX_API_KEY: got (avail=%v, known=%v), want authenticated", avail, known)
	}
	t.Setenv("CODEX_API_KEY", "")

	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCodexAuth(t, filepath.Join(home, ".codex", "auth.json"), `{"tokens":{"access_token":"oauth-token"}}`)
	if avail, known := m.AgentAuthState("coder", 0, "codex", false, false); !known || !avail {
		t.Fatalf("codex OAuth auth.json: got (avail=%v, known=%v), want authenticated", avail, known)
	}

	emptyCodexSharedPath(t)
	overrideCodexHomePrefix(t, filepath.Join(t.TempDir(), ".codex-"))
	writeCodexAuth(t, filepath.Join(codexHomePath("coder"), "auth.json"), `{"OPENAI_API_KEY":"api-key-login"}`)
	if avail, known := m.AgentAuthState("coder", 2007, "codex", false, false); !known || !avail {
		t.Fatalf("codex per-agent CODEX_HOME auth.json: got (avail=%v, known=%v), want authenticated", avail, known)
	}

	paths := agentCodexAuthPaths("coder", 2007, "codex")
	if len(paths) < 2 || paths[0] != filepath.Join(codexHomePath("coder"), "auth.json") {
		t.Fatalf("agentCodexAuthPaths should prefer CODEX_HOME auth.json; got %v", paths)
	}
}

func TestAgentAuthState_BobAPIKeyGate(t *testing.T) {
	m := &Manager{}
	t.Setenv("HOME", "")
	if home := AgentHome("shared", 0, "claude"); home != "/data/home" {
		t.Fatalf("fallback HOME = %q, want /data/home", home)
	}
	if home := AgentHome("claude-agent", 2001, "claude"); home != interactiveHomePath("claude-agent") {
		t.Fatalf("claude per-UID home = %q, want %q (#4596 per-agent layout)", home, interactiveHomePath("claude-agent"))
	}
	if avail, known := m.AgentAuthState("custom", 0, "custom-backend", false, false); avail || known {
		t.Fatalf("custom non-interactive backend: got (avail=%v, known=%v), want unknown", avail, known)
	}
	if avail, known := m.AgentAuthState("gem", 0, "gemini", false, false); avail || known {
		t.Fatalf("gemini probe: got (avail=%v, known=%v), want unknown", avail, known)
	}

	if avail, known := m.AgentAuthState("writer", 0, bobBackend, false, true); !known || avail {
		t.Fatalf("bob without key: got (avail=%v, known=%v), want known unauthenticated", avail, known)
	}

	m.SetBobAPIKeyResolver(func() string { return "bob-key" })
	if avail, known := m.AgentAuthState("writer", 0, bobBackend, false, true); !known || !avail {
		t.Fatalf("bob with key: got (avail=%v, known=%v), want authenticated", avail, known)
	}
}

func TestAgentAuthAvailableResolvesProcessState(t *testing.T) {
	emptySharedPaths(t)
	m := &Manager{
		agents: map[string]*AgentProcess{
			"runner": {
				Name:  "runner",
				UID:   2001,
				State: StateRunning,
				Config: config.AgentConfig{
					Backend: "claude",
				},
			},
			"override": {
				Name:            "override",
				State:           StateStopped,
				Config:          config.AgentConfig{Backend: "claude"},
				BackendOverride: "litellm",
			},
		},
	}

	if avail, known := m.AgentAuthAvailable("missing"); avail || known {
		t.Fatalf("missing agent: got (avail=%v, known=%v), want unknown", avail, known)
	}

	if avail, known := m.AgentAuthAvailable("runner"); avail || known {
		t.Fatalf("running claude agent: got (avail=%v, known=%v), want unknown/no badge", avail, known)
	}

	m.agents["runner"].paneMu.Lock()
	m.agents["runner"].NeedsLogin = true
	m.agents["runner"].paneMu.Unlock()
	if avail, known := m.AgentAuthAvailable("runner"); !known || avail {
		t.Fatalf("login prompt: got (avail=%v, known=%v), want needs-login", avail, known)
	}

	if avail, known := m.AgentAuthAvailable("override"); !known || !avail {
		t.Fatalf("backend override to inference: got (avail=%v, known=%v), want authenticated", avail, known)
	}
}

func TestPermissionsWatcherRepairsBobStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), bobStateDirBase)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir bob state dir: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat bob state dir: %v", err)
	}
	fixEntry(dir, info, logger)

	info, err = os.Stat(dir)
	if err != nil {
		t.Fatalf("stat repaired bob state dir: %v", err)
	}
	if got := info.Mode().Perm(); got&bobStateDirGroupRWX != bobStateDirGroupRWX {
		t.Fatalf("bob state dir mode = %v, want group rwx bits", got)
	}

	fixBobStateDirGroupWrite(dir, info.Mode(), logger)
	again, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat idempotent bob state dir: %v", err)
	}
	if again.Mode().Perm() != info.Mode().Perm() {
		t.Fatalf("idempotent repair changed mode from %v to %v", info.Mode().Perm(), again.Mode().Perm())
	}
}

func TestPermissionsWatcherEnsuresAndWalksRedirectedDirs(t *testing.T) {
	root := t.TempDir()
	watched := filepath.Join(root, "watched")
	goose := filepath.Join(root, "goose", "logs")
	oldWatched, oldGoose := WatchedHomeDirs, GooseLogsDir
	WatchedHomeDirs = []string{watched}
	GooseLogsDir = goose
	t.Cleanup(func() {
		WatchedHomeDirs = oldWatched
		GooseLogsDir = oldGoose
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ensureWatchedDirs(logger)
	for _, dir := range []string{watched, goose} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("expected %s to be created: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
	}

	nestedBob := filepath.Join(watched, "agent", bobStateDirBase)
	if err := os.MkdirAll(nestedBob, 0o755); err != nil {
		t.Fatalf("mkdir nested bob dir: %v", err)
	}
	fixPermissions(logger)
	info, err := os.Stat(nestedBob)
	if err != nil {
		t.Fatalf("stat nested bob dir: %v", err)
	}
	if info.Mode().Perm()&bobStateDirGroupRWX != bobStateDirGroupRWX {
		t.Fatalf("nested bob dir mode = %v, want group rwx bits", info.Mode().Perm())
	}

	nonDir := filepath.Join(root, "not-dir")
	if err := os.WriteFile(nonDir, []byte("x"), 0o600); err != nil {
		t.Fatalf("write non-dir: %v", err)
	}
	WatchedHomeDirs = []string{nonDir}
	fixPermissions(logger)
}

type fakeFileInfo struct {
	name  string
	mode  os.FileMode
	isDir bool
	sys   any
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.isDir }
func (f fakeFileInfo) Sys() any           { return f.sys }

func TestFixEntryWithSyntheticOwnership(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := filepath.Join(t.TempDir(), "dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fixEntry(dir, fakeFileInfo{
		name:  "dir",
		mode:  0o700 | os.ModeDir,
		isDir: true,
		sys:   &syscall.Stat_t{Uid: uint32(DevUID), Gid: uint32(NodeGID)},
	}, logger)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := info.Mode().Perm(); got&DirPerms != DirPerms {
		t.Fatalf("dir mode = %v, want %v bits", got, DirPerms)
	}

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fixEntry(file, fakeFileInfo{
		name: "file",
		mode: 0o600,
		sys:  &syscall.Stat_t{Uid: uint32(DevUID), Gid: uint32(NodeGID)},
	}, logger)
	info, err = os.Stat(file)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if got := info.Mode().Perm(); got&FilePerms != FilePerms {
		t.Fatalf("file mode = %v, want %v bits", got, FilePerms)
	}

	fixEntry(file, fakeFileInfo{name: "no-stat", mode: 0o600, sys: struct{}{}}, logger)
	fixEntry(file, fakeFileInfo{
		name: "other-owner",
		mode: 0o600,
		sys:  &syscall.Stat_t{Uid: uint32(DevUID + 100), Gid: uint32(NodeGID)},
	}, logger)
	fixEntry(file, fakeFileInfo{
		name: "root-owned",
		mode: 0o600,
		sys:  &syscall.Stat_t{Uid: 0, Gid: 0},
	}, logger)
}

func TestCoverageGuardsForRestartAndBobRelaunch(t *testing.T) {
	m := &Manager{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		agents: map[string]*AgentProcess{},
	}
	if err := m.RestartThenSendKick(context.Background(), "missing", "hello"); err == nil {
		t.Fatal("RestartThenSendKick missing agent succeeded, want error")
	}
	if got := m.RelaunchBobAgentsAwaitingKey(context.Background()); got != nil {
		t.Fatalf("RelaunchBobAgentsAwaitingKey without key = %v, want nil", got)
	}
	m.SetBobAPIKeyResolver(func() string { return "key" })
	if got := m.RelaunchBobAgentsAwaitingKey(context.Background()); len(got) != 0 {
		t.Fatalf("RelaunchBobAgentsAwaitingKey with no candidates = %v, want empty", got)
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
