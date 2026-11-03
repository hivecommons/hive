package tokens

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realCopilotShutdown is a verbatim session.shutdown event captured from a
// scanner run on hive's own spoke (Copilot CLI 1.0.88, 2026-10-22). Note
// usage.inputTokens (794742) == tokenDetails.input (44) + cache_read (769228)
// + cache_write (25470): inputTokens is the WHOLE prompt.
const realCopilotShutdown = `{"type":"session.shutdown","data":{"shutdownType":"routine","totalPremiumRequests":15,"tokenDetails":{"input":{"tokenCount":44},"cache_read":{"tokenCount":769228},"cache_write":{"tokenCount":25470},"output":{"tokenCount":6577}},"sessionStartTime":1792636812617,"modelMetrics":{"claude-fable-5.1":{"requests":{"count":21,"cost":15},"usage":{"inputTokens":794742,"outputTokens":6577,"cacheReadTokens":769228,"cacheWriteTokens":25470,"reasoningTokens":238},"tokenDetails":{"input":{"tokenCount":44},"cache_read":{"tokenCount":769228},"cache_write":{"tokenCount":25470},"output":{"tokenCount":6577}}}},"currentModel":"claude-fable-5.1"},"id":"6ba117d3-83a7-471e-9966-15ad1604c4a8","timestamp":"2026-10-22T02:46:39.000Z","parentId":null}`

const realCopilotStart = `{"type":"session.start","data":{"sessionId":"659e2a72-b426-49c2-8388-1915d943e860","version":1,"producer":"copilot-agent","copilotVersion":"1.0.88","selectedModel":"claude-fable-5.1","context":{"cwd":"/data/agents/scanner"}},"timestamp":"2026-10-22T02:40:12.751Z"}`

func writeCopilotSession(t *testing.T, sessionsDir, id string, lines ...string) {
	t.Helper()
	dir := filepath.Join(sessionsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseCopilotSessionFile_RealShutdownUsesUncachedInput(t *testing.T) {
	dir := t.TempDir()
	writeCopilotSession(t, dir, "s1", realCopilotStart,
		`{"type":"user.message","timestamp":"2026-10-22T02:40:13.000Z","data":{"content":"go"}}`,
		realCopilotShutdown)

	summary, err := parseCopilotSessionFile(filepath.Join(dir, "s1", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if summary.InputTokens != 44 {
		t.Errorf("InputTokens = %d, want 44 (tokenDetails.input, not the cache-inclusive usage.inputTokens)", summary.InputTokens)
	}
	if summary.OutputTokens != 6577 || summary.CacheRead != 769228 || summary.CacheCreate != 25470 {
		t.Errorf("splits = out %d / cr %d / cw %d", summary.OutputTokens, summary.CacheRead, summary.CacheCreate)
	}
	if summary.TotalTokens != 44+6577+769228+25470 {
		t.Errorf("TotalTokens = %d", summary.TotalTokens)
	}
	if summary.Agent != "scanner" || summary.Model != "claude-fable-5.1" {
		t.Errorf("agent/model = %s/%s", summary.Agent, summary.Model)
	}
}

func TestCopilotModelMetricSplits(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	cases := []struct {
		name string
		m    copilotModelMetric
		want [4]int64
	}{
		{"tokenDetails wins", copilotModelMetric{
			Usage:        copilotUsage{InputTokens: 1000, OutputTokens: 5, CacheReadTokens: 900, CacheWriteTokens: 50},
			TokenDetails: map[string]copilotTokenDetail{"input": {TokenCount: n(50)}},
		}, [4]int64{50, 5, 900, 50}},
		{"no details, cache fits inside input", copilotModelMetric{
			Usage: copilotUsage{InputTokens: 1000, OutputTokens: 5, CacheReadTokens: 900, CacheWriteTokens: 50},
		}, [4]int64{50, 5, 900, 50}},
		{"no details, cache exceeds input: reported as-is", copilotModelMetric{
			Usage: copilotUsage{InputTokens: 100, OutputTokens: 5, CacheReadTokens: 900, CacheWriteTokens: 50},
		}, [4]int64{100, 5, 900, 50}},
		{"no cache at all", copilotModelMetric{
			Usage: copilotUsage{InputTokens: 100, OutputTokens: 5},
		}, [4]int64{100, 5, 0, 0}},
	}
	for _, tc := range cases {
		in, out, cr, cw := tc.m.splits()
		if got := [4]int64{in, out, cr, cw}; got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestCopilotSessionTrees_DerivesPerAgentHomes(t *testing.T) {
	home := t.TempDir()
	shared := filepath.Join(home, ".copilot", "session-state")
	for _, a := range []string{"scanner", "reviewer", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(home, "agents", a, ".copilot", "session-state"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// An agent home without a .copilot tree must not appear.
	if err := os.MkdirAll(filepath.Join(home, "agents", "inference-only"), 0o755); err != nil {
		t.Fatal(err)
	}

	trees := CopilotSessionTrees(shared)
	if len(trees) != 3 {
		t.Fatalf("trees = %+v, want shared + scanner + reviewer", trees)
	}
	if trees[0].Dir != shared || trees[0].Agent != "" {
		t.Errorf("trees[0] = %+v, want shared dir with no agent", trees[0])
	}
	if trees[1].Agent != "reviewer" || trees[2].Agent != "scanner" {
		t.Errorf("per-agent trees = %+v", trees[1:])
	}
	if trees[2].Dir != filepath.Join(home, "agents", "scanner", ".copilot", "session-state") {
		t.Errorf("scanner dir = %q", trees[2].Dir)
	}
}

func TestCopilotSessionTrees_NonCanonicalDirIsSharedOnly(t *testing.T) {
	if got := CopilotSessionTrees(""); got != nil {
		t.Errorf("empty dir → %+v, want nil", got)
	}
	got := CopilotSessionTrees("/some/custom/sessions")
	if len(got) != 1 || got[0].Dir != "/some/custom/sessions" {
		t.Errorf("custom dir → %+v", got)
	}
}

// TestCollectorScansPerAgentCopilotHomes is the #13470 regression: since the
// per-agent HOME layout (#9444) the Copilot CLI writes under
// <home>/agents/<name>/.copilot/session-state, which the shared-dir scan never
// visited, so every Claude-via-Copilot session went uncounted and only the
// proxy-sniffed gpt-4o-mini side calls (I/O classifier) reached the cost table.
func TestCollectorScansPerAgentCopilotHomes(t *testing.T) {
	metricsDir := t.TempDir()
	home := t.TempDir()
	shared := filepath.Join(home, ".copilot", "session-state")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	scannerTree := filepath.Join(home, "agents", "scanner", ".copilot", "session-state")
	writeCopilotSession(t, scannerTree, "s1", realCopilotStart,
		`{"type":"user.message","timestamp":"2026-10-22T02:40:13.000Z","data":{"content":"go"}}`,
		realCopilotShutdown)
	// A session whose cwd points elsewhere is still owned by the home it sits in.
	reviewerTree := filepath.Join(home, "agents", "reviewer", ".copilot", "session-state")
	writeCopilotSession(t, reviewerTree, "r1",
		`{"type":"session.start","data":{"sessionId":"r1r1r1r1r1r1","selectedModel":"claude-sonnet-5.5","context":{"cwd":"/tmp/wt"}},"timestamp":"2026-10-22T03:00:00.000Z"}`,
		`{"type":"user.message","timestamp":"2026-10-22T03:00:01.000Z","data":{"content":"review"}}`,
		`{"type":"session.shutdown","data":{"currentModel":"claude-sonnet-5.5","modelMetrics":{"claude-sonnet-5.5":{"usage":{"inputTokens":1000,"outputTokens":100,"cacheReadTokens":800,"cacheWriteTokens":100}}}},"timestamp":"2026-10-22T03:05:00.000Z"}`)

	// The proxy sink only ever saw the classifier's gpt-4o-mini calls for
	// scanner — live capture must NOT zero scanner's claude-fable-5.1 totals.
	sink := NewInferenceSink(metricsDir, testLogger())
	sink.RecordCopilot("scanner", "gpt-4o-mini", 5000, 10)

	c := NewCollector(metricsDir, testLogger())
	c.SetPersistPath(filepath.Join(metricsDir, "token-summary.json"))
	c.SetCopilotSessionsDir(shared)
	c.SetCopilotLiveCapture(1) // every session is "after" live capture began
	c.scan()

	summary := c.Summary()
	if summary == nil {
		t.Fatal("nil summary")
	}
	sc := summary.ByAgentDetail["scanner"]
	if sc == nil {
		t.Fatalf("scanner missing from ByAgentDetail: %+v", summary.ByAgentDetail)
	}
	if sc.Input != 44+5000 || sc.Output != 6577+10 || sc.CacheRead != 769228 || sc.CacheCreate != 25470 {
		t.Errorf("scanner detail = %+v, want fable shutdown totals + sink gpt-4o-mini", sc)
	}
	rv := summary.ByAgentDetail["reviewer"]
	if rv == nil || rv.Input != 100 || rv.Output != 100 || rv.CacheRead != 800 {
		t.Errorf("reviewer detail = %+v (home dir must own the session even when cwd is elsewhere)", rv)
	}
	if _, ok := summary.ByModelDetail["claude-fable-5.1"]; !ok {
		t.Errorf("claude-fable-5.1 missing from ByModelDetail: %v", summary.ByModelDetail)
	}
}

// TestScanCopilotSessionTree_PerModelDeferral: only models the sink actually
// captured are deferred; a session mixing a captured and an uncaptured model
// keeps the uncaptured one's totals.
func TestScanCopilotSessionTree_PerModelDeferral(t *testing.T) {
	dir := t.TempDir()
	writeCopilotSession(t, dir, "m1",
		`{"type":"session.start","data":{"sessionId":"m1m1m1m1m1m1","selectedModel":"claude-opus-5.5","context":{"cwd":"/data/agents/quality"}},"timestamp":"2026-10-22T03:00:00.000Z"}`,
		`{"type":"user.message","timestamp":"2026-10-22T03:00:01.000Z","data":{"content":"go"}}`,
		`{"type":"session.shutdown","data":{"currentModel":"claude-opus-5.5","modelMetrics":{"claude-opus-5.5":{"usage":{"inputTokens":300,"outputTokens":30}},"gpt-5":{"usage":{"inputTokens":200,"outputTokens":20}}}},"timestamp":"2026-10-22T03:05:00.000Z"}`)

	captured := func(agent, model string) bool { return agent == "quality" && model == "gpt-5" }
	agg, err := ScanCopilotSessionTree(dir, CopilotScanOptions{LiveCaptureSinceMs: 1, LiveCaptured: captured})
	if err != nil {
		t.Fatal(err)
	}
	if agg.TotalInput != 300 || agg.TotalOutput != 30 {
		t.Errorf("totals = in %d / out %d, want 300/30 (gpt-5 deferred to sink, opus kept)", agg.TotalInput, agg.TotalOutput)
	}

	// Legacy nil predicate: everything deferred.
	agg, _ = ScanCopilotSessionTree(dir, CopilotScanOptions{LiveCaptureSinceMs: 1})
	if agg.TotalTokens != 0 || agg.TotalMessages != 1 {
		t.Errorf("legacy deferral: tokens %d msgs %d, want 0/1", agg.TotalTokens, agg.TotalMessages)
	}

	// Pre-capture session: nothing deferred regardless of predicate.
	agg, _ = ScanCopilotSessionTree(dir, CopilotScanOptions{LiveCaptureSinceMs: 1_900_000_000_000, LiveCaptured: captured})
	if agg.TotalInput != 500 {
		t.Errorf("pre-capture session: input %d, want 500", agg.TotalInput)
	}
}

func TestCopilotLiveCapturedFromAggregate(t *testing.T) {
	agg := &AggregateSummary{Sessions: []SessionSummary{
		{Agent: "scanner", Backend: BackendCopilot, Model: "gpt-4o-mini", Usage: []UsageEvent{{Model: "gpt-5"}}},
		{Agent: "reviewer", Backend: BackendInference, Model: "gpt-4o-mini"},
		{Agent: "guide", Backend: "", Model: "claude-opus-5.5"},
	}}
	captured := copilotLiveCapturedFromAggregate(agg)
	for _, tc := range []struct {
		agent, model string
		want         bool
	}{
		{"scanner", "gpt-4o-mini", true},
		{"scanner", "gpt-5", true},
		{"scanner", "claude-fable-5.1", false},
		{"reviewer", "gpt-4o-mini", false}, // inference sink, not live Copilot
		{"guide", "claude-opus-5.5", false},
		{"nobody", "gpt-4o-mini", false},
	} {
		if got := captured(tc.agent, tc.model); got != tc.want {
			t.Errorf("captured(%s,%s) = %v want %v", tc.agent, tc.model, got, tc.want)
		}
	}
	if copilotLiveCapturedFromAggregate(nil)("a", "m") {
		t.Error("nil aggregate must capture nothing")
	}
}
