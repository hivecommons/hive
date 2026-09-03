package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

// ---------------------------------------------------------------------------
// SetAppAuth + StartAgentTokenRefresh + refreshAgentTokens
// ---------------------------------------------------------------------------

type fakeMinter struct {
	calls    int
	fail     bool
	lastTier string
}

func (f *fakeMinter) WriteAgentToken(ctx context.Context, agentName, tier string, agentUID int) error {
	f.calls++
	f.lastTier = tier
	if f.fail {
		return context.Canceled
	}
	return nil
}

func TestRefreshAgentTokens(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "claude"},
		"cxb": {Backend: "claude"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})

	// Without appAuth set -> early return, no panic.
	m.refreshAgentTokens(context.Background())

	fm := &fakeMinter{}
	m.SetAppAuth(fm)

	m.mu.Lock()
	// Only running agents with UID>0 are refreshed.
	m.agents["cxa"].State = StateRunning
	m.agents["cxa"].UID = 2001
	m.agents["cxb"].State = StateStopped // skipped
	m.mu.Unlock()

	m.refreshAgentTokens(context.Background())
	if fm.calls != 1 {
		t.Errorf("expected 1 token mint, got %d", fm.calls)
	}
}

func TestRefreshAgentTokens_MintError(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"cxa": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	fm := &fakeMinter{fail: true}
	m.SetAppAuth(fm)
	m.mu.Lock()
	m.agents["cxa"].State = StateRunning
	m.agents["cxa"].UID = 2001
	m.mu.Unlock()
	m.refreshAgentTokens(context.Background()) // logs warning, no panic
	if fm.calls != 1 {
		t.Errorf("expected 1 call, got %d", fm.calls)
	}
}

func TestStartAgentTokenRefresh_CancelStops(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.StartAgentTokenRefresh(ctx) // blocks on ticker until ctx cancelled
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("StartAgentTokenRefresh did not return after cancel")
	}
}

// ---------------------------------------------------------------------------
// codexHomePath + setupCodexHome
// ---------------------------------------------------------------------------

func TestCodexHomePath(t *testing.T) {
	if got := codexHomePath("scanner"); got != codexHomePrefix+"scanner" {
		t.Errorf("codexHomePath = %q", got)
	}
}

func TestSetupCodexHome_RootAgentNoOp(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"cxa": {Backend: "codex"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()
	// UID 0 -> early return (no su-exec).
	m.setupCodexHome(agent)
}

func TestSetupCodexHome_NonRootRunsSuExec(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"cxa": {Backend: "codex"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["cxa"]
	agent.UID = 2001 // triggers su-exec path; su-exec absent -> warn branches
	m.mu.Unlock()
	m.setupCodexHome(agent) // must not panic even when su-exec is missing
}

// codexHealTestManager returns a manager and a codex agent with the given UID.
func codexHealTestManager(t *testing.T, uid int) (*Manager, *AgentProcess) {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{"cxa": {Backend: "codex"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["cxa"]
	agent.UID = uid
	m.mu.Unlock()
	return m, agent
}

// TestHealCodexHomeOwnership_AbsentDirNoOp pins that a fresh spoke (no
// CODEX_HOME yet) is untouched: the following mkdir owns the create path.
func TestHealCodexHomeOwnership_AbsentDirNoOp(t *testing.T) {
	m, agent := codexHealTestManager(t, 2001)
	dir := filepath.Join(t.TempDir(), "codex-cxa")
	if got := m.healCodexHomeOwnership(agent, dir, "hive-cxa"); got != nil {
		t.Errorf("absent dir must salvage nothing, got %q", got)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("absent dir must not be created by the heal, stat err=%v", err)
	}
}

// stubCodexHealMechanisms replaces the two root-privileged helpers for the
// duration of a test. The real ones go through su-exec, which exists only
// inside the hive image; substituting them lets the tests below exercise the
// heal's DECISION logic (chown first, rebuild only on failure) on any host.
// chownOK/removeOK select whether each mechanism succeeds. The returned
// counters record how many times each was invoked.
func stubCodexHealMechanisms(t *testing.T, chownOK, removeOK bool) (chownCalls, removeCalls *int) {
	t.Helper()
	origChown, origRemove := chownTreeAsRoot, removeTreeAsRoot
	t.Cleanup(func() { chownTreeAsRoot, removeTreeAsRoot = origChown, origRemove })
	chownCalls, removeCalls = new(int), new(int)
	chownTreeAsRoot = func(dir, spec string) error {
		*chownCalls++
		if !chownOK {
			return fmt.Errorf("stub: chown refused")
		}
		return nil // a real chown would re-own; ownership is not observable here
	}
	removeTreeAsRoot = func(dir string) error {
		*removeCalls++
		if !removeOK {
			return fmt.Errorf("stub: rm refused")
		}
		return os.RemoveAll(dir) // local FS in tests; production shells out
	}
	return chownCalls, removeCalls
}

// TestHealCodexHomeOwnership_ForeignDirChownedNotRemoved is the #5379
// regression test. A CODEX_HOME left owned by the PREVIOUS agent after a
// rename must be re-owned IN PLACE — the lane's codex state (cache/, history)
// must survive, and no removal may be attempted. Before the fix this path
// called os.RemoveAll, which additionally cannot succeed on the NFSv3 /data
// PVC at all.
func TestHealCodexHomeOwnership_ForeignDirChownedNotRemoved(t *testing.T) {
	chownCalls, removeCalls := stubCodexHealMechanisms(t, true, true)
	m, agent := codexHealTestManager(t, os.Getuid()+12345)
	dir := filepath.Join(t.TempDir(), "codex-cxa")
	if err := os.MkdirAll(filepath.Join(dir, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	const cfg = "model = \"gpt-5.1-codex\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.healCodexHomeOwnership(agent, dir, "hive-cxa"); got != nil {
		t.Errorf("chown path preserves the dir, so nothing needs salvaging; got %q", got)
	}
	if *chownCalls != 1 {
		t.Errorf("foreign-owned home must be chowned once, got %d calls", *chownCalls)
	}
	if *removeCalls != 0 {
		t.Errorf("a successful chown must not fall back to removal, got %d calls", *removeCalls)
	}
	if _, err := os.Lstat(filepath.Join(dir, "cache")); err != nil {
		t.Errorf("codex state must survive the re-own, stat err=%v", err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil || string(content) != cfg {
		t.Errorf("config.toml must survive the re-own in place, content=%q err=%v", content, err)
	}
}

// TestHealCodexHomeOwnership_ChownFailureFallsBackToRebuild pins that the
// rebuild is still reachable when the chown cannot run, and that it salvages
// the operator-authored config.toml on the way out.
func TestHealCodexHomeOwnership_ChownFailureFallsBackToRebuild(t *testing.T) {
	chownCalls, removeCalls := stubCodexHealMechanisms(t, false, true)
	m, agent := codexHealTestManager(t, os.Getuid()+12345)
	dir := filepath.Join(t.TempDir(), "codex-cxa")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const cfg = "model = \"gpt-5.1-codex\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	got := m.healCodexHomeOwnership(agent, dir, "hive-cxa")
	if string(got) != cfg {
		t.Errorf("readable config.toml must be salvaged before the rebuild, got %q", got)
	}
	if *chownCalls != 1 || *removeCalls != 1 {
		t.Errorf("expected one chown attempt then one removal, got chown=%d remove=%d", *chownCalls, *removeCalls)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("fallback rebuild must remove the dir, stat err=%v", err)
	}
}

// TestHealCodexHomeOwnership_BothMechanismsFailStaysLoud pins the constraint
// that an unrepairable home keeps its loud ERROR and salvages nothing —
// silence here would hide a dead agent.
func TestHealCodexHomeOwnership_BothMechanismsFailStaysLoud(t *testing.T) {
	chownCalls, removeCalls := stubCodexHealMechanisms(t, false, false)
	m, agent := codexHealTestManager(t, os.Getuid()+12345)
	dir := filepath.Join(t.TempDir(), "codex-cxa")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.healCodexHomeOwnership(agent, dir, "hive-cxa"); got != nil {
		t.Errorf("an unrepairable home must salvage nothing, got %q", got)
	}
	if *chownCalls != 1 || *removeCalls != 1 {
		t.Errorf("both mechanisms must be attempted, got chown=%d remove=%d", *chownCalls, *removeCalls)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Errorf("an unrepairable home must be left alone for manual repair, stat err=%v", err)
	}
}

// TestHealCodexHomeOwnership_AgentOwnedDirKept pins the invariant that a
// correctly-owned CODEX_HOME (and its config.toml) is never touched.
func TestHealCodexHomeOwnership_AgentOwnedDirKept(t *testing.T) {
	m, agent := codexHealTestManager(t, os.Getuid())
	dir := filepath.Join(t.TempDir(), "codex-cxa")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	const cfg = "model = \"gpt-5.1-codex\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.healCodexHomeOwnership(agent, dir, "hive-cxa"); got != nil {
		t.Errorf("agent-owned home must salvage nothing, got %q", got)
	}
	content, err := os.ReadFile(cfgPath)
	if err != nil || string(content) != cfg {
		t.Errorf("agent-owned config.toml must be untouched, content=%q err=%v", content, err)
	}
}

func TestFileOwnerUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := fileOwnerUID(path); got != os.Getuid() {
		t.Errorf("fileOwnerUID = %d, want %d", got, os.Getuid())
	}
	if got := fileOwnerUID(filepath.Join(t.TempDir(), "absent")); got != -1 {
		t.Errorf("fileOwnerUID(absent) = %d, want -1", got)
	}
}

// ---------------------------------------------------------------------------
// CheckAndRestartCrashedAgents
// ---------------------------------------------------------------------------

func TestCheckAndRestartCrashedAgents_SkipsNonEligible(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"stopped":  {Backend: "claude"},
		"paused":   {Backend: "claude"},
		"ondemand": {Backend: "claude", OnDemand: true},
	}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	m.agents["stopped"].State = StateStopped
	m.agents["paused"].State = StateRunning
	m.agents["paused"].Paused = true
	m.agents["ondemand"].State = StateRunning
	m.mu.Unlock()
	// None are eligible; no tmux sessions exist so nothing is declared crashed
	// in a way that would restart (they're all filtered out first).
	restarted := m.CheckAndRestartCrashedAgents(context.Background())
	if len(restarted) != 0 {
		t.Errorf("expected no restarts, got %v", restarted)
	}
}

func TestCheckAndRestartCrashedAgents_MissingSessionRestarts(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})
	now := time.Now().Add(-2 * time.Minute) // past boot grace
	m.mu.Lock()
	m.agents["cxa"].State = StateRunning
	m.agents["cxa"].StartedAt = &now
	m.agents["cxa"].tmuxSession = "hive-a-never-created"
	m.mu.Unlock()
	// Session missing -> agent declared crashed -> Restart -> relaunch in tmux.
	restarted := m.CheckAndRestartCrashedAgents(context.Background())
	defer cleanupAgent(t, m, "cxa")
	if len(restarted) != 1 || restarted[0] != "cxa" {
		t.Errorf("expected agent 'a' restarted, got %v", restarted)
	}
}

// ---------------------------------------------------------------------------
// fixEntry — cover both file and directory perm-fix branches with a file we own.
// ---------------------------------------------------------------------------

func TestFixEntry_FileAndDir(t *testing.T) {
	dir := t.TempDir()

	f := filepath.Join(dir, "file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(f)
	fixEntry(f, fi, discardLogger())

	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	di, _ := os.Stat(sub)
	fixEntry(sub, di, discardLogger())
}

// ---------------------------------------------------------------------------
// readCoveragePreamble — metrics cache absent / malformed.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// agentEnvPairs — inference + copilot token + display name + HIVE_ID env.
// ---------------------------------------------------------------------------

func TestAgentEnvPairs_InferenceCov(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "litellm", Model: "deepseek", DisplayName: "Deep"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	m.SetCopilotToken("gho_x")
	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()

	pairs := m.agentEnvPairs(agent)
	found := map[string]string{}
	for _, p := range pairs {
		found[p.Key] = p.Value
	}

	if found["ANTHROPIC_API_KEY"] != "sk-hive-cxa" {
		t.Errorf("inference should set ANTHROPIC_API_KEY, got %q", found["ANTHROPIC_API_KEY"])
	}
	if _, ok := found["ANTHROPIC_BASE_URL"]; !ok {
		t.Error("inference should set ANTHROPIC_BASE_URL")
	}
	if found["HIVE_AGENT_DISPLAY_NAME"] != "Deep" {
		t.Errorf("display name = %q", found["HIVE_AGENT_DISPLAY_NAME"])
	}
	if _, ok := found["COPILOT_GITHUB_TOKEN"]; !ok {
		t.Error("copilot token should be present as a secret pair")
	}
}

func TestAgentEnvPairs_ConfiguredGatewayGetsTranslatorEnv(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"guide": {Backend: "OpenRouter", Model: "anthropic/claude-opus-4.8"},
	}, discardLogger(), ProjectContext{})
	m.SetGatewayBackendChecker(func(backend string) bool {
		return strings.EqualFold(backend, "openrouter")
	})
	m.mu.RLock()
	agent := m.agents["guide"]
	m.mu.RUnlock()

	found := map[string]string{}
	for _, p := range m.agentEnvPairs(agent) {
		found[p.Key] = p.Value
	}
	if found["ANTHROPIC_API_KEY"] != "sk-hive-guide" {
		t.Errorf("gateway backend should set ANTHROPIC_API_KEY, got %q", found["ANTHROPIC_API_KEY"])
	}
	if _, ok := found["ANTHROPIC_BASE_URL"]; !ok {
		t.Error("gateway backend should set ANTHROPIC_BASE_URL")
	}
	if found["NO_PROXY"] == "" {
		t.Error("gateway backend should set NO_PROXY")
	}
}

func TestAgentEnvPairs_WithToolsEffectiveMode(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "claude", Tools: denyTools("mcp__github__merge_pull_request")},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()
	pairs := m.agentEnvPairs(agent)
	var haveMode bool
	for _, p := range pairs {
		if p.Key == "HIVE_AGENT_MODE" {
			haveMode = true
		}
	}
	if !haveMode {
		t.Error("HIVE_AGENT_MODE should be present")
	}
}

func TestAgentEnvPairs_OptionalEnvVars(t *testing.T) {
	t.Setenv("HIVE_ID", "hive-123")
	t.Setenv("HIVE_SHA", "abc123")
	t.Setenv("HIVE_ADVISORY_ISSUE", "42")
	m := NewManager(map[string]config.AgentConfig{"cxa": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()
	found := map[string]bool{}
	for _, p := range m.agentEnvPairs(agent) {
		found[p.Key] = true
	}
	for _, k := range []string{"HIVE_ID", "HIVE_SHA", "HIVE_ADVISORY_ISSUE"} {
		if !found[k] {
			t.Errorf("%s should be present when env var set", k)
		}
	}
}
