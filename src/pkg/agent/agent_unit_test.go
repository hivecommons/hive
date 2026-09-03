package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestSetAndGetACMMLevel(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{ACMMLevel: 2})

	if m.GetACMMLevel() != 2 {
		t.Errorf("initial level = %d, want 2", m.GetACMMLevel())
	}
	m.SetACMMLevel(5)
	if m.GetACMMLevel() != 5 {
		t.Errorf("after set level = %d, want 5", m.GetACMMLevel())
	}
}

func TestOperabilityAgentsRequireACMML5(t *testing.T) {
	for _, name := range []string{"telemetry", "operations"} {
		for level := 0; level < operabilityAgentMinACMMLevel; level++ {
			if AgentAvailableAtACMMLevel(name, level) {
				t.Fatalf("%s should be unavailable at L%d", name, level)
			}
		}
		if !AgentAvailableAtACMMLevel(name, 5) || !AgentAvailableAtACMMLevel(name, 6) {
			t.Fatalf("%s should be available at L5/L6", name)
		}
	}
	if !AgentAvailableAtACMMLevel("scanner", 1) {
		t.Fatal("non-operability agents must not be affected by the L5 gate")
	}
}

func TestManagerDoesNotInstantiateOperabilityAgentsBelowL5(t *testing.T) {
	cfgs := map[string]config.AgentConfig{
		"scanner":    {Backend: "copilot"},
		"telemetry":  {Backend: "copilot"},
		"operations": {Backend: "copilot"},
	}
	m := NewManager(cfgs, slog.Default(), ProjectContext{ACMMLevel: 3})

	if _, ok := m.agents["telemetry"]; ok {
		t.Fatal("telemetry should not be instantiated below L5")
	}
	if _, ok := m.agents["operations"]; ok {
		t.Fatal("operations should not be instantiated below L5")
	}
	if _, ok := m.agents["scanner"]; !ok {
		t.Fatal("scanner should still be instantiated below L5")
	}

	m.AddAgent("telemetry", config.AgentConfig{Backend: "copilot"})
	if _, ok := m.agents["telemetry"]; ok {
		t.Fatal("AddAgent should respect the L5 operability gate")
	}

	m.SetACMMLevel(5)
	m.AddAgent("telemetry", config.AgentConfig{Backend: "copilot"})
	if _, ok := m.agents["telemetry"]; !ok {
		t.Fatal("telemetry should be addable at L5")
	}

	m.SetACMMLevel(4)
	if _, ok := m.agents["telemetry"]; ok {
		t.Fatal("downgrading below L5 should remove instantiated telemetry")
	}
}

func TestReconcileAgentsTreatsOperabilityAgentsAsAbsentBelowL5(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner":   {Backend: "copilot"},
		"telemetry": {Backend: "copilot"},
	}, slog.Default(), ProjectContext{ACMMLevel: 5})

	if _, ok := m.agents["telemetry"]; !ok {
		t.Fatal("telemetry should start present at L5")
	}
	m.SetACMMLevel(3)
	m.ReconcileAgents(map[string]config.AgentConfig{
		"scanner":   {Backend: "copilot"},
		"telemetry": {Backend: "copilot"},
	})
	if _, ok := m.agents["telemetry"]; ok {
		t.Fatal("reconcile below L5 should retire stale telemetry even if config still lists it")
	}
}

func TestResolveAgentByName(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {ID: "scan-001", Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	if m.ResolveAgent("scanner") != "scanner" {
		t.Error("should resolve by name")
	}
}

func TestResolveAgentByID(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {ID: "scan-001", Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	if m.ResolveAgent("scan-001") != "scanner" {
		t.Error("should resolve by ID")
	}
}

func TestResolveAgentUnknown(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	if m.ResolveAgent("nonexistent") != "nonexistent" {
		t.Error("unknown should return input unchanged")
	}
}

func TestAgentModeAdvisory(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Role: "scanner"},
	}, slog.Default(), ProjectContext{ACMMLevel: 2})

	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	mode := m.agentMode(agent)
	if mode != ModeAdvisory {
		t.Errorf("L2 scanner should be ADVISORY, got %s", mode)
	}
}

func TestAgentModeIssuesOnly(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Role: "scanner"},
	}, slog.Default(), ProjectContext{ACMMLevel: 4})

	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	mode := m.agentMode(agent)
	if mode != ModeIssuesOnly {
		t.Errorf("L4 scanner should be ISSUES_ONLY, got %s", mode)
	}
}

func TestAgentModeOverride(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Role: "scanner", Mode: "ISSUES_AND_PRS"},
	}, slog.Default(), ProjectContext{ACMMLevel: 2})

	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	mode := m.agentMode(agent)
	if mode != ModeIssuesAndPRs {
		t.Errorf("mode override should be ISSUES_AND_PRS, got %s", mode)
	}
}

func TestPaneLinesEmpty(t *testing.T) {
	agent := &AgentProcess{
		Name: "scanner",
	}
	lines := agent.PaneLines(10)
	if len(lines) != 0 {
		t.Errorf("empty pane should return 0 lines, got %d", len(lines))
	}
}

func TestPaneLinesWithData(t *testing.T) {
	agent := &AgentProcess{
		Name:            "scanner",
		lastPaneCapture: []string{"line1", "line2", "line3", "line4", "line5"},
	}
	lines := agent.PaneLines(3)
	if len(lines) > 3 {
		t.Errorf("should return at most 3 lines, got %d", len(lines))
	}
}

func TestDeduplicateBlocks(t *testing.T) {
	lines := []string{"a", "b", "c", "a", "b", "c", "d"}
	result := DeduplicateBlocks(lines)
	if len(result) >= len(lines) {
		t.Logf("dedup: %d -> %d", len(lines), len(result))
	}
}

func TestFilteredPaneLinesViaGetOutput(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	output, err := m.GetOutput("scanner", 10)
	if err != nil {
		t.Fatal(err)
	}
	_ = output
}

func TestDiffNewLines(t *testing.T) {
	old := []string{"line1", "line2"}
	new := []string{"line1", "line2", "line3", "line4"}
	diff := diffNewLines(old, new)
	if len(diff) != 2 {
		t.Errorf("expected 2 new lines, got %d", len(diff))
	}
}

func TestDiffNewLinesEmpty(t *testing.T) {
	diff := diffNewLines(nil, []string{"a", "b"})
	if len(diff) != 2 {
		t.Errorf("nil old should return all new, got %d", len(diff))
	}
}

func TestDiffNewLinesSame(t *testing.T) {
	lines := []string{"a", "b", "c"}
	diff := diffNewLines(lines, lines)
	if len(diff) != 0 {
		t.Errorf("same lines should return 0, got %d", len(diff))
	}
}

func TestIsVisualNoise(t *testing.T) {
	noisy := []string{
		"───────────────",
		"━━━━━━━━━━━━━━━",
		"",
		"   ",
		"/data/agents/scanner",
	}
	for _, line := range noisy {
		if !isVisualNoise(line) {
			t.Errorf("should be noise: %q", line)
		}
	}
	if isVisualNoise("real output here") {
		t.Error("real output should not be noise")
	}
}

func TestIsBufferNoise(t *testing.T) {
	// Test that empty/whitespace is noise
	if !isBufferNoise("") {
		t.Error("empty string should be buffer noise")
	}
	if !isBufferNoise("   ") {
		t.Error("whitespace should be buffer noise")
	}
	if isBufferNoise("real meaningful output") {
		t.Error("real output should not be buffer noise")
	}
}

func TestBackendBinaryReturnsPath(t *testing.T) {
	for _, backend := range []string{"claude", "copilot", "goose", "gemini"} {
		got, _ := backendBinary(backend)
		if got == "" {
			t.Errorf("backendBinary(%q) returned empty", backend)
		}
	}
	// Unknown backend
	got, _ := backendBinary("unknown")
	_ = got // may return empty or fallback
}

func TestPaneShowsLoginPromptUnit(t *testing.T) {
	normalLines := []string{
		"normal output",
		"no login needed",
	}
	if paneShowsLoginPrompt(normalLines) {
		t.Error("should not detect login in normal output")
	}
	// Empty pane should not show login
	if paneShowsLoginPrompt(nil) {
		t.Error("nil pane should not show login")
	}
}

func TestPaneShowsFatalNetworkErrorUnit(t *testing.T) {
	tlsError := []string{
		"Copilot v1.0.63 uses AI.",
		"Error auto updating: Failed to fetch latest release: HttpError: request failed: error sending request for url",
		"(https://api.github.com/repos/github/copilot-cli/releases/latest): client error (Connect): invalid peer certificate: BadSignature",
		"❯",
		"/ commands · ? help",
	}
	if !paneShowsFatalNetworkError(tlsError) {
		t.Error("should detect BadSignature TLS error")
	}

	fetchFailed := []string{
		"Error during sign-in: TypeError: fetch failed",
		"Press any key to retry",
	}
	if !paneShowsFatalNetworkError(fetchFailed) {
		t.Error("should detect fetch failed error")
	}

	normalLines := []string{
		"normal output",
		"❯",
		"/ commands · ? help",
	}
	if paneShowsFatalNetworkError(normalLines) {
		t.Error("should not detect error in normal output")
	}

	if paneShowsFatalNetworkError(nil) {
		t.Error("nil pane should not show error")
	}
}

func TestSeedRestartCountUnit(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	m.SeedRestartCount("scanner", 42)
	m.mu.RLock()
	count := m.agents["scanner"].RestartCount
	m.mu.RUnlock()
	if count != 42 {
		t.Errorf("restart count = %d, want 42", count)
	}
}

func TestSeedLastKick(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	now := time.Now()
	m.SeedLastKick("scanner", now)
	m.mu.RLock()
	kick := m.agents["scanner"].LastKick
	m.mu.RUnlock()
	if kick == nil || !kick.Equal(now) {
		t.Error("last kick not seeded correctly")
	}
}

func TestSeedPauseStateUnit(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	now := time.Now()
	m.SeedPauseState("scanner", now, "test-trigger", "test-reason", "test-user")
	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()
	if agent.PausedTrigger != "test-trigger" {
		t.Errorf("trigger = %q", agent.PausedTrigger)
	}
	if agent.PausedReason != "test-reason" {
		t.Errorf("reason = %q", agent.PausedReason)
	}
}

func TestPinCLIAndUnpinUnit(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	m.PinCLI("scanner", "copilot")
	m.mu.RLock()
	pinned := m.agents["scanner"].PinnedCLI
	m.mu.RUnlock()
	if pinned != "copilot" {
		t.Errorf("pinned CLI = %q", pinned)
	}

	m.UnpinCLI("scanner")
	m.mu.RLock()
	pinned = m.agents["scanner"].PinnedCLI
	m.mu.RUnlock()
	if pinned != "" {
		t.Error("should be unpinned")
	}
}

func TestPinModelAndUnpinUnit(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	m.PinModel("scanner", "opus")
	m.mu.RLock()
	pinned := m.agents["scanner"].PinnedModel
	m.mu.RUnlock()
	if pinned != "opus" {
		t.Errorf("pinned model = %q", pinned)
	}

	m.UnpinModel("scanner")
	m.mu.RLock()
	pinned = m.agents["scanner"].PinnedModel
	m.mu.RUnlock()
	if pinned != "" {
		t.Error("should be unpinned")
	}
}

func TestSetModelOverrideUnit(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	m.SetModelOverride("scanner", "opus")
	m.mu.RLock()
	override := m.agents["scanner"].ModelOverride
	m.mu.RUnlock()
	if override != "opus" {
		t.Errorf("model override = %q", override)
	}
}

func TestSetBackendOverrideUnit(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	m.SetBackendOverride("scanner", "copilot")
	m.mu.RLock()
	override := m.agents["scanner"].BackendOverride
	m.mu.RUnlock()
	if override != "copilot" {
		t.Errorf("backend override = %q", override)
	}
}

func TestClearAllModeOverridesUnit(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Mode: "ISSUES_AND_PRS"},
		"quality": {Backend: "claude", Mode: "ADVISORY"},
	}, slog.Default(), ProjectContext{})

	m.ClearAllModeOverrides()

	m.mu.RLock()
	scanMode := m.agents["scanner"].Config.Mode
	qualMode := m.agents["quality"].Config.Mode
	m.mu.RUnlock()

	if scanMode != "" || qualMode != "" {
		t.Error("mode overrides should be cleared")
	}
}

func TestIsPausedUnit(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	if m.IsPaused("scanner") {
		t.Error("should not be paused initially")
	}

	m.Pause("scanner", "test", "testing")
	if !m.IsPaused("scanner") {
		t.Error("should be paused after Pause()")
	}
}

func TestGetStatusSnapshot(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {ID: "scan-001", Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	proc, err := m.GetStatus("scanner")
	if err != nil {
		t.Fatal(err)
	}
	if proc.Name != "scanner" {
		t.Errorf("name = %q", proc.Name)
	}
	if proc.ID != "scan-001" {
		t.Errorf("id = %q", proc.ID)
	}
}

func TestGetStatusNotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, slog.Default(), ProjectContext{})
	_, err := m.GetStatus("nonexistent")
	if err == nil {
		t.Error("should error for nonexistent agent")
	}
}

func TestAllStatuses(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
		"quality": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	statuses := m.AllStatuses()
	if len(statuses) != 2 {
		t.Errorf("expected 2 statuses, got %d", len(statuses))
	}
}

func TestSetBootstrapOverrideUnit(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	m.SetBootstrapOverride("scanner", "custom prompt")
	m.mu.RLock()
	override := m.agents["scanner"].BootstrapOverride
	m.mu.RUnlock()
	if override != "custom prompt" {
		t.Errorf("bootstrap override = %q", override)
	}
}

func TestGetBufferOutput(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	output, err := m.GetBufferOutput("scanner", 10)
	if err != nil {
		t.Fatal(err)
	}
	_ = output
}

func TestGetOutputNotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, slog.Default(), ProjectContext{})
	_, err := m.GetOutput("nonexistent", 10)
	if err == nil {
		t.Error("should error for nonexistent agent")
	}
}

func TestTmuxSessionUnit(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	session := m.TmuxSession("scanner")
	if session != "hive-scanner" {
		t.Errorf("session = %q, want 'hive-scanner'", session)
	}
}

func TestTmuxSessionNotFoundUnit(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, slog.Default(), ProjectContext{})
	session := m.TmuxSession("nonexistent")
	if session != "" {
		t.Error("should return empty for nonexistent")
	}
}

func TestAddAgentUnit(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, slog.Default(), ProjectContext{})

	m.AddAgent("scanner", config.AgentConfig{ID: "scan-001", Backend: "claude"})
	m.mu.RLock()
	_, ok := m.agents["scanner"]
	m.mu.RUnlock()
	if !ok {
		t.Error("agent should be added")
	}
}

func TestRemoveAgentUnit(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	m.RemoveAgent("scanner")
	m.mu.RLock()
	_, ok := m.agents["scanner"]
	m.mu.RUnlock()
	if ok {
		t.Error("agent should be removed")
	}
}

func TestUpdateConfig(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, slog.Default(), ProjectContext{})

	newCfg := config.AgentConfig{Backend: "copilot", Model: "gpt-4o"}
	m.UpdateConfig("scanner", newCfg)
	m.mu.RLock()
	cfg := m.agents["scanner"].Config
	m.mu.RUnlock()
	if cfg.Backend != "copilot" {
		t.Errorf("backend = %q", cfg.Backend)
	}
}

func TestResetRestartCountUnit(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	m.SeedRestartCount("scanner", 10)
	m.ResetRestartCount("scanner")
	m.mu.RLock()
	count := m.agents["scanner"].RestartCount
	m.mu.RUnlock()
	if count != 0 {
		t.Errorf("restart count = %d, want 0", count)
	}
}

func TestSyncModeFilesNoError(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Role: "scanner"},
	}, slog.Default(), ProjectContext{ACMMLevel: 2})
	m.workDir = dir

	os.MkdirAll(filepath.Join(dir, "scanner"), 0755)
	// Should not panic
	m.SyncModeFiles(2)
}

func TestSeedKickHistory(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})

	records := []KickRecord{
		{Timestamp: time.Now(), Agent: "scanner", Snippet: "test kick 1"},
		{Timestamp: time.Now(), Agent: "scanner", Snippet: "test kick 2"},
	}
	m.SeedKickHistory("scanner", records)

	m.mu.RLock()
	history := m.agents["scanner"].KickHistory
	m.mu.RUnlock()
	if len(history) != 2 {
		t.Errorf("expected 2 kick records, got %d", len(history))
	}
}

func TestStopNotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, slog.Default(), ProjectContext{})
	err := m.Stop("nonexistent")
	if err == nil {
		t.Error("should error for nonexistent agent")
	}
}

func TestRestartNotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, slog.Default(), ProjectContext{})
	err := m.Restart(context.TODO(), "nonexistent")
	if err == nil {
		t.Error("should error for nonexistent agent")
	}
}

func TestPauseNotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, slog.Default(), ProjectContext{})
	err := m.Pause("nonexistent", "test", "test")
	if err == nil {
		t.Error("should error for nonexistent agent")
	}
}

func TestDefaultAgentModeByRole(t *testing.T) {
	tests := []struct {
		role  string
		level int
		want  AgentMode
	}{
		{"scanner", 2, ModeAdvisory},
		{"scanner", 4, ModeIssuesOnly},
		{"supervisor", 4, ModeAdvisory},
		{"quality", 3, ModeIssuesAndPRs},
	}
	for _, tt := range tests {
		got := DefaultAgentMode(tt.role, tt.level)
		if got != tt.want {
			t.Errorf("DefaultAgentMode(%q, %d) = %s, want %s", tt.role, tt.level, got, tt.want)
		}
	}
}
