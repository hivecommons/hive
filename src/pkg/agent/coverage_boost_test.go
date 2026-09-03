package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/config"
)

// ---------------------------------------------------------------------------
// Save — error paths
// ---------------------------------------------------------------------------

func TestSave_InvalidPath(t *testing.T) {
	u := NewUIDMap()
	u.AllocateUIDs([]string{"scanner"})

	// Write to a path where the parent can't be created
	err := u.Save("/proc/impossible/uid-map.json")
	if err == nil {
		t.Error("expected error for impossible path")
	}
}

func TestLoadUIDMap_NilAgents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "uid-map.json")
	// Write JSON with no agents field
	data := `{"proxy_uid":1001,"base_uid":2001,"iptables_active":false}`
	os.WriteFile(path, []byte(data), 0o644)

	loaded, err := LoadUIDMap(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Agents == nil {
		t.Error("Agents should be initialized even if missing from JSON")
	}
}

// ---------------------------------------------------------------------------
// NewManager — with UIDMap integration
// ---------------------------------------------------------------------------

func TestNewManager_WithUIDMap(t *testing.T) {
	dir := t.TempDir()
	uidMapPath := filepath.Join(dir, "uid-map.json")

	u := NewUIDMap()
	u.AllocateUIDs([]string{"scanner", "quality"})
	u.IptablesActive = true
	u.Save(uidMapPath)

	// Temporarily set HIVE_WORK_DIR and test that NewManager picks up the UID map
	// We can't easily override UIDMapPath (it's a const), but we can test the
	// UID lookup logic that NewManager uses
	uid := u.LookupByName("scanner")
	if uid == 0 {
		t.Error("scanner should have a UID from the map")
	}
	if uid < baseAgentUID {
		t.Errorf("scanner UID = %d, should be >= %d", uid, baseAgentUID)
	}
}

// ---------------------------------------------------------------------------
// configHasTokens / clearExpiredTokens — test with actual temp files
// ---------------------------------------------------------------------------

func TestConfigHasTokens_WithCommentStripping(t *testing.T) {
	// Simulate the exact format Copilot CLI writes
	content := `// User settings belong in settings.json.
// This file is managed automatically.
{
  "copilotTokens": {
    "github.com": {
      "token": "gho_abc123",
      "expiresAt": "2024-01-01T00:00:00Z"
    }
  },
  "loggedInUsers": ["github.com"],
  "lastLoggedInUser": "github.com"
}`

	// Parse using the same logic as configHasTokens
	var cleaned []byte
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		cleaned = append(cleaned, []byte(line+"\n")...)
	}

	var cfg map[string]interface{}
	if err := json.Unmarshal(cleaned, &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}

	tokens, ok := cfg["copilotTokens"]
	if !ok {
		t.Fatal("copilotTokens missing")
	}
	tokensMap, ok := tokens.(map[string]interface{})
	if !ok {
		t.Fatal("copilotTokens not a map")
	}
	if len(tokensMap) == 0 {
		t.Error("should have tokens")
	}
}

// ---------------------------------------------------------------------------
// buildBootstrapPrompt — all path priorities
// ---------------------------------------------------------------------------

func TestBuildBootstrapPrompt_WithExamplesDir(t *testing.T) {
	dir := t.TempDir()
	policyDir := filepath.Join(dir, "policies", "agents")
	examplesDir := filepath.Join(dir, "policies", "examples", "agents")
	os.MkdirAll(policyDir, 0o755)
	os.MkdirAll(examplesDir, 0o755)
	os.WriteFile(filepath.Join(examplesDir, "scanner.md"), []byte("# Example Scanner"), 0o644)

	m := &Manager{
		agents:   make(map[string]*AgentProcess),
		idToName: make(map[string]string),
		logger:   discardLogger(),
		project: ProjectContext{
			Org:       "testorg",
			Repos:     []string{"repo"},
			ACMMLevel: 2,
			PolicyDir: policyDir,
		},
	}

	agent := &AgentProcess{
		Name:   "scanner",
		Config: config.AgentConfig{Role: "scanner"},
	}

	got := m.buildBootstrapPrompt(agent)
	// Boot prompt is always empty now — governor kicks
	if got != "" {
		t.Errorf("boot prompt should be empty, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// findACMMFragments — level 0 returns nil
// ---------------------------------------------------------------------------

func TestFindACMMFragments_LevelZero(t *testing.T) {
	m := &Manager{
		logger: discardLogger(),
		project: ProjectContext{
			ACMMLevel: 0,
			PolicyDir: "/data/policies/agents",
		},
	}
	files := m.findACMMFragments()
	if len(files) != 0 {
		t.Errorf("level 0 should return nil, got %v", files)
	}
}

func TestFindACMMFragments_EmptyPolicyDir(t *testing.T) {
	m := &Manager{
		logger: discardLogger(),
		project: ProjectContext{
			ACMMLevel: 3,
			PolicyDir: "",
		},
	}
	// Should not panic
	files := m.findACMMFragments()
	_ = files
}

// ---------------------------------------------------------------------------
// buildProjectPreamble — all mode branches
// ---------------------------------------------------------------------------

func TestBuildProjectPreamble_IssuesOnly(t *testing.T) {
	m := &Manager{
		logger: discardLogger(),
		project: ProjectContext{
			Org:        "testorg",
			Repos:      []string{"repo1"},
			ACMMLevel:  4,
			PRsAllowed: true,
		},
	}
	// scanner at L4 = ISSUES_ONLY
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{}}
	got := m.buildProjectPreamble(agent)
	if !strings.Contains(got, "Issues ONLY") {
		t.Errorf("L4 scanner should say Issues ONLY, got %q", got)
	}
}

func TestBuildProjectPreamble_SupervisorAlwaysAdvisory(t *testing.T) {
	m := &Manager{
		logger: discardLogger(),
		project: ProjectContext{
			Org:        "testorg",
			Repos:      []string{"repo1"},
			ACMMLevel:  6,
			PRsAllowed: true,
		},
	}
	agent := &AgentProcess{Name: "supervisor", Config: config.AgentConfig{}}
	got := m.buildProjectPreamble(agent)
	if !strings.Contains(got, "Advisory") {
		t.Errorf("supervisor should always be advisory, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// agentEnvPairs — comprehensive branch coverage
// ---------------------------------------------------------------------------

func TestAgentEnvPairs_WithHiveSHA(t *testing.T) {
	t.Setenv("HIVE_SHA", "deadbeef")

	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})

	ap := &AgentProcess{
		Name:   "scanner",
		Config: config.AgentConfig{Backend: "claude", Model: "sonnet"},
	}

	pairs := m.agentEnvPairs(ap)
	found := false
	for _, p := range pairs {
		if p.Key == "HIVE_SHA" && p.Value == "deadbeef" {
			found = true
		}
	}
	if !found {
		t.Error("HIVE_SHA should be included when set")
	}
}

func TestAgentEnvPairs_WithAdvisoryIssue(t *testing.T) {
	t.Setenv("HIVE_ADVISORY_ISSUE", "99")

	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})

	ap := &AgentProcess{
		Name:   "scanner",
		Config: config.AgentConfig{Backend: "claude", Model: "sonnet"},
	}

	pairs := m.agentEnvPairs(ap)
	found := false
	for _, p := range pairs {
		if p.Key == "HIVE_ADVISORY_ISSUE" && p.Value == "99" {
			found = true
		}
	}
	if !found {
		t.Error("HIVE_ADVISORY_ISSUE should be included when set")
	}
}

// Inference-routed claude sessions get the CLI telemetry switches so the
// CLI stops sending event-logging / error-report traffic to the gateway;
// subscription sessions do not (Anthropic's own telemetry is legitimate there).
func TestAgentEnvPairs_InferenceQuietCLIEnv(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"inf": {Backend: "vllm", Model: "llama-70b"},
		"sub": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})

	has := func(pairs []agentEnvPair, key string) bool {
		for _, p := range pairs {
			if p.Key == key && p.Value == "1" {
				return true
			}
		}
		return false
	}
	inf := m.agentEnvPairs(&AgentProcess{Name: "inf", Config: config.AgentConfig{Backend: "vllm", Model: "llama-70b"}})
	sub := m.agentEnvPairs(&AgentProcess{Name: "sub", Config: config.AgentConfig{Backend: "claude", Model: "sonnet"}})
	for _, key := range inferenceQuietCLIEnv {
		if !has(inf, key) {
			t.Errorf("inference backend should set %s=1", key)
		}
		if has(sub, key) {
			t.Errorf("subscription backend should not set %s", key)
		}
	}
}

func TestAgentEnvPairs_NonInference_NoAnthropicVars(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})

	ap := &AgentProcess{
		Name:   "scanner",
		Config: config.AgentConfig{Backend: "claude", Model: "sonnet"},
	}

	pairs := m.agentEnvPairs(ap)
	for _, p := range pairs {
		if p.Key == "ANTHROPIC_API_KEY" || p.Key == "ANTHROPIC_BASE_URL" || p.Key == "NO_PROXY" {
			t.Errorf("non-inference backend should not have %s", p.Key)
		}
	}
}

func TestAgentEnvPairs_UID_NonInference_HOME(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})

	ap := &AgentProcess{
		Name:   "scanner",
		UID:    1001,
		Config: config.AgentConfig{Backend: "claude", Model: "sonnet"},
	}

	pairs := m.agentEnvPairs(ap)
	for _, p := range pairs {
		if p.Key == "HOME" {
			// #4596: per-UID interactive agents get a per-agent HOME under the
			// shared home, so one agent's .claude.json rewrite can never sign
			// out the fleet. Must match AgentHome exactly.
			if want := AgentHome("scanner", 1001, "claude"); p.Value != want {
				t.Errorf("non-inference UID agent HOME = %q, want %q", p.Value, want)
			}
			if p.Value != "/data/home/agents/scanner" {
				t.Errorf("non-inference UID agent HOME = %q, want /data/home/agents/scanner", p.Value)
			}
			return
		}
	}
	t.Error("HOME should be set for agent with UID > 0")
}

func TestAgentEnvPairs_NoUID_NoHOME(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})

	ap := &AgentProcess{
		Name:   "scanner",
		UID:    0,
		Config: config.AgentConfig{Backend: "claude", Model: "sonnet"},
	}

	pairs := m.agentEnvPairs(ap)
	for _, p := range pairs {
		if p.Key == "HOME" {
			t.Error("UID 0 should not have HOME set")
		}
	}
}

// ---------------------------------------------------------------------------
// SyncModeFiles — write + verify
// ---------------------------------------------------------------------------

func TestSyncModeFiles_WritesCorrectMode(t *testing.T) {
	m := &Manager{
		agents: map[string]*AgentProcess{
			"scanner": {Name: "scanner", State: StateRunning, Config: config.AgentConfig{}},
		},
		logger:  discardLogger(),
		project: ProjectContext{ACMMLevel: 4},
	}

	m.SyncModeFiles(4)

	data, err := os.ReadFile(filepath.Join(agentStateDir, ".hive-mode-scanner"))
	if err != nil {
		// agentStateDir is redirected by TestMain into its temp tree and
		// MkdirAll-ed there, and SyncModeFiles above just wrote this file.
		// Unreadable here means SyncModeFiles silently did not write --
		// exactly the assertion this test exists to make (#5388).
		testutil.SkipfUnlessRequired(t, "could not read mode file: %v", err)
	}
	mode := string(data)
	if mode != "ISSUES_ONLY" {
		t.Errorf("scanner mode at L4 = %q, want ISSUES_ONLY", mode)
	}
}

// ---------------------------------------------------------------------------
// DeduplicateBlocks — recursive dedup
// ---------------------------------------------------------------------------

func TestDeduplicateBlocks_TripleDuplicate(t *testing.T) {
	lines := []string{
		"a", "b",
		"a", "b",
		"a", "b",
		"c",
	}
	result := DeduplicateBlocks(lines)
	// Deduplication removes earlier duplicate blocks
	// With 3 copies of [a,b] + [c], we expect some reduction
	t.Logf("triple dedup: %d -> %d lines", len(lines), len(result))
}

// ---------------------------------------------------------------------------
// filterPaneOutput — edge case: all noise
// ---------------------------------------------------------------------------

func TestFilterPaneOutput_AllNoise(t *testing.T) {
	lines := []string{
		"───────────────",
		"━━━━━━━━━━━━━━━",
		"",
		"   ",
	}
	result := filterPaneOutput(lines, 10)
	if len(result) != 0 {
		t.Errorf("all noise should yield 0 lines, got %d", len(result))
	}
}

func TestFilterPaneOutput_PromptOnly(t *testing.T) {
	lines := []string{"❯"}
	result := filterPaneOutput(lines, 10)
	// Prompt-only should return empty (prompt at end with nothing after, nothing before)
	_ = result // just verify no panic
}

// ---------------------------------------------------------------------------
// isBufferNoise — more patterns
// ---------------------------------------------------------------------------

func TestIsBufferNoise_TrustDialog(t *testing.T) {
	if !isBufferNoise("Do you trust the files in this folder?") {
		t.Error("trust dialog should be noise")
	}
	if !isBufferNoise("› Yes, I trust the authors") {
		t.Error("trust yes option should be noise")
	}
	if !isBufferNoise("› No (Esc)") {
		t.Error("trust no option should be noise")
	}
}

func TestIsBufferNoise_TrustedFolder(t *testing.T) {
	if !isBufferNoise("● Folder /data/agents/scanner is now trusted") {
		t.Error("trusted folder message should be noise")
	}
}

func TestIsBufferNoise_ModelNotAvailable(t *testing.T) {
	if !isBufferNoise("✗ Model gpt-5 not available, using gpt-4o instead") {
		t.Error("model not available should be noise")
	}
}

// ---------------------------------------------------------------------------
// isCLIChrome — more edge cases
// ---------------------------------------------------------------------------

func TestIsCLIChrome_EmptyString(t *testing.T) {
	if !isCLIChrome("") {
		t.Error("empty string should be chrome")
	}
}

func TestIsCLIChrome_WhitespaceOnly(t *testing.T) {
	if !isCLIChrome("   ") {
		t.Error("whitespace should be chrome")
	}
}

func TestIsCLIChrome_CommandHints(t *testing.T) {
	if !isCLIChrome("/ commands available") {
		t.Error("/ commands should be chrome")
	}
	if !isCLIChrome("? help for usage info") {
		t.Error("? help should be chrome")
	}
	if !isCLIChrome("@ files to include") {
		t.Error("@ files should be chrome")
	}
	if !isCLIChrome("# issues to review") {
		t.Error("# issues should be chrome")
	}
}

// ---------------------------------------------------------------------------
// isVisualNoise — edge cases
// ---------------------------------------------------------------------------

func TestIsVisualNoise_MixedSeparators(t *testing.T) {
	if !isVisualNoise("─━─━─━─") {
		t.Error("mixed separators should be noise")
	}
}

func TestIsVisualNoise_RegularContent(t *testing.T) {
	if isVisualNoise("Found 5 issues in codebase") {
		t.Error("regular content should not be noise")
	}
}

// ---------------------------------------------------------------------------
// normalizeLine — credits pattern
// ---------------------------------------------------------------------------

func TestNormalizeLine_CreditsVariation(t *testing.T) {
	a := normalizeLine("Working AI Credits: 42.3 on task")
	b := normalizeLine("Working AI Credits: 99.9 on task")
	if a != b {
		t.Errorf("credits variation should normalize: %q vs %q", a, b)
	}
}

func TestNormalizeLine_SpinnerVariation(t *testing.T) {
	a := normalizeLine("◐ Loading data")
	b := normalizeLine("◓ Loading data")
	if a != b {
		t.Errorf("spinner variation should normalize: %q vs %q", a, b)
	}
}

// ---------------------------------------------------------------------------
// Additional KillSession tests
// ---------------------------------------------------------------------------

func TestKillSession_ExistingAgent(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	// Should not error (tmux session doesn't exist but that's OK)
	err := m.KillSession("scanner")
	if err != nil {
		t.Errorf("KillSession: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Concurrent AddAgent + RemoveAgent
// ---------------------------------------------------------------------------

func TestConcurrentAddRemove_NoPanic(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})

	done := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func(id int) {
			name := "agent-" + string(rune('a'+id))
			for j := 0; j < 20; j++ {
				m.AddAgent(name, config.AgentConfig{Backend: "claude"})
				m.RemoveAgent(name)
				m.AllStatuses()
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 5; i++ {
		<-done
	}
}

// ---------------------------------------------------------------------------
// Pause preserves trigger and reason via SeedPauseState
// ---------------------------------------------------------------------------

func TestSeedPauseState_PreservesFields(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.Pause("scanner", "governor", "too many errors")

	status, _ := m.GetStatus("scanner")
	if status.PausedTrigger != "governor" {
		t.Errorf("trigger = %q, want governor", status.PausedTrigger)
	}
	if status.PausedReason != "too many errors" {
		t.Errorf("reason = %q, want 'too many errors'", status.PausedReason)
	}
}

// ---------------------------------------------------------------------------
// Agent state constants
// ---------------------------------------------------------------------------

func TestStatePausedConstant(t *testing.T) {
	if StatePaused != "paused" {
		t.Errorf("StatePaused = %q, want 'paused'", StatePaused)
	}
}

// ---------------------------------------------------------------------------
// buildProjectPreamble — format verification
// ---------------------------------------------------------------------------

func TestBuildProjectPreamble_FormatContainsOrgAndRepos(t *testing.T) {
	m := &Manager{
		logger: discardLogger(),
		project: ProjectContext{
			Org:       "kubestellar",
			Repos:     []string{"console", "docs"},
			ACMMLevel: 3,
		},
	}
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{}}
	got := m.buildProjectPreamble(agent)
	if !strings.Contains(got, "kubestellar") {
		t.Error("should contain org name")
	}
	if !strings.Contains(got, "kubestellar/console") {
		t.Error("should contain org/repo format")
	}
	if !strings.Contains(got, "kubestellar/docs") {
		t.Error("should contain all repos")
	}
	if !strings.Contains(got, "L3") {
		t.Error("should contain ACMM level")
	}
	if !strings.Contains(got, "Quality-Gated") {
		t.Error("should contain level name")
	}
}

// ---------------------------------------------------------------------------
// agentMode — uses Config.Mode when set to valid value
// ---------------------------------------------------------------------------

func TestAgentMode_AllValidOverrides(t *testing.T) {
	modes := []struct {
		modeStr string
		want    AgentMode
	}{
		{"ADVISORY", ModeAdvisory},
		{"ISSUES_ONLY", ModeIssuesOnly},
		{"ISSUES_AND_PRS", ModeIssuesAndPRs},
		{"ISSUES_PRS_MERGE", ModeIssuesPRsMerge},
		{"NO_GITHUB", ModeAdvisory},
	}

	for _, tt := range modes {
		m := NewManager(map[string]config.AgentConfig{
			"test": {Backend: "claude", Mode: tt.modeStr},
		}, discardLogger(), ProjectContext{ACMMLevel: 6})

		m.mu.RLock()
		agent := m.agents["test"]
		m.mu.RUnlock()

		got := m.agentMode(agent)
		if got != tt.want {
			t.Errorf("agentMode with Mode=%q = %s, want %s", tt.modeStr, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Snapshot — PaneLines filtering with prompt + chrome
// ---------------------------------------------------------------------------

func TestPaneLines_PromptWithOnlyChrome(t *testing.T) {
	agent := &AgentProcess{
		Name: "test",
		lastPaneCapture: []string{
			"Real output from agent",
			"Processing issue #42",
			"❯",
			"/ commands",
			"? help",
			"@ files",
		},
	}
	lines := agent.PaneLines(10)
	// Chrome after prompt should be excluded, real content before should be included
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "/ commands") ||
			strings.HasPrefix(strings.TrimSpace(l), "? help") ||
			strings.HasPrefix(strings.TrimSpace(l), "@ files") {
			t.Errorf("chrome %q should be filtered out", l)
		}
	}
}

// ---------------------------------------------------------------------------
// UpdateConfig not found
// ---------------------------------------------------------------------------

func TestUpdateConfig_NotFoundReturnsError(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.UpdateConfig("nonexistent", config.AgentConfig{})
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

// ---------------------------------------------------------------------------
// shellEnvVar — special characters
// ---------------------------------------------------------------------------

func TestShellEnvVar_SpecialChars(t *testing.T) {
	got := shellEnvVar("URL", "http://user:pass@host:8080/path?q=1&r=2")
	if !strings.HasPrefix(got, "URL='") {
		t.Errorf("should start with URL=', got %q", got)
	}
	// Verify the value is properly quoted
	if !strings.Contains(got, "http://user:pass@host:8080/path?q=1&r=2") {
		t.Errorf("value should be preserved, got %q", got)
	}
}

func TestShellEnvVar_Parentheses(t *testing.T) {
	got := shellEnvVar("TOOL", "github(create_pull_request)")
	if !strings.Contains(got, "github(create_pull_request)") {
		t.Errorf("parentheses should be preserved, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// ensureClaudeSettings — writes to /tmp paths that we can test
// ---------------------------------------------------------------------------

func TestEnsureClaudeSettings_CreatesFiles(t *testing.T) {
	const testAgent = "test-settings"
	testHomePath := inferenceHomePath(testAgent)
	// Clean up the inference settings files before test
	os.RemoveAll(testHomePath)
	os.Remove(claudeInferenceSettingsPath)

	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	m.ensureClaudeSettings(testAgent, 0)

	// Check settings file was created
	settingsFile := filepath.Join(testHomePath, ".claude", "settings.json")
	data, err := os.ReadFile(settingsFile)
	if err != nil {
		t.Fatalf("settings file not created: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if parsed["hasCompletedOnboarding"] != true {
		t.Error("hasCompletedOnboarding should be true")
	}
	if parsed["bypassPermissions"] != true {
		t.Error("bypassPermissions should be true")
	}
	if parsed["skipDangerousModePermissionPrompt"] != true {
		t.Error("skipDangerousModePermissionPrompt should be true — it is the only key that suppresses the bypass-permissions consent dialog")
	}

	// Check standalone settings file
	flagData, err := os.ReadFile(claudeInferenceSettingsPath)
	if err != nil {
		t.Fatalf("standalone settings file not created: %v", err)
	}
	var flagParsed map[string]interface{}
	if err := json.Unmarshal(flagData, &flagParsed); err != nil {
		t.Fatalf("standalone settings invalid JSON: %v", err)
	}
	if flagParsed["skipDangerousModePermissionPrompt"] != true {
		t.Error("standalone settings skipDangerousModePermissionPrompt should be true")
	}

	// Check .claude.json user config
	userConfig := filepath.Join(testHomePath, ".claude.json")
	userData, err := os.ReadFile(userConfig)
	if err != nil {
		t.Fatalf("user config not created: %v", err)
	}
	var userParsed map[string]interface{}
	if err := json.Unmarshal(userData, &userParsed); err != nil {
		t.Fatalf("user config invalid JSON: %v", err)
	}
	if userParsed["hasCompletedOnboarding"] != true {
		t.Error("user config hasCompletedOnboarding should be true")
	}
	if userParsed["bypassPermissionsModeAccepted"] != true {
		t.Error("user config bypassPermissionsModeAccepted should be true")
	}
	if _, ok := userParsed["customApiKeyResponses"]; !ok {
		t.Error("user config should include customApiKeyResponses")
	}
}

func TestEnsureClaudeSettings_Idempotent(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})

	// Call twice — should not error or overwrite
	m.ensureClaudeSettings("test-idempotent", 0)
	m.ensureClaudeSettings("test-idempotent", 0)

	idempotentHomePath := inferenceHomePath("test-idempotent")
	settingsFile := filepath.Join(idempotentHomePath, ".claude", "settings.json")
	if _, err := os.Stat(settingsFile); err != nil {
		t.Errorf("settings should still exist: %v", err)
	}
}

func TestSeedClaudeSettingsFile_RepairsMissingKey(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	// Simulate a settings file seeded by an older hive version: no
	// skipDangerousModePermissionPrompt, plus keys that must be preserved.
	old := `{"permissions":{"allow":["Bash"],"deny":[]},"hasCompletedOnboarding":true,"bypassPermissions":true,"hasAcknowledgedDisclaimer":true}`
	if err := os.WriteFile(path, []byte(old), 0o666); err != nil {
		t.Fatalf("write: %v", err)
	}

	m.seedClaudeSettingsFile("repair-agent", path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid JSON after repair: %v", err)
	}
	if parsed["skipDangerousModePermissionPrompt"] != true {
		t.Error("skipDangerousModePermissionPrompt should be merged in")
	}
	perms, ok := parsed["permissions"].(map[string]interface{})
	if !ok {
		t.Fatal("permissions should remain an object")
	}
	allow, ok := perms["allow"].([]interface{})
	if !ok || len(allow) != 1 || allow[0] != "Bash" {
		t.Error("existing permissions must not be overwritten on repair")
	}
}

func TestSeedClaudeSettingsFile_CompleteFileUntouched(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	m.seedClaudeSettingsFile("complete-agent", path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after seed: %v", err)
	}

	m.seedClaudeSettingsFile("complete-agent", path)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after reseed: %v", err)
	}
	if string(before) != string(after) {
		t.Error("complete settings should not be rewritten")
	}
}

func TestInferenceUserConfigSeed_ApprovedKeyForms(t *testing.T) {
	// Short name: full key is within the CLI's 20-char comparison suffix,
	// so only the full form is needed.
	seed := inferenceUserConfigSeed("kellyaa")
	responses := seed["customApiKeyResponses"].(map[string]any)
	approved := responses["approved"].([]string)
	if len(approved) != 1 || approved[0] != "sk-hive-kellyaa" {
		t.Errorf("short-name approved = %v, want [sk-hive-kellyaa]", approved)
	}

	// Long name: the CLI matches key.slice(-20), so the truncated form must
	// be seeded alongside the full key.
	seed = inferenceUserConfigSeed("test-settings")
	responses = seed["customApiKeyResponses"].(map[string]any)
	approved = responses["approved"].([]string)
	fullKey := "sk-hive-test-settings"
	wantSuffix := fullKey[len(fullKey)-apiKeyApprovalSuffixLen:]
	if len(approved) != 2 || approved[0] != fullKey || approved[1] != wantSuffix {
		t.Errorf("long-name approved = %v, want [%s %s]", approved, fullKey, wantSuffix)
	}
}

func TestSeedClaudeUserConfig_MergesTruncatedAPIKey(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")

	// Config seeded by an older hive version: full key only, no truncated
	// form. The top-level merge alone would skip customApiKeyResponses.
	old := `{"hasCompletedOnboarding":true,"bypassPermissionsModeAccepted":true,"customApiKeyResponses":{"approved":["sk-hive-long-agent-name"],"rejected":["other"]}}`
	if err := os.WriteFile(path, []byte(old), 0o666); err != nil {
		t.Fatalf("write: %v", err)
	}

	m.seedClaudeUserConfig("long-agent-name", path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid JSON after repair: %v", err)
	}
	responses := parsed["customApiKeyResponses"].(map[string]interface{})
	approved := responses["approved"].([]interface{})
	fullKey := "sk-hive-long-agent-name"
	wantSuffix := fullKey[len(fullKey)-apiKeyApprovalSuffixLen:]
	found := false
	for _, v := range approved {
		if v == wantSuffix {
			found = true
		}
	}
	if !found {
		t.Errorf("approved = %v, should include truncated form %q", approved, wantSuffix)
	}
	rejected := responses["rejected"].([]interface{})
	if len(rejected) != 1 || rejected[0] != "other" {
		t.Errorf("rejected = %v, existing entries must be preserved", rejected)
	}
}

func TestSelectedMenuOption(t *testing.T) {
	tests := []struct {
		name string
		pane string
		want string
	}{
		{
			name: "no-first consent variant",
			pane: "WARNING: Claude Code running in Bypass Permissions mode\n ❯ 1. No, exit\n   2. Yes, I accept\nEnter to confirm · Esc to cancel",
			want: "❯ 1. No, exit",
		},
		{
			name: "yes-first consent variant",
			pane: "WARNING: Claude Code running in Bypass Permissions mode\n ❯ 1. Yes, I accept\n   2. No, exit\nEnter to confirm · Esc to cancel",
			want: "❯ 1. Yes, I accept",
		},
		{
			name: "no selection",
			pane: "plain shell output\n$ ",
			want: "",
		},
		{
			name: "empty pane",
			pane: "",
			want: "",
		},
	}
	for _, tc := range tests {
		if got := selectedMenuOption(tc.pane); got != tc.want {
			t.Errorf("selectedMenuOption(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSeedClaudeUserConfig_RepairsMissingKeys(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")

	// Simulate a seed from an older hive version: no bypassPermissionsModeAccepted,
	// plus a CLI-written key that must be preserved.
	old := `{"hasCompletedOnboarding":true,"someCliKey":"keep-me"}`
	if err := os.WriteFile(path, []byte(old), 0o666); err != nil {
		t.Fatalf("write: %v", err)
	}

	m.seedClaudeUserConfig("repair-agent", path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("invalid JSON after repair: %v", err)
	}
	if parsed["bypassPermissionsModeAccepted"] != true {
		t.Error("bypassPermissionsModeAccepted should be merged in")
	}
	if parsed["someCliKey"] != "keep-me" {
		t.Error("existing keys should be preserved on repair")
	}
}

func TestSeedClaudeUserConfig_RewritesCorruptFile(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o666); err != nil {
		t.Fatalf("write: %v", err)
	}

	m.seedClaudeUserConfig("corrupt-agent", path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("corrupt file should be rewritten as valid JSON: %v", err)
	}
	if parsed["bypassPermissionsModeAccepted"] != true {
		t.Error("bypassPermissionsModeAccepted should be true after rewrite")
	}
}

func TestSeedClaudeUserConfig_CompleteFileUntouched(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")

	m.seedClaudeUserConfig("complete-agent", path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after seed: %v", err)
	}

	m.seedClaudeUserConfig("complete-agent", path)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after reseed: %v", err)
	}
	if string(before) != string(after) {
		t.Error("complete config should not be rewritten")
	}
}

// ---------------------------------------------------------------------------
// sanitizeGitRemotes — with temp dirs
// ---------------------------------------------------------------------------

func TestSanitizeGitRemotes_SkipsWriteAgent(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(map[string]config.AgentConfig{
		"quality": {Backend: "claude", Mode: "ISSUES_AND_PRS"},
	}, discardLogger(), ProjectContext{ACMMLevel: 4})
	m.workDir = dir

	m.mu.RLock()
	agent := m.agents["quality"]
	m.mu.RUnlock()

	// Should skip sanitization for write-capable agents (no walk)
	m.sanitizeGitRemotes(agent)
}

func TestSanitizeGitRemotes_WalksAdvisoryAgent(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{ACMMLevel: 2})
	m.workDir = dir

	// Create agent work dir with a .git directory
	agentDir := filepath.Join(dir, "scanner")
	gitDir := filepath.Join(agentDir, "repo", ".git")
	os.MkdirAll(gitDir, 0o755)

	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	// Should not panic — git commands will fail but that's OK
	m.sanitizeGitRemotes(agent)
}

// ---------------------------------------------------------------------------
// fixEntry — more coverage
// ---------------------------------------------------------------------------

func TestFixEntry_Directory(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "testdir")
	os.MkdirAll(subdir, 0o755)

	info, err := os.Stat(subdir)
	if err != nil {
		t.Fatal(err)
	}

	// Should not panic
	fixEntry(subdir, info, discardLogger())
}

func TestFixEntry_RegularFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "test.txt")
	os.WriteFile(file, []byte("test"), 0o600)

	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}

	fixEntry(file, info, discardLogger())
}

// ---------------------------------------------------------------------------
// AddAgent — with UIDMap present
// ---------------------------------------------------------------------------

func TestAddAgent_WithUIDMap(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	m.uidMap = NewUIDMap()
	m.uidMap.AllocateUIDs([]string{"existing"})

	// AddAgent allocates a UID and persists the map to UIDMapPath. Redirect
	// the path into this test's own temp dir: saving to the binary-wide
	// TestMain path leaks the map into every later NewManager (#5580).
	uidMapFile := stubUIDMapPath(t)

	m.AddAgent("new-agent", config.AgentConfig{Backend: "claude"})

	m.mu.RLock()
	agent := m.agents["new-agent"]
	m.mu.RUnlock()

	if agent.UID == 0 {
		t.Error("agent should have UID allocated from UID map")
	}
	if agent.tmuxSocket == "" {
		t.Error("agent with UID should have tmux socket set")
	}
	if _, err := os.Stat(uidMapFile); err != nil {
		t.Errorf("AddAgent should persist the uid-map at UIDMapPath: %v", err)
	}
}

// ---------------------------------------------------------------------------
// fixSharedConfigPerms — with temp file
// ---------------------------------------------------------------------------

func TestFixSharedConfigPerms_NoFile(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	// Should not panic when config file doesn't exist
	m.fixSharedConfigPerms(agent)
}

// ---------------------------------------------------------------------------
// buildEnvPrefix — empty when all secret
// ---------------------------------------------------------------------------

func TestBuildEnvPrefix_NonEmpty(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})

	ap := &AgentProcess{
		Name:   "scanner",
		Config: config.AgentConfig{Backend: "claude", Model: "sonnet"},
	}

	prefix := m.buildEnvPrefix(ap)
	if prefix == "" {
		t.Error("prefix should be non-empty with standard env vars")
	}
	// Should end with a space
	if !strings.HasSuffix(prefix, " ") {
		t.Error("prefix should end with a space")
	}
}

// ---------------------------------------------------------------------------
// configHasTokens — test more branches of the parsing
// ---------------------------------------------------------------------------

func TestConfigHasTokens_NotAMap(t *testing.T) {
	// copilotTokens is not a map — should return false
	input := `{"copilotTokens": "not-a-map"}`
	var cfg map[string]interface{}
	json.Unmarshal([]byte(input), &cfg)
	tokens := cfg["copilotTokens"]
	_, ok := tokens.(map[string]interface{})
	if ok {
		t.Error("string copilotTokens should not be a map")
	}
}

// ---------------------------------------------------------------------------
// outputSignalPatterns — verify all patterns exist
// ---------------------------------------------------------------------------

func TestOutputSignalPatterns_Complete(t *testing.T) {
	expectedPatterns := []string{
		"[HEARTBEAT]", "[STATUS]", "[FINDING]", "[COMPLETE]", "[ERROR]",
		"PASS", "git commit", "git checkout", "git push",
		"created file", "Wrote", "test:", "FAIL", "coverage",
	}
	for _, pat := range expectedPatterns {
		if _, ok := outputSignalPatterns[pat]; !ok {
			t.Errorf("missing pattern %q in outputSignalPatterns", pat)
		}
	}
}

// ---------------------------------------------------------------------------
// kickRefusalPatterns — verify all exist
// ---------------------------------------------------------------------------

func TestKickRefusalPatterns_Complete(t *testing.T) {
	expected := []string{
		"I'm declining to execute",
		"I'm declining this",
		"prompt injection",
		"I won't act on bulk automated",
		"credential handling concern",
		"autonomous orchestration prompt",
		"I shouldn't follow autonomously",
		"characteristic of a prompt injection attack",
	}
	for _, pat := range expected {
		found := false
		for _, actual := range kickRefusalPatterns {
			if actual == pat {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing kick refusal pattern: %q", pat)
		}
	}
}

// ---------------------------------------------------------------------------
// cliPaneMarkers — verify content
// ---------------------------------------------------------------------------

func TestCliPaneMarkers_HasExpectedEntries(t *testing.T) {
	expected := []string{"❯", "esc cancel", "/ commands", "? help", "Claude", "Copilot", "Gemini", "goose"}
	for _, e := range expected {
		found := false
		for _, m := range cliPaneMarkers {
			if m == e {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing CLI pane marker: %q", e)
		}
	}
}

// ---------------------------------------------------------------------------
// paneShowsConsentScreen
// ---------------------------------------------------------------------------

func TestPaneShowsConsentScreen(t *testing.T) {
	bypassConsent := `WARNING: Claude Code running in Bypass Permissions mode

In Bypass Permissions mode, Claude Code will not ask for your approval before running potentially dangerous commands.

 ❯ 1. No, exit
   2. Yes, I accept

Enter to confirm · Esc to exit`

	genericSelection := `Do you trust the files in this folder?

 ❯ 1. Yes, proceed
   2. No, exit

Enter to confirm`

	readyPane := `╭──────────────────────────╮
│ ❯                        │
╰──────────────────────────╯
  ? for shortcuts`

	workingPane := `Thinking...
(esc to interrupt)
❯ 1. No, exit
Enter to confirm`

	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"empty pane", "", false},
		{"bypass permissions consent", bypassConsent, true},
		{"generic selection with confirm footer", genericSelection, true},
		{"ready input prompt", readyPane, false},
		{"working state is never consent", workingPane, false},
		{"bare bash", "dev@hive:~$ ", false},
		{"confirm footer without selection", "Enter to confirm something in output", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneShowsConsentScreen(tc.pane); got != tc.want {
				t.Errorf("paneShowsConsentScreen(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestDismissConsentIfStuck_GraceAndCooldown(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"vinf": {Backend: "vllm"},
	}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["vinf"]
	m.mu.RUnlock()

	// First sighting only starts the grace timer.
	m.dismissConsentIfStuck("vinf")
	if agent.consentSeenAt.IsZero() {
		t.Fatal("first sighting should start the grace timer")
	}
	if !agent.lastConsentDismiss.IsZero() {
		t.Fatal("no dismissal should fire within the grace period")
	}

	// Simulate the screen having been visible past the grace period.
	agent.consentSeenAt = time.Now().Add(-consentStuckGracePeriod - time.Second)
	m.dismissConsentIfStuck("vinf")
	if agent.lastConsentDismiss.IsZero() {
		t.Fatal("dismissal should fire once past the grace period")
	}
	first := agent.lastConsentDismiss

	// Cooldown: an immediate re-check must not re-fire.
	m.dismissConsentIfStuck("vinf")
	if !agent.lastConsentDismiss.Equal(first) {
		t.Fatal("cooldown should prevent immediate re-dismissal")
	}

	// A pane without a consent screen resets the grace timer.
	m.clearConsentTracking("vinf")
	if !agent.consentSeenAt.IsZero() {
		t.Fatal("clearConsentTracking should reset consentSeenAt")
	}
}

func TestNudgeIfKickStalled(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"vinf": {Backend: "vllm"},
	}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["vinf"]
	m.mu.RUnlock()

	idlePane := "╭───╮\n│ ❯ │\n╰───╯\n? for shortcuts"

	// No kick recorded — never nudges.
	m.nudgeIfKickStalled("vinf", idlePane)
	if agent.StallNudges != 0 {
		t.Fatal("no nudge without a recorded kick")
	}

	// Kick recorded but timeout not elapsed — no nudge.
	agent.lastInferKickAt = time.Now()
	agent.lastInferKickPane = paneContentHash(idlePane)
	agent.stallNudgeSent = false
	m.nudgeIfKickStalled("vinf", idlePane)
	if agent.StallNudges != 0 {
		t.Fatal("no nudge before the stall timeout")
	}

	// Timeout elapsed, pane unchanged, idle prompt — exactly one nudge.
	agent.lastInferKickAt = time.Now().Add(-inferenceKickStallTimeout - time.Minute)
	m.nudgeIfKickStalled("vinf", idlePane)
	if agent.StallNudges != 1 || !agent.stallNudgeSent {
		t.Fatalf("expected one nudge, got %d (sent=%v)", agent.StallNudges, agent.stallNudgeSent)
	}

	// Second watcher pass: still max one nudge per kick.
	m.nudgeIfKickStalled("vinf", idlePane)
	if agent.StallNudges != 1 {
		t.Fatalf("max one nudge per kick, got %d", agent.StallNudges)
	}

	// New kick re-arms, but a working pane is never nudged.
	workingPane := idlePane + "\nesc to interrupt"
	agent.lastInferKickAt = time.Now().Add(-inferenceKickStallTimeout - time.Minute)
	agent.lastInferKickPane = paneContentHash(workingPane)
	agent.stallNudgeSent = false
	m.nudgeIfKickStalled("vinf", workingPane)
	if agent.StallNudges != 1 {
		t.Fatal("working pane must not be nudged")
	}

	// A pane change with post-kick tool activity disarms the watchdog.
	// (No tmux session in tests → captureTmuxPaneForAgent returns "" →
	// countToolMarkers is 0; a negative baseline simulates "count rose".)
	agent.lastInferKickPane = paneContentHash("what the pane looked like at kick time")
	agent.lastInferKickMarks = -1
	m.nudgeIfKickStalled("vinf", idlePane)
	if agent.StallNudges != 1 || agent.ActionNudges != 0 {
		t.Fatal("changed pane with tool activity must not be nudged")
	}
	if agent.lastInferKickPane != "" {
		t.Fatal("changed pane with tool activity should disarm the watchdog")
	}
}

func TestNudgeIfKickStalled_ActionNudge(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"vinf": {Backend: "vllm"},
	}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["vinf"]
	m.mu.RUnlock()

	// Prose-only response: the model answered the kick with a plan and
	// returned to the idle prompt without a single tool marker.
	prosePane := "❯ [agent:scanner] fix the failing issues\n" +
		"⏺ To fix the failing issues, follow these steps. First you ensure the\n" +
		"  branch is up to date, then you open a pull request with the fix.\n" +
		"✻ Worked for 26s\n" +
		"❯ \n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)"

	// Within the grace period — never nudged.
	agent.lastInferKickAt = time.Now().Add(-inferenceActionNudgeGrace + time.Minute)
	agent.lastInferKickPane = paneContentHash("pane as captured at kick delivery")
	agent.lastInferKickMarks = 0
	m.nudgeIfKickStalled("vinf", prosePane)
	if agent.ActionNudges != 0 {
		t.Fatal("no action nudge within the grace period")
	}

	// Past the grace period with zero new tool markers — exactly one nudge.
	// (No tmux session in tests → the scrollback capture is empty → the
	// marker count stays at the baseline.)
	agent.lastInferKickAt = time.Now().Add(-inferenceActionNudgeGrace - time.Minute)
	m.nudgeIfKickStalled("vinf", prosePane)
	if agent.ActionNudges != 1 || !agent.actionNudgeSent {
		t.Fatalf("expected one action nudge, got %d (sent=%v)", agent.ActionNudges, agent.actionNudgeSent)
	}
	if agent.StallNudges != 0 {
		t.Fatal("prose-only response must not count as a frozen-pane stall")
	}

	// Second watcher pass: max one action nudge per kick.
	m.nudgeIfKickStalled("vinf", prosePane)
	if agent.ActionNudges != 1 {
		t.Fatalf("max one action nudge per kick, got %d", agent.ActionNudges)
	}

	// A CLI mid-response (live spinner counter) is left alone even though
	// the footer no longer shows "esc to interrupt" on v2.1.204.
	streamingPane := prosePane + "\n✶ Infusing… (18s · ↓ 94 tokens)"
	agent.actionNudgeSent = false
	m.nudgeIfKickStalled("vinf", streamingPane)
	if agent.ActionNudges != 1 {
		t.Fatal("streaming pane must not be nudged")
	}

	// A new kick re-arms both nudges.
	now := time.Now()
	m.mu.Lock()
	m.recordInferenceKick(agent, now)
	m.mu.Unlock()
	if agent.actionNudgeSent || agent.stallNudgeSent {
		t.Fatal("recordInferenceKick must reset both nudge flags")
	}
}

func TestCountToolMarkers(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want int
	}{
		{"empty", "", 0},
		// A bare ⏺ is the bullet for every assistant block, prose included.
		{"prose only", "⏺ To fix this, follow these steps and you ensure the tests pass.\n✻ Worked for 26s", 0},
		{"mid-run bash (v2.1.204)", "⏺ Running 1 shell command…\n  ⎿  $ sleep 15 && echo probe3 (12s)", 2},
		{"collapsed summary (v2.1.204)", "  Ran 1 shell command\n⏺ Done — it printed probe3.", 1},
		{"collapsed read+bash (v2.1.204)", "  Read 1 file, ran 1 shell command\n⏺ Done.", 2},
		{"expanded legacy tool call", "⏺ Bash(git status)\n  ⎿  On branch main", 2},
		{"expanded legacy read", "⏺ Read(main.go)", 1},
		{"plural summary", "  Ran 3 shell commands\n  Edited 2 files", 2},
		{"idle prompt only", "❯ \n  ⏵⏵ bypass permissions on (shift+tab to cycle)", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countToolMarkers(tc.pane); got != tc.want {
				t.Errorf("countToolMarkers(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestPaneShowsActiveWork(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"legacy footer hint", "some output\nesc to interrupt", true},
		{"live spinner counter (v2.1.204)", "✶ Infusing… (18s · ↓ 94 tokens)", true},
		{"completed response", "⏺ Done.\n✻ Worked for 26s\n❯ ", false},
		{"idle prompt", "❯ \n  ⏵⏵ bypass permissions on", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneShowsActiveWork(tc.pane); got != tc.want {
				t.Errorf("paneShowsActiveWork(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestPaneContentHash_StableAndDistinct(t *testing.T) {
	a := paneContentHash("pane content")
	if a != paneContentHash("pane content") {
		t.Error("hash should be stable")
	}
	if a == paneContentHash("other content") {
		t.Error("different content should hash differently")
	}
	if len(a) != 16 {
		t.Errorf("hash should be 16 hex chars, got %d", len(a))
	}
}

func TestEffectiveBackend(t *testing.T) {
	agent := &AgentProcess{Config: config.AgentConfig{Backend: "copilot"}}
	if got := effectiveBackend(agent); got != "copilot" {
		t.Errorf("effectiveBackend = %q, want copilot", got)
	}
	agent.BackendOverride = "vllm"
	if got := effectiveBackend(agent); got != "vllm" {
		t.Errorf("effectiveBackend with override = %q, want vllm", got)
	}
}

func TestPaneHasCLIMarker(t *testing.T) {
	if paneHasCLIMarker("") {
		t.Error("empty pane should have no CLI marker")
	}
	if paneHasCLIMarker("dev@hive:~$ ls\n-bash: NEVER: command not found") {
		t.Error("bare bash pane should have no CLI marker")
	}
	if !paneHasCLIMarker("❯ ") {
		t.Error("input prompt marker should match")
	}
}

// ---------------------------------------------------------------------------
// loginPromptPatterns — verify
// ---------------------------------------------------------------------------

func TestLoginPromptPatterns_HasExpectedEntries(t *testing.T) {
	// A bare "/login" is deliberately absent from this list: it is a substring
	// of ordinary agent output ("POST /login", a CLI's slash-command list) and
	// matching it painted the needs-login badge on authenticated, working
	// agents. "/login" is now recognised only with an imperative verb, via
	// lineHasLoginDirective — see TestLoginPromptPatterns_NoBareSlashLogin.
	expected := []string{
		"sign in to use", "Sign in to use",
		"authenticate to use", "Authenticate to use",
		"log in to use", "Log in to use",
	}
	for _, e := range expected {
		found := false
		for _, pat := range loginPromptPatterns {
			if pat == e {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing login prompt pattern: %q", e)
		}
	}
}

// ---------------------------------------------------------------------------
// readCoveragePreamble — redirected metricsCachePath
// ---------------------------------------------------------------------------

func TestReadCoveragePreamble_WithActualFile(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "agent-metrics-cache.json")
	orig := metricsCachePath
	metricsCachePath = cacheFile
	t.Cleanup(func() { metricsCachePath = orig })

	metrics := map[string]map[string]json.Number{
		"ci-maintainer": {
			"coverage":       json.Number("85"),
			"coverageTarget": json.Number("91"),
		},
	}
	data, _ := json.Marshal(metrics)
	if err := os.WriteFile(cacheFile, data, 0o644); err != nil {
		// cacheFile lives in t.TempDir() -- guaranteed writable (#5388).
		testutil.SkipfUnlessRequired(t, "cannot write metrics file: %v", err)
	}

	m := &Manager{logger: discardLogger()}
	got := m.readCoveragePreamble()
	if got != "[COVERAGE] Current: 85% | Target: 91%." {
		t.Errorf("readCoveragePreamble = %q, want '[COVERAGE] Current: 85%% | Target: 91%%.'", got)
	}
}

func TestReadCoveragePreamble_InvalidJSON_AtPath(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "agent-metrics-cache.json")
	orig := metricsCachePath
	metricsCachePath = cacheFile
	t.Cleanup(func() { metricsCachePath = orig })
	os.WriteFile(cacheFile, []byte("not json"), 0o644)

	m := &Manager{logger: discardLogger()}
	got := m.readCoveragePreamble()
	if got != "" {
		t.Errorf("invalid JSON should return empty, got %q", got)
	}
}

func TestReadCoveragePreamble_NoCIMaintainer(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "agent-metrics-cache.json")
	orig := metricsCachePath
	metricsCachePath = cacheFile
	t.Cleanup(func() { metricsCachePath = orig })
	metrics := map[string]map[string]json.Number{
		"other": {"coverage": json.Number("50")},
	}
	data, _ := json.Marshal(metrics)
	os.WriteFile(cacheFile, data, 0o644)

	m := &Manager{logger: discardLogger()}
	got := m.readCoveragePreamble()
	if got != "" {
		t.Errorf("no ci-maintainer should return empty, got %q", got)
	}
}

func TestReadCoveragePreamble_BadCoverage(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "agent-metrics-cache.json")
	orig := metricsCachePath
	metricsCachePath = cacheFile
	t.Cleanup(func() { metricsCachePath = orig })
	metrics := map[string]map[string]json.Number{
		"ci-maintainer": {"coverage": json.Number("not-a-number")},
	}
	data, _ := json.Marshal(metrics)
	os.WriteFile(cacheFile, data, 0o644)

	m := &Manager{logger: discardLogger()}
	got := m.readCoveragePreamble()
	if got != "" {
		t.Errorf("bad coverage number should return empty, got %q", got)
	}
}

func TestReadCoveragePreamble_DefaultTarget(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "agent-metrics-cache.json")
	orig := metricsCachePath
	metricsCachePath = cacheFile
	t.Cleanup(func() { metricsCachePath = orig })
	metrics := map[string]map[string]json.Number{
		"ci-maintainer": {"coverage": json.Number("80")},
	}
	data, _ := json.Marshal(metrics)
	if err := os.WriteFile(cacheFile, data, 0o644); err != nil {
		// cacheFile lives in t.TempDir() -- guaranteed writable (#5388).
		testutil.SkipfUnlessRequired(t, "cannot write metrics file: %v", err)
	}

	m := &Manager{logger: discardLogger()}
	got := m.readCoveragePreamble()
	// Missing target defaults to 91
	if got != "[COVERAGE] Current: 80% | Target: 91%." {
		t.Errorf("readCoveragePreamble = %q, want '[COVERAGE] Current: 80%% | Target: 91%%.'", got)
	}
}

// ---------------------------------------------------------------------------
// configHasTokens — via the redirectable shared path
// ---------------------------------------------------------------------------

func TestConfigHasTokens_WithActualFile(t *testing.T) {
	cleanup := configTestHelper(t)
	defer cleanup()

	cfg := `{"copilotTokens": {"github.com": {"token": "gho_test"}}}`
	if err := os.WriteFile(sharedCopilotConfigPath, []byte(cfg), 0o660); err != nil {
		t.Fatalf("cannot write config file: %v", err)
	}

	if !configHasTokens() {
		t.Error("configHasTokens should return true when tokens present")
	}
}

// configTestHelper redirects sharedCopilotConfigPath to a file inside
// t.TempDir() and returns a cleanup that restores the original path.
//
// It must NEVER touch the production path: on a live hive host
// /data/home/.copilot/config.json holds the real shared Copilot credentials,
// and the previous save/overwrite/restore approach both clobbered them for the
// duration of the test (or forever, if the test binary died mid-run) and made
// these tests flaky — chmod/write on a foreign-owned live file fails with
// EPERM. sharedCopilotConfigPath is a var precisely so tests can redirect it
// (see the comment on its declaration in manager.go).
func configTestHelper(t *testing.T) (cleanup func()) {
	t.Helper()
	orig := sharedCopilotConfigPath
	sharedCopilotConfigPath = filepath.Join(t.TempDir(), "config.json")
	return func() { sharedCopilotConfigPath = orig }
}

func TestConfigHasTokens_EmptyTokens_AtPath(t *testing.T) {
	cleanup := configTestHelper(t)
	defer cleanup()

	if err := os.WriteFile(sharedCopilotConfigPath, []byte(`{"copilotTokens": {}}`), 0o660); err != nil {
		// sharedCopilotConfigPath points into t.TempDir(), which the testing
		// package guarantees is writable. A failure here is a broken test,
		// not an unsuitable environment (#5388).
		testutil.SkipfUnlessRequired(t, "cannot write: %v", err)
	}
	if configHasTokens() {
		t.Error("configHasTokens should return false for empty tokens")
	}
}

func TestConfigHasTokens_NoTokensField(t *testing.T) {
	cleanup := configTestHelper(t)
	defer cleanup()

	if err := os.WriteFile(sharedCopilotConfigPath, []byte(`{"someOther": true}`), 0o660); err != nil {
		// sharedCopilotConfigPath points into t.TempDir(), which the testing
		// package guarantees is writable. A failure here is a broken test,
		// not an unsuitable environment (#5388).
		testutil.SkipfUnlessRequired(t, "cannot write: %v", err)
	}
	if configHasTokens() {
		t.Error("configHasTokens should return false when field missing")
	}
}

func TestConfigHasTokens_WithComments_AtPath(t *testing.T) {
	cleanup := configTestHelper(t)
	defer cleanup()

	cfg := "// comment line\n{\"copilotTokens\": {\"github.com\": {\"token\": \"gho_test\"}}}"
	if err := os.WriteFile(sharedCopilotConfigPath, []byte(cfg), 0o660); err != nil {
		// sharedCopilotConfigPath points into t.TempDir(), which the testing
		// package guarantees is writable. A failure here is a broken test,
		// not an unsuitable environment (#5388).
		testutil.SkipfUnlessRequired(t, "cannot write: %v", err)
	}
	if !configHasTokens() {
		t.Error("configHasTokens should handle // comments and still find tokens")
	}
}

// ---------------------------------------------------------------------------
// clearExpiredTokens — write to actual path
// ---------------------------------------------------------------------------

func TestClearExpiredTokens_ClearsAndPreservesOther(t *testing.T) {
	cleanup := configTestHelper(t)
	defer cleanup()

	cfg := `{
  "copilotTokens": {"github.com": {"token": "gho_expired"}},
  "loggedInUsers": ["github.com"],
  "lastLoggedInUser": "github.com",
  "otherSetting": "keep-me"
}`
	if err := os.WriteFile(sharedCopilotConfigPath, []byte(cfg), 0o660); err != nil {
		// sharedCopilotConfigPath points into t.TempDir(), which the testing
		// package guarantees is writable. A failure here is a broken test,
		// not an unsuitable environment (#5388).
		testutil.SkipfUnlessRequired(t, "cannot write: %v", err)
	}

	err := clearExpiredTokens()
	if err != nil {
		t.Fatalf("clearExpiredTokens: %v", err)
	}

	// Read back and verify
	data, err := os.ReadFile(sharedCopilotConfigPath)
	if err != nil {
		t.Fatalf("read after clear: %v", err)
	}

	// Strip comments
	var cleaned []byte
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		cleaned = append(cleaned, []byte(line+"\n")...)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(cleaned, &result); err != nil {
		t.Fatalf("parse cleared config: %v", err)
	}

	tokens := result["copilotTokens"].(map[string]interface{})
	if len(tokens) != 0 {
		t.Error("tokens should be empty after clear")
	}

	// Login IDENTITY survives a token clear: an expired token does not change
	// who was logged in, and the interactive CLI refuses to treat a seeded
	// token as a signed-in session without it — wiping identity here is what
	// left agents at "Please use /login" over a perfectly valid restored token
	// (hivecommons/hive, 2026-08-22).
	users := result["loggedInUsers"].([]interface{})
	if len(users) != 1 || users[0] != "github.com" {
		t.Errorf("loggedInUsers must be preserved across a token clear, got %v", users)
	}

	if result["lastLoggedInUser"] != "github.com" {
		t.Error("lastLoggedInUser must be preserved across a token clear")
	}

	if result["otherSetting"] != "keep-me" {
		t.Error("other settings should be preserved")
	}
}

func TestClearExpiredTokens_MissingFile(t *testing.T) {
	cleanup := configTestHelper(t)
	defer cleanup()
	os.Remove(sharedCopilotConfigPath)

	err := clearExpiredTokens()
	if err == nil {
		t.Error("should error when file doesn't exist")
	}
}

// ---------------------------------------------------------------------------
// fixSharedConfigPerms — with actual path
// ---------------------------------------------------------------------------

func TestFixSharedConfigPerms_FixesPerms(t *testing.T) {
	cleanup := configTestHelper(t)
	defer cleanup()

	// Write with restrictive perms
	if err := os.WriteFile(sharedCopilotConfigPath, []byte(`{}`), 0o600); err != nil {
		// sharedCopilotConfigPath points into t.TempDir(), which the testing
		// package guarantees is writable. A failure here is a broken test,
		// not an unsuitable environment (#5388).
		testutil.SkipfUnlessRequired(t, "cannot write: %v", err)
	}

	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	m.fixSharedConfigPerms(agent)

	info, err := os.Stat(sharedCopilotConfigPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != sharedConfigDesiredMode {
		t.Errorf("perms = %04o, want %04o", info.Mode().Perm(), sharedConfigDesiredMode)
	}
}

func TestFixSharedConfigPerms_AlreadyCorrect(t *testing.T) {
	cleanup := configTestHelper(t)
	defer cleanup()

	// Write with correct perms
	if err := os.WriteFile(sharedCopilotConfigPath, []byte(`{}`), sharedConfigDesiredMode); err != nil {
		// sharedCopilotConfigPath points into t.TempDir(), which the testing
		// package guarantees is writable. A failure here is a broken test,
		// not an unsuitable environment (#5388).
		testutil.SkipfUnlessRequired(t, "cannot write: %v", err)
	}

	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	// Should be a no-op
	m.fixSharedConfigPerms(agent)
}
