package tokens

import (
	"log/slog"
	"testing"
)

// TestInferenceSinkRecordsIntoCollectableFile verifies that usage recorded by
// the sink lands in a file the token Collector globs and aggregates, attributed
// to the correct agent. This is the counting fix for bare-mode inference agents
// (litellm/vllm/llm-d) that never write a Claude session transcript.
func TestInferenceSinkRecordsIntoCollectableFile(t *testing.T) {
	dir := t.TempDir()
	sink := NewInferenceSink(dir, slog.Default())

	// Two responses for the same agent should accumulate.
	sink.Record("kellyaa", "litellm-model", 100, 40)
	sink.Record("kellyaa", "litellm-model", 60, 30)
	// A different agent tracked separately.
	sink.Record("scanner", "litellm-model", 10, 5)

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir: %v", err)
	}

	// kellyaa: input 160 + output 70 = 230; scanner: 15; total 245.
	const wantTotal = int64(245)
	if agg.TotalTokens != wantTotal {
		t.Fatalf("TotalTokens = %d, want %d", agg.TotalTokens, wantTotal)
	}
	if got := agg.ByAgent["kellyaa"]; got != 230 {
		t.Fatalf("ByAgent[kellyaa] = %d, want 230", got)
	}
	if got := agg.ByAgent["scanner"]; got != 15 {
		t.Fatalf("ByAgent[scanner] = %d, want 15", got)
	}
	// Explicit agent attribution must not fall through to "unknown".
	if _, ok := agg.ByAgent["unknown"]; ok {
		t.Fatalf("unexpected 'unknown' agent bucket: %+v", agg.ByAgent)
	}
}

// TestInferenceSinkNoOps verifies the sink safely ignores empty agents, zero
// usage, and a disabled (empty-dir) sink without writing files or panicking.
func TestInferenceSinkNoOps(t *testing.T) {
	dir := t.TempDir()
	sink := NewInferenceSink(dir, slog.Default())

	sink.Record("", "model", 100, 100)  // empty agent
	sink.Record("agent", "model", 0, 0) // no usage
	var nilSink *InferenceSink
	nilSink.Record("agent", "model", 100, 100) // nil sink

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir: %v", err)
	}
	if agg.TotalTokens != 0 {
		t.Fatalf("expected no tokens recorded, got %d", agg.TotalTokens)
	}
}

// TestParseSessionFileExplicitAgent verifies the parser honors an explicit
// per-entry agent field over keyword detection.
func TestParseSessionFileExplicitAgent(t *testing.T) {
	dir := t.TempDir()
	// First user message ("architect") would keyword-detect as architect,
	// but the explicit agent field must win.
	content := `{"role":"user","agent":"kellyaa","message":"architect"}
{"role":"assistant","agent":"kellyaa","model":"m","input_tokens":10,"output_tokens":5}
`
	writeFile(t, dir, "inference-kellyaa.jsonl", content)

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir: %v", err)
	}
	if got := agg.ByAgent["kellyaa"]; got != 15 {
		t.Fatalf("ByAgent[kellyaa] = %d, want 15 (explicit agent should win)", got)
	}
}
