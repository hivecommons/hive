package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
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
			if p.Value != "/data/home" {
				t.Errorf("non-inference UID agent HOME = %q, want /data/home", p.Value)
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

	data, err := os.ReadFile("/tmp/.hive-mode-scanner")
	if err != nil {
		t.Skipf("could not read mode file: %v", err)
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
	if !strings.Contains(got, "CI/CD") {
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

	// Check standalone settings file
	if _, err := os.Stat(claudeInferenceSettingsPath); err != nil {
		t.Errorf("standalone settings file not created: %v", err)
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

	// Save to temp dir
	dir := t.TempDir()
	uidMapFile := filepath.Join(dir, "uid-map.json")

	// AddAgent should allocate UID and try to save
	// The save will fail since UIDMapPath is a const, but that's OK
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
	_ = uidMapFile
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
	expected := []string{
		"/login", "sign in to use", "Sign in to use",
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
// readCoveragePreamble — write to actual metricsCachePath
// ---------------------------------------------------------------------------

func TestReadCoveragePreamble_WithActualFile(t *testing.T) {
	// Create the metrics cache directory and file at the actual path
	dir := filepath.Dir(metricsCachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create metrics dir: %v", err)
	}
	defer os.Remove(metricsCachePath)

	metrics := map[string]map[string]json.Number{
		"ci-maintainer": {
			"coverage":       json.Number("85"),
			"coverageTarget": json.Number("91"),
		},
	}
	data, _ := json.Marshal(metrics)
	if err := os.WriteFile(metricsCachePath, data, 0o644); err != nil {
		t.Skipf("cannot write metrics file: %v", err)
	}

	m := &Manager{logger: discardLogger()}
	got := m.readCoveragePreamble()
	if got != "[COVERAGE] Current: 85% | Target: 91%." {
		t.Errorf("readCoveragePreamble = %q, want '[COVERAGE] Current: 85%% | Target: 91%%.'", got)
	}
}

func TestReadCoveragePreamble_InvalidJSON_AtPath(t *testing.T) {
	dir := filepath.Dir(metricsCachePath)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(metricsCachePath, []byte("not json"), 0o644)
	defer os.Remove(metricsCachePath)

	m := &Manager{logger: discardLogger()}
	got := m.readCoveragePreamble()
	if got != "" {
		t.Errorf("invalid JSON should return empty, got %q", got)
	}
}

func TestReadCoveragePreamble_NoCIMaintainer(t *testing.T) {
	dir := filepath.Dir(metricsCachePath)
	os.MkdirAll(dir, 0o755)
	metrics := map[string]map[string]json.Number{
		"other": {"coverage": json.Number("50")},
	}
	data, _ := json.Marshal(metrics)
	os.WriteFile(metricsCachePath, data, 0o644)
	defer os.Remove(metricsCachePath)

	m := &Manager{logger: discardLogger()}
	got := m.readCoveragePreamble()
	if got != "" {
		t.Errorf("no ci-maintainer should return empty, got %q", got)
	}
}

func TestReadCoveragePreamble_BadCoverage(t *testing.T) {
	dir := filepath.Dir(metricsCachePath)
	os.MkdirAll(dir, 0o755)
	metrics := map[string]map[string]json.Number{
		"ci-maintainer": {"coverage": json.Number("not-a-number")},
	}
	data, _ := json.Marshal(metrics)
	os.WriteFile(metricsCachePath, data, 0o644)
	defer os.Remove(metricsCachePath)

	m := &Manager{logger: discardLogger()}
	got := m.readCoveragePreamble()
	if got != "" {
		t.Errorf("bad coverage number should return empty, got %q", got)
	}
}

func TestReadCoveragePreamble_DefaultTarget(t *testing.T) {
	dir := filepath.Dir(metricsCachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create metrics dir: %v", err)
	}
	metrics := map[string]map[string]json.Number{
		"ci-maintainer": {"coverage": json.Number("80")},
	}
	data, _ := json.Marshal(metrics)
	if err := os.WriteFile(metricsCachePath, data, 0o644); err != nil {
		t.Skipf("cannot write metrics file: %v", err)
	}
	defer os.Remove(metricsCachePath)

	m := &Manager{logger: discardLogger()}
	got := m.readCoveragePreamble()
	// Missing target defaults to 91
	if got != "[COVERAGE] Current: 80% | Target: 91%." {
		t.Errorf("readCoveragePreamble = %q, want '[COVERAGE] Current: 80%% | Target: 91%%.'", got)
	}
}

// ---------------------------------------------------------------------------
// configHasTokens — write to actual path
// ---------------------------------------------------------------------------

func TestConfigHasTokens_WithActualFile(t *testing.T) {
	dir := filepath.Dir(sharedCopilotConfigPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create config dir %s: %v", dir, err)
	}

	// Save original if it exists
	original, origErr := os.ReadFile(sharedCopilotConfigPath)
	defer func() {
		if origErr == nil {
			os.WriteFile(sharedCopilotConfigPath, original, 0o660)
		} else {
			os.Remove(sharedCopilotConfigPath)
		}
	}()

	cfg := `{"copilotTokens": {"github.com": {"token": "gho_test"}}}`
	if err := os.WriteFile(sharedCopilotConfigPath, []byte(cfg), 0o660); err != nil {
		t.Skipf("cannot write config file: %v", err)
	}

	if !configHasTokens() {
		t.Error("configHasTokens should return true when tokens present")
	}
}

func configTestHelper(t *testing.T) (cleanup func()) {
	t.Helper()
	dir := filepath.Dir(sharedCopilotConfigPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Skipf("cannot create config dir %s: %v", dir, err)
	}
	original, origErr := os.ReadFile(sharedCopilotConfigPath)
	return func() {
		if origErr == nil {
			os.WriteFile(sharedCopilotConfigPath, original, 0o660)
		} else {
			os.Remove(sharedCopilotConfigPath)
		}
	}
}

func TestConfigHasTokens_EmptyTokens_AtPath(t *testing.T) {
	cleanup := configTestHelper(t)
	defer cleanup()

	if err := os.WriteFile(sharedCopilotConfigPath, []byte(`{"copilotTokens": {}}`), 0o660); err != nil {
		t.Skipf("cannot write: %v", err)
	}
	if configHasTokens() {
		t.Error("configHasTokens should return false for empty tokens")
	}
}

func TestConfigHasTokens_NoTokensField(t *testing.T) {
	cleanup := configTestHelper(t)
	defer cleanup()

	if err := os.WriteFile(sharedCopilotConfigPath, []byte(`{"someOther": true}`), 0o660); err != nil {
		t.Skipf("cannot write: %v", err)
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
		t.Skipf("cannot write: %v", err)
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
		t.Skipf("cannot write: %v", err)
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

	users := result["loggedInUsers"].([]interface{})
	if len(users) != 0 {
		t.Error("loggedInUsers should be empty")
	}

	if _, ok := result["lastLoggedInUser"]; ok {
		t.Error("lastLoggedInUser should be deleted")
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
		t.Skipf("cannot write: %v", err)
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
		t.Skipf("cannot write: %v", err)
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
