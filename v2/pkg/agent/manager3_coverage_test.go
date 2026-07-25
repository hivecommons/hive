package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
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

func TestReadCoveragePreamble_NoFile(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	// metricsCachePath is a fixed path unlikely to exist -> "".
	if got := m.readCoveragePreamble(); got != "" {
		// If a cache file happens to exist on the host, just ensure no panic.
		t.Logf("readCoveragePreamble returned %q (cache present)", got)
	}
}

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
