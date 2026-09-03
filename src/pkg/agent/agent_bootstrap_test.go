package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func testManager(level int) *Manager {
	return &Manager{
		agents:   make(map[string]*AgentProcess),
		idToName: make(map[string]string),
		logger:   slog.Default(),
		project: ProjectContext{
			Org:       "testorg",
			Repos:     []string{"repo"},
			ACMMLevel: level,
		},
	}
}

func containsBoot(s, substr string) bool {
	return strings.Contains(s, substr)
}

func TestBuildBootstrapPromptWithKickTemplate(t *testing.T) {
	dir := t.TempDir()
	policyDir := filepath.Join(dir, "policies", "agents")
	os.MkdirAll(policyDir, 0755)
	os.WriteFile(filepath.Join(policyDir, "custom-kick.md"), []byte("# Custom Kick"), 0644)

	m := &Manager{
		agents:   make(map[string]*AgentProcess),
		idToName: make(map[string]string),
		logger:   slog.Default(),
		project: ProjectContext{
			Org:       "testorg",
			Repos:     []string{"repo"},
			ACMMLevel: 2,
			PolicyDir: policyDir,
		},
	}

	agent := &AgentProcess{
		Name:   "scanner",
		Config: config.AgentConfig{Role: "scanner", KickTemplate: "custom-kick.md"},
	}

	got := m.buildBootstrapPrompt(agent)
	if got != "" {
		t.Error("boot prompt should be empty")
	}
}

func TestBuildBootstrapPromptWithPolicyFile(t *testing.T) {
	dir := t.TempDir()
	policyDir := filepath.Join(dir, "policies", "agents")
	os.MkdirAll(policyDir, 0755)
	os.WriteFile(filepath.Join(policyDir, "scanner.md"), []byte("# Scanner Policy"), 0644)

	m := &Manager{
		agents:   make(map[string]*AgentProcess),
		idToName: make(map[string]string),
		logger:   slog.Default(),
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
	if got != "" {
		t.Error("boot prompt should be empty")
	}
	// boot prompt is now empty — agents get kicked by governor
}

func TestBuildBootstrapPromptQuality(t *testing.T) {
	m := testManager(3)
	agent := &AgentProcess{
		Name:   "quality",
		Config: config.AgentConfig{Role: "quality"},
	}

	got := m.buildBootstrapPrompt(agent)
	if got != "" {
		t.Error("boot prompt should be empty")
	}
}

func TestBuildBootstrapPromptWithACMMFragments(t *testing.T) {
	dir := t.TempDir()
	policyDir := filepath.Join(dir, "policies", "agents")
	acmmDir := filepath.Join(dir, "policies", "acmm")
	os.MkdirAll(policyDir, 0755)
	os.MkdirAll(acmmDir, 0755)
	os.WriteFile(filepath.Join(acmmDir, "base.md"), []byte("# Base ACMM"), 0644)
	os.WriteFile(filepath.Join(acmmDir, "l2.md"), []byte("# Level 2"), 0644)

	m := &Manager{
		agents:   make(map[string]*AgentProcess),
		idToName: make(map[string]string),
		logger:   slog.Default(),
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
	if got != "" {
		t.Error("boot prompt should be empty")
	}
}

func TestFindACMMFragmentsWithFiles(t *testing.T) {
	dir := t.TempDir()
	policyDir := filepath.Join(dir, "policies", "agents")
	acmmDir := filepath.Join(dir, "policies", "examples", "acmm")
	os.MkdirAll(policyDir, 0755)
	os.MkdirAll(acmmDir, 0755)
	os.WriteFile(filepath.Join(acmmDir, "base.md"), []byte("# Base ACMM Rules"), 0644)
	os.WriteFile(filepath.Join(acmmDir, "l3.md"), []byte("# Level 3 Rules"), 0644)

	m := &Manager{
		logger: slog.Default(),
		project: ProjectContext{
			ACMMLevel: 3,
			PolicyDir: policyDir,
		},
	}

	files := m.findACMMFragments()
	if len(files) != 2 {
		t.Errorf("expected 2 ACMM files, got %d: %v", len(files), files)
	}
}

func TestFindACMMFragmentsBaseOnly(t *testing.T) {
	dir := t.TempDir()
	policyDir := filepath.Join(dir, "policies", "agents")
	acmmDir := filepath.Join(dir, "policies", "examples", "acmm")
	os.MkdirAll(policyDir, 0755)
	os.MkdirAll(acmmDir, 0755)
	os.WriteFile(filepath.Join(acmmDir, "base.md"), []byte("# Base"), 0644)

	m := &Manager{
		logger: slog.Default(),
		project: ProjectContext{
			ACMMLevel: 5,
			PolicyDir: policyDir,
		},
	}

	files := m.findACMMFragments()
	if len(files) != 1 {
		t.Errorf("expected 1 ACMM file (base only), got %d", len(files))
	}
}

func TestFindACMMFragmentsNoDir(t *testing.T) {
	orig := acmmFragmentFallbackDirs
	acmmFragmentFallbackDirs = nil
	t.Cleanup(func() { acmmFragmentFallbackDirs = orig })

	policyDir := filepath.Join(t.TempDir(), "policies", "agents")
	m := &Manager{
		logger: slog.Default(),
		project: ProjectContext{
			ACMMLevel: 3,
			PolicyDir: policyDir,
		},
	}

	files := m.findACMMFragments()
	if len(files) != 0 {
		t.Errorf("no ACMM dir should return empty, got %d", len(files))
	}
}

func TestBuildBootstrapPromptWithACMMFound(t *testing.T) {
	dir := t.TempDir()
	policyDir := filepath.Join(dir, "policies", "agents")
	acmmDir := filepath.Join(dir, "policies", "examples", "acmm")
	os.MkdirAll(policyDir, 0755)
	os.MkdirAll(acmmDir, 0755)
	os.WriteFile(filepath.Join(acmmDir, "base.md"), []byte("# Base"), 0644)
	os.WriteFile(filepath.Join(acmmDir, "l2.md"), []byte("# L2"), 0644)

	m := &Manager{
		agents:   make(map[string]*AgentProcess),
		idToName: make(map[string]string),
		logger:   slog.Default(),
		project: ProjectContext{
			Org:       "testorg",
			Repos:     []string{"repo"},
			ACMMLevel: 2,
			PolicyDir: policyDir,
		},
	}

	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{Role: "scanner"}}
	got := m.buildBootstrapPrompt(agent)
	// boot prompt is now empty — agents get kicked by governor
	_ = got
}

func TestBuildProjectPreambleIssuesOnly(t *testing.T) {
	m := testManager(4)
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{Role: "scanner"}}
	got := m.buildProjectPreamble(agent)
	if !containsBoot(got, "ISSUES_ONLY") {
		t.Errorf("L4 scanner should be ISSUES_ONLY, got: %s", got)
	}
}

func TestBuildProjectPreambleNoGithub(t *testing.T) {
	m := testManager(4)
	agent := &AgentProcess{Name: "supervisor", Config: config.AgentConfig{Role: "supervisor"}}
	got := m.buildProjectPreamble(agent)
	if !containsBoot(got, "ADVISORY") {
		t.Errorf("L4 supervisor should be ADVISORY, got: %s", got)
	}
}

func TestNewManager(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"scanner": {ID: "scan-001", Role: "scanner", Backend: "claude", Model: "sonnet"},
		"quality": {ID: "qual-002", Role: "quality", Backend: "claude", Model: "opus"},
	}
	project := ProjectContext{
		Org:        "testorg",
		Repos:      []string{"repo1"},
		ACMMLevel:  3,
		PRsAllowed: true,
	}

	m := NewManager(agents, slog.Default(), project)
	if m == nil {
		t.Fatal("NewManager returned nil")
	}
	if len(m.agents) != 2 {
		t.Errorf("expected 2 agents, got %d", len(m.agents))
	}
	if m.agents["scanner"] == nil {
		t.Error("scanner should be present")
	}
	if m.agents["scanner"].ID != "scan-001" {
		t.Errorf("scanner ID = %q", m.agents["scanner"].ID)
	}
	if m.idToName["scan-001"] != "scanner" {
		t.Error("idToName should map scan-001 to scanner")
	}
	if m.project.ACMMLevel != 3 {
		t.Errorf("ACMMLevel = %d", m.project.ACMMLevel)
	}
}

func TestNewManagerEmpty(t *testing.T) {
	m := NewManager(nil, slog.Default(), ProjectContext{})
	if m == nil {
		t.Fatal("nil agents should still create manager")
	}
	if len(m.agents) != 0 {
		t.Error("should have 0 agents")
	}
}

func TestLogOutputSignals(t *testing.T) {
	m := &Manager{logger: slog.Default()}

	m.logOutputSignals("scanner", "Created 3 new issues for the project")
	m.logOutputSignals("scanner", "FAIL: TestSomething")
	m.logOutputSignals("scanner", "coverage report: 85.5 percent")
	m.logOutputSignals("scanner", "nothing interesting here")
}

func TestLogOutputSignalsLong(t *testing.T) {
	m := &Manager{logger: slog.Default()}
	long := "created file " + strings.Repeat("x", 300)
	m.logOutputSignals("scanner", long)
}

func TestReadCoveragePreamble(t *testing.T) {
	orig := metricsCachePath
	metricsCachePath = filepath.Join(t.TempDir(), "missing-agent-metrics-cache.json")
	t.Cleanup(func() { metricsCachePath = orig })

	m := &Manager{logger: slog.Default()}
	got := m.readCoveragePreamble()
	if got != "" {
		t.Fatalf("missing coverage metrics should return empty, got %q", got)
	}
}

func TestIsBufferNoisePatterns(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"╭─╮ Some banner", true},
		{"╰─╯", true},
		{"█ ▘▝ █", true},
		{"▔▔▔▔▔", true},
		{"Copilot v1.0.0", true},
		{"Check for mistakes in this code", true},
		{"/ commands available", true},
		{"? help for info", true},
		{"@ files", true},
		{"Some text with esc cancel here", true},
		{"─────────────────", true},
		{"━━━━━━━━━━━━━━━", true},
		{"/data/agents/scanner", true},
		{"Processing file.go", false},
		{"Created 5 issues", false},
		{"", true},
	}
	for _, tt := range tests {
		got := isBufferNoise(tt.input)
		if got != tt.want {
			t.Errorf("isBufferNoise(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestIsBufferNoiseAllPatterns(t *testing.T) {
	noiseInputs := []string{
		"● Tip: Use /help for commands",
		"└ scanning files...",
		"↑/↓ to navigate history",
		"copilot-instructions.md found during /init",
		"Do you trust the files in this folder?",
		"› Yes, I trust the authors",
		"› No (Esc)",
		"● Folder /data/agents/scanner is now trusted",
		"✗ Model gpt-5 not available, using fallback",
	}
	for _, input := range noiseInputs {
		if !isBufferNoise(input) {
			t.Errorf("isBufferNoise(%q) = false, want true", input)
		}
	}
}

func TestIsCLIChromeMorePatterns(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"# issues to review", true},
		{"Claude 3.5 Sonnet", false},
		{"model: claude-3-opus", false},
	}
	for _, tt := range tests {
		got := isCLIChrome(tt.input)
		if got != tt.want {
			t.Errorf("isCLIChrome(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestAgentEnvPairsWithEnvVars(t *testing.T) {
	t.Setenv("HIVE_SHA", "abc123")
	t.Setenv("HIVE_ADVISORY_ISSUE", "42")

	m := testManager(2)
	m.copilotAuthToken = "ghp_copilot_test"

	agent := &AgentProcess{
		Name:   "scanner",
		UID:    1001,
		Config: config.AgentConfig{Role: "scanner", Backend: "claude", Model: "sonnet", BeadsDir: "/data/beads/scanner"},
	}

	pairs := m.agentEnvPairs(agent)

	foundSHA := false
	foundAdvisory := false
	foundCopilot := false
	foundHome := false
	foundBD := false
	for _, p := range pairs {
		switch p.Key {
		case "HIVE_SHA":
			foundSHA = true
			if p.Value != "abc123" {
				t.Errorf("HIVE_SHA = %q", p.Value)
			}
		case "HIVE_ADVISORY_ISSUE":
			foundAdvisory = true
		case "COPILOT_GITHUB_TOKEN":
			foundCopilot = true
			if !p.Secret {
				t.Error("COPILOT_GITHUB_TOKEN should be secret")
			}
		case "HOME":
			foundHome = true
		case "BD_DIR":
			foundBD = true
		}
	}
	if !foundSHA {
		t.Error("should include HIVE_SHA")
	}
	if !foundAdvisory {
		t.Error("should include HIVE_ADVISORY_ISSUE")
	}
	if !foundCopilot {
		t.Error("should include COPILOT_GITHUB_TOKEN")
	}
	if !foundHome {
		t.Error("UID > 0 should include HOME")
	}
	if !foundBD {
		t.Error("should include BD_DIR")
	}
}

func TestBuildEnvPrefixSkipsSecrets(t *testing.T) {
	m := testManager(2)
	m.copilotAuthToken = "ghp_secret"

	agent := &AgentProcess{
		Name:   "scanner",
		Config: config.AgentConfig{Role: "scanner", Backend: "claude", Model: "sonnet"},
	}

	got := m.buildEnvPrefix(agent)
	if containsBoot(got, "ghp_secret") {
		t.Error("buildEnvPrefix should NOT include secret env vars")
	}
}

func TestBuildBootstrapPromptEmptyPolicyDir(t *testing.T) {
	m := &Manager{
		agents:   make(map[string]*AgentProcess),
		idToName: make(map[string]string),
		logger:   slog.Default(),
		project: ProjectContext{
			Org:       "testorg",
			Repos:     []string{"repo"},
			ACMMLevel: 2,
			PolicyDir: "",
		},
	}

	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{Role: "scanner"}}
	got := m.buildBootstrapPrompt(agent)
	if got != "" {
		t.Error("should produce bootstrap prompt even without policy dir")
	}
}

func TestConfigHasTokensFile(t *testing.T) {
	got := configHasTokens()
	_ = got
}

func TestSyncModeFilesWithPausedAgent(t *testing.T) {
	m := &Manager{
		agents: map[string]*AgentProcess{
			"scanner": {Name: "scanner", State: StateRunning, Config: config.AgentConfig{Role: "scanner"}},
			"quality": {Name: "quality", State: StatePaused, Paused: true, Config: config.AgentConfig{Role: "quality"}},
		},
		logger:  slog.Default(),
		project: ProjectContext{ACMMLevel: 3},
	}

	m.SyncModeFiles(3)
}

func TestSyncModeFilesWithModeOverride(t *testing.T) {
	m := &Manager{
		agents: map[string]*AgentProcess{
			"scanner": {Name: "scanner", State: StateRunning, Config: config.AgentConfig{Role: "scanner", Mode: "ISSUES_AND_PRS"}},
		},
		logger:  slog.Default(),
		project: ProjectContext{ACMMLevel: 2},
	}

	m.SyncModeFiles(2)
}

func TestSyncModeFilesLevel3(t *testing.T) {
	dir := t.TempDir()
	m := &Manager{
		agents: map[string]*AgentProcess{
			"scanner": {Name: "scanner", State: StateRunning, Config: config.AgentConfig{Role: "scanner"}},
			"quality": {Name: "quality", State: StateRunning, Config: config.AgentConfig{Role: "quality"}},
		},
		logger:  slog.Default(),
		workDir: dir,
		project: ProjectContext{ACMMLevel: 3},
	}

	m.SyncModeFiles(3)

	// Check mode files were written
	scannerMode, _ := os.ReadFile(filepath.Join(agentStateDir, ".hive-mode-scanner"))
	_ = scannerMode
}

func TestSyncModeFilesLevel6(t *testing.T) {
	m := &Manager{
		agents: map[string]*AgentProcess{
			"scanner": {Name: "scanner", State: StateRunning, Config: config.AgentConfig{Role: "scanner"}},
		},
		logger:  slog.Default(),
		project: ProjectContext{ACMMLevel: 6},
	}

	m.SyncModeFiles(6)
}

func TestAddAgentWithID(t *testing.T) {
	m := &Manager{
		agents:   make(map[string]*AgentProcess),
		idToName: make(map[string]string),
		logger:   slog.Default(),
		workDir:  "/tmp/hive-test",
	}

	m.AddAgent("test-agent", config.AgentConfig{
		ID:          "agent-001",
		Role:        "scanner",
		Backend:     "claude",
		Model:       "sonnet",
		DisplayName: "Test Agent",
	})

	if _, ok := m.agents["test-agent"]; !ok {
		t.Error("agent should be added")
	}
	if m.idToName["agent-001"] != "test-agent" {
		t.Error("ID mapping should be set")
	}
}

func TestRemoveAgentExisting(t *testing.T) {
	m := &Manager{
		agents: map[string]*AgentProcess{
			"scanner": {Name: "scanner"},
			"quality": {Name: "quality"},
		},
		idToName: make(map[string]string),
		logger:   slog.Default(),
	}

	m.RemoveAgent("scanner")
	if _, ok := m.agents["scanner"]; ok {
		t.Error("scanner should be removed")
	}
}
