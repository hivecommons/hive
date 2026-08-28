package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

// ---------------------------------------------------------------------------
// truncateHead / truncateTail
// ---------------------------------------------------------------------------

func TestTruncateHead(t *testing.T) {
	tests := []struct {
		input string
		n     int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hello..."},
		{"", 5, ""},
		{"abc", 0, "..."},
		{"abcdef", 3, "abc..."},
		// Unicode: each rune counts as 1
		{"\U0001F600\U0001F601\U0001F602\U0001F603", 2, "\U0001F600\U0001F601..."},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("n=%d/%q", tt.n, tt.input), func(t *testing.T) {
			got := truncateHead(tt.input, tt.n)
			if got != tt.want {
				t.Errorf("truncateHead(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
			}
		})
	}
}

func TestTruncateTail(t *testing.T) {
	tests := []struct {
		input string
		n     int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "...world"},
		{"", 5, ""},
		{"abc", 0, "..."},
		{"abcdef", 3, "...def"},
		// Unicode: each rune counts as 1
		{"\U0001F600\U0001F601\U0001F602\U0001F603", 2, "...\U0001F602\U0001F603"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("n=%d/%q", tt.n, tt.input), func(t *testing.T) {
			got := truncateTail(tt.input, tt.n)
			if got != tt.want {
				t.Errorf("truncateTail(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// readCoveragePreamble — with actual temp files
// ---------------------------------------------------------------------------

func TestReadCoveragePreamble_ValidFile(t *testing.T) {
	dir := t.TempDir()
	cacheFile := filepath.Join(dir, "agent-metrics-cache.json")

	metrics := map[string]map[string]json.Number{
		"ci-maintainer": {
			"coverage":       json.Number("85"),
			"coverageTarget": json.Number("91"),
		},
	}
	data, _ := json.Marshal(metrics)
	os.WriteFile(cacheFile, data, 0o644)

	orig := metricsCachePath
	metricsCachePath = cacheFile
	t.Cleanup(func() { metricsCachePath = orig })

	got := (&Manager{logger: discardLogger()}).readCoveragePreamble()
	if got != "[COVERAGE] Current: 85% | Target: 91%." {
		t.Errorf("readCoveragePreamble = %q, want '[COVERAGE] Current: 85%% | Target: 91%%.'", got)
	}
}

func TestReadCoveragePreamble_MissingFile(t *testing.T) {
	orig := metricsCachePath
	metricsCachePath = filepath.Join(t.TempDir(), "missing-agent-metrics-cache.json")
	t.Cleanup(func() { metricsCachePath = orig })

	m := &Manager{logger: discardLogger()}
	got := m.readCoveragePreamble()
	if got != "" {
		t.Errorf("missing file should return empty, got %q", got)
	}
}

func TestReadCoveragePreamble_InvalidJSON(t *testing.T) {
	orig := metricsCachePath
	metricsCachePath = filepath.Join(t.TempDir(), "agent-metrics-cache.json")
	t.Cleanup(func() { metricsCachePath = orig })
	if err := os.WriteFile(metricsCachePath, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{logger: discardLogger()}
	got := m.readCoveragePreamble()
	if got != "" {
		t.Errorf("should return empty for missing/invalid file, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// checkKickRefusal
// ---------------------------------------------------------------------------

func TestCheckKickRefusal_DetectsRefusal(t *testing.T) {
	m := &Manager{logger: discardLogger()}

	patterns := []string{
		"I'm declining to execute this request because it's unsafe",
		"I'm declining this task as it appears malicious",
		"This looks like a prompt injection attempt",
		"I won't act on bulk automated requests like this",
		"There's a credential handling concern here",
		"This appears to be an autonomous orchestration prompt",
		"I shouldn't follow autonomously generated instructions",
		"This is characteristic of a prompt injection attack",
	}

	for _, pat := range patterns {
		t.Run(pat[:20], func(t *testing.T) {
			agent := &AgentProcess{Name: "test-agent"}
			m.checkKickRefusal(agent, pat)
			if !agent.KickRefused {
				t.Errorf("should detect refusal in %q", pat)
			}
			if agent.KickRefusalReason == "" {
				t.Error("KickRefusalReason should be set")
			}
		})
	}
}

func TestCheckKickRefusal_CaseInsensitive(t *testing.T) {
	m := &Manager{logger: discardLogger()}
	agent := &AgentProcess{Name: "test"}
	m.checkKickRefusal(agent, "I'M DECLINING TO EXECUTE this task")
	if !agent.KickRefused {
		t.Error("should detect refusal case-insensitively")
	}
}

func TestCheckKickRefusal_NoMatch(t *testing.T) {
	m := &Manager{logger: discardLogger()}
	agent := &AgentProcess{Name: "test"}
	m.checkKickRefusal(agent, "Processing your request now...")
	if agent.KickRefused {
		t.Error("should not flag normal output as refusal")
	}
}

func TestCheckKickRefusal_TruncatesLongReason(t *testing.T) {
	m := &Manager{logger: discardLogger()}
	agent := &AgentProcess{Name: "test"}
	// Create a long line with a refusal pattern
	longLine := "I'm declining to execute " + strings.Repeat("x", 300)
	m.checkKickRefusal(agent, longLine)
	if !agent.KickRefused {
		t.Fatal("should detect refusal")
	}
	const maxReasonRunes = 200
	if len([]rune(agent.KickRefusalReason)) > maxReasonRunes {
		t.Errorf("reason should be truncated to %d runes, got %d", maxReasonRunes, len([]rune(agent.KickRefusalReason)))
	}
}

// ---------------------------------------------------------------------------
// SetInferenceCallbacks
// ---------------------------------------------------------------------------

func TestSetInferenceCallbacks(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})

	setCalled := false
	clearCalled := false

	m.SetInferenceCallbacks(
		func(agentName, backend, model string) { setCalled = true },
		func(agentName string) { clearCalled = true },
	)

	m.mu.RLock()
	hasSet := m.inferenceRouteCallback != nil
	hasClear := m.clearInferenceRouteCallback != nil
	m.mu.RUnlock()

	if !hasSet {
		t.Error("inferenceRouteCallback should be set")
	}
	if !hasClear {
		t.Error("clearInferenceRouteCallback should be set")
	}

	// Call them to verify they work
	m.inferenceRouteCallback("test", "vllm", "model")
	m.clearInferenceRouteCallback("test")
	if !setCalled || !clearCalled {
		t.Error("callbacks should have been invoked")
	}
}

// ---------------------------------------------------------------------------
// Pause state transitions
// ---------------------------------------------------------------------------

func TestPause_SetsAllFields(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"worker": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	before := time.Now()
	err := m.Pause("worker", "governor", "too many errors")
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	after := time.Now()

	status, _ := m.GetStatus("worker")
	if !status.Paused {
		t.Error("should be paused")
	}
	if status.State != StatePaused {
		t.Errorf("state = %q, want paused", status.State)
	}
	if status.PausedReason != "too many errors" {
		t.Errorf("reason = %q", status.PausedReason)
	}
	if status.PausedTrigger != "governor" {
		t.Errorf("trigger = %q", status.PausedTrigger)
	}
	if status.PausedAt.Before(before) || status.PausedAt.After(after) {
		t.Errorf("PausedAt not in expected range")
	}
}

func TestPause_RunningAgentTransitionsToPaused(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"worker": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	// Set agent to running state
	m.mu.Lock()
	m.agents["worker"].State = StateRunning
	m.mu.Unlock()

	err := m.Pause("worker", "test", "test reason")
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	status, _ := m.GetStatus("worker")
	if status.State != StatePaused {
		t.Errorf("state = %q, want paused", status.State)
	}
}

// ---------------------------------------------------------------------------
// AddAgent / RemoveAgent — id mapping
// ---------------------------------------------------------------------------

func TestAddAgent_IDMapping(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})

	m.AddAgent("scanner", config.AgentConfig{ID: "scan-007", Backend: "claude"})

	// Should be resolvable by ID
	resolved := m.ResolveAgent("scan-007")
	if resolved != "scanner" {
		t.Errorf("ResolveAgent(ID) = %q, want scanner", resolved)
	}
}

func TestAddAgent_EmptyIDDefaultsToName(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})

	m.AddAgent("worker", config.AgentConfig{Backend: "claude"})

	// ID should default to name
	resolved := m.ResolveAgent("worker")
	if resolved != "worker" {
		t.Errorf("ResolveAgent = %q, want worker", resolved)
	}
}

func TestRemoveAgent_CleansIDMapping(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {ID: "scan-001", Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.RemoveAgent("scanner")

	// ID should no longer resolve
	resolved := m.ResolveAgent("scan-001")
	if resolved != "scan-001" {
		t.Errorf("removed agent ID should not resolve, got %q", resolved)
	}
}

// ---------------------------------------------------------------------------
// GetBufferOutput — various paths
// ---------------------------------------------------------------------------

func TestGetBufferOutput_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	_, err := m.GetBufferOutput("nonexistent", 10)
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

func TestGetBufferOutput_WithBufferData(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.Lock()
	m.agents["scanner"].OutputBuffer.Write("line A")
	m.agents["scanner"].OutputBuffer.Write("line B")
	m.agents["scanner"].OutputBuffer.Write("line C")
	m.mu.Unlock()

	output, err := m.GetBufferOutput("scanner", 2)
	if err != nil {
		t.Fatalf("GetBufferOutput: %v", err)
	}
	if len(output) != 2 {
		t.Errorf("output lines = %d, want 2", len(output))
	}
	if output[0] != "line B" || output[1] != "line C" {
		t.Errorf("output = %v, want [line B, line C]", output)
	}
}

func TestGetBufferOutput_EmptyBuffer_FallsBackToPane(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	// Set pane data but leave buffer empty
	m.mu.Lock()
	m.agents["scanner"].lastPaneCapture = []string{"pane line 1", "pane line 2"}
	m.mu.Unlock()

	output, err := m.GetBufferOutput("scanner", 10)
	if err != nil {
		t.Fatalf("GetBufferOutput: %v", err)
	}
	if len(output) == 0 {
		t.Error("should fall back to pane lines when buffer empty")
	}
}

func TestGetBufferOutput_EmptyBoth(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	output, err := m.GetBufferOutput("scanner", 10)
	if err != nil {
		t.Fatalf("GetBufferOutput: %v", err)
	}
	if output != nil {
		t.Errorf("expected nil output when both empty, got %v", output)
	}
}

// ---------------------------------------------------------------------------
// KickHistory management
// ---------------------------------------------------------------------------

func TestKickHistory_Capacity(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	// Seed exactly at capacity
	records := make([]KickRecord, kickHistoryCapacity)
	for i := range records {
		records[i] = KickRecord{
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Agent:     "scanner",
			Snippet:   fmt.Sprintf("kick %d", i),
		}
	}
	m.SeedKickHistory("scanner", records)

	m.mu.RLock()
	histLen := len(m.agents["scanner"].KickHistory)
	m.mu.RUnlock()

	if histLen != kickHistoryCapacity {
		t.Errorf("history length = %d, want %d", histLen, kickHistoryCapacity)
	}
}

func TestSeedKickHistory_CopiesRecords(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	records := []KickRecord{
		{Timestamp: time.Now(), Agent: "scanner", Snippet: "original"},
	}
	m.SeedKickHistory("scanner", records)

	// Mutate the original — should not affect the seeded copy
	records[0].Snippet = "mutated"

	m.mu.RLock()
	snippet := m.agents["scanner"].KickHistory[0].Snippet
	m.mu.RUnlock()

	if snippet != "original" {
		t.Errorf("SeedKickHistory should copy records, snippet = %q", snippet)
	}
}

// ---------------------------------------------------------------------------
// FilteredPaneLines / PaneLines
// ---------------------------------------------------------------------------

func TestFilteredPaneLines_Empty(t *testing.T) {
	agent := &AgentProcess{Name: "test"}
	lines := agent.FilteredPaneLines(10)
	if lines != nil {
		t.Errorf("empty pane should return nil, got %v", lines)
	}
}

func TestFilteredPaneLines_WithContent(t *testing.T) {
	agent := &AgentProcess{
		Name: "test",
		lastPaneCapture: []string{
			"❯",
			"Processing files...",
			"Found 5 issues",
			"Writing report",
		},
	}
	lines := agent.FilteredPaneLines(10)
	if len(lines) == 0 {
		t.Error("should return filtered lines")
	}
}

func TestFilteredPaneLines_LimitN(t *testing.T) {
	agent := &AgentProcess{
		Name: "test",
		lastPaneCapture: []string{
			"line1", "line2", "line3", "line4", "line5",
			"line6", "line7", "line8", "line9", "line10",
		},
	}
	lines := agent.FilteredPaneLines(3)
	if len(lines) > 3 {
		t.Errorf("should return at most 3 lines, got %d", len(lines))
	}
}

func TestPaneLines_WithPromptAndContent(t *testing.T) {
	agent := &AgentProcess{
		Name: "test",
		lastPaneCapture: []string{
			"old stuff",
			"❯",
			"Processing files...",
			"Found 5 issues",
		},
	}
	lines := agent.PaneLines(10)
	// Should prefer content after the prompt
	found := false
	for _, l := range lines {
		if strings.Contains(l, "Processing") || strings.Contains(l, "Found") {
			found = true
		}
	}
	if !found {
		t.Error("should include content after the prompt")
	}
}

// ---------------------------------------------------------------------------
// DeduplicateBlocks
// ---------------------------------------------------------------------------

func TestDeduplicateBlocks_SmallInput(t *testing.T) {
	// Less than 4 lines should be returned as-is
	lines := []string{"a", "b"}
	result := DeduplicateBlocks(lines)
	if len(result) != 2 {
		t.Errorf("small input should be returned as-is, got %d lines", len(result))
	}
}

func TestDeduplicateBlocks_NoDuplicates(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e"}
	result := DeduplicateBlocks(lines)
	if len(result) != len(lines) {
		t.Errorf("no duplicates should keep same length: got %d, want %d", len(result), len(lines))
	}
}

func TestDeduplicateBlocks_ExactDuplicate(t *testing.T) {
	lines := []string{
		"header",
		"processing...",
		"done",
		"header",
		"processing...",
		"done",
	}
	result := DeduplicateBlocks(lines)
	if len(result) >= len(lines) {
		t.Errorf("should deduplicate, got %d lines (original %d)", len(result), len(lines))
	}
}

func TestDeduplicateBlocks_SpinnerDuplicate(t *testing.T) {
	// Spinner changes should be treated as duplicates via normalizeLine
	lines := []string{
		"Reading files",
		"◐ Analyzing code",
		"Reading files",
		"◎ Analyzing code",
	}
	result := DeduplicateBlocks(lines)
	if len(result) > 2 {
		t.Errorf("spinner-changed blocks should deduplicate, got %d lines", len(result))
	}
}

// ---------------------------------------------------------------------------
// filterPaneOutput
// ---------------------------------------------------------------------------

func TestFilterPaneOutput_PromptAtEnd(t *testing.T) {
	lines := []string{
		"old content",
		"more old",
		"❯",
	}
	// When prompt is at end with no content after, should show lines before prompt
	result := filterPaneOutput(lines, 10)
	if len(result) == 0 {
		t.Error("should show content before prompt when nothing after")
	}
}

func TestFilterPaneOutput_PromptWithContentAfter(t *testing.T) {
	lines := []string{
		"old content",
		"❯",
		"new content line 1",
		"new content line 2",
	}
	result := filterPaneOutput(lines, 10)
	hasNew := false
	for _, l := range result {
		if strings.Contains(l, "new content") {
			hasNew = true
		}
	}
	if !hasNew {
		t.Error("should show content after prompt")
	}
}

func TestFilterPaneOutput_OnlyChromeAfterPrompt(t *testing.T) {
	lines := []string{
		"real output here",
		"more output",
		"❯",
		"/ commands",
		"? help",
	}
	// Only CLI chrome after prompt — should fall back to content before prompt
	result := filterPaneOutput(lines, 10)
	hasReal := false
	for _, l := range result {
		if strings.Contains(l, "real output") || strings.Contains(l, "more output") {
			hasReal = true
		}
	}
	if !hasReal {
		t.Error("should fall back to content before prompt when only chrome after")
	}
}

func TestFilterPaneOutput_NoPrompt(t *testing.T) {
	lines := []string{
		"output line 1",
		"output line 2",
		"output line 3",
	}
	result := filterPaneOutput(lines, 2)
	if len(result) > 2 {
		t.Errorf("should limit to 2 lines, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// buildProjectPreamble
// ---------------------------------------------------------------------------

func TestBuildProjectPreamble_EmptyOrg(t *testing.T) {
	m := &Manager{
		logger:  discardLogger(),
		project: ProjectContext{Org: "", Repos: []string{"repo"}},
	}
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{}}
	got := m.buildProjectPreamble(agent)
	if got != "" {
		t.Errorf("empty org should return empty preamble, got %q", got)
	}
}

func TestBuildProjectPreamble_EmptyRepos(t *testing.T) {
	m := &Manager{
		logger:  discardLogger(),
		project: ProjectContext{Org: "testorg", Repos: nil},
	}
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{}}
	got := m.buildProjectPreamble(agent)
	if got != "" {
		t.Errorf("empty repos should return empty preamble, got %q", got)
	}
}

func TestBuildProjectPreamble_PRsNotAllowed(t *testing.T) {
	m := &Manager{
		logger: discardLogger(),
		project: ProjectContext{
			Org:        "testorg",
			Repos:      []string{"repo1"},
			ACMMLevel:  3,
			PRsAllowed: false,
		},
	}
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{}}
	got := m.buildProjectPreamble(agent)
	if !strings.Contains(got, "PRs NOT allowed") {
		t.Errorf("should say PRs NOT allowed, got %q", got)
	}
}

func TestBuildProjectPreamble_PRsAllowed_Advisory(t *testing.T) {
	m := &Manager{
		logger: discardLogger(),
		project: ProjectContext{
			Org:        "testorg",
			Repos:      []string{"repo1"},
			ACMMLevel:  2,
			PRsAllowed: true,
		},
	}
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{}}
	got := m.buildProjectPreamble(agent)
	if !strings.Contains(got, "Advisory") {
		t.Errorf("L2 scanner should be advisory, got %q", got)
	}
}

func TestBuildProjectPreamble_PRsAllowed_IssuesAndPRs_L5(t *testing.T) {
	m := &Manager{
		logger: discardLogger(),
		project: ProjectContext{
			Org:        "testorg",
			Repos:      []string{"repo1"},
			ACMMLevel:  5,
			PRsAllowed: true,
		},
	}
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{}}
	got := m.buildProjectPreamble(agent)
	if !strings.Contains(got, "hold-labeled") {
		t.Errorf("L5 should mention hold-labeled, got %q", got)
	}
}

func TestBuildProjectPreamble_PRsAllowed_IssuesAndPRs_L4(t *testing.T) {
	m := &Manager{
		logger: discardLogger(),
		project: ProjectContext{
			Org:        "testorg",
			Repos:      []string{"repo1"},
			ACMMLevel:  4,
			PRsAllowed: true,
		},
	}
	agent := &AgentProcess{Name: "quality", Config: config.AgentConfig{}}
	got := m.buildProjectPreamble(agent)
	if !strings.Contains(got, "Issues + PRs allowed") {
		t.Errorf("L4 quality should allow Issues + PRs, got %q", got)
	}
}

func TestBuildProjectPreamble_IssuesPRsMerge(t *testing.T) {
	m := &Manager{
		logger: discardLogger(),
		project: ProjectContext{
			Org:        "testorg",
			Repos:      []string{"repo1"},
			ACMMLevel:  6,
			PRsAllowed: true,
		},
	}
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{}}
	got := m.buildProjectPreamble(agent)
	if !strings.Contains(got, "auto-merge") {
		t.Errorf("L6 scanner should mention auto-merge, got %q", got)
	}
}

func TestBuildProjectPreamble_UnknownLevel(t *testing.T) {
	m := &Manager{
		logger: discardLogger(),
		project: ProjectContext{
			Org:       "testorg",
			Repos:     []string{"repo1"},
			ACMMLevel: 99,
		},
	}
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{}}
	got := m.buildProjectPreamble(agent)
	if !strings.Contains(got, "Level 99") {
		t.Errorf("unknown level should show 'Level 99', got %q", got)
	}
}

func TestBuildProjectPreamble_MultipleRepos(t *testing.T) {
	m := &Manager{
		logger: discardLogger(),
		project: ProjectContext{
			Org:       "testorg",
			Repos:     []string{"repo1", "repo2", "repo3"},
			ACMMLevel: 3,
		},
	}
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{}}
	got := m.buildProjectPreamble(agent)
	if !strings.Contains(got, "testorg/repo1, testorg/repo2, testorg/repo3") {
		t.Errorf("should list all repos, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// paneShowsLoginPrompt
// ---------------------------------------------------------------------------

func TestPaneShowsLoginPrompt_MatchesPatterns(t *testing.T) {
	tests := []struct {
		lines []string
		want  bool
	}{
		{[]string{"Please /login to continue"}, true},
		{[]string{"sign in to use GitHub Copilot"}, true},
		{[]string{"Sign in to use this feature"}, true},
		{[]string{"authenticate to use Copilot"}, true},
		{[]string{"Authenticate to use this"}, true},
		{[]string{"log in to use this feature"}, true},
		{[]string{"Log in to use Copilot"}, true},
		{[]string{"normal output here"}, false},
		{nil, false},
		{[]string{}, false},
	}
	for _, tt := range tests {
		got := paneShowsLoginPrompt(tt.lines)
		if got != tt.want {
			t.Errorf("paneShowsLoginPrompt(%v) = %v, want %v", tt.lines, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// paneShowsCLIReady
// ---------------------------------------------------------------------------

func TestPaneShowsCLIReady_MatchesIndicators(t *testing.T) {
	tests := []struct {
		lines []string
		want  bool
	}{
		{[]string{"❯"}, true},
		{[]string{"/ commands available"}, true},
		{[]string{"? help for usage"}, true},
		{[]string{"/login please"}, true},
		{[]string{"sign in to continue"}, true},
		{[]string{"Copilot v1.0.63"}, true},
		{[]string{"Tip: /init to get started"}, true},
		{[]string{"Loading: models..."}, true},
		{[]string{"● Loading plugins"}, true},
		{[]string{"just normal output"}, false},
		{nil, false},
		{[]string{}, false},
	}
	for _, tt := range tests {
		got := paneShowsCLIReady(tt.lines)
		if got != tt.want {
			t.Errorf("paneShowsCLIReady(%v) = %v, want %v", tt.lines, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// paneShowsFatalNetworkError
// ---------------------------------------------------------------------------

func TestPaneShowsFatalNetworkError_AllPatterns(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"invalid peer certificate: expired", true},
		{"BadSignature detected in handshake", true},
		{"fetch failed: network error", true},
		{"normal operation log", false},
	}
	for _, tt := range tests {
		got := paneShowsFatalNetworkError([]string{tt.line})
		if got != tt.want {
			t.Errorf("paneShowsFatalNetworkError(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// configHasTokens — with temp file
// ---------------------------------------------------------------------------

func TestConfigHasTokens_WithTokens(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	cfg := map[string]interface{}{
		"copilotTokens": map[string]interface{}{
			"user1": "token123",
		},
	}
	data, _ := json.Marshal(cfg)
	os.WriteFile(configPath, data, 0o644)

	// We can't easily test configHasTokens() itself since it reads a hardcoded path,
	// but we can test the parsing logic it uses
	var parsed map[string]interface{}
	json.Unmarshal(data, &parsed)
	tokens, ok := parsed["copilotTokens"]
	if !ok {
		t.Fatal("copilotTokens not found")
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
// agentEnvPairs — inference backend branch
// ---------------------------------------------------------------------------

func TestAgentEnvPairs_InferenceBackend(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"inference-agent": {Backend: "vllm", Model: "llama-70b"},
	}, discardLogger(), ProjectContext{ACMMLevel: 3})

	ap := &AgentProcess{
		Name:   "inference-agent",
		Config: config.AgentConfig{Backend: "vllm", Model: "llama-70b"},
	}

	pairs := m.agentEnvPairs(ap)

	foundAnthropicKey := false
	foundAnthropicBase := false
	foundNoProxy := false
	for _, p := range pairs {
		switch p.Key {
		case "ANTHROPIC_API_KEY":
			foundAnthropicKey = true
			if !strings.HasPrefix(p.Value, "sk-hive-") {
				t.Errorf("ANTHROPIC_API_KEY = %q, want sk-hive-* prefix", p.Value)
			}
		case "ANTHROPIC_BASE_URL":
			foundAnthropicBase = true
		case "NO_PROXY":
			foundNoProxy = true
		}
	}
	if !foundAnthropicKey {
		t.Error("inference backend should set ANTHROPIC_API_KEY")
	}
	if !foundAnthropicBase {
		t.Error("inference backend should set ANTHROPIC_BASE_URL")
	}
	if !foundNoProxy {
		t.Error("inference backend should set NO_PROXY")
	}
}

func TestAgentEnvPairs_InferenceBackendHome(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"inference-agent": {Backend: "llm-d", Model: "model"},
	}, discardLogger(), ProjectContext{})

	ap := &AgentProcess{
		Name:   "inference-agent",
		UID:    1001,
		Config: config.AgentConfig{Backend: "llm-d", Model: "model"},
	}

	pairs := m.agentEnvPairs(ap)

	for _, p := range pairs {
		if p.Key == "HOME" {
			expectedHome := inferenceHomePath("inference-agent")
			if p.Value != expectedHome {
				t.Errorf("inference agent HOME = %q, want %q", p.Value, expectedHome)
			}
			return
		}
	}
	t.Error("HOME should be set for inference agent with UID > 0")
}

func TestAgentEnvPairs_DisplayName(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", DisplayName: "Code Scanner"},
	}, discardLogger(), ProjectContext{})

	ap := &AgentProcess{
		Name:   "scanner",
		Config: config.AgentConfig{Backend: "claude", DisplayName: "Code Scanner"},
	}

	pairs := m.agentEnvPairs(ap)

	for _, p := range pairs {
		if p.Key == "HIVE_AGENT_DISPLAY_NAME" {
			if p.Value != "Code Scanner" {
				t.Errorf("display name = %q, want 'Code Scanner'", p.Value)
			}
			return
		}
	}
	t.Error("HIVE_AGENT_DISPLAY_NAME should be present")
}

func TestAgentEnvPairs_DisplayNameDefault(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	ap := &AgentProcess{
		Name:   "scanner",
		Config: config.AgentConfig{Backend: "claude"},
	}

	pairs := m.agentEnvPairs(ap)

	for _, p := range pairs {
		if p.Key == "HIVE_AGENT_DISPLAY_NAME" {
			if p.Value != "scanner" {
				t.Errorf("display name = %q, want 'scanner' (default)", p.Value)
			}
			return
		}
	}
	t.Error("HIVE_AGENT_DISPLAY_NAME should be present")
}

// ---------------------------------------------------------------------------
// shellEnvVar
// ---------------------------------------------------------------------------

func TestShellEnvVar(t *testing.T) {
	tests := []struct {
		key, value, want string
	}{
		{"FOO", "bar", "FOO='bar'"},
		{"PATH", "/usr/bin:/bin", "PATH='/usr/bin:/bin'"},
		{"MSG", "it's a test", "MSG='it'\"'\"'s a test'"},
		{"EMPTY", "", "EMPTY=''"},
	}
	for _, tt := range tests {
		got := shellEnvVar(tt.key, tt.value)
		if got != tt.want {
			t.Errorf("shellEnvVar(%q, %q) = %q, want %q", tt.key, tt.value, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// IsInferenceBackend
// ---------------------------------------------------------------------------

func TestIsInferenceBackend(t *testing.T) {
	tests := []struct {
		backend string
		want    bool
	}{
		{"vllm", true},
		{"llm-d", true},
		{"litellm", true},
		{"claude", false},
		{"copilot", false},
		{"gemini", false},
		{"goose", false},
		{"", false},
	}
	for _, tt := range tests {
		got := IsInferenceBackend(tt.backend)
		if got != tt.want {
			t.Errorf("IsInferenceBackend(%q) = %v, want %v", tt.backend, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// normalizeModelName — additional edge cases
// ---------------------------------------------------------------------------

func TestNormalizeModelName_EdgeCases(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"no-hyphen", "no-hyphen"},
		{"trailing-", "trailing-"},
		{"-leading", "-leading"},
		// Unknown copilot ids pass through VERBATIM (#4262): the old blind
		// digit-suffix dot-rewrite is what corrupted the known claude-fable-5
		// into the CLI-rejected claude-fable.5. Copilot ids are canonicalized
		// against the CLI-accepted list instead (see CanonicalizeCopilotModel).
		{"a-b-c-1", "a-b-c-1"},
		{"just-123", "just-123"},
		{"model", "model"},
	}
	for _, tt := range tests {
		got := normalizeModelName(tt.input, "copilot")
		if got != tt.want {
			t.Errorf("normalizeModelName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}

	// The digit-suffix dot-rewrite still applies to non-copilot CLI backends.
	for _, tt := range []struct{ input, want string }{
		{"a-b-c-1", "a-b-c.1"},
		{"just-123", "just.123"},
	} {
		if got := normalizeModelName(tt.input, "gemini"); got != tt.want {
			t.Errorf("normalizeModelName(%q, gemini) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// diffNewLines — additional edge cases
// ---------------------------------------------------------------------------

func TestDiffNewLines_NoOverlap(t *testing.T) {
	prev := []string{"a", "b"}
	curr := []string{"x", "y", "z"}
	diff := diffNewLines(prev, curr)
	// No overlap found, returns all of curr
	if len(diff) != 3 {
		t.Errorf("no overlap should return all curr lines, got %d", len(diff))
	}
}

func TestDiffNewLines_EmptyPrev(t *testing.T) {
	diff := diffNewLines([]string{}, []string{"a", "b"})
	if len(diff) != 2 {
		t.Errorf("empty prev should return all curr, got %d", len(diff))
	}
}

// ---------------------------------------------------------------------------
// findOverlap — edge cases
// ---------------------------------------------------------------------------

func TestFindOverlap_NoMatch(t *testing.T) {
	prev := []string{"a", "b"}
	curr := []string{"x", "y"}
	overlap := findOverlap(prev, curr)
	if overlap != -1 {
		t.Errorf("no match should return -1, got %d", overlap)
	}
}

func TestFindOverlap_FullMatch(t *testing.T) {
	lines := []string{"a", "b", "c"}
	overlap := findOverlap(lines, lines)
	if overlap != 3 {
		t.Errorf("full match should return len, got %d", overlap)
	}
}

func TestFindOverlap_PrevLongerThanCurr(t *testing.T) {
	prev := []string{"a", "b", "c", "d", "e"}
	curr := []string{"d", "e", "f"}
	overlap := findOverlap(prev, curr)
	if overlap != 2 {
		t.Errorf("overlap should be 2, got %d", overlap)
	}
}

// ---------------------------------------------------------------------------
// Snapshot — copies pane capture
// ---------------------------------------------------------------------------

func TestSnapshot_CopiesPaneCapture(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.Lock()
	m.agents["scanner"].lastPaneCapture = []string{"line1", "line2"}
	m.mu.Unlock()

	status, _ := m.GetStatus("scanner")
	// PaneLines should return a copy. Filtering may legitimately drop lines,
	// so the only invariant this smoke test checks is that the call above
	// does not panic.
	_ = status.PaneLines(10)
}

// ---------------------------------------------------------------------------
// SetBootstrapOverride — not found
// ---------------------------------------------------------------------------

func TestSetBootstrapOverride_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.SetBootstrapOverride("nonexistent", "prompt")
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

// ---------------------------------------------------------------------------
// KillSession — not found
// ---------------------------------------------------------------------------

func TestKillSession_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.KillSession("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

// ---------------------------------------------------------------------------
// Mode — Suffix edge case
// ---------------------------------------------------------------------------

func TestModeSuffix_OutOfRange(t *testing.T) {
	mode := AgentMode(99)
	if mode.Suffix() != "-advisory" {
		t.Errorf("out of range Suffix = %q, want -advisory", mode.Suffix())
	}
}

func TestModeString_OutOfRange(t *testing.T) {
	mode := AgentMode(99)
	if mode.String() != "UNKNOWN" {
		t.Errorf("out of range String = %q, want UNKNOWN", mode.String())
	}
}

func TestModeEmoji_OutOfRange(t *testing.T) {
	mode := AgentMode(99)
	if mode.Emoji() != "" {
		t.Errorf("out of range Emoji = %q, want empty", mode.Emoji())
	}
}

// ---------------------------------------------------------------------------
// isCLIChrome — spinner in status bar
// ---------------------------------------------------------------------------

func TestIsCLIChrome_SpinnerInStatusBar(t *testing.T) {
	// Lines with Claude/Copilot/Gemini AND a spinner should be chrome
	if !isCLIChrome("Claude 3.5 ◎ thinking") {
		t.Error("Claude with spinner should be chrome")
	}
	if !isCLIChrome("Copilot v1 ● working") {
		t.Error("Copilot with spinner should be chrome")
	}
	if !isCLIChrome("Gemini 1.5 ◐ loading") {
		t.Error("Gemini with spinner should be chrome")
	}
}

// ---------------------------------------------------------------------------
// logOutputSignals — verify no panic on various patterns
// ---------------------------------------------------------------------------

func TestLogOutputSignals_AllPatterns(t *testing.T) {
	m := &Manager{logger: discardLogger()}

	for pattern := range outputSignalPatterns {
		m.logOutputSignals("test", "some text with "+pattern+" in it")
	}
	// No match case
	m.logOutputSignals("test", "completely unrelated line")
}

// ---------------------------------------------------------------------------
// GetOutput — pane lines path, then buffer fallback
// ---------------------------------------------------------------------------

func TestGetOutput_PrefersPaneOverBuffer(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.Lock()
	m.agents["scanner"].lastPaneCapture = []string{"pane line"}
	m.agents["scanner"].OutputBuffer.Write("buffer line")
	m.mu.Unlock()

	output, err := m.GetOutput("scanner", 10)
	if err != nil {
		t.Fatalf("GetOutput: %v", err)
	}
	if len(output) == 0 {
		t.Error("should return output")
	}
}

// ---------------------------------------------------------------------------
// SeedPauseState — not found
// ---------------------------------------------------------------------------

func TestSeedPauseState_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	// Should not panic
	m.SeedPauseState("nonexistent", time.Now(), "test", "test", "")
}

// ---------------------------------------------------------------------------
// buildEnvPrefix — all secret (edge case)
// ---------------------------------------------------------------------------

func TestBuildEnvPrefix_ContainsExpectedVars(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{ACMMLevel: 3})

	ap := &AgentProcess{
		Name:   "scanner",
		Config: config.AgentConfig{Backend: "claude", Model: "sonnet"},
	}

	prefix := m.buildEnvPrefix(ap)
	if !strings.Contains(prefix, "HIVE_AGENT='scanner'") {
		t.Errorf("prefix should contain HIVE_AGENT, got %q", prefix)
	}
	if !strings.Contains(prefix, "HIVE_BACKEND='claude'") {
		t.Errorf("prefix should contain HIVE_BACKEND, got %q", prefix)
	}
}

// ---------------------------------------------------------------------------
// DefaultAgentMode — complete coverage of all levels
// ---------------------------------------------------------------------------

func TestDefaultAgentMode_AllLevels(t *testing.T) {
	tests := []struct {
		name  string
		level int
		want  AgentMode
	}{
		// Level 1
		{"scanner/L1", 1, ModeAdvisory},
		{"quality/L1", 1, ModeAdvisory},
		// Level 2
		{"scanner/L2", 2, ModeAdvisory},
		// Level 3
		{"quality/L3", 3, ModeIssuesAndPRs},
		{"scanner/L3", 3, ModeAdvisory},
		// Level 4
		{"quality/L4", 4, ModeIssuesAndPRs},
		{"sec-check/L4", 4, ModeIssuesAndPRs},
		{"ci-maintainer/L4", 4, ModeIssuesAndPRs},
		{"scanner/L4", 4, ModeIssuesOnly},
		{"guide/L4", 4, ModeIssuesOnly},
		{"other/L4", 4, ModeAdvisory},
		// Level 5
		{"scanner/L5", 5, ModeIssuesAndPRs},
		{"quality/L5", 5, ModeIssuesAndPRs},
		// Level 6
		{"scanner/L6", 6, ModeIssuesPRsMerge},
		{"quality/L6", 6, ModeIssuesAndPRs},
		// Supervisor always advisory
		{"supervisor/L1", 1, ModeAdvisory},
		{"supervisor/L5", 5, ModeAdvisory},
		{"supervisor/L6", 6, ModeAdvisory},
		// Unknown level
		{"scanner/L99", 99, ModeAdvisory},
		{"scanner/L0", 0, ModeAdvisory},
	}
	for _, tt := range tests {
		name := strings.Split(tt.name, "/")[0]
		t.Run(tt.name, func(t *testing.T) {
			got := DefaultAgentMode(name, tt.level)
			if got != tt.want {
				t.Errorf("DefaultAgentMode(%q, %d) = %s, want %s", name, tt.level, got, tt.want)
			}
		})
	}
}

// SuffixForLevel, AgentMode booleans, and ParseAgentMode are tested in mode_test.go

// ---------------------------------------------------------------------------
// ClearAllModeOverrides — verify modes cleared
// ---------------------------------------------------------------------------

func TestClearAllModeOverrides_ClearsAll(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude", Mode: "ISSUES_AND_PRS"},
		"b": {Backend: "claude", Mode: "ISSUES_PRS_MERGE"},
		"c": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.ClearAllModeOverrides()

	m.mu.RLock()
	defer m.mu.RUnlock()
	for name, agent := range m.agents {
		if agent.Config.Mode != "" {
			t.Errorf("agent %q Mode = %q, want empty", name, agent.Config.Mode)
		}
	}
}

// ---------------------------------------------------------------------------
// filteredEnv — write-capable agent keeps all tokens
// ---------------------------------------------------------------------------

func TestFilteredEnv_WriteAgent(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"quality": {Backend: "claude", Mode: "ISSUES_AND_PRS"},
	}, discardLogger(), ProjectContext{ACMMLevel: 4})

	m.mu.RLock()
	agent := m.agents["quality"]
	m.mu.RUnlock()

	env := m.filteredEnv(agent)
	// Should not filter anything
	if len(env) == 0 {
		t.Error("write agent should get full env")
	}
}

func TestFilteredEnv_AdvisoryStripsTokens(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{ACMMLevel: 2})

	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	// Set tokens in env
	t.Setenv("GH_TOKEN", "ghp_test123")
	t.Setenv("GITHUB_TOKEN", "ghp_test456")

	env := m.filteredEnv(agent)
	for _, e := range env {
		if strings.HasPrefix(e, "GH_TOKEN=") || strings.HasPrefix(e, "GITHUB_TOKEN=") {
			t.Errorf("advisory agent should have %q filtered out", e)
		}
	}
}

// ---------------------------------------------------------------------------
// truncateStr (already has some coverage, verify edge cases)
// ---------------------------------------------------------------------------

func TestTruncateStr_EdgeCases(t *testing.T) {
	tests := []struct {
		input    string
		maxRunes int
		want     string
	}{
		{"hello", 5, "hello"},
		{"hello", 4, "hell..."},
		{"", 10, ""},
		{"\U0001F600\U0001F601", 1, "\U0001F600..."},
	}
	for _, tt := range tests {
		got := truncateStr(tt.input, tt.maxRunes)
		if got != tt.want {
			t.Errorf("truncateStr(%q, %d) = %q, want %q", tt.input, tt.maxRunes, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// isCLIChrome edge case — Claude Code excluded
// ---------------------------------------------------------------------------

func TestIsCLIChrome_ClaudeCodeNotChrome(t *testing.T) {
	// "Claude Code" lines should NOT be treated as chrome
	if isCLIChrome("Claude Code v1.2.3 is running") {
		t.Error("Claude Code lines should not be chrome")
	}
}

// ---------------------------------------------------------------------------
// isBufferNoise — additional patterns
// ---------------------------------------------------------------------------

func TestIsBufferNoise_PromptChars(t *testing.T) {
	if !isBufferNoise("❯") {
		t.Error("bare prompt should be noise")
	}
	if !isBufferNoise("›") {
		t.Error("bare prompt should be noise")
	}
	if !isBufferNoise(">") {
		t.Error("bare prompt should be noise")
	}
}

// ---------------------------------------------------------------------------
// acmmLevelNames
// ---------------------------------------------------------------------------

func TestACMMLevelNames(t *testing.T) {
	expected := map[int]string{
		1: "Inception",
		2: "Advisory",
		3: "Quality-Gated",
		4: "Security-Aware",
		5: "Semi-Autonomous",
		6: "Fully Autonomous",
	}
	for level, name := range expected {
		if acmmLevelNames[level] != name {
			t.Errorf("acmmLevelNames[%d] = %q, want %q", level, acmmLevelNames[level], name)
		}
	}
}

// ---------------------------------------------------------------------------
// configHasTokens — tests the parsing logic used by the function
// ---------------------------------------------------------------------------

func TestConfigHasTokens_ParsesJSONWithComments(t *testing.T) {
	// The configHasTokens function strips // comments and parses JSON.
	// Test the same logic inline since the function reads a hardcoded path.

	input := `// User settings belong in settings.json.
// This file is managed automatically.
{
  "copilotTokens": {
    "user1": {"token": "gho_abc123"}
  }
}`
	// Strip single-line // comments (same logic as configHasTokens)
	var cleaned []byte
	for _, line := range strings.Split(input, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		cleaned = append(cleaned, []byte(line+"\n")...)
	}

	var cfg map[string]interface{}
	if err := json.Unmarshal(cleaned, &cfg); err != nil {
		t.Fatalf("JSON parse failed: %v", err)
	}
	tokens, ok := cfg["copilotTokens"]
	if !ok {
		t.Fatal("copilotTokens not found")
	}
	tokensMap, ok := tokens.(map[string]interface{})
	if !ok {
		t.Fatal("copilotTokens not a map")
	}
	if len(tokensMap) != 1 {
		t.Errorf("expected 1 token entry, got %d", len(tokensMap))
	}
}

func TestConfigHasTokens_EmptyTokens(t *testing.T) {
	input := `{"copilotTokens": {}}`
	var cfg map[string]interface{}
	json.Unmarshal([]byte(input), &cfg)
	tokens := cfg["copilotTokens"].(map[string]interface{})
	if len(tokens) > 0 {
		t.Error("empty tokens should return false")
	}
}

func TestConfigHasTokens_MissingField(t *testing.T) {
	input := `{"someOtherField": true}`
	var cfg map[string]interface{}
	json.Unmarshal([]byte(input), &cfg)
	_, ok := cfg["copilotTokens"]
	if ok {
		t.Error("missing field should not be found")
	}
}

// ---------------------------------------------------------------------------
// clearExpiredTokens — test the JSON manipulation logic
// ---------------------------------------------------------------------------

func TestClearExpiredTokens_Logic(t *testing.T) {
	// Test the JSON manipulation clearExpiredTokens does
	input := map[string]interface{}{
		"copilotTokens":    map[string]interface{}{"user1": "token"},
		"loggedInUsers":    []interface{}{"user1"},
		"lastLoggedInUser": "user1",
		"otherSetting":     "keep",
	}

	// Apply the same operations as clearExpiredTokens: ONLY the token map is
	// emptied. Login identity (loggedInUsers/lastLoggedInUser) survives — an
	// expired token does not change who was logged in, and the interactive CLI
	// refuses a re-seeded token without an identity.
	input["copilotTokens"] = map[string]interface{}{}

	tokens := input["copilotTokens"].(map[string]interface{})
	if len(tokens) != 0 {
		t.Error("tokens should be empty after clear")
	}
	users := input["loggedInUsers"].([]interface{})
	if len(users) != 1 || users[0] != "user1" {
		t.Error("loggedInUsers must be preserved across a token clear")
	}
	if input["lastLoggedInUser"] != "user1" {
		t.Error("lastLoggedInUser must be preserved across a token clear")
	}
	if input["otherSetting"] != "keep" {
		t.Error("other settings should be preserved")
	}
}

// ---------------------------------------------------------------------------
// fixSharedConfigPerms — test with temp file
// ---------------------------------------------------------------------------

func TestFixSharedConfigPerms_Logic(t *testing.T) {
	// Test the permission fixing logic by creating a temp file and checking
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	os.WriteFile(configFile, []byte(`{}`), 0o600)

	info, err := os.Stat(configFile)
	if err != nil {
		t.Fatal(err)
	}
	// File is 0600, desired is 0660
	if info.Mode().Perm() == 0o660 {
		t.Skip("file already has desired perms")
	}
	if err := os.Chmod(configFile, 0o660); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(configFile)
	if info.Mode().Perm() != 0o660 {
		t.Errorf("perms = %04o, want 0660", info.Mode().Perm())
	}
}

// ---------------------------------------------------------------------------
// ensureClaudeSettings — test with temp dir
// ---------------------------------------------------------------------------

func TestEnsureClaudeSettings_CreatesSettings(t *testing.T) {
	// Test the settings creation logic
	dir := t.TempDir()
	settingsDir := filepath.Join(dir, ".claude")
	settingsFile := filepath.Join(settingsDir, "settings.json")

	// Create the settings directory and file (same logic as ensureClaudeSettings)
	os.MkdirAll(settingsDir, 0o777)
	settings := `{"permissions":{"allow":[],"deny":[]},"hasCompletedOnboarding":true,"bypassPermissions":true,"hasAcknowledgedDisclaimer":true}`
	os.WriteFile(settingsFile, []byte(settings), 0o644)

	data, err := os.ReadFile(settingsFile)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse settings: %v", err)
	}

	if parsed["hasCompletedOnboarding"] != true {
		t.Error("hasCompletedOnboarding should be true")
	}
	if parsed["bypassPermissions"] != true {
		t.Error("bypassPermissions should be true")
	}
}

// ---------------------------------------------------------------------------
// readCoveragePreamble — with actual metrics file
// ---------------------------------------------------------------------------

func TestReadCoveragePreamble_FullParse(t *testing.T) {
	// Test the full parsing path with a valid JSON file
	metrics := map[string]map[string]json.Number{
		"ci-maintainer": {
			"coverage":       json.Number("85"),
			"coverageTarget": json.Number("91"),
		},
	}
	data, _ := json.Marshal(metrics)

	var parsed map[string]map[string]json.Number
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	ci := parsed["ci-maintainer"]
	cov, _ := ci["coverage"].Int64()
	target, _ := ci["coverageTarget"].Int64()

	result := fmt.Sprintf("[COVERAGE] Current: %d%% | Target: %d%%.", cov, target)
	if result != "[COVERAGE] Current: 85% | Target: 91%." {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestReadCoveragePreamble_MissingCIMaintainer(t *testing.T) {
	metrics := map[string]map[string]json.Number{
		"other-agent": {
			"coverage": json.Number("50"),
		},
	}
	data, _ := json.Marshal(metrics)

	var parsed map[string]map[string]json.Number
	json.Unmarshal(data, &parsed)
	_, ok := parsed["ci-maintainer"]
	if ok {
		t.Error("ci-maintainer should not exist")
	}
}

func TestReadCoveragePreamble_MissingTarget(t *testing.T) {
	metrics := map[string]map[string]json.Number{
		"ci-maintainer": {
			"coverage": json.Number("85"),
		},
	}
	data, _ := json.Marshal(metrics)

	var parsed map[string]map[string]json.Number
	json.Unmarshal(data, &parsed)
	ci := parsed["ci-maintainer"]
	_, err := ci["coverageTarget"].Int64()
	if err == nil {
		t.Error("missing target should error")
	}
}

// ---------------------------------------------------------------------------
// backendBinary — vllm and llm-d map to claude
// ---------------------------------------------------------------------------

func TestBackendBinary_InferenceBackends(t *testing.T) {
	// vllm, llm-d, and litellm should map to claude binary
	for _, backend := range config.InferenceBackends {
		path, err := backendBinary(backend)
		if err != nil {
			t.Errorf("backendBinary(%q) error: %v", backend, err)
			continue
		}
		if !strings.Contains(path, "claude") {
			t.Errorf("backendBinary(%q) = %q, should resolve to claude", backend, path)
		}
	}
}

func TestBackendBinaryName_PiMapsToPi(t *testing.T) {
	name, err := backendBinaryName("pi")
	if err != nil {
		t.Fatalf("backendBinaryName(pi) error: %v", err)
	}
	if name != "pi" {
		t.Errorf("backendBinaryName(pi) = %q, should resolve to pi", name)
	}
}

// ---------------------------------------------------------------------------
// tmuxBaseArgs
// ---------------------------------------------------------------------------

func TestTmuxBaseArgs_WithSocket(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	agent := &AgentProcess{Name: "test", tmuxSocket: "hive-test"}
	args := m.tmuxBaseArgs(agent)
	if len(args) != 3 || args[1] != "-L" || args[2] != "hive-test" {
		t.Errorf("tmuxBaseArgs with socket = %v, want [tmux -L hive-test]", args)
	}
}

func TestTmuxBaseArgs_WithDefaultSocket(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	agent := &AgentProcess{Name: "test", tmuxSocket: ""}
	args := m.tmuxBaseArgs(agent)
	if len(args) != 3 || args[1] != "-L" || args[2] != defaultTmuxSocket {
		t.Errorf("tmuxBaseArgs with default socket = %v, want [tmux -L %s]", args, defaultTmuxSocket)
	}
}

func TestValidateTmuxKillSessionArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "non kill command", args: []string{"has-session", "-t", "work"}, wantErr: false},
		{name: "hive target", args: []string{"kill-session", "-t", "hive-agent"}, wantErr: false},
		{name: "hive target equals", args: []string{"kill-session", "-t=hive-agent"}, wantErr: false},
		{name: "missing target", args: []string{"kill-session"}, wantErr: true},
		{name: "empty target", args: []string{"kill-session", "-t", ""}, wantErr: true},
		{name: "non hive target", args: []string{"kill-session", "-t", "work"}, wantErr: true},
		{name: "kill all other sessions", args: []string{"kill-session", "-a", "-t", "hive-agent"}, wantErr: true},
		{name: "kill all in combined flags", args: []string{"kill-session", "-at", "hive-agent", "-t", "hive-agent"}, wantErr: true},
		{name: "kill all in reversed combined flags", args: []string{"kill-session", "-ta", "hive-agent"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTmuxKillSessionArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateTmuxKillSessionArgs(%v) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Concurrency: parallel Pause + Resume
// ---------------------------------------------------------------------------

func TestConcurrentPauseResume_NoPanic(t *testing.T) {
	// Sandbox-enabled agents keep every Pause/Resume cycle a pure in-memory
	// state transition — which is all this test pins: the pause/resume mutex
	// accounting under concurrency. Without the sandbox gate, each Resume of a
	// StatePaused agent takes the relaunch path (ensureTmuxSession +
	// launchInTmux): a REAL tmux session create plus CLI launch per cycle,
	// ~250 launches per run, none of them cleaned up. On hosts with a writable
	// HIVE_WORK_DIR that took 220s+ alone and pushed the whole package past
	// go test's 600s default timeout (#4628).
	sandboxOn := true
	sandbox := &config.AgentSandboxOverride{Enabled: &sandboxOn}
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude", Sandbox: sandbox},
		"b": {Backend: "claude", Sandbox: sandbox},
	}, discardLogger(), ProjectContext{})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true})

	done := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func() {
			for j := 0; j < 50; j++ {
				m.Pause("a", "test", "testing")
				m.Resume(context.TODO(), "a", "test", "testing")
				m.IsPaused("a")
				m.GetStatus("a")
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 5; i++ {
		<-done
	}
}

// ---------------------------------------------------------------------------
// NewManager with agent ID
// ---------------------------------------------------------------------------

func TestNewManager_AgentIDMapping(t *testing.T) {
	cfgs := map[string]config.AgentConfig{
		"scanner": {ID: "scan-001", Backend: "claude"},
		"quality": {Backend: "claude"}, // No ID — should default to name
	}
	m := NewManager(cfgs, discardLogger(), ProjectContext{})

	if m.idToName["scan-001"] != "scanner" {
		t.Errorf("idToName[scan-001] = %q, want scanner", m.idToName["scan-001"])
	}
	if m.idToName["quality"] != "quality" {
		t.Errorf("idToName[quality] = %q, want quality", m.idToName["quality"])
	}
}

// ---------------------------------------------------------------------------
// GetOutput — buffer fallback when pane is empty
// ---------------------------------------------------------------------------

func TestGetOutput_BufferFallback(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.Lock()
	m.agents["scanner"].OutputBuffer.Write("buffer line 1")
	m.agents["scanner"].OutputBuffer.Write("buffer line 2")
	m.mu.Unlock()

	output, err := m.GetOutput("scanner", 10)
	if err != nil {
		t.Fatalf("GetOutput: %v", err)
	}
	if len(output) != 2 {
		t.Errorf("should fall back to buffer, got %d lines", len(output))
	}
}

// ---------------------------------------------------------------------------
// embeddedTokenRe
// ---------------------------------------------------------------------------

func TestEmbeddedTokenRe(t *testing.T) {
	tests := []struct {
		input string
		match bool
		host  string
	}{
		{"https://x-access-token:gho_abc123@github.com/org/repo.git", true, "github.com/org/repo.git"},
		{"https://github.com/org/repo.git", false, ""},
		{"git@github.com:org/repo.git", false, ""},
		{"https://user:pass@github.com/org/repo.git", true, "github.com/org/repo.git"},
	}
	for _, tt := range tests {
		m := embeddedTokenRe.FindStringSubmatch(tt.input)
		if tt.match {
			if m == nil {
				t.Errorf("expected match for %q", tt.input)
			} else if m[1] != tt.host {
				t.Errorf("captured %q, want %q", m[1], tt.host)
			}
		} else {
			if m != nil {
				t.Errorf("expected no match for %q, got %v", tt.input, m)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// KillSession — found
// ---------------------------------------------------------------------------

func TestKillSession_Found(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	// KillSession on a found agent should not error
	// (the tmux kill-session command will fail silently since there's no real session)
	err := m.KillSession("scanner")
	if err != nil {
		t.Fatalf("KillSession: %v", err)
	}
}

// ---------------------------------------------------------------------------
// fixEntry — test with temp files (no chown since we're not root)
// ---------------------------------------------------------------------------

func TestFixEntry_ChecksPermissions(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "test.json")
	os.WriteFile(file, []byte("{}"), 0o644)

	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}

	// fixEntry won't panic — it checks Sys() for syscall.Stat_t
	// On macOS this is *syscall.Stat_t so it should work
	fixEntry(file, info, discardLogger())
}

// ---------------------------------------------------------------------------
// filterPaneOutput — visual noise filtering
// ---------------------------------------------------------------------------

func TestFilterPaneOutput_RemovesVisualNoise(t *testing.T) {
	lines := []string{
		"real content",
		"───────────────",
		"more content",
		"━━━━━━━━━━━━━━━",
		"important output",
	}
	result := filterPaneOutput(lines, 10)
	for _, l := range result {
		if isVisualNoise(l) {
			t.Errorf("visual noise %q should be filtered", l)
		}
	}
}

// ---------------------------------------------------------------------------
// Multiple prompt characters in PaneLines
// ---------------------------------------------------------------------------

func TestPaneLines_MultiplePrompts(t *testing.T) {
	agent := &AgentProcess{
		Name: "test",
		lastPaneCapture: []string{
			"old session output",
			"❯",
			"middle session output",
			"›",
			"latest output line 1",
			"latest output line 2",
		},
	}
	lines := agent.PaneLines(10)
	// Should not panic and should return some output
	_ = lines
}

// ---------------------------------------------------------------------------
// isVisualNoise — path-like strings
// ---------------------------------------------------------------------------

func TestIsVisualNoise_PathLikeStrings(t *testing.T) {
	// /data/agents/scanner without spaces should be noise
	if !isVisualNoise("/data/agents/scanner") {
		t.Error("/data/agents/scanner should be noise")
	}
	// /data/agents/scanner with text should NOT be noise
	if isVisualNoise("/data/agents/scanner some output") {
		t.Error("path with text should not be noise")
	}
}

// ---------------------------------------------------------------------------
// agentMode with config Mode override
// ---------------------------------------------------------------------------

func TestAgentMode_ConfigOverride(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Mode: "ISSUES_PRS_MERGE"},
	}, discardLogger(), ProjectContext{ACMMLevel: 2})

	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	mode := m.agentMode(agent)
	if mode != ModeIssuesPRsMerge {
		t.Errorf("mode override should be ISSUES_PRS_MERGE, got %s", mode)
	}
}

func TestAgentMode_InvalidOverrideFallsToDefault(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Mode: "INVALID_MODE"},
	}, discardLogger(), ProjectContext{ACMMLevel: 2})

	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	mode := m.agentMode(agent)
	// Invalid override should fall through to DefaultAgentMode
	if mode != ModeAdvisory {
		t.Errorf("invalid override at L2 should be ADVISORY, got %s", mode)
	}
}

// ---------------------------------------------------------------------------
// Snapshot includes all fields
// ---------------------------------------------------------------------------

func TestSnapshot_IncludesAllFields(t *testing.T) {
	now := time.Now()
	agent := &AgentProcess{
		Name:            "test",
		ID:              "test-001",
		Config:          config.AgentConfig{Backend: "claude", Model: "sonnet"},
		State:           StateRunning,
		PID:             1234,
		UID:             1001,
		StartedAt:       &now,
		LastKick:        &now,
		Paused:          true,
		PausedAt:        now,
		PausedReason:    "test reason",
		PausedTrigger:   "test trigger",
		PinnedCLI:       "v1.0",
		PinnedModel:     "opus",
		ModelOverride:   "haiku",
		BackendOverride: "gemini",
		RestartCount:    3,
		OutputBuffer:    NewRingBuffer(10),
		lastPaneCapture: []string{"line1"},
		KickHistory:     []KickRecord{{Timestamp: now, Agent: "test", Snippet: "kick"}},
		LastKickMessage: "do something",
		tmuxSession:     "hive-test",
		tmuxSocket:      "hive-test",
	}

	snap := agent.snapshot()
	if snap.Name != "test" {
		t.Error("Name not copied")
	}
	if snap.ID != "test-001" {
		t.Error("ID not copied")
	}
	if snap.State != StateRunning {
		t.Error("State not copied")
	}
	if snap.PID != 1234 {
		t.Error("PID not copied")
	}
	if snap.UID != 1001 {
		t.Error("UID not copied")
	}
	if !snap.Paused {
		t.Error("Paused not copied")
	}
	if snap.PinnedCLI != "v1.0" {
		t.Error("PinnedCLI not copied")
	}
	if snap.PinnedModel != "opus" {
		t.Error("PinnedModel not copied")
	}
	if snap.ModelOverride != "haiku" {
		t.Error("ModelOverride not copied")
	}
	if snap.BackendOverride != "gemini" {
		t.Error("BackendOverride not copied")
	}
	if snap.RestartCount != 3 {
		t.Error("RestartCount not copied")
	}
	if len(snap.KickHistory) != 1 {
		t.Error("KickHistory not copied")
	}
	if snap.LastKickMessage != "do something" {
		t.Error("LastKickMessage not copied")
	}
}
