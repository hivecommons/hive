package tokens

import (
	"bytes"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture is a hand-written chat recording in the Gemini CLI 0.70.0
// on-disk format (see the format note in gemini_scanner.go). It is a literal
// file, not marshalled from the scanner's own structs, so a wrong JSON tag
// cannot round-trip and pass. It uses two models, cached input, thoughts,
// a tool-use prompt count, a message re-appended three times, a $patch, a
// $set and a $rewindTo.
//
// Expected per model (input excludes cached; thoughts are output):
//
//	gemini-2.5-pro:   g1 in 10000-8000=2000 out 200+300=500 cached 8000
//	                  g3 in 15000        out 400+1000=1400 cached 0
//	                  => in 17000 out 1900 cached 8000 total 26900
//	gemini-2.5-flash: g2 in 12000-11000+50=1050 out 100 cached 11000
//	                  => total 12150
const geminiFixture = "gemini_session_two_models.jsonl"

// geminiFixtureSessionID is the fixture's recorded sessionId.
const geminiFixtureSessionID = "5d1c0f6e-1111-4222-8333-944455556666"

func writeGeminiFixture(t *testing.T, chatsDir, name string) string {
	t.Helper()
	return writeGeminiFixtureAs(t, chatsDir, name, geminiFixtureSessionID)
}

// writeGeminiFixtureAs writes the fixture under a different sessionId, for
// tests that need several distinct sessions.
func writeGeminiFixtureAs(t *testing.T, chatsDir, name, sessionID string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", geminiFixture))
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(strings.ReplaceAll(string(raw), geminiFixtureSessionID, sessionID))
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(chatsDir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func geminiSessionByModel(t *testing.T, agg *AggregateSummary, model string) SessionSummary {
	t.Helper()
	for _, s := range agg.Sessions {
		if s.Model == model {
			return s
		}
	}
	t.Fatalf("no session for model %q in %+v", model, agg.Sessions)
	return SessionSummary{}
}

func TestScanGeminiSessionTree_TwoModelsCachedAndThoughts(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gemini")
	writeGeminiFixture(t, filepath.Join(home, "tmp", "repo", "chats"), "session-2026-11-20T10-00-5d1c0f6e.jsonl")

	agg, err := ScanGeminiSessionTree(home, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if agg.SessionCount != 2 {
		t.Fatalf("SessionCount = %d, want 2 (one per model)", agg.SessionCount)
	}

	pro := geminiSessionByModel(t, agg, "gemini-2.5-pro")
	if pro.InputTokens != 17000 || pro.OutputTokens != 1900 || pro.CacheRead != 8000 || pro.CacheCreate != 0 {
		t.Errorf("pro split = in %d out %d cache %d/%d, want 17000/1900/8000/0", pro.InputTokens, pro.OutputTokens, pro.CacheRead, pro.CacheCreate)
	}
	if pro.TotalTokens != 26900 {
		t.Errorf("pro TotalTokens = %d, want 26900 (sum of totalTokenCount)", pro.TotalTokens)
	}
	// Two assistant messages plus the two user turns, counted once.
	if pro.Messages != 4 {
		t.Errorf("pro Messages = %d, want 4", pro.Messages)
	}
	if pro.Backend != BackendGemini {
		t.Errorf("Backend = %q, want %q", pro.Backend, BackendGemini)
	}
	if pro.SessionID != geminiFixtureSessionID+"#gemini-2.5-pro" {
		t.Errorf("SessionID = %q", pro.SessionID)
	}
	if got := pro.UsageTotal(); got != pro.TotalTokens {
		t.Errorf("pro timeline total = %d, want %d", got, pro.TotalTokens)
	}
	if want := parseTimestampToUnixMilli("2026-11-20T10:00:05.000Z"); pro.FirstActive != want {
		t.Errorf("pro FirstActive = %d, want %d", pro.FirstActive, want)
	}
	if want := parseTimestampToUnixMilli("2026-11-20T10:00:20.000Z"); pro.LastActive != want {
		t.Errorf("pro LastActive = %d, want %d", pro.LastActive, want)
	}

	flash := geminiSessionByModel(t, agg, "gemini-2.5-flash")
	if flash.InputTokens != 1050 || flash.OutputTokens != 100 || flash.CacheRead != 11000 {
		t.Errorf("flash split = in %d out %d cache %d, want 1050/100/11000", flash.InputTokens, flash.OutputTokens, flash.CacheRead)
	}
	if flash.TotalTokens != 12150 || flash.Messages != 1 {
		t.Errorf("flash total/messages = %d/%d, want 12150/1", flash.TotalTokens, flash.Messages)
	}

	if agg.TotalTokens != 26900+12150 {
		t.Errorf("agg TotalTokens = %d, want %d", agg.TotalTokens, 26900+12150)
	}
	if agg.ByModelDetail["gemini-2.5-pro"].Input != 17000 || agg.ByModelDetail["gemini-2.5-flash"].CacheRead != 11000 {
		t.Errorf("ByModelDetail = %+v / %+v", agg.ByModelDetail["gemini-2.5-pro"], agg.ByModelDetail["gemini-2.5-flash"])
	}
}

func TestScanGeminiSessionTree_PricesEachMessageAtItsModel(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gemini")
	writeGeminiFixture(t, filepath.Join(home, "tmp", "repo", "chats"), "session-a.jsonl")
	agg, err := ScanGeminiSessionTree(home, "builder", nil)
	if err != nil {
		t.Fatal(err)
	}

	cost := EstimateFromSummary(agg)
	proUSD, proExact := EstimateCostUSD("gemini-2.5-pro", 17000, 1900, 8000, 0)
	flashUSD, flashExact := EstimateCostUSD("gemini-2.5-flash", 1050, 100, 11000, 0)
	if !proExact || !flashExact {
		t.Fatalf("fixture models must be in the price table (pro %v, flash %v)", proExact, flashExact)
	}
	want := proUSD + flashUSD
	if math.Abs(cost.TotalUSD-want) > 1e-9 {
		t.Errorf("TotalUSD = %v, want %v", cost.TotalUSD, want)
	}
	agent := cost.ByAgent["builder"]
	if math.Abs(agent.USD-want) > 1e-9 || !agent.Priced {
		t.Errorf("ByAgent[builder] = %+v, want USD %v priced", agent, want)
	}
	// Pricing the whole recording at its first model would overstate it.
	allPro, _ := EstimateCostUSD("gemini-2.5-pro", 17000+1050, 1900+100, 8000+11000, 0)
	if math.Abs(agent.USD-allPro) < 1e-9 {
		t.Errorf("agent cost %v equals the single-model price; per-message models were ignored", agent.USD)
	}
	if len(cost.UnpricedModels) != 0 {
		t.Errorf("UnpricedModels = %v, want none", cost.UnpricedModels)
	}
}

func TestScanGeminiSessionTree_RescanDoesNotDoubleCount(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gemini")
	path := writeGeminiFixture(t, filepath.Join(home, "tmp", "repo", "chats"), "session-a.jsonl")

	first, err := ScanGeminiSessionTree(home, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ScanGeminiSessionTree(home, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.TotalTokens != first.TotalTokens || again.SessionCount != first.SessionCount {
		t.Fatalf("unchanged re-scan = %d tokens / %d sessions, want %d / %d", again.TotalTokens, again.SessionCount, first.TotalTokens, first.SessionCount)
	}

	// The CLI keeps appending: g2 re-appended whole (same tokens), a new
	// g4, and a half-written final line that must be ignored until done.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	appended := strings.Join([]string{
		`{"id":"g2","timestamp":"2026-11-20T10:00:09.000Z","type":"gemini","content":"Looking at the test.","model":"gemini-2.5-flash","tokens":{"input":12000,"output":100,"cached":11000,"thoughts":0,"tool":50,"total":12150},"toolCalls":[{"id":"t2","name":"grep","args":{},"status":"success","timestamp":"2026-11-20T10:00:09.500Z"}]}`,
		`{"id":"g4","timestamp":"2026-11-20T10:01:00.000Z","type":"gemini","content":"ok","model":"gemini-2.5-flash","tokens":{"input":500,"output":20,"cached":0,"thoughts":10,"tool":0,"total":530}}`,
		`{"id":"g5","timestamp":"2026-11-20T10:02:00.000Z","type":"gemini","model":"gemini-2.5-flash","tokens":{"input":9`,
	}, "\n")
	if _, err := f.WriteString(appended); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	grown, err := ScanGeminiSessionTree(home, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := first.TotalTokens + 530; grown.TotalTokens != want {
		t.Errorf("grown TotalTokens = %d, want %d (only g4 is new)", grown.TotalTokens, want)
	}
	if flash := geminiSessionByModel(t, grown, "gemini-2.5-flash"); flash.Messages != 2 {
		t.Errorf("flash Messages = %d, want 2 (g2 once, g4)", flash.Messages)
	}

	// Through the collector, two scans of the same files keep the same totals.
	c := NewCollector(t.TempDir(), testLogger())
	c.SetPersistPath(filepath.Join(t.TempDir(), "summary.json"))
	c.SetGeminiSessionsDir(home)
	c.scan()
	c.scan()
	if got := c.Summary().TotalTokens; got != grown.TotalTokens {
		t.Errorf("collector TotalTokens after two scans = %d, want %d", got, grown.TotalTokens)
	}
}

func TestScanGeminiSessionTree_Attribution(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	shared := filepath.Join(home, ".gemini")

	// Shared tree, project root under an agent work dir: that agent.
	writeGeminiFixtureAs(t, filepath.Join(shared, "tmp", "repo", "chats"), "session-a.jsonl", "sess-a")
	if err := os.WriteFile(filepath.Join(shared, "tmp", "repo", ".project_root"), []byte("/data/agents/scanner/repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Shared tree, subagent recording nested under the parent session id.
	writeGeminiFixtureAs(t, filepath.Join(shared, "tmp", "repo", "chats", "sess-a"), "sub-1.jsonl", "sub-1")
	// Shared tree, unknown project: the generic gemini bucket.
	writeGeminiFixtureAs(t, filepath.Join(shared, "tmp", "elsewhere", "chats"), "session-b.jsonl", "sess-b")
	// Per-agent HOME with no project marker: the tree owner.
	writeGeminiFixtureAs(t, filepath.Join(home, "agents", "fixer", ".gemini", "tmp", "proj", "chats"), "session-c.jsonl", "sess-c")

	c := NewCollector(t.TempDir(), testLogger())
	c.SetPersistPath(filepath.Join(root, "summary.json"))
	c.SetGeminiSessionsDir(shared)
	c.scan()
	sum := c.Summary()

	const perFile = 26900 + 12150
	for agent, want := range map[string]int64{"scanner": 2 * perFile, "gemini": perFile, "fixer": perFile} {
		if got := sum.ByAgent[agent]; got != want {
			t.Errorf("ByAgent[%s] = %d, want %d", agent, got, want)
		}
	}
	if sum.SessionCount != 8 {
		t.Errorf("SessionCount = %d, want 8 (4 recordings x 2 models)", sum.SessionCount)
	}
}

func TestScanGeminiSessionTree_UnknownModelIsLoggedAndNotFree(t *testing.T) {
	geminiUnpricedWarned.Clear()
	home := filepath.Join(t.TempDir(), ".gemini")
	chats := filepath.Join(home, "tmp", "repo", "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatal(err)
	}
	// One session on an unpriced model; another whose only message has no
	// model and no neighbour to borrow one from.
	unpriced := strings.Join([]string{
		`{"sessionId":"s-unpriced","projectHash":"repo","startTime":"2026-11-20T10:00:00.000Z","lastUpdated":"2026-11-20T10:00:00.000Z"}`,
		`{"id":"g1","timestamp":"2026-11-20T10:00:01.000Z","type":"gemini","content":"a","model":"gemini-99-hypothetical","tokens":{"input":1000,"output":100,"cached":0,"thoughts":0,"tool":0,"total":1100}}`,
	}, "\n") + "\n"
	noModel := strings.Join([]string{
		`{"sessionId":"s-nomodel","projectHash":"repo","startTime":"2026-11-20T10:00:00.000Z","lastUpdated":"2026-11-20T10:00:00.000Z"}`,
		`{"id":"g2","timestamp":"2026-11-20T10:00:02.000Z","type":"gemini","content":"b","tokens":{"input":2000,"output":200,"cached":0,"thoughts":0,"tool":0,"total":2200}}`,
	}, "\n") + "\n"
	for name, content := range map[string]string{"session-u.jsonl": unpriced, "session-n.jsonl": noModel} {
		if err := os.WriteFile(filepath.Join(chats, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	agg, err := ScanGeminiSessionTree(home, "", logger)
	if err != nil {
		t.Fatal(err)
	}
	geminiSessionByModel(t, agg, "gemini-99-hypothetical")
	geminiSessionByModel(t, agg, geminiUnknownModel)

	out := logs.String()
	if !strings.Contains(out, "no exact price") || !strings.Contains(out, "gemini-99-hypothetical") {
		t.Errorf("missing unpriced-model log line, got:\n%s", out)
	}
	if !strings.Contains(out, "without a model") {
		t.Errorf("missing no-model log line, got:\n%s", out)
	}

	cost := EstimateFromSummary(agg)
	if cost.TotalUSD <= 0 {
		t.Errorf("TotalUSD = %v, want > 0 (tier fallback, never $0)", cost.TotalUSD)
	}
	for _, m := range []string{"gemini-99-hypothetical", geminiUnknownModel} {
		if !containsString(cost.UnpricedModels, m) {
			t.Errorf("UnpricedModels = %v, want it to list %s as approximate", cost.UnpricedModels, m)
		}
	}

	// The warning is once per model per process, not once per scan.
	logs.Reset()
	if _, err := ScanGeminiSessionTree(home, "", logger); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Errorf("second scan logged again:\n%s", logs.String())
	}
}

func TestScanGeminiSessionTree_LegacyJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".gemini")
	chats := filepath.Join(home, "tmp", "0123abcd", "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-JSONL releases wrote one pretty-printed ConversationRecord.
	legacy := `{
  "sessionId": "legacy-1",
  "projectHash": "0123abcd",
  "startTime": "2026-11-19T09:00:00.000Z",
  "lastUpdated": "2026-11-19T09:05:00.000Z",
  "messages": [
    {
      "id": "u1",
      "timestamp": "2026-11-19T09:00:01.000Z",
      "type": "user",
      "content": "hi"
    },
    {
      "id": "g1",
      "timestamp": "2026-11-19T09:00:03.000Z",
      "type": "gemini",
      "content": "hello",
      "model": "gemini-2.5-pro",
      "tokens": {
        "input": 400,
        "output": 30,
        "cached": 100,
        "thoughts": 20,
        "tool": 0,
        "total": 450
      }
    }
  ]
}
`
	path := filepath.Join(chats, "session-2026-11-19T09-00-legacy01.json")
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	agg, err := ScanGeminiSessionTree(home, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := geminiSessionByModel(t, agg, "gemini-2.5-pro")
	if s.InputTokens != 300 || s.OutputTokens != 50 || s.CacheRead != 100 || s.TotalTokens != 450 {
		t.Errorf("legacy split = %+v", s)
	}

	// On resume the CLI migrates it to "<name>.jsonl" and leaves the .json:
	// only the .jsonl may be counted.
	if err := os.WriteFile(path+"l", []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	agg, err = ScanGeminiSessionTree(home, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if agg.TotalTokens != 450 || agg.SessionCount != 1 {
		t.Errorf("after migration TotalTokens/SessionCount = %d/%d, want 450/1", agg.TotalTokens, agg.SessionCount)
	}
}

func TestGeminiHomeBesideClaude(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/data/home/.claude/projects", "/data/home/.gemini"},
		{"/srv/hive/home/.claude/projects/", "/srv/hive/home/.gemini"},
		{"/custom/claude", "/data/home/.gemini"},
		{"", "/data/home/.gemini"},
	}
	for _, tc := range cases {
		if got := GeminiHomeBesideClaude(tc.in); got != tc.want {
			t.Errorf("GeminiHomeBesideClaude(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDiagnostics_GeminiScanErrorIsSurfacedThenCleared(t *testing.T) {
	c := newDiagCollector(t, t.TempDir())
	c.SetGeminiSessionsDir(filepath.Join(t.TempDir(), "ge[mini"))
	c.scan()
	if d := c.Diagnostics(); d.LastGeminiScanError == "" {
		t.Fatalf("diagnostics = %+v, want non-empty LastGeminiScanError", d)
	}
	c.SetGeminiSessionsDir(t.TempDir())
	c.scan()
	if d := c.Diagnostics(); d.LastGeminiScanError != "" {
		t.Fatalf("diagnostics = %+v, want LastGeminiScanError cleared after healthy scan", d)
	}
}
