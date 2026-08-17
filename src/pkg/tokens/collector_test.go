package tokens

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jsonlLine builds a minimal JSONL line for session entries used in tests.
func jsonlLine(fields map[string]interface{}) string {
	parts := []string{}
	for k, v := range fields {
		switch val := v.(type) {
		case string:
			parts = append(parts, fmt.Sprintf("%q:%q", k, val))
		case int:
			parts = append(parts, fmt.Sprintf("%q:%d", k, val))
		case int64:
			parts = append(parts, fmt.Sprintf("%q:%d", k, val))
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// writeFile writes content to a file in dir with the given name.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeFile %s: %v", path, err)
	}
	return path
}

// ---------------------------------------------------------------------------
// CollectFromDir
// ---------------------------------------------------------------------------

func TestCollectFromDir_MultipleFiles(t *testing.T) {
	dir := t.TempDir()

	// session-a: scanner agent (has "scanner" keyword), model gpt-4
	sessionA := strings.Join([]string{
		`{"role":"user","message":"scanner triage all open bugs"}`,
		`{"model":"gpt-4","input_tokens":100,"output_tokens":50}`,
		`{"role":"assistant","message":"done"}`,
	}, "\n") + "\n"

	// session-b: ci-maintainer agent (has "review" keyword), model gpt-3.5
	sessionB := strings.Join([]string{
		`{"role":"user","message":"review the latest ci coverage"}`,
		`{"model":"gpt-3.5","input_tokens":200,"output_tokens":80}`,
		`{"role":"assistant","message":"lgtm"}`,
	}, "\n") + "\n"

	writeFile(t, dir, "session-a.jsonl", sessionA)
	writeFile(t, dir, "session-b.jsonl", sessionB)

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir error: %v", err)
	}

	if agg.SessionCount != 2 {
		t.Errorf("SessionCount = %d, want 2", agg.SessionCount)
	}

	wantTotal := int64((100 + 50) + (200 + 80))
	if agg.TotalTokens != wantTotal {
		t.Errorf("TotalTokens = %d, want %d", agg.TotalTokens, wantTotal)
	}

	// Both sessions should appear in ByAgent
	if _, ok := agg.ByAgent["scanner"]; !ok {
		t.Error("ByAgent missing 'scanner'")
	}
	if _, ok := agg.ByAgent["ci-maintainer"]; !ok {
		t.Error("ByAgent missing 'ci-maintainer'")
	}

	// Both models should appear in ByModel
	if _, ok := agg.ByModel["gpt-4"]; !ok {
		t.Error("ByModel missing 'gpt-4'")
	}
	if _, ok := agg.ByModel["gpt-3.5"]; !ok {
		t.Error("ByModel missing 'gpt-3.5'")
	}

	if len(agg.Sessions) != 2 {
		t.Errorf("len(Sessions) = %d, want 2", len(agg.Sessions))
	}
}

func TestCollectFromDir_SkipsZeroTokenFiles(t *testing.T) {
	dir := t.TempDir()

	// zero-token session: only user message, no token counts
	zeroSession := `{"role":"user","message":"hello"}` + "\n"
	// non-zero session
	realSession := strings.Join([]string{
		`{"role":"user","message":"scanner run"}`,
		`{"model":"gpt-4","input_tokens":10,"output_tokens":5}`,
	}, "\n") + "\n"

	writeFile(t, dir, "zero.jsonl", zeroSession)
	writeFile(t, dir, "real.jsonl", realSession)

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir error: %v", err)
	}

	if agg.SessionCount != 1 {
		t.Errorf("SessionCount = %d, want 1 (zero-token file should be skipped)", agg.SessionCount)
	}
}

func TestCollectFromDir_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir error: %v", err)
	}

	if agg.SessionCount != 0 {
		t.Errorf("SessionCount = %d, want 0", agg.SessionCount)
	}
	if agg.TotalTokens != 0 {
		t.Errorf("TotalTokens = %d, want 0", agg.TotalTokens)
	}
	if len(agg.Sessions) != 0 {
		t.Errorf("len(Sessions) = %d, want 0", len(agg.Sessions))
	}
}

func TestCollectFromDir_IgnoresNonJSONLFiles(t *testing.T) {
	dir := t.TempDir()

	// .json (not .jsonl) should be ignored
	writeFile(t, dir, "session.json", `{"model":"gpt-4","input_tokens":100,"output_tokens":50}`)
	// .txt should be ignored
	writeFile(t, dir, "session.txt", "some text")

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir error: %v", err)
	}

	if agg.SessionCount != 0 {
		t.Errorf("SessionCount = %d, want 0 (non-.jsonl files should be ignored)", agg.SessionCount)
	}
}

func TestCollectFromDir_NilAgentDetector(t *testing.T) {
	dir := t.TempDir()

	content := strings.Join([]string{
		`{"role":"user","message":"scanner triage"}`,
		`{"model":"gpt-4","input_tokens":50,"output_tokens":20}`,
	}, "\n") + "\n"
	writeFile(t, dir, "session.jsonl", content)

	// nil detector: agent should remain "unknown"
	agg, err := CollectFromDir(dir, nil)
	if err != nil {
		t.Fatalf("CollectFromDir error: %v", err)
	}

	if agg.SessionCount != 1 {
		t.Fatalf("SessionCount = %d, want 1", agg.SessionCount)
	}
	if agg.Sessions[0].Agent != "unknown" {
		t.Errorf("Agent = %q, want 'unknown' when detector is nil", agg.Sessions[0].Agent)
	}
}

func TestCollectFromDir_AggregationByAgentAndModel(t *testing.T) {
	dir := t.TempDir()

	// Two sessions with the same agent and model
	sessionA := strings.Join([]string{
		`{"role":"user","message":"scanner triage"}`,
		`{"model":"gpt-4","input_tokens":100,"output_tokens":40}`,
	}, "\n") + "\n"
	sessionB := strings.Join([]string{
		`{"role":"user","message":"scanner triage again"}`,
		`{"model":"gpt-4","input_tokens":60,"output_tokens":20}`,
	}, "\n") + "\n"

	writeFile(t, dir, "a.jsonl", sessionA)
	writeFile(t, dir, "b.jsonl", sessionB)

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir error: %v", err)
	}

	// scanner: (100+40) + (60+20) = 220
	wantScannerTokens := int64(140 + 80)
	if agg.ByAgent["scanner"] != wantScannerTokens {
		t.Errorf("ByAgent[scanner] = %d, want %d", agg.ByAgent["scanner"], wantScannerTokens)
	}

	// gpt-4: same total
	if agg.ByModel["gpt-4"] != wantScannerTokens {
		t.Errorf("ByModel[gpt-4] = %d, want %d", agg.ByModel["gpt-4"], wantScannerTokens)
	}
}

// ---------------------------------------------------------------------------
// parseSessionFile
// ---------------------------------------------------------------------------

func TestParseSessionFile_ExtractsModelFromFirstEntry(t *testing.T) {
	dir := t.TempDir()

	content := strings.Join([]string{
		`{"model":"claude-3-opus","input_tokens":10,"output_tokens":5}`,
		`{"model":"claude-3-haiku","input_tokens":20,"output_tokens":8}`, // second — should be ignored
	}, "\n") + "\n"
	path := writeFile(t, dir, "my-session.jsonl", content)

	summary, err := parseSessionFile(path, nil)
	if err != nil {
		t.Fatalf("parseSessionFile error: %v", err)
	}

	if summary.Model != "claude-3-opus" {
		t.Errorf("Model = %q, want 'claude-3-opus'", summary.Model)
	}
}

func TestParseSessionFile_SessionIDFromFilename(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "abc-123.jsonl", `{"model":"gpt-4","input_tokens":1}`+"\n")

	summary, err := parseSessionFile(path, nil)
	if err != nil {
		t.Fatalf("parseSessionFile error: %v", err)
	}

	if summary.SessionID != "abc-123" {
		t.Errorf("SessionID = %q, want 'abc-123'", summary.SessionID)
	}
}

func TestParseSessionFile_SumsInputTokensWithCache(t *testing.T) {
	dir := t.TempDir()

	// InputTokens = input_tokens only; cache tracked separately
	content := strings.Join([]string{
		`{"model":"gpt-4","input_tokens":100,"cache_creation":30,"cache_read":20,"output_tokens":50}`,
		`{"input_tokens":10,"cache_creation":5,"cache_read":5,"output_tokens":10}`,
	}, "\n") + "\n"
	path := writeFile(t, dir, "session.jsonl", content)

	summary, err := parseSessionFile(path, nil)
	if err != nil {
		t.Fatalf("parseSessionFile error: %v", err)
	}

	// input_tokens only: 100+10=110
	wantInput := int64(110)
	if summary.InputTokens != wantInput {
		t.Errorf("InputTokens = %d, want %d", summary.InputTokens, wantInput)
	}

	// output: 50+10=60
	wantOutput := int64(60)
	if summary.OutputTokens != wantOutput {
		t.Errorf("OutputTokens = %d, want %d", summary.OutputTokens, wantOutput)
	}

	// cache_read: 20+5=25
	wantCacheRead := int64(25)
	if summary.CacheRead != wantCacheRead {
		t.Errorf("CacheRead = %d, want %d", summary.CacheRead, wantCacheRead)
	}

	// cache_create: 30+5=35
	wantCacheCreate := int64(35)
	if summary.CacheCreate != wantCacheCreate {
		t.Errorf("CacheCreate = %d, want %d", summary.CacheCreate, wantCacheCreate)
	}

	// TotalTokens = 110 + 60 + 25 + 35 = 230
	wantTotal := int64(230)
	if summary.TotalTokens != wantTotal {
		t.Errorf("TotalTokens = %d, want %d", summary.TotalTokens, wantTotal)
	}
}

func TestParseSessionFile_CountsUserAndAssistantMessages(t *testing.T) {
	dir := t.TempDir()

	content := strings.Join([]string{
		`{"role":"user","message":"hello"}`,
		`{"role":"assistant","message":"hi"}`,
		`{"role":"user","message":"how are you"}`,
		`{"role":"assistant","message":"fine"}`,
		`{"role":"system","message":"sys msg"}`, // system: should not be counted
		`{"model":"gpt-4","input_tokens":10,"output_tokens":5}`,
	}, "\n") + "\n"
	path := writeFile(t, dir, "session.jsonl", content)

	summary, err := parseSessionFile(path, nil)
	if err != nil {
		t.Fatalf("parseSessionFile error: %v", err)
	}

	// 2 user + 2 assistant = 4
	if summary.Messages != 4 {
		t.Errorf("Messages = %d, want 4", summary.Messages)
	}
}

func TestParseSessionFile_AgentDetectedFromFirstUserMessage(t *testing.T) {
	dir := t.TempDir()

	content := strings.Join([]string{
		`{"role":"user","message":"Please run the supervisor sweep now"}`,
		`{"model":"gpt-4","input_tokens":10,"output_tokens":5}`,
	}, "\n") + "\n"
	path := writeFile(t, dir, "session.jsonl", content)

	summary, err := parseSessionFile(path, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("parseSessionFile error: %v", err)
	}

	if summary.Agent != "supervisor" {
		t.Errorf("Agent = %q, want 'supervisor'", summary.Agent)
	}
}

func TestParseSessionFile_UsesFirstUserMessageNotLater(t *testing.T) {
	dir := t.TempDir()

	// First user message triggers "scanner"; second mentions "ci-maintainer" — agent must be scanner
	content := strings.Join([]string{
		`{"role":"user","message":"scanner triage open bugs"}`,
		`{"role":"assistant","message":"ok"}`,
		`{"role":"user","message":"now do a ci-maintainer review"}`,
		`{"model":"gpt-4","input_tokens":10,"output_tokens":5}`,
	}, "\n") + "\n"
	path := writeFile(t, dir, "session.jsonl", content)

	summary, err := parseSessionFile(path, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("parseSessionFile error: %v", err)
	}

	if summary.Agent != "scanner" {
		t.Errorf("Agent = %q, want 'scanner'", summary.Agent)
	}
}

func TestParseSessionFile_SkipsMalformedLines(t *testing.T) {
	dir := t.TempDir()

	content := strings.Join([]string{
		`{"model":"gpt-4","input_tokens":100,"output_tokens":50}`,
		`not valid json {{{{`,
		`{"input_tokens":20,"output_tokens":10}`,
		``,
		`{broken`,
	}, "\n") + "\n"
	path := writeFile(t, dir, "session.jsonl", content)

	summary, err := parseSessionFile(path, nil)
	if err != nil {
		t.Fatalf("parseSessionFile error: %v", err)
	}

	// Only valid lines contribute
	wantInput := int64(100 + 20)
	if summary.InputTokens != wantInput {
		t.Errorf("InputTokens = %d, want %d", summary.InputTokens, wantInput)
	}
	wantOutput := int64(50 + 10)
	if summary.OutputTokens != wantOutput {
		t.Errorf("OutputTokens = %d, want %d", summary.OutputTokens, wantOutput)
	}
}

func TestParseSessionFile_LargeBufferForLongLines(t *testing.T) {
	dir := t.TempDir()

	// Build a line with a very long message (> default scanner buffer of 64 KB)
	longMsg := strings.Repeat("x", 9*1024*1024) // 9 MB
	line := fmt.Sprintf(`{"role":"user","message":%q}`, longMsg)
	tokenLine := `{"model":"gpt-4","input_tokens":1,"output_tokens":1}`
	content := line + "\n" + tokenLine + "\n"
	path := writeFile(t, dir, "large.jsonl", content)

	summary, err := parseSessionFile(path, nil)
	if err != nil {
		t.Fatalf("parseSessionFile error on large line: %v", err)
	}

	if summary.TotalTokens != 2 {
		t.Errorf("TotalTokens = %d, want 2", summary.TotalTokens)
	}
}

func TestParseSessionFile_NonexistentFile(t *testing.T) {
	_, err := parseSessionFile("/nonexistent/path/file.jsonl", nil)
	if err == nil {
		t.Error("expected error for nonexistent file, got nil")
	}
}

func TestParseSessionFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "empty.jsonl", "")

	summary, err := parseSessionFile(path, nil)
	if err != nil {
		t.Fatalf("parseSessionFile error: %v", err)
	}

	if summary.TotalTokens != 0 {
		t.Errorf("TotalTokens = %d, want 0", summary.TotalTokens)
	}
	if summary.Messages != 0 {
		t.Errorf("Messages = %d, want 0", summary.Messages)
	}
	if summary.Agent != "unknown" {
		t.Errorf("Agent = %q, want 'unknown'", summary.Agent)
	}
}

func TestParseSessionFile_NoUserMessageSkipsDetector(t *testing.T) {
	dir := t.TempDir()

	// Only non-user/assistant entries
	content := `{"model":"gpt-4","input_tokens":10,"output_tokens":5}` + "\n"
	path := writeFile(t, dir, "session.jsonl", content)

	called := false
	detector := func(msg string) string {
		called = true
		return "detected"
	}

	summary, err := parseSessionFile(path, detector)
	if err != nil {
		t.Fatalf("parseSessionFile error: %v", err)
	}

	if called {
		t.Error("detector was called despite no user message")
	}
	if summary.Agent != "unknown" {
		t.Errorf("Agent = %q, want 'unknown'", summary.Agent)
	}
}

// ---------------------------------------------------------------------------
// DefaultAgentDetector
// ---------------------------------------------------------------------------

func TestDefaultAgentDetector_Scanner(t *testing.T) {
	cases := []struct {
		msg     string
		keyword string
	}{
		{"run the scanner on this repo", "scanner"},
		{"triage all open issues", "triage"},
		{"file a new issue for the bug", "issue"},
		{"this is a bug report", "bug"},
	}

	for _, tc := range cases {
		t.Run(tc.keyword, func(t *testing.T) {
			got := DefaultAgentDetector(tc.msg)
			if got != "scanner" {
				t.Errorf("DefaultAgentDetector(%q) = %q, want 'scanner'", tc.msg, got)
			}
		})
	}
}

func TestDefaultAgentDetector_Reviewer(t *testing.T) {
	cases := []struct {
		msg     string
		keyword string
	}{
		{"please use the ci-maintainer for this PR", "ci-maintainer"},
		{"review the pull request", "review"},
		{"check ci status", "ci"},
		{"measure coverage now", "coverage"},
		{"track ga4 events", "ga4"},
	}

	for _, tc := range cases {
		t.Run(tc.keyword, func(t *testing.T) {
			got := DefaultAgentDetector(tc.msg)
			if got != "ci-maintainer" {
				t.Errorf("DefaultAgentDetector(%q) = %q, want 'ci-maintainer'", tc.msg, got)
			}
		})
	}
}

func TestDefaultAgentDetector_Architect(t *testing.T) {
	cases := []struct {
		msg     string
		keyword string
	}{
		{"architect the new service", "architect"},
		{"write a rfc for the proposal", "rfc"},
		{"refactor the token collector", "refactor"},
	}

	for _, tc := range cases {
		t.Run(tc.keyword, func(t *testing.T) {
			got := DefaultAgentDetector(tc.msg)
			if got != "architect" {
				t.Errorf("DefaultAgentDetector(%q) = %q, want 'architect'", tc.msg, got)
			}
		})
	}
}

func TestDefaultAgentDetector_Outreach(t *testing.T) {
	cases := []struct {
		msg     string
		keyword string
	}{
		{"run outreach to CNCF projects", "outreach"},
		{"check the adopters list", "adopters"},
		{"community meeting notes", "community"},
	}

	for _, tc := range cases {
		t.Run(tc.keyword, func(t *testing.T) {
			got := DefaultAgentDetector(tc.msg)
			if got != "outreach" {
				t.Errorf("DefaultAgentDetector(%q) = %q, want 'outreach'", tc.msg, got)
			}
		})
	}
}

func TestDefaultAgentDetector_Supervisor(t *testing.T) {
	cases := []struct {
		msg     string
		keyword string
	}{
		{"start the supervisor process", "supervisor"},
		{"run the daily sweep", "sweep"},
		{"monitor cluster health", "monitor"},
	}

	for _, tc := range cases {
		t.Run(tc.keyword, func(t *testing.T) {
			got := DefaultAgentDetector(tc.msg)
			if got != "supervisor" {
				t.Errorf("DefaultAgentDetector(%q) = %q, want 'supervisor'", tc.msg, got)
			}
		})
	}
}

func TestDefaultAgentDetector_SecCheck(t *testing.T) {
	cases := []struct {
		msg     string
		keyword string
	}{
		{"run a security audit on the codebase", "security"},
		{"trigger sec-check now", "sec-check"},
		{"scan for vulnerability in deps", "vulnerability"},
	}

	for _, tc := range cases {
		t.Run(tc.keyword, func(t *testing.T) {
			got := DefaultAgentDetector(tc.msg)
			if got != "sec-check" {
				t.Errorf("DefaultAgentDetector(%q) = %q, want 'sec-check'", tc.msg, got)
			}
		})
	}
}

func TestDefaultAgentDetector_Unknown(t *testing.T) {
	cases := []string{
		"",
		"hello world",
		"please do something useful",
		"random content with no keywords",
	}

	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			got := DefaultAgentDetector(msg)
			if got != "unknown" {
				t.Errorf("DefaultAgentDetector(%q) = %q, want 'unknown'", msg, got)
			}
		})
	}
}

func TestDefaultAgentDetector_CaseInsensitive(t *testing.T) {
	cases := []struct {
		msg   string
		agent string
	}{
		{"SCANNER triage", "scanner"},
		{"REVIEWER review", "ci-maintainer"},
		{"ARCHITECT rfc", "architect"},
		{"OUTREACH adopters", "outreach"},
		{"SUPERVISOR sweep", "supervisor"},
		{"SECURITY vulnerability", "sec-check"},
	}

	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			got := DefaultAgentDetector(tc.msg)
			if got != tc.agent {
				t.Errorf("DefaultAgentDetector(%q) = %q, want %q", tc.msg, got, tc.agent)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SessionSummary / AggregateSummary field correctness
// ---------------------------------------------------------------------------

func TestCollectFromDir_SessionSummaryFields(t *testing.T) {
	dir := t.TempDir()

	content := strings.Join([]string{
		`{"role":"user","message":"outreach to CNCF adopters"}`,
		`{"model":"claude-3-sonnet","input_tokens":200,"cache_creation":50,"cache_read":25,"output_tokens":100}`,
		`{"role":"assistant","message":"done"}`,
	}, "\n") + "\n"
	writeFile(t, dir, "outreach-session.jsonl", content)

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir error: %v", err)
	}

	if agg.SessionCount != 1 {
		t.Fatalf("SessionCount = %d, want 1", agg.SessionCount)
	}

	s := agg.Sessions[0]

	if s.SessionID != "outreach-session" {
		t.Errorf("SessionID = %q, want 'outreach-session'", s.SessionID)
	}
	if s.Agent != "outreach" {
		t.Errorf("Agent = %q, want 'outreach'", s.Agent)
	}
	if s.Model != "claude-3-sonnet" {
		t.Errorf("Model = %q, want 'claude-3-sonnet'", s.Model)
	}

	// InputTokens = 200 (input only, cache tracked separately)
	wantInput := int64(200)
	if s.InputTokens != wantInput {
		t.Errorf("InputTokens = %d, want %d", s.InputTokens, wantInput)
	}
	if s.OutputTokens != 100 {
		t.Errorf("OutputTokens = %d, want 100", s.OutputTokens)
	}
	if s.CacheRead != 25 {
		t.Errorf("CacheRead = %d, want 25", s.CacheRead)
	}
	if s.CacheCreate != 50 {
		t.Errorf("CacheCreate = %d, want 50", s.CacheCreate)
	}
	// TotalTokens = 200 + 100 + 25 + 50 = 375
	if s.TotalTokens != 375 {
		t.Errorf("TotalTokens = %d, want 375", s.TotalTokens)
	}
	// user message + assistant message = 2
	if s.Messages != 2 {
		t.Errorf("Messages = %d, want 2", s.Messages)
	}
}

func TestCollectFromDir_ByAgentAndByModelSums(t *testing.T) {
	dir := t.TempDir()

	// Two scanner sessions with different models
	sessionA := strings.Join([]string{
		`{"role":"user","message":"scanner triage"}`,
		`{"model":"gpt-4","input_tokens":100,"output_tokens":40}`,
	}, "\n") + "\n"
	sessionB := strings.Join([]string{
		`{"role":"user","message":"scanner issue"}`,
		`{"model":"gpt-3.5","input_tokens":60,"output_tokens":20}`,
	}, "\n") + "\n"

	writeFile(t, dir, "a.jsonl", sessionA)
	writeFile(t, dir, "b.jsonl", sessionB)

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir error: %v", err)
	}

	// ByAgent["scanner"] = (100+40) + (60+20) = 220
	if agg.ByAgent["scanner"] != 220 {
		t.Errorf("ByAgent[scanner] = %d, want 220", agg.ByAgent["scanner"])
	}
	if agg.ByModel["gpt-4"] != 140 {
		t.Errorf("ByModel[gpt-4] = %d, want 140", agg.ByModel["gpt-4"])
	}
	if agg.ByModel["gpt-3.5"] != 80 {
		t.Errorf("ByModel[gpt-3.5] = %d, want 80", agg.ByModel["gpt-3.5"])
	}
	if agg.TotalTokens != 220 {
		t.Errorf("TotalTokens = %d, want 220", agg.TotalTokens)
	}
}
