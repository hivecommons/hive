package tokens

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// hivecommons/hive#20003: codex agents' usage lives only in their rollout
// files. These tests use synthetic rollouts.

func codexTurnContextLine(ts, model string) string {
	return `{"timestamp":"` + ts + `","type":"turn_context","payload":{"model":"` + model + `","cwd":"/data/agents/x"}}`
}

func codexTokenCountLine(ts string, total, last *codexTokenUsage) string {
	info := map[string]any{}
	if total != nil {
		info["total_token_usage"] = total
	}
	if last != nil {
		info["last_token_usage"] = last
	}
	b, _ := json.Marshal(map[string]any{
		"timestamp": ts,
		"type":      "event_msg",
		"payload":   map[string]any{"type": "token_count", "info": info},
	})
	return string(b)
}

func writeCodexRolloutFile(t *testing.T, homesRoot, agent, day, name string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(homesRoot, ".codex-"+agent, "sessions", filepath.FromSlash(day))
	if agent == "" {
		dir = filepath.Join(homesRoot, ".codex", "sessions", filepath.FromSlash(day))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseCodexRollout_MapsFieldsAndSkipsRepeats(t *testing.T) {
	turn1 := &codexTokenUsage{Input: 1000, CachedRead: 800, CacheWrite: 100, Output: 50, Reasoning: 20, Total: 1050}
	total1 := *turn1
	turn2 := &codexTokenUsage{Input: 2000, CachedRead: 1500, Output: 70, Total: 2070}
	total2 := codexTokenUsage{Input: 3000, CachedRead: 2300, CacheWrite: 100, Output: 120, Reasoning: 20, Total: 3120}
	rollout := strings.Join([]string{
		`{"timestamp":"2026-11-25T01:00:00Z","type":"session_meta","payload":{"id":"x"}}`,
		codexTurnContextLine("2026-11-25T01:00:00Z", "gpt-6-luna"),
		`{"timestamp":"2026-11-25T01:00:01Z","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{}}}`,
		codexTokenCountLine("2026-11-25T01:00:02Z", &total1, turn1),
		// The same event again, as codex emits beside a rate-limit update.
		codexTokenCountLine("2026-11-25T01:00:03Z", &total1, turn1),
		`not json {{ "token_count"`,
		codexTokenCountLine("2026-11-25T01:00:04Z", &total2, turn2),
	}, "\n")

	sess, err := parseCodexRollout(strings.NewReader(rollout), "codex-sup-r1", "supervisor", 0)
	if err != nil || sess == nil {
		t.Fatalf("parse: %v %v", sess, err)
	}
	// input is input_tokens minus cached and cache-write tokens.
	wantInput := (1000 - 800 - 100) + (2000 - 1500)
	if sess.InputTokens != int64(wantInput) || sess.CacheRead != 2300 || sess.CacheCreate != 100 || sess.OutputTokens != 120 {
		t.Fatalf("field mapping wrong: %+v", sess)
	}
	if sess.TotalTokens != total2.Input+total2.Output {
		t.Fatalf("total %d must equal codex input+output %d (cached tokens counted once)", sess.TotalTokens, total2.Input+total2.Output)
	}
	if sess.Messages != 2 {
		t.Fatalf("repeated token_count must not be counted twice: messages=%d", sess.Messages)
	}
	if sess.Model != "gpt-6-luna" || sess.Backend != BackendCodex || sess.Agent != "supervisor" {
		t.Fatalf("labels wrong: model=%q backend=%q agent=%q", sess.Model, sess.Backend, sess.Agent)
	}
	if sess.UsageTotal() != sess.TotalTokens {
		t.Fatalf("timeline %d must reconcile with total %d", sess.UsageTotal(), sess.TotalTokens)
	}
	if sess.FirstActive == 0 || sess.LastActive <= sess.FirstActive {
		t.Fatalf("activity bracket not set: %d..%d", sess.FirstActive, sess.LastActive)
	}
}

func TestParseCodexRollout_PerEventModelAndDominantLabel(t *testing.T) {
	small := &codexTokenUsage{Input: 10, Output: 1, Total: 11}
	big := &codexTokenUsage{Input: 500, Output: 5, Total: 505}
	t1 := *small
	t2 := codexTokenUsage{Input: 510, Output: 6, Total: 516}
	rollout := strings.Join([]string{
		codexTurnContextLine("2026-11-25T01:00:00Z", "gpt-6.2-astra"),
		codexTokenCountLine("2026-11-25T01:00:01Z", &t1, small),
		codexTurnContextLine("2026-11-25T01:00:02Z", "gpt-6.2-sol"),
		codexTokenCountLine("2026-11-25T01:00:03Z", &t2, big),
	}, "\n")
	sess, err := parseCodexRollout(strings.NewReader(rollout), "id", "reviewer", 0)
	if err != nil || sess == nil {
		t.Fatalf("parse: %v %v", sess, err)
	}
	if sess.Model != "gpt-6.2-sol" {
		t.Fatalf("session must be labelled with the model that spent most: %q", sess.Model)
	}
	if len(sess.Usage) != 2 || sess.Usage[0].Model != "gpt-6.2-astra" || sess.Usage[1].Model != "gpt-6.2-sol" {
		t.Fatalf("per-event models not kept: %+v", sess.Usage)
	}
}

func TestParseCodexRollout_TotalDeltaWhenNoLastUsage(t *testing.T) {
	t1 := codexTokenUsage{Input: 100, CachedRead: 40, Output: 10, Total: 110}
	t2 := codexTokenUsage{Input: 250, CachedRead: 100, Output: 30, Total: 280}
	rollout := strings.Join([]string{
		codexTokenCountLine("2026-11-25T01:00:01Z", &t1, nil),
		codexTokenCountLine("2026-11-25T01:00:02Z", &t2, nil),
	}, "\n")
	sess, err := parseCodexRollout(strings.NewReader(rollout), "id", "a", 0)
	if err != nil || sess == nil {
		t.Fatalf("parse: %v %v", sess, err)
	}
	if sess.TotalTokens != t2.Input+t2.Output || sess.CacheRead != t2.CachedRead {
		t.Fatalf("delta from totals wrong: %+v", sess)
	}
	if sess.Model != "unknown" {
		t.Fatalf("no turn_context must label unknown, got %q", sess.Model)
	}
}

func TestCodexTurnUsage_CounterResetCountsNewTotal(t *testing.T) {
	prev := codexTokenUsage{Input: 500, Output: 50, Total: 550}
	reset := codexTokenUsage{Input: 30, Output: 3, Total: 33}
	got, ok := codexTurnUsage(&reset, nil, &prev)
	if !ok || got != reset {
		t.Fatalf("a counter reset must count the new total, got %+v ok=%v", got, ok)
	}
	if _, ok := codexTurnUsage(nil, nil, &prev); ok {
		t.Fatal("an event with no usage must count nothing")
	}
}

func TestParseCodexRollout_SinceDropsEarlierAndUntimedEvents(t *testing.T) {
	u := &codexTokenUsage{Input: 100, Output: 10, Total: 110}
	t1 := *u
	t2 := codexTokenUsage{Input: 200, Output: 20, Total: 220}
	t3 := codexTokenUsage{Input: 300, Output: 30, Total: 330}
	rollout := strings.Join([]string{
		codexTurnContextLine("2026-11-25T01:00:00Z", "gpt-6-luna"),
		codexTokenCountLine("2026-11-25T01:00:00Z", &t1, u),
		codexTokenCountLine("", &t2, u),
		codexTokenCountLine("2026-11-25T03:00:00Z", &t3, u),
	}, "\n")
	since := time.Date(2026, 11, 25, 2, 0, 0, 0, time.UTC).UnixMilli()
	sess, err := parseCodexRollout(strings.NewReader(rollout), "id", "a", since)
	if err != nil || sess == nil {
		t.Fatalf("parse: %v %v", sess, err)
	}
	if sess.Messages != 1 || sess.TotalTokens != 110 {
		t.Fatalf("only the event after since must count: %+v", sess)
	}
	none, err := parseCodexRollout(strings.NewReader(rollout), "id", "a", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli())
	if err != nil || none != nil {
		t.Fatalf("a rollout with no usage after since must yield no session: %+v %v", none, err)
	}
}

func TestReadCodexLine_SkipsOverlongLines(t *testing.T) {
	u := &codexTokenUsage{Input: 10, Output: 1, Total: 11}
	long := `{"type":"response_item","payload":{"output":"` + strings.Repeat("x", maxCodexRolloutLineBytes+10) + ` token_count"}}`
	rollout := long + "\n" + codexTokenCountLine("2026-11-25T01:00:00Z", u, u) + "\n"
	sess, err := parseCodexRollout(strings.NewReader(rollout), "id", "a", 0)
	if err != nil || sess == nil || sess.TotalTokens != 11 {
		t.Fatalf("the line after an overlong one must still be read: %+v %v", sess, err)
	}

	br := bufio.NewReaderSize(strings.NewReader("abc\r\nlast"), 16)
	line, err := readCodexLine(br)
	if string(line) != "abc" || err != nil {
		t.Fatalf("first line: %q %v", line, err)
	}
	line, err = readCodexLine(br)
	if string(line) != "last" || err != io.EOF {
		t.Fatalf("unterminated last line: %q %v", line, err)
	}
}

func TestScanCodexRollouts_AttributesPerAgentAndSkipsShared(t *testing.T) {
	root := t.TempDir()
	u := &codexTokenUsage{Input: 100, CachedRead: 50, Output: 10, Total: 110}
	writeCodexRolloutFile(t, root, "supervisor", "2026/11/25", "rollout-2026-11-25T01-00-00-aaa.jsonl",
		codexTurnContextLine("2026-11-25T01:00:00Z", "gpt-6-luna"), codexTokenCountLine("2026-11-25T01:00:01Z", u, u))
	writeCodexRolloutFile(t, root, "reviewer", "2026/11/25", "rollout-2026-11-25T02-00-00-bbb.jsonl",
		codexTurnContextLine("2026-11-25T02:00:00Z", "gpt-6.2-sol"), codexTokenCountLine("2026-11-25T02:00:01Z", u, u))
	// A rollout with no usage (a turn that made no model call).
	writeCodexRolloutFile(t, root, "guide", "2026/11/25", "rollout-2026-11-25T03-00-00-ccc.jsonl",
		codexTurnContextLine("2026-11-25T03:00:00Z", "gpt-6.2-sol"))
	// The shared home has no owner and is not scanned.
	writeCodexRolloutFile(t, root, "", "2026/11/25", "rollout-2026-11-25T04-00-00-ddd.jsonl",
		codexTokenCountLine("2026-11-25T04:00:01Z", u, u))
	// Too old for the 30-day window.
	old := writeCodexRolloutFile(t, root, "scanner", "2026/09/01", "rollout-2026-09-01T00-00-00-eee.jsonl",
		codexTokenCountLine("2026-09-01T00:00:01Z", u, u))
	past := time.Now().Add(-maxCodexSessionAge - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	agg, err := ScanCodexRollouts(root, 0)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if agg.SessionCount != 2 {
		t.Fatalf("want 2 sessions (supervisor, reviewer), got %d: %+v", agg.SessionCount, agg.Sessions)
	}
	if agg.ByAgent["supervisor"] != 110 || agg.ByAgent["reviewer"] != 110 {
		t.Fatalf("per-agent attribution wrong: %v", agg.ByAgent)
	}
	if agg.ByModel["gpt-6-luna"] != 110 || agg.ByModel["gpt-6.2-sol"] != 110 {
		t.Fatalf("per-model attribution wrong: %v", agg.ByModel)
	}
	for _, s := range agg.Sessions {
		if !strings.HasPrefix(s.SessionID, codexSessionIDPrefix+s.Agent+"-rollout-") {
			t.Fatalf("session id not namespaced: %q", s.SessionID)
		}
	}
}

func TestCollectorCountsCodexUsageFromMeteringStart(t *testing.T) {
	root := t.TempDir()
	metrics := t.TempDir()
	persist := filepath.Join(t.TempDir(), "token-summary.json")
	u := &codexTokenUsage{Input: 100, Output: 10, Total: 110}
	t1 := *u
	t2 := codexTokenUsage{Input: 200, Output: 20, Total: 220}
	history := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	writeCodexRolloutFile(t, root, "supervisor", "2026/11/25", "rollout-2026-11-25T01-00-00-aaa.jsonl",
		codexTurnContextLine(history, "gpt-6-luna"),
		codexTokenCountLine(history, &t1, u),
		codexTokenCountLine(recent, &t2, u))

	c := NewCollector(metrics, nil)
	c.SetPersistPath(persist)
	c.SetCodexHomesRoot(root)
	c.scan()

	sum := c.Summary()
	if sum.ByAgent["supervisor"] != 110 {
		t.Fatalf("only usage after the metering start may count (history is not backfilled into the budget window): %v", sum.ByAgent)
	}
	marker := filepath.Join(filepath.Dir(persist), codexMeteringSinceFile)
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("metering start marker not written: %v", err)
	}
	since, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || since <= 0 {
		t.Fatalf("marker content invalid: %q", data)
	}

	// A restarted collector reuses the recorded start instead of moving it.
	c2 := NewCollector(metrics, nil)
	c2.SetPersistPath(persist)
	c2.SetCodexHomesRoot(root)
	c2.scan()
	if got := c2.codexMeteringSince(); got != since {
		t.Fatalf("restart moved the metering start: %d != %d", got, since)
	}
	if c2.Summary().ByAgent["supervisor"] != 110 {
		t.Fatalf("restart changed codex totals: %v", c2.Summary().ByAgent)
	}
	if d := c2.Diagnostics(); d.LastCodexScanError != "" {
		t.Fatalf("unexpected codex scan error: %q", d.LastCodexScanError)
	}
}

func TestCollectorWithoutCodexRootIgnoresRollouts(t *testing.T) {
	root := t.TempDir()
	u := &codexTokenUsage{Input: 100, Output: 10, Total: 110}
	writeCodexRolloutFile(t, root, "supervisor", "2026/11/25", "rollout-x.jsonl", codexTokenCountLine(time.Now().UTC().Format(time.RFC3339), u, u))
	c := NewCollector(t.TempDir(), nil)
	c.SetPersistPath(filepath.Join(t.TempDir(), "s.json"))
	c.scan()
	if c.Summary().ByAgent["supervisor"] != 0 {
		t.Fatal("codex rollouts must be read only when the codex homes root is set")
	}
}

func TestCodexAgentFromRolloutPath(t *testing.T) {
	root := "/data/home"
	cases := map[string]string{
		"/data/home/.codex-reviewer/sessions/2026/11/25/rollout-a.jsonl": "reviewer",
		"/data/home/.codex/sessions/2026/11/25/rollout-a.jsonl":          "",
		"/data/home/.codex-/sessions/2026/11/25/rollout-a.jsonl":         "",
	}
	for path, want := range cases {
		if got := codexAgentFromRolloutPath(root, path); got != want {
			t.Errorf("%s: got %q want %q", path, got, want)
		}
	}
}

// An agent sessions tree the hive cannot read must surface as an error, not
// as silent zero usage.
func TestScanCodexRollouts_UnreadableTreeIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	root := t.TempDir()
	u := &codexTokenUsage{Input: 100, Output: 10, Total: 110}
	writeCodexRolloutFile(t, root, "reviewer", "2026/11/25", "rollout-a.jsonl", codexTokenCountLine("2026-11-25T01:00:01Z", u, u))
	writeCodexRolloutFile(t, root, "supervisor", "2026/11/25", "rollout-b.jsonl", codexTokenCountLine("2026-11-25T01:00:01Z", u, u))
	locked := filepath.Join(root, ".codex-supervisor", "sessions")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	agg, err := ScanCodexRollouts(root, 0)
	if err == nil || !strings.Contains(err.Error(), "codex sessions") {
		t.Fatalf("an unreadable sessions tree must be reported, got err=%v", err)
	}
	if agg.ByAgent["reviewer"] != 110 {
		t.Fatalf("readable agents must still be counted: %v", agg.ByAgent)
	}
	if missing := codexUnreadableSessionsErr(t.TempDir()); missing != nil {
		t.Fatalf("no codex homes must not be an error: %v", missing)
	}
}
