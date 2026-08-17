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

// TestInferenceSinkRestoresAcrossRestart verifies that a new sink reloads the
// cumulative totals already persisted on disk, so a hive restart does not reset
// every agent's counter to zero (which would overwrite the file with a lower
// value on the next Record and silently lose all pre-restart tokens). This is
// load-bearing for the Copilot live path, whose only durable record is this file
// once the session-shutdown scanner defers to the sink.
func TestInferenceSinkRestoresAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	sink1 := NewInferenceSink(dir, slog.Default())
	sink1.Record("worker1", "gpt-5", 100, 40)
	sink1.Record("worker1", "gpt-5", 50, 20)
	sink1.Record("worker2", "claude-sonnet-4.5", 10, 5)

	// Simulate a restart: brand-new sink over the same dir.
	sink2 := NewInferenceSink(dir, slog.Default())
	// One more response for worker1 must accumulate ON TOP of the restored total.
	sink2.Record("worker1", "gpt-5", 30, 10)

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir: %v", err)
	}
	// worker1: (100+50+30) input + (40+20+10) output = 180 + 70 = 250.
	if got := agg.ByAgent["worker1"]; got != 250 {
		t.Fatalf("ByAgent[worker1] = %d, want 250 (restored 210 + 40)", got)
	}
	// worker2 untouched by sink2 but must still be present from disk.
	if got := agg.ByAgent["worker2"]; got != 15 {
		t.Fatalf("ByAgent[worker2] = %d, want 15", got)
	}
}

// TestInferenceSinkRestoreIgnoresMalformed verifies restore skips unreadable /
// malformed files without failing construction.
func TestInferenceSinkRestoreIgnoresMalformed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "inference-broken.jsonl", "not json at all\n{bad")
	writeFile(t, dir, "inference-empty.jsonl", "")
	// A well-formed file to confirm the good one still restores.
	writeFile(t, dir, "inference-good.jsonl",
		`{"role":"user","agent":"good","message":"good"}`+"\n"+
			`{"role":"assistant","agent":"good","model":"m","input_tokens":7,"output_tokens":3}`+"\n")

	sink := NewInferenceSink(dir, slog.Default())
	sink.Record("good", "m", 1, 1) // 7+1=8 in, 3+1=4 out => 12

	agg, err := CollectFromDir(dir, DefaultAgentDetector)
	if err != nil {
		t.Fatalf("CollectFromDir: %v", err)
	}
	if got := agg.ByAgent["good"]; got != 12 {
		t.Fatalf("ByAgent[good] = %d, want 12", got)
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
