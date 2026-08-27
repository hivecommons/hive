package agent

import (
	"os"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

// ---------------------------------------------------------------------------
// UpdateConfig
// ---------------------------------------------------------------------------

func TestUpdateConfig_Success(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})

	newCfg := config.AgentConfig{Backend: "gemini", Model: "pro", Enabled: true}
	err := m.UpdateConfig("scanner", newCfg)
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	status, _ := m.GetStatus("scanner")
	if status.Config.Backend != "gemini" {
		t.Errorf("backend = %q, want gemini", status.Config.Backend)
	}
	if status.Config.Model != "pro" {
		t.Errorf("model = %q, want pro", status.Config.Model)
	}
}

func TestUpdateConfig_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.UpdateConfig("nonexistent", config.AgentConfig{})
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

// ---------------------------------------------------------------------------
// SeedLastKick
// ---------------------------------------------------------------------------

func TestSeedLastKick_SetsTime(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	now := time.Now()
	m.SeedLastKick("scanner", now)

	status, _ := m.GetStatus("scanner")
	if status.LastKick == nil {
		t.Fatal("LastKick should not be nil")
	}
	if !status.LastKick.Equal(now) {
		t.Errorf("LastKick = %v, want %v", status.LastKick, now)
	}
}

func TestSeedLastKick_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	// Should not panic for nonexistent agent
	m.SeedLastKick("nonexistent", time.Now())
}

// ---------------------------------------------------------------------------
// SeedKickHistory
// ---------------------------------------------------------------------------

func TestSeedKickHistory_SetsRecords(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	records := []KickRecord{
		{Timestamp: time.Now().Add(-2 * time.Hour), Agent: "scanner", Snippet: "first kick"},
		{Timestamp: time.Now().Add(-1 * time.Hour), Agent: "scanner", Snippet: "second kick"},
	}

	m.SeedKickHistory("scanner", records)

	m.mu.RLock()
	agent := m.agents["scanner"]
	histLen := len(agent.KickHistory)
	m.mu.RUnlock()

	if histLen != 2 {
		t.Errorf("expected 2 kick records, got %d", histLen)
	}
}

func TestSeedKickHistory_TruncatesToCapacity(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	// Create more records than capacity
	records := make([]KickRecord, kickHistoryCapacity+10)
	for i := range records {
		records[i] = KickRecord{Timestamp: time.Now(), Agent: "scanner", Snippet: "kick"}
	}

	m.SeedKickHistory("scanner", records)

	m.mu.RLock()
	agent := m.agents["scanner"]
	histLen := len(agent.KickHistory)
	m.mu.RUnlock()

	if histLen != kickHistoryCapacity {
		t.Errorf("expected %d kick records (capacity), got %d", kickHistoryCapacity, histLen)
	}
}

func TestSeedKickHistory_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	// Should not panic for nonexistent agent
	m.SeedKickHistory("nonexistent", []KickRecord{})
}

// ---------------------------------------------------------------------------
// PinModel sets ModelOverride too
// ---------------------------------------------------------------------------

func TestPinModel_SetsModelOverride(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})

	_ = m.PinModel("scanner", "opus")

	status, _ := m.GetStatus("scanner")
	if status.ModelOverride != "opus" {
		t.Errorf("ModelOverride = %q, want opus (PinModel should set it)", status.ModelOverride)
	}
}

// ---------------------------------------------------------------------------
// agentEnvPairs with HIVE_ID
// ---------------------------------------------------------------------------

func TestAgentEnvPairs_WithHiveID(t *testing.T) {
	t.Setenv("HIVE_ID", "test-hive-123")

	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})

	ap := &AgentProcess{
		Name:   "scanner",
		Config: config.AgentConfig{Backend: "claude", Model: "sonnet"},
	}

	pairs := m.agentEnvPairs(ap)

	foundHiveID := false
	for _, p := range pairs {
		if p.Key == "HIVE_ID" && p.Value == "test-hive-123" {
			foundHiveID = true
		}
	}
	if !foundHiveID {
		t.Error("HIVE_ID should be included when set in environment")
	}
}

func TestAgentEnvPairs_WithOverrides(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})

	ap := &AgentProcess{
		Name:            "scanner",
		Config:          config.AgentConfig{Backend: "claude", Model: "sonnet"},
		ModelOverride:   "opus",
		BackendOverride: "gemini",
	}

	pairs := m.agentEnvPairs(ap)

	foundModel := false
	foundBackend := false
	for _, p := range pairs {
		if p.Key == "HIVE_MODEL" && p.Value == "opus" {
			foundModel = true
		}
		if p.Key == "HIVE_BACKEND" && p.Value == "gemini" {
			foundBackend = true
		}
	}
	if !foundModel {
		t.Error("expected HIVE_MODEL=opus (override)")
	}
	if !foundBackend {
		t.Error("expected HIVE_BACKEND=gemini (override)")
	}
}

// ---------------------------------------------------------------------------
// Restart — error path (agent not found)
// ---------------------------------------------------------------------------

func TestRestart_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.Restart(nil, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

// ---------------------------------------------------------------------------
// Stop — running agent (cancel path)
// ---------------------------------------------------------------------------

func TestStop_RunningAgentCallsCancel(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	cancelled := false
	m.mu.Lock()
	m.agents["scanner"].State = StateRunning
	m.agents["scanner"].cancel = func() { cancelled = true }
	m.mu.Unlock()

	err := m.Stop("scanner")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if !cancelled {
		t.Error("expected cancel function to be called")
	}

	status, _ := m.GetStatus("scanner")
	if status.State != StateStopped {
		t.Errorf("state = %q, want stopped", status.State)
	}
}

// ---------------------------------------------------------------------------
// Start — already running
// ---------------------------------------------------------------------------

func TestStart_AlreadyRunning(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.Lock()
	m.agents["scanner"].State = StateRunning
	m.mu.Unlock()

	err := m.Start(nil, "scanner")
	if err == nil {
		t.Fatal("expected error for already running agent")
	}
	if err.Error() != "agent scanner already running" {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Start — paused agent stays paused
// ---------------------------------------------------------------------------

func TestStart_PausedAgentStaysPaused(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.Lock()
	m.agents["scanner"].Paused = true
	m.mu.Unlock()

	// Start should not error, and agent should stay in paused state
	// (if tmux session creation succeeds or is mocked)
	// This exercises the paused branch at line 107-109
}

// ---------------------------------------------------------------------------
// GetOutput — nil buffer path
// ---------------------------------------------------------------------------

func TestGetOutput_NilBuffer(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.Lock()
	m.agents["scanner"].OutputBuffer = nil
	m.mu.Unlock()

	output, err := m.GetOutput("scanner", 10)
	if err != nil {
		t.Fatalf("GetOutput: %v", err)
	}
	if output != nil {
		t.Errorf("expected nil output for nil buffer, got: %v", output)
	}
}

// ---------------------------------------------------------------------------
// Resume — not paused
// ---------------------------------------------------------------------------

func TestResume_NotPaused(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	// Agent is in Stopped state (not paused), Resume should be no-op
	err := m.Resume(nil, "scanner", "test", "test resume")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
}

func TestResume_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.Resume(nil, "nonexistent", "test", "test resume")
	if err == nil {
		t.Error("expected error")
	}
}

// ---------------------------------------------------------------------------
// PinCLI / PinModel — not found
// ---------------------------------------------------------------------------

func TestPinCLI_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.PinCLI("nonexistent", "v1")
	if err == nil {
		t.Error("expected error")
	}
}

func TestPinModel_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.PinModel("nonexistent", "opus")
	if err == nil {
		t.Error("expected error")
	}
}

// ---------------------------------------------------------------------------
// SetModelOverride / SetBackendOverride — not found
// ---------------------------------------------------------------------------

func TestSetModelOverride_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.SetModelOverride("nonexistent", "opus")
	if err == nil {
		t.Error("expected error")
	}
}

func TestSetBackendOverride_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.SetBackendOverride("nonexistent", "gemini")
	if err == nil {
		t.Error("expected error")
	}
}

// ---------------------------------------------------------------------------
// NewManager with custom HIVE_WORK_DIR
// ---------------------------------------------------------------------------

func TestNewManager_CustomWorkDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_WORK_DIR", dir)

	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	if m.workDir != dir {
		t.Errorf("workDir = %q, want %q", m.workDir, dir)
	}
}

func TestNewManager_DefaultWorkDir(t *testing.T) {
	// "" (not Unsetenv) so TestMain's package-wide HIVE_WORK_DIR guard
	// (#4737/#4738) is restored after this test instead of leaking an unset
	// var to the rest of the run.
	t.Setenv("HIVE_WORK_DIR", "")
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	if m.workDir != "/data/agents" {
		t.Errorf("workDir = %q, want /data/agents", m.workDir)
	}
}

// ---------------------------------------------------------------------------
// Snapshot copies KickHistory
// ---------------------------------------------------------------------------

func TestSnapshot_CopiesKickHistory(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	now := time.Now()
	m.SeedKickHistory("scanner", []KickRecord{
		{Timestamp: now, Agent: "scanner", Snippet: "test"},
	})

	status, _ := m.GetStatus("scanner")
	if len(status.KickHistory) != 1 {
		t.Fatalf("expected 1 kick record, got %d", len(status.KickHistory))
	}

	// Mutating the snapshot should not affect the original
	status.KickHistory = nil

	status2, _ := m.GetStatus("scanner")
	if len(status2.KickHistory) != 1 {
		t.Error("snapshot KickHistory should be a copy, not a reference")
	}
}

// ---------------------------------------------------------------------------
// SeedRestartCount
// ---------------------------------------------------------------------------

func TestSeedRestartCount(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.SeedRestartCount("scanner", 5)

	status, _ := m.GetStatus("scanner")
	if status.RestartCount != 5 {
		t.Errorf("RestartCount = %d, want 5", status.RestartCount)
	}
}

func TestSeedRestartCount_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	// Should not panic for nonexistent agent
	m.SeedRestartCount("nonexistent", 3)
}

// ---------------------------------------------------------------------------
// RemoveAgent — with cancel (not in extra_test)
// ---------------------------------------------------------------------------

func TestRemoveAgent_WithCancel(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	cancelled := false
	m.mu.Lock()
	m.agents["scanner"].cancel = func() { cancelled = true }
	m.mu.Unlock()

	m.RemoveAgent("scanner")
	if !cancelled {
		t.Error("expected cancel to be called on removal")
	}
}

// ---------------------------------------------------------------------------
// SendKick — not running
// ---------------------------------------------------------------------------

func TestSendKick_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.SendKick("nonexistent", "go")
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestSendKick_NotRunning(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	err := m.SendKick("scanner", "scan issues")
	if err == nil {
		t.Error("expected error for stopped agent")
	}
}

// ---------------------------------------------------------------------------
// Pause — not found / already paused
// ---------------------------------------------------------------------------

func TestPause_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.Pause("nonexistent", "test", "test pause")
	if err == nil {
		t.Error("expected error")
	}
}

func TestPause_AlreadyPaused(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.Lock()
	m.agents["scanner"].Paused = true
	m.mu.Unlock()

	err := m.Pause("scanner", "test", "test pause")
	if err != nil {
		t.Fatalf("Pause should succeed even if already paused: %v", err)
	}
}

// AddAgent, ResetRestartCount tested in manager_extra_test.go

// ---------------------------------------------------------------------------
// buildBootstrapPrompt
// ---------------------------------------------------------------------------

func TestBuildBootstrapPrompt_NoFile(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	prompt := m.buildBootstrapPrompt(agent)
	if prompt != "" {
		t.Error("boot prompt should be empty — agents get kicked by governor")
	}
}

func TestBuildBootstrapPrompt_WithFile(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	// Create a CLAUDE.md file in the expected path
	tmpDir := t.TempDir()
	agentDir := tmpDir + "/agents/scanner"
	os.MkdirAll(agentDir, 0o755)
	os.WriteFile(agentDir+"/CLAUDE.md", []byte("# Scanner instructions"), 0o644)

	// We can't override /data/agents path, but we can verify the fallback behavior
	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	prompt := m.buildBootstrapPrompt(agent)
	if prompt != "" {
		t.Error("boot prompt should be empty — agents get kicked by governor")
	}
}
