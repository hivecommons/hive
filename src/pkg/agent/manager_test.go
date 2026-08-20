package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

var stubBinDir string

func testTmuxCommand(args ...string) *exec.Cmd {
	tmuxArgs := append([]string{"-L", defaultTmuxSocket}, args...)
	return exec.Command("tmux", tmuxArgs...)
}

func testEnvPairs(ap *AgentProcess) []agentEnvPair {
	m := NewManager(map[string]config.AgentConfig{
		ap.Name: ap.Config,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), ProjectContext{})
	return m.agentEnvPairs(ap)
}

func TestMain(m *testing.M) {
	defaultTmuxSocket = fmt.Sprintf("ht%d", os.Getpid())

	dir, err := os.MkdirTemp(".", ".hive-agent-stubs-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: MkdirTemp: %v\n", err)
		os.Exit(1)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: Abs: %v\n", err)
		os.Exit(1)
	}

	tmuxDir, err := os.MkdirTemp(".", ".hive-agent-tmux-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: tmux MkdirTemp: %v\n", err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	tmuxDir, err = filepath.Abs(tmuxDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: tmux Abs: %v\n", err)
		_ = os.RemoveAll(dir)
		_ = os.RemoveAll(tmuxDir)
		os.Exit(1)
	}

	stubBinDir = dir

	const stubScript = "#!/bin/sh\nexec cat\n"

	for _, name := range []string{"claude", "copilot", "gemini", "goose", "bob"} {
		path := fmt.Sprintf("%s/%s", dir, name)
		if err := os.WriteFile(path, []byte(stubScript), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "TestMain: writing stub %s: %v\n", name, err)
			os.Exit(1)
		}
	}

	originalPath := os.Getenv("PATH")
	originalTMUX := os.Getenv("TMUX")
	originalTMUXTmpDir := os.Getenv("TMUX_TMPDIR")
	os.Setenv("PATH", dir+":"+originalPath)
	os.Unsetenv("TMUX")
	os.Setenv("TMUX_TMPDIR", tmuxDir)

	code := m.Run()

	os.Setenv("PATH", originalPath)
	if originalTMUX == "" {
		os.Unsetenv("TMUX")
	} else {
		os.Setenv("TMUX", originalTMUX)
	}
	if originalTMUXTmpDir == "" {
		os.Unsetenv("TMUX_TMPDIR")
	} else {
		os.Setenv("TMUX_TMPDIR", originalTMUXTmpDir)
	}
	_ = os.RemoveAll(dir)
	_ = os.RemoveAll(tmuxDir)

	os.Exit(code)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func makeAgentConfig(backend, model string) config.AgentConfig {
	return config.AgentConfig{
		Backend: backend,
		Model:   model,
		Enabled: true,
	}
}

func waitForState(t *testing.T, m *Manager, name string, want ...ProcessState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ap, _ := m.GetStatus(name)
		for _, w := range want {
			if ap.State == w {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	ap, _ := m.GetStatus(name)
	wantStrs := make([]string, len(want))
	for i, w := range want {
		wantStrs[i] = string(w)
	}
	t.Errorf("timed out waiting for %q state: got %q", strings.Join(wantStrs, "|"), ap.State)
}

// ---------------------------------------------------------------------------
// NewManager
// ---------------------------------------------------------------------------

func TestNewManager_InitializesAgentsAsStopped(t *testing.T) {
	cfgs := map[string]config.AgentConfig{
		"scanner": makeAgentConfig("claude", "claude-3-5-sonnet"),
		"worker":  makeAgentConfig("gemini", "gemini-pro"),
	}

	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	if len(m.agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(m.agents))
	}

	for name, ap := range m.agents {
		if ap.State != StateStopped {
			t.Errorf("agent %q: expected state %q, got %q", name, StateStopped, ap.State)
		}
		if ap.Name != name {
			t.Errorf("agent %q: Name field = %q, want %q", name, ap.Name, name)
		}
		if ap.PID != 0 {
			t.Errorf("agent %q: expected PID 0 before start, got %d", name, ap.PID)
		}
		if ap.StartedAt != nil {
			t.Errorf("agent %q: expected nil StartedAt before start", name)
		}
	}
}

func TestNewManager_EmptyAgentMap(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	if len(m.agents) != 0 {
		t.Fatalf("expected 0 agents, got %d", len(m.agents))
	}
}

func TestNewManager_ConfigPreserved(t *testing.T) {
	cfg := makeAgentConfig("gemini", "gemini-ultra")
	cfg.BeadsDir = "/tmp/beads"

	m := NewManager(map[string]config.AgentConfig{"agent1": cfg}, discardLogger(), ProjectContext{})

	ap := m.agents["agent1"]
	if ap.Config.Backend != "gemini" {
		t.Errorf("Config.Backend = %q, want %q", ap.Config.Backend, "gemini")
	}
	if ap.Config.Model != "gemini-ultra" {
		t.Errorf("Config.Model = %q, want %q", ap.Config.Model, "gemini-ultra")
	}
	if ap.Config.BeadsDir != "/tmp/beads" {
		t.Errorf("Config.BeadsDir = %q, want %q", ap.Config.BeadsDir, "/tmp/beads")
	}
}

// ---------------------------------------------------------------------------
// GetStatus
// ---------------------------------------------------------------------------

func TestGetStatus_ReturnsCorrectAgent(t *testing.T) {
	cfgs := map[string]config.AgentConfig{
		"alpha": makeAgentConfig("claude", "opus"),
		"beta":  makeAgentConfig("gemini", "pro"),
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	ap, err := m.GetStatus("alpha")
	if err != nil {
		t.Fatalf("GetStatus(%q) unexpected error: %v", "alpha", err)
	}
	if ap.Name != "alpha" {
		t.Errorf("Name = %q, want %q", ap.Name, "alpha")
	}
	if ap.Config.Backend != "claude" {
		t.Errorf("Config.Backend = %q, want %q", ap.Config.Backend, "claude")
	}
}

func TestGetStatus_UnknownAgentReturnsError(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})

	_, err := m.GetStatus("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown agent, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error %q should mention the agent name", err.Error())
	}
}

func TestGetStatus_ReturnsConsistentSnapshots(t *testing.T) {
	cfgs := map[string]config.AgentConfig{"a": makeAgentConfig("claude", "haiku")}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	ap1, _ := m.GetStatus("a")
	ap2, _ := m.GetStatus("a")

	if ap1.Name != ap2.Name || ap1.State != ap2.State {
		t.Error("expected GetStatus to return consistent snapshots")
	}
}

// ---------------------------------------------------------------------------
// AllStatuses
// ---------------------------------------------------------------------------

func TestAllStatuses_ReturnsAllAgents(t *testing.T) {
	cfgs := map[string]config.AgentConfig{
		"x": makeAgentConfig("claude", "opus"),
		"y": makeAgentConfig("gemini", "pro"),
		"z": makeAgentConfig("goose", ""),
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	all := m.AllStatuses()

	if len(all) != 3 {
		t.Fatalf("AllStatuses() returned %d entries, want 3", len(all))
	}
	for _, name := range []string{"x", "y", "z"} {
		if _, ok := all[name]; !ok {
			t.Errorf("AllStatuses() missing agent %q", name)
		}
	}
}

func TestAllStatuses_ReturnsCopy(t *testing.T) {
	cfgs := map[string]config.AgentConfig{"a": makeAgentConfig("claude", "sonnet")}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	all := m.AllStatuses()
	delete(all, "a")

	all2 := m.AllStatuses()
	if _, ok := all2["a"]; !ok {
		t.Error("AllStatuses() returned the internal map instead of a copy — delete affected manager state")
	}
}

func TestAllStatuses_EmptyManager(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	all := m.AllStatuses()
	if len(all) != 0 {
		t.Errorf("expected empty map, got %d entries", len(all))
	}
}

// ---------------------------------------------------------------------------
// backendBinary
// ---------------------------------------------------------------------------

func TestBackendBinary_UnknownBackendReturnsError(t *testing.T) {
	_, err := backendBinary("nonexistent-backend")
	if err == nil {
		t.Fatal("expected error for unknown backend, got nil")
	}
	if !strings.Contains(err.Error(), "unknown backend") {
		t.Errorf("error %q should contain 'unknown backend'", err.Error())
	}
}

func TestBackendBinary_EmptyBackendReturnsError(t *testing.T) {
	_, err := backendBinary("")
	if err == nil {
		t.Fatal("expected error for empty backend, got nil")
	}
}

func TestBackendBinary_KnownBackendsResolveToStubs(t *testing.T) {
	knownBackends := []string{"claude", "copilot", "gemini", "goose", "bob"}

	for _, backend := range knownBackends {
		t.Run(backend, func(t *testing.T) {
			path, err := backendBinary(backend)
			if err != nil {
				t.Errorf("backendBinary(%q) unexpected error: %v", backend, err)
				return
			}
			if !strings.HasPrefix(path, "/") {
				t.Errorf("backendBinary(%q) returned non-absolute path %q", backend, path)
			}
			if path == "" {
				t.Errorf("backendBinary(%q) returned empty path", backend)
			}
		})
	}
}

func TestBackendBinary_ReturnsAbsolutePath(t *testing.T) {
	path, err := backendBinary("claude")
	if err != nil {
		t.Fatalf("backendBinary(claude) error: %v", err)
	}
	if !strings.HasPrefix(path, "/") {
		t.Errorf("expected absolute path, got %q", path)
	}
}

// ---------------------------------------------------------------------------
// agentEnvVars
// ---------------------------------------------------------------------------

func TestAgentEnvPairs_ContainsRequiredKeys(t *testing.T) {
	ap := &AgentProcess{
		Name: "test-agent",
		Config: config.AgentConfig{
			Backend: "claude",
			Model:   "claude-3-5-sonnet",
		},
	}

	pairs := testEnvPairs(ap)

	want := map[string]string{
		"HIVE_AGENT":   "test-agent",
		"HIVE_BACKEND": "claude",
		"HIVE_MODEL":   "claude-3-5-sonnet",
	}

	for _, p := range pairs {
		if expected, ok := want[p.Key]; ok {
			if p.Value != expected {
				t.Errorf("env var %q = %q, want %q", p.Key, p.Value, expected)
			}
			delete(want, p.Key)
		}
	}

	for missing := range want {
		t.Errorf("env var %q missing from agentEnvPairs output", missing)
	}
}

const baseEnvVarCount = 13 // HIVE_AGENT, HIVE_AGENT_DISPLAY_NAME, HIVE_BACKEND, HIVE_MODEL, HIVE_ACMM_LEVEL, HIVE_AGENT_MODE, HTTPS_PROXY, HTTP_PROXY, HIVE_PROXY_AGENT, GIT_TERMINAL_PROMPT, NODE_EXTRA_CA_CERTS, GIT_SSL_CAINFO, HIVE_EXPLAIN_MODE

func TestAgentEnvPairs_BaseEntryCount(t *testing.T) {
	ap := &AgentProcess{
		Name:   "agent",
		Config: config.AgentConfig{Backend: "gemini", Model: "pro"},
	}
	pairs := testEnvPairs(ap)
	if len(pairs) != baseEnvVarCount {
		t.Errorf("testEnvPairs() returned %d vars, want %d", len(pairs), baseEnvVarCount)
	}
}

func TestAgentEnvPairs_EmptyModelAllowed(t *testing.T) {
	ap := &AgentProcess{
		Name:   "nomodel",
		Config: config.AgentConfig{Backend: "goose", Model: ""},
	}
	pairs := testEnvPairs(ap)

	found := false
	for _, p := range pairs {
		if p.Key == "HIVE_MODEL" && p.Value == "" {
			found = true
		}
	}
	if !found {
		t.Error("expected HIVE_MODEL with empty value to be present when model is unset")
	}
}

func TestAgentEnvPairs_BDDirFromBeadsDir(t *testing.T) {
	ap := &AgentProcess{
		Name: "scanner",
		Config: config.AgentConfig{
			Backend:  "claude",
			Model:    "claude-sonnet-4-6",
			BeadsDir: "/data/beads/scanner",
		},
	}

	pairs := testEnvPairs(ap)

	found := false
	for _, p := range pairs {
		if p.Key == "BD_DIR" {
			found = true
			if p.Value != "/data/beads/scanner" {
				t.Errorf("BD_DIR = %q, want %q", p.Value, "/data/beads/scanner")
			}
		}
	}
	if !found {
		t.Error("BD_DIR should be present when BeadsDir is configured")
	}

	// Count should be baseEnvVarCount + 1 for BD_DIR
	const expectedWithBDDir = baseEnvVarCount + 1
	if len(pairs) != expectedWithBDDir {
		t.Errorf("testEnvPairs() returned %d vars, want %d (base + BD_DIR)", len(pairs), expectedWithBDDir)
	}
}

func TestAgentEnvPairs_NoBDDirWhenEmpty(t *testing.T) {
	ap := &AgentProcess{
		Name:   "worker",
		Config: config.AgentConfig{Backend: "claude", Model: "sonnet"},
	}

	pairs := testEnvPairs(ap)

	for _, p := range pairs {
		if p.Key == "BD_DIR" {
			t.Error("BD_DIR should not be present when BeadsDir is empty")
		}
	}
}

func TestCopilotToken_NotInEnvPrefix(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"worker": {Backend: "copilot", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})
	m.copilotAuthToken = "github_pat_secret123"

	ap := &AgentProcess{
		Name:   "worker",
		Config: config.AgentConfig{Backend: "copilot", Model: "sonnet"},
	}

	prefix := m.buildEnvPrefix(ap)
	if strings.Contains(prefix, "COPILOT_GITHUB_TOKEN") {
		t.Error("COPILOT_GITHUB_TOKEN must not appear in inline env prefix (visible in ps output)")
	}
	if strings.Contains(prefix, "github_pat_secret123") {
		t.Error("token value must not appear in inline env prefix")
	}

	pairs := m.agentEnvPairs(ap)
	found := false
	for _, p := range pairs {
		if p.Key == "COPILOT_GITHUB_TOKEN" {
			found = true
			if !p.Secret {
				t.Error("COPILOT_GITHUB_TOKEN must be marked as Secret")
			}
		}
	}
	if !found {
		t.Error("COPILOT_GITHUB_TOKEN should be in agentEnvPairs when token is set")
	}
}

// ---------------------------------------------------------------------------
// Pause / Resume
// ---------------------------------------------------------------------------

func TestPause_UnknownAgentReturnsError(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.Pause("ghost", "test", "test pause")
	if err == nil {
		t.Fatal("expected error pausing unknown agent, got nil")
	}
}

func TestPause_SetsPausedFlag(t *testing.T) {
	cfgs := map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	if err := m.Pause("worker", "test", "test pause"); err != nil {
		t.Fatalf("Pause() error: %v", err)
	}

	ap, _ := m.GetStatus("worker")
	if !ap.Paused {
		t.Error("expected agent to be paused after Pause()")
	}
}

func TestResume_ClearsPausedFlag(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	cfgs := map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	_ = m.Pause("worker", "test", "test pause")
	if err := m.Resume(context.Background(), "worker", "test", "test resume"); err != nil {
		t.Fatalf("Resume() error: %v", err)
	}

	ap, _ := m.GetStatus("worker")
	if ap.Paused {
		t.Error("expected agent to not be paused after Resume()")
	}
}

// ---------------------------------------------------------------------------
// PinCLI / PinModel
// ---------------------------------------------------------------------------

func TestPinCLI_SetsValue(t *testing.T) {
	cfgs := map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	if err := m.PinCLI("worker", "1.2.3"); err != nil {
		t.Fatalf("PinCLI() error: %v", err)
	}

	ap, _ := m.GetStatus("worker")
	if ap.PinnedCLI != "1.2.3" {
		t.Errorf("PinnedCLI = %q, want %q", ap.PinnedCLI, "1.2.3")
	}
}

func TestPinModel_SetsValue(t *testing.T) {
	cfgs := map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	if err := m.PinModel("worker", "opus"); err != nil {
		t.Fatalf("PinModel() error: %v", err)
	}

	ap, _ := m.GetStatus("worker")
	if ap.PinnedModel != "opus" {
		t.Errorf("PinnedModel = %q, want %q", ap.PinnedModel, "opus")
	}
}

// ---------------------------------------------------------------------------
// ModelOverride / BackendOverride
// ---------------------------------------------------------------------------

func TestSetModelOverride(t *testing.T) {
	cfgs := map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	if err := m.SetModelOverride("worker", "opus"); err != nil {
		t.Fatalf("SetModelOverride() error: %v", err)
	}

	ap, _ := m.GetStatus("worker")
	if ap.ModelOverride != "opus" {
		t.Errorf("ModelOverride = %q, want %q", ap.ModelOverride, "opus")
	}
}

func TestSetBackendOverride(t *testing.T) {
	cfgs := map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	if err := m.SetBackendOverride("worker", "gemini"); err != nil {
		t.Fatalf("SetBackendOverride() error: %v", err)
	}

	ap, _ := m.GetStatus("worker")
	if ap.BackendOverride != "gemini" {
		t.Errorf("BackendOverride = %q, want %q", ap.BackendOverride, "gemini")
	}
}

// ---------------------------------------------------------------------------
// SendKick — non-running agent
// ---------------------------------------------------------------------------

func TestSendKick_UnknownAgentReturnsError(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.SendKick("nobody", "hello")
	if err == nil {
		t.Fatal("expected error for unknown agent, got nil")
	}
	if !strings.Contains(err.Error(), "nobody") {
		t.Errorf("error %q should mention the agent name", err.Error())
	}
}

func TestSendKick_NonRunningAgentReturnsError(t *testing.T) {
	cfgs := map[string]config.AgentConfig{
		"idle": makeAgentConfig("claude", "haiku"),
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	err := m.SendKick("idle", "wake up")
	if err == nil {
		t.Fatal("expected error kicking non-running agent, got nil")
	}
	// The message must name the SPECIFIC state. It used to read "agent idle
	// not running", which was identical for a deliberately paused agent, a
	// crashed one and a never-started one — and was read as a launch failure
	// during a live incident on agents that were paused exactly as intended.
	if !strings.Contains(err.Error(), "cannot be kicked") {
		t.Errorf("error %q should explain that the kick was refused", err.Error())
	}
	if !strings.Contains(err.Error(), "stopped") {
		t.Errorf("error %q should name the state that blocked the kick", err.Error())
	}
	if !strings.Contains(err.Error(), "idle") {
		t.Errorf("error %q should mention the agent name", err.Error())
	}
	// Never the old ambiguous phrasing.
	if strings.Contains(err.Error(), "not running") {
		t.Errorf("error %q reverted to the ambiguous wording", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Stop
// ---------------------------------------------------------------------------

func TestStop_UnknownAgentReturnsError(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.Stop("ghost")
	if err == nil {
		t.Fatal("expected error stopping unknown agent, got nil")
	}
}

func TestStop_NonRunningAgentIsNoOp(t *testing.T) {
	cfgs := map[string]config.AgentConfig{
		"idle": makeAgentConfig("claude", "haiku"),
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	if err := m.Stop("idle"); err != nil {
		t.Fatalf("Stop() on non-running agent returned error: %v", err)
	}

	ap, _ := m.GetStatus("idle")
	if ap.State != StateStopped {
		t.Errorf("State = %q after no-op Stop(), want %q", ap.State, StateStopped)
	}
}

// ---------------------------------------------------------------------------
// Start — error paths
// ---------------------------------------------------------------------------

func TestStart_UnknownAgentReturnsError(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})

	err := m.Start(context.Background(), "ghost")
	if err == nil {
		t.Fatal("expected error starting unknown agent, got nil")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error %q should mention the agent name", err.Error())
	}
}

func TestStart_UnknownBackendSetsFailed(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	cfgs := map[string]config.AgentConfig{
		"bad": makeAgentConfig("not-a-real-backend", ""),
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	_ = m.Start(context.Background(), "bad")
	ap, err := m.GetStatus("bad")
	if err != nil {
		t.Fatalf("GetStatus error: %v", err)
	}
	if ap.State != StateFailed {
		t.Errorf("expected state %q, got %q", StateFailed, ap.State)
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

func TestConcurrentGetStatus_NoPanic(t *testing.T) {
	cfgs := map[string]config.AgentConfig{
		"a": makeAgentConfig("claude", "haiku"),
		"b": makeAgentConfig("gemini", "pro"),
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				m.GetStatus("a")
				m.GetStatus("b")
				m.AllStatuses()
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

// ---------------------------------------------------------------------------
// ProcessState constants sanity check
// ---------------------------------------------------------------------------

func TestProcessStateConstants(t *testing.T) {
	states := []ProcessState{StateIdle, StateRunning, StateStopped, StateFailed}
	seen := make(map[ProcessState]bool)
	for _, s := range states {
		if seen[s] {
			t.Errorf("duplicate ProcessState value: %q", s)
		}
		seen[s] = true
		if string(s) == "" {
			t.Error("ProcessState must not be empty string")
		}
	}
}

// ---------------------------------------------------------------------------
// backendBinary: PATH not found error branch
// ---------------------------------------------------------------------------

func TestBackendBinary_KnownBackendMissingFromPath(t *testing.T) {
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent-path-for-testing")
	defer os.Setenv("PATH", origPath)

	_, err := backendBinary("claude")
	if err == nil {
		t.Fatal("expected error when backend not in PATH, got nil")
	}
	if !strings.Contains(err.Error(), "not found in PATH") {
		t.Errorf("error %q should mention 'not found in PATH'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// normalizeModelName
// ---------------------------------------------------------------------------

func TestNormalizeModelName_HyphenToDotsForVersionSuffix(t *testing.T) {
	tests := []struct {
		input   string
		backend string
		want    string
	}{
		{"claude-sonnet-4-6", "copilot", "claude-sonnet-4.6"},
		{"claude-opus-4-6", "copilot", "claude-opus-4.6"},
		{"claude-haiku-4-5", "copilot", "claude-haiku-4.5"},
		{"gemini-pro", "copilot", "gemini-pro"},
		{"claude-3-5-sonnet", "copilot", "claude-3-5-sonnet"},
		{"", "copilot", ""},
		// Claude CLI requires hyphenated names — no conversion.
		{"claude-sonnet-4-6", "claude", "claude-sonnet-4-6"},
		{"claude-opus-4-7", "claude", "claude-opus-4-7"},
	}

	for _, tt := range tests {
		t.Run(tt.backend+"/"+tt.input, func(t *testing.T) {
			got := normalizeModelName(tt.input, tt.backend)
			if got != tt.want {
				t.Errorf("normalizeModelName(%q, %q) = %q, want %q", tt.input, tt.backend, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// os.Environ integration
// ---------------------------------------------------------------------------

func TestStart_EnvIncludesHiveVars(t *testing.T) {
	ap := &AgentProcess{
		Name:   "env-test",
		Config: config.AgentConfig{Backend: "claude", Model: "opus"},
	}
	pairs := testEnvPairs(ap)

	found := false
	for _, p := range pairs {
		if p.Key == "HIVE_AGENT" && p.Value == "env-test" {
			found = true
			break
		}
	}
	if !found {
		t.Error("HIVE_AGENT=env-test not found in env pairs")
	}
}
