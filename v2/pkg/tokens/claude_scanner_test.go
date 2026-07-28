package tokens

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseClaudeSessionFile_NestedFormat(t *testing.T) {
	dir := t.TempDir()

	// Claude Code native format: nested message.usage
	content := strings.Join([]string{
		`{"type":"human","timestamp":"2025-01-01T00:00:00Z","message":{"text":"scanner triage all open bugs"}}`,
		`{"type":"assistant","timestamp":"2025-01-01T00:01:00Z","message":{"model":"claude-sonnet-4-20250514","usage":{"input_tokens":200,"output_tokens":100,"cache_read_input_tokens":30,"cache_creation_input_tokens":15}}}`,
		`{"type":"human","timestamp":"2025-01-01T00:02:00Z","message":{"text":"what did you find?"}}`,
		`{"type":"assistant","timestamp":"2025-01-01T00:03:00Z","message":{"model":"claude-sonnet-4-20250514","usage":{"input_tokens":150,"output_tokens":80,"cache_read_input_tokens":25,"cache_creation_input_tokens":10}}}`,
	}, "\n") + "\n"

	path := filepath.Join(dir, "session-abc.jsonl")
	os.WriteFile(path, []byte(content), 0o600)

	summary, err := parseClaudeSessionFile(path, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("parseClaudeSessionFile error: %v", err)
	}

	if summary.SessionID != "session-abc" {
		t.Errorf("SessionID = %q, want 'session-abc'", summary.SessionID)
	}
	if summary.Agent != "scanner" {
		t.Errorf("Agent = %q, want 'scanner'", summary.Agent)
	}
	if summary.Model != "claude-sonnet-4-20250514" {
		t.Errorf("Model = %q, want 'claude-sonnet-4-20250514'", summary.Model)
	}

	// InputTokens = 200 + 150 = 350
	wantInput := int64(350)
	if summary.InputTokens != wantInput {
		t.Errorf("InputTokens = %d, want %d", summary.InputTokens, wantInput)
	}
	// OutputTokens = 100 + 80 = 180
	wantOutput := int64(180)
	if summary.OutputTokens != wantOutput {
		t.Errorf("OutputTokens = %d, want %d", summary.OutputTokens, wantOutput)
	}
	// CacheRead = 30 + 25 = 55
	if summary.CacheRead != 55 {
		t.Errorf("CacheRead = %d, want 55", summary.CacheRead)
	}
	// CacheCreate = 15 + 10 = 25
	if summary.CacheCreate != 25 {
		t.Errorf("CacheCreate = %d, want 25", summary.CacheCreate)
	}
	// TotalTokens = 350 + 180 + 55 + 25 = 610
	if summary.TotalTokens != 610 {
		t.Errorf("TotalTokens = %d, want 610", summary.TotalTokens)
	}
	// Messages = 2 human + 2 assistant = 4
	if summary.Messages != 4 {
		t.Errorf("Messages = %d, want 4", summary.Messages)
	}
}

func TestParseClaudeSessionFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	os.WriteFile(path, []byte(""), 0o600)

	summary, err := parseClaudeSessionFile(path, nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if summary.TotalTokens != 0 {
		t.Errorf("TotalTokens = %d, want 0", summary.TotalTokens)
	}
	if summary.Agent != "unknown" {
		t.Errorf("Agent = %q, want 'unknown'", summary.Agent)
	}
}

func TestParseClaudeSessionFile_FlatFormatFallback(t *testing.T) {
	dir := t.TempDir()

	// Flat format (same as the existing collector expects)
	content := strings.Join([]string{
		`{"role":"user","message":"scanner triage all open bugs"}`,
		`{"model":"gpt-4","input_tokens":100,"output_tokens":50}`,
		`{"role":"assistant","message":"done"}`,
	}, "\n") + "\n"

	path := filepath.Join(dir, "flat-session.jsonl")
	os.WriteFile(path, []byte(content), 0o600)

	summary, err := parseClaudeSessionFile(path, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if summary.Agent != "scanner" {
		t.Errorf("Agent = %q, want 'scanner'", summary.Agent)
	}
	if summary.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100", summary.InputTokens)
	}
	if summary.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", summary.OutputTokens)
	}
	if summary.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d, want 150", summary.TotalTokens)
	}
}

func TestParseClaudeSessionFile_MixedFormats(t *testing.T) {
	dir := t.TempDir()

	// Mix of nested Claude format and flat entries
	content := strings.Join([]string{
		`{"type":"human","timestamp":"2025-01-01T00:00:00Z","message":{"text":"triage bugs"}}`,
		`{"type":"assistant","timestamp":"2025-01-01T00:01:00Z","message":{"model":"opus","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":10,"cache_creation_input_tokens":5}}}`,
		`{"model":"opus","input_tokens":50,"output_tokens":25}`,
	}, "\n") + "\n"

	path := filepath.Join(dir, "mixed.jsonl")
	os.WriteFile(path, []byte(content), 0o600)

	summary, err := parseClaudeSessionFile(path, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	// Nested: 100 input + 50 output + 10 cache_read + 5 cache_create
	// Flat: 50 input + 25 output
	wantInput := int64(150)
	wantOutput := int64(75)
	if summary.InputTokens != wantInput {
		t.Errorf("InputTokens = %d, want %d", summary.InputTokens, wantInput)
	}
	if summary.OutputTokens != wantOutput {
		t.Errorf("OutputTokens = %d, want %d", summary.OutputTokens, wantOutput)
	}
}

func TestParseClaudeSessionFile_Timestamp(t *testing.T) {
	dir := t.TempDir()

	content := `{"type":"assistant","timestamp":"2025-06-15T10:30:00Z","message":{"model":"sonnet","usage":{"input_tokens":10,"output_tokens":5}}}` + "\n"
	path := filepath.Join(dir, "ts.jsonl")
	os.WriteFile(path, []byte(content), 0o600)

	summary, err := parseClaudeSessionFile(path, nil)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if summary.LastActive == 0 {
		t.Error("LastActive should be non-zero")
	}
}

func TestScanClaudeSessions_ProjectDirStructure(t *testing.T) {
	// Create a mock Claude projects directory structure
	projectsDir := t.TempDir()
	projectHash := filepath.Join(projectsDir, "-data-agents-scanner")
	os.MkdirAll(projectHash, 0o755)

	content := strings.Join([]string{
		`{"type":"human","timestamp":"2025-01-01T00:00:00Z","message":{"text":"triage all open bugs"}}`,
		`{"type":"assistant","timestamp":"2025-01-01T00:01:00Z","message":{"model":"sonnet","usage":{"input_tokens":200,"output_tokens":100}}}`,
	}, "\n") + "\n"

	os.WriteFile(filepath.Join(projectHash, "session1.jsonl"), []byte(content), 0o600)

	agg, err := ScanClaudeSessionsWithPathDetection(projectsDir)
	if err != nil {
		t.Fatalf("ScanClaudeSessionsWithPathDetection error: %v", err)
	}

	if agg.SessionCount != 1 {
		t.Fatalf("SessionCount = %d, want 1", agg.SessionCount)
	}

	// Agent should be detected from the directory name
	if agg.Sessions[0].Agent != "scanner" {
		t.Errorf("Agent = %q, want 'scanner' (from path)", agg.Sessions[0].Agent)
	}
}

func TestScanClaudeSessions_SubagentFiles(t *testing.T) {
	projectsDir := t.TempDir()
	projectHash := filepath.Join(projectsDir, "some-project-hash")
	subagentDir := filepath.Join(projectHash, "subagents")
	os.MkdirAll(subagentDir, 0o755)

	content := `{"type":"assistant","timestamp":"2025-01-01T00:00:00Z","message":{"model":"haiku","usage":{"input_tokens":50,"output_tokens":20}}}` + "\n"
	os.WriteFile(filepath.Join(subagentDir, "sub1.jsonl"), []byte(content), 0o600)

	agg, err := ScanClaudeSessions(projectsDir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	if agg.SessionCount != 1 {
		t.Errorf("SessionCount = %d, want 1 (subagent file)", agg.SessionCount)
	}
	if agg.TotalTokens != 70 {
		t.Errorf("TotalTokens = %d, want 70", agg.TotalTokens)
	}
}

func TestScanClaudeSessions_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	agg, err := ScanClaudeSessions(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if agg.SessionCount != 0 {
		t.Errorf("SessionCount = %d, want 0", agg.SessionCount)
	}
}

func TestScanClaudeSessions_EmptyPath(t *testing.T) {
	agg, err := ScanClaudeSessions("", DefaultAgentDetector)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if agg.SessionCount != 0 {
		t.Errorf("SessionCount = %d, want 0", agg.SessionCount)
	}
}

func TestMergeAggregates_Deduplication(t *testing.T) {
	dst := &AggregateSummary{
		ByAgent:       map[string]int64{"scanner": 100},
		ByModel:       map[string]int64{"sonnet": 100},
		ByAgentDetail: map[string]*AgentModelBucket{"scanner": {Input: 50, Output: 50, Sessions: 1}},
		ByModelDetail: map[string]*AgentModelBucket{"sonnet": {Input: 50, Output: 50, Sessions: 1}},
		Sessions:      []SessionSummary{{SessionID: "dup-session", Agent: "scanner", TotalTokens: 100}},
		TotalTokens:   100,
		SessionCount:  1,
	}

	src := &AggregateSummary{
		ByAgent:       map[string]int64{"scanner": 100},
		ByModel:       map[string]int64{"sonnet": 100},
		ByAgentDetail: map[string]*AgentModelBucket{"scanner": {Input: 50, Output: 50, Sessions: 1}},
		ByModelDetail: map[string]*AgentModelBucket{"sonnet": {Input: 50, Output: 50, Sessions: 1}},
		Sessions:      []SessionSummary{{SessionID: "dup-session", Agent: "scanner", TotalTokens: 100}},
		TotalTokens:   100,
		SessionCount:  1,
	}

	MergeAggregates(dst, src)

	// Should not double-count the duplicate session
	if dst.SessionCount != 1 {
		t.Errorf("SessionCount = %d, want 1 (deduped)", dst.SessionCount)
	}
	if dst.TotalTokens != 100 {
		t.Errorf("TotalTokens = %d, want 100 (deduped)", dst.TotalTokens)
	}
}

func TestMergeAggregates_NewSessions(t *testing.T) {
	dst := &AggregateSummary{
		ByAgent:       map[string]int64{"scanner": 100},
		ByModel:       map[string]int64{"sonnet": 100},
		ByAgentDetail: map[string]*AgentModelBucket{"scanner": {Input: 50, Output: 50, Sessions: 1}},
		ByModelDetail: map[string]*AgentModelBucket{"sonnet": {Input: 50, Output: 50, Sessions: 1}},
		Sessions:      []SessionSummary{{SessionID: "session-a", Agent: "scanner", Model: "sonnet", InputTokens: 50, OutputTokens: 50, TotalTokens: 100}},
		TotalTokens:   100,
		TotalInput:    50,
		TotalOutput:   50,
		SessionCount:  1,
	}

	src := &AggregateSummary{
		ByAgent:       map[string]int64{"architect": 200},
		ByModel:       map[string]int64{"opus": 200},
		ByAgentDetail: map[string]*AgentModelBucket{"architect": {Input: 100, Output: 100, Sessions: 1}},
		ByModelDetail: map[string]*AgentModelBucket{"opus": {Input: 100, Output: 100, Sessions: 1}},
		Sessions:      []SessionSummary{{SessionID: "session-b", Agent: "architect", Model: "opus", InputTokens: 100, OutputTokens: 100, TotalTokens: 200}},
		TotalTokens:   200,
		TotalInput:    100,
		TotalOutput:   100,
		SessionCount:  1,
	}

	MergeAggregates(dst, src)

	if dst.SessionCount != 2 {
		t.Errorf("SessionCount = %d, want 2", dst.SessionCount)
	}
	if dst.TotalTokens != 300 {
		t.Errorf("TotalTokens = %d, want 300", dst.TotalTokens)
	}
	if dst.ByAgent["architect"] != 200 {
		t.Errorf("ByAgent[architect] = %d, want 200", dst.ByAgent["architect"])
	}
}

func TestMergeAggregates_NilSrc(t *testing.T) {
	dst := &AggregateSummary{
		ByAgent:       map[string]int64{},
		ByModel:       map[string]int64{},
		ByAgentDetail: map[string]*AgentModelBucket{},
		ByModelDetail: map[string]*AgentModelBucket{},
		SessionCount:  0,
	}
	MergeAggregates(dst, nil)
	if dst.SessionCount != 0 {
		t.Errorf("SessionCount = %d after nil merge", dst.SessionCount)
	}
}

func TestAgentFromTmuxEnv(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/root/.claude/projects/-data-agents-scanner/session.jsonl", "scanner"},
		{"/root/.claude/projects/-data-agents-ci-maintainer/session.jsonl", "ci"},
		{"/root/.claude/projects/-data-agents-architect/session.jsonl", "architect"},
		{"/root/.claude/projects/some-random-hash/session.jsonl", ""},
		{"/root/.claude/projects/-home-dev-kubestellar-console/session.jsonl", ""},
	}

	for _, tt := range tests {
		got := AgentFromTmuxEnv(tt.path)
		if got != tt.want {
			t.Errorf("AgentFromTmuxEnv(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestParseTimestampToUnixMilli(t *testing.T) {
	tests := []struct {
		input    string
		wantNon0 bool
	}{
		{"2025-01-01T00:00:00Z", true},
		{"2025-01-01T00:00:00+00:00", true},
		{"2025-06-15T10:30:00.123456789Z", true},
		{"", false},
		{"invalid", false},
	}

	for _, tt := range tests {
		got := parseTimestampToUnixMilli(tt.input)
		if tt.wantNon0 && got == 0 {
			t.Errorf("parseTimestampToUnixMilli(%q) = 0, want non-zero", tt.input)
		}
		if !tt.wantNon0 && got != 0 {
			t.Errorf("parseTimestampToUnixMilli(%q) = %d, want 0", tt.input, got)
		}
	}
}

func TestHiveAgentDetector_PathBased(t *testing.T) {
	detector := HiveAgentDetector("/root/.claude/projects/-data-agents-scanner/session.jsonl")
	got := detector("random message without keywords")
	if got != "scanner" {
		t.Errorf("HiveAgentDetector (path) = %q, want 'scanner'", got)
	}
}

func TestHiveAgentDetector_FallbackToKeyword(t *testing.T) {
	detector := HiveAgentDetector("/root/.claude/projects/random-hash/session.jsonl")
	got := detector("triage all open issues")
	if got != "scanner" {
		t.Errorf("HiveAgentDetector (keyword) = %q, want 'scanner'", got)
	}
}

func TestCollectorSetClaudeSessionsDir(t *testing.T) {
	c := NewCollector("/tmp/metrics", testLogger())
	c.SetClaudeSessionsDir("/root/.claude/projects")
	if c.claudeSessionsDir != "/root/.claude/projects" {
		t.Errorf("claudeSessionsDir = %q", c.claudeSessionsDir)
	}
}

// TestCollectorCopilotLiveCapture verifies the collector honors the live-capture
// flag: with it set, the copilot scanner defers token accrual to the sink, so the
// scanned copilot session contributes no tokens (but is still counted), and the
// per-agent live usage the sink wrote is what surfaces instead.
func TestCollectorCopilotLiveCapture(t *testing.T) {
	metricsDir := t.TempDir()
	copilotDir := t.TempDir()

	// A live copilot usage record from the proxy sink (per-response).
	sink := NewInferenceSink(metricsDir, testLogger())
	sink.Record("scanner", "gpt-5", 300, 100)

	// A copilot session file whose shutdown metrics would double-count if the
	// scanner accrued them.
	sessionDir := filepath.Join(copilotDir, "s1")
	os.MkdirAll(sessionDir, 0o755)
	content := strings.Join([]string{
		`{"type":"session.start","data":{"sessionId":"abc123456789","selectedModel":"gpt-5","context":{"cwd":"/data/agents/scanner"}}}`,
		`{"type":"user.message","timestamp":"2025-06-01T00:00:00Z","data":{"content":"go"}}`,
		`{"type":"session.shutdown","data":{"currentModel":"gpt-5","modelMetrics":{"gpt-5":{"usage":{"inputTokens":300,"outputTokens":100}}}}}`,
	}, "\n") + "\n"
	os.WriteFile(filepath.Join(sessionDir, "events.jsonl"), []byte(content), 0o600)

	c := NewCollector(metricsDir, testLogger())
	c.SetCopilotSessionsDir(copilotDir)
	c.SetCopilotLiveCapture(true)
	c.scan()

	summary := c.Summary()
	if summary == nil {
		t.Fatal("nil summary")
	}
	// scanner's tokens come ONLY from the sink (400), not doubled by the
	// shutdown scan (which would make it 800).
	if got := summary.ByAgent["scanner"]; got != 400 {
		t.Fatalf("ByAgent[scanner] = %d, want 400 (live sink only, no double-count)", got)
	}
}

func TestCollectorScanWithClaudeSessions(t *testing.T) {
	metricsDir := t.TempDir()
	claudeDir := t.TempDir()

	// Create a flat-format session in metrics dir
	flatContent := strings.Join([]string{
		`{"role":"user","message":"scanner triage"}`,
		`{"model":"gpt-4","input_tokens":100,"output_tokens":50}`,
	}, "\n") + "\n"
	os.WriteFile(filepath.Join(metricsDir, "flat.jsonl"), []byte(flatContent), 0o600)

	// Create a Claude-format session in claude projects dir
	projectHash := filepath.Join(claudeDir, "-data-agents-architect")
	os.MkdirAll(projectHash, 0o755)
	claudeContent := strings.Join([]string{
		`{"type":"human","timestamp":"2025-01-01T00:00:00Z","message":{"text":"architect a new rfc"}}`,
		`{"type":"assistant","timestamp":"2025-01-01T00:01:00Z","message":{"model":"opus","usage":{"input_tokens":200,"output_tokens":100}}}`,
	}, "\n") + "\n"
	os.WriteFile(filepath.Join(projectHash, "claude-session.jsonl"), []byte(claudeContent), 0o600)

	c := NewCollector(metricsDir, testLogger())
	c.SetClaudeSessionsDir(claudeDir)
	c.scan()

	summary := c.Summary()
	if summary == nil {
		t.Fatal("expected non-nil summary after scan with Claude sessions")
	}

	if summary.SessionCount != 2 {
		t.Errorf("SessionCount = %d, want 2 (1 flat + 1 Claude)", summary.SessionCount)
	}

	// Check both agents are present
	if _, ok := summary.ByAgent["scanner"]; !ok {
		t.Error("missing 'scanner' agent from flat-format session")
	}
	if _, ok := summary.ByAgent["architect"]; !ok {
		t.Error("missing 'architect' agent from Claude-format session")
	}

	// Total tokens should be sum of both
	wantTotal := int64(150 + 300) // flat: 100+50, claude: 200+100
	if summary.TotalTokens != wantTotal {
		t.Errorf("TotalTokens = %d, want %d", summary.TotalTokens, wantTotal)
	}
}
