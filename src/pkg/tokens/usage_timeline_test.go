package tokens

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeClaudeSession writes a JSONL session file and returns its path.
func writeClaudeSession(t *testing.T, dir, name, body string) string {
	t.Helper()
	projDir := filepath.Join(dir, "proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(projDir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// TestClaudeUsageTimelineRetainsPerMessageGrain is the core Phase 2 assertion:
// the scanner keeps a per-message, timestamped usage entry AND the pre-existing
// summed totals, and the two reconcile exactly.
func TestClaudeUsageTimelineRetainsPerMessageGrain(t *testing.T) {
	dir := t.TempDir()
	body := `{"type":"assistant","timestamp":"2026-08-01T10:00:00Z","message":{"model":"claude-opus-4-1","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":20,"cache_creation_input_tokens":10}}}
{"type":"assistant","timestamp":"2026-08-01T11:00:00Z","message":{"model":"claude-opus-4-1","usage":{"input_tokens":200,"output_tokens":75,"cache_read_input_tokens":0,"cache_creation_input_tokens":5}}}
{"type":"human","timestamp":"2026-08-01T10:30:00Z","message":{"text":"go"}}
`
	path := writeClaudeSession(t, dir, "s1.jsonl", body)

	sum, err := parseClaudeSessionFile(path, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Summed fields must be untouched by Phase 2.
	if sum.InputTokens != 300 || sum.OutputTokens != 125 || sum.CacheRead != 20 || sum.CacheCreate != 15 {
		t.Fatalf("summed fields changed: %+v", sum)
	}
	if sum.TotalTokens != 460 {
		t.Fatalf("TotalTokens = %d, want 460", sum.TotalTokens)
	}
	if sum.Backend != BackendClaude {
		t.Fatalf("Backend = %q, want %q", sum.Backend, BackendClaude)
	}

	// Timeline must exist at per-message grain and reconcile with the totals.
	if len(sum.Usage) != 2 {
		t.Fatalf("len(Usage) = %d, want 2: %+v", len(sum.Usage), sum.Usage)
	}
	if got := sum.UsageTotal(); got != sum.TotalTokens {
		t.Fatalf("UsageTotal() = %d, want TotalTokens %d", got, sum.TotalTokens)
	}
	if sum.UsageCoalesced != 0 {
		t.Fatalf("UsageCoalesced = %d, want 0 (no coalescing needed)", sum.UsageCoalesced)
	}

	// Ordered ascending by time, with the real per-message split preserved.
	if sum.Usage[0].TimestampMs >= sum.Usage[1].TimestampMs {
		t.Fatalf("timeline not time-ordered: %+v", sum.Usage)
	}
	if sum.Usage[0].Input != 100 || sum.Usage[1].Input != 200 {
		t.Fatalf("per-message split lost: %+v", sum.Usage)
	}
	if sum.Usage[0].Model != "claude-opus-4-1" {
		t.Fatalf("model not carried: %+v", sum.Usage[0])
	}
}

// TestClaudeUsageTimelineCoalescesRatherThanTruncates asserts the memory bound
// preserves tokens. A session far over the cap must still reconcile exactly
// against TotalTokens, and must report that it was coalesced — silent
// truncation of a cost timeline is forbidden.
func TestClaudeUsageTimelineCoalescesRatherThanTruncates(t *testing.T) {
	dir := t.TempDir()
	const n = maxUsageEventsPerSession*2 + 500
	body := ""
	for i := 0; i < n; i++ {
		body += fmt.Sprintf(
			`{"type":"assistant","timestamp":"2026-08-01T%02d:%02d:%02dZ","message":{"model":"m","usage":{"input_tokens":1,"output_tokens":1}}}`+"\n",
			i/3600%24, i/60%60, i%60)
	}
	path := writeClaudeSession(t, dir, "big.jsonl", body)

	sum, err := parseClaudeSessionFile(path, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if sum.TotalTokens != int64(n*2) {
		t.Fatalf("TotalTokens = %d, want %d", sum.TotalTokens, n*2)
	}
	if len(sum.Usage) > maxUsageEventsPerSession {
		t.Fatalf("timeline unbounded: %d events > cap %d", len(sum.Usage), maxUsageEventsPerSession)
	}
	// The whole point: no tokens lost to the bound.
	if got := sum.UsageTotal(); got != sum.TotalTokens {
		t.Fatalf("coalescing lost tokens: UsageTotal %d != TotalTokens %d", got, sum.TotalTokens)
	}
	if sum.UsageCoalesced == 0 {
		t.Fatal("UsageCoalesced = 0 — degraded timeline must be reported, not hidden")
	}
	// Coalesced counts must add up to the number of real events.
	var raw int
	for _, u := range sum.Usage {
		raw += u.Coalesced
	}
	if raw != n {
		t.Fatalf("sum of Coalesced = %d, want %d raw events", raw, n)
	}
}

// TestUsageTimelineUnknownTimestampsSortLast pins the ordering rule that keeps
// unplaceable tokens out of a repo's interval: a zero timestamp must never sort
// to the front where it would look like the session's opening message.
func TestUsageTimelineUnknownTimestampsSortLast(t *testing.T) {
	var tl usageTimeline
	tl.add(UsageEvent{TimestampMs: 0, Input: 5})
	tl.add(UsageEvent{TimestampMs: 2000, Input: 5})
	tl.add(UsageEvent{TimestampMs: 1000, Input: 5})
	events, _ := tl.finish()
	if len(events) != 3 {
		t.Fatalf("len = %d, want 3", len(events))
	}
	if events[0].TimestampMs != 1000 || events[1].TimestampMs != 2000 || events[2].TimestampMs != 0 {
		t.Fatalf("bad order: %+v", events)
	}
}

// TestSnapshotOmitsUsageTimeline pins that the live-only timeline never reaches
// the persisted snapshot. It is recomputed from the source JSONL on every scan,
// so persisting it buys nothing — but it would rewrite megabytes to
// /data/token-summary.json every scan interval. The summed totals must survive
// untouched, and the in-memory aggregate must NOT be mutated by saving.
func TestSnapshotOmitsUsageTimeline(t *testing.T) {
	agg := &AggregateSummary{
		TotalTokens: 300,
		Sessions: []SessionSummary{{
			SessionID: "s1", Backend: BackendClaude,
			InputTokens: 200, OutputTokens: 100, TotalTokens: 300,
			Usage:          []UsageEvent{{TimestampMs: 1000, Coalesced: 2, Input: 200, Output: 100}},
			UsageCoalesced: 1,
		}},
	}

	stripped := stripUsageTimelines(agg)
	if len(stripped.Sessions[0].Usage) != 0 || stripped.Sessions[0].UsageCoalesced != 0 {
		t.Fatalf("timeline reached the snapshot: %+v", stripped.Sessions[0])
	}
	if stripped.Sessions[0].TotalTokens != 300 || stripped.TotalTokens != 300 {
		t.Fatalf("stripping changed the totals: %+v", stripped.Sessions[0])
	}
	// The live aggregate must be unaffected — the join still needs its timeline.
	if len(agg.Sessions[0].Usage) != 1 {
		t.Fatal("stripUsageTimelines mutated the live aggregate")
	}
}

// TestUsageTimelineDropsZeroTokenEvents keeps the timeline free of no-cost
// entries, which would only dilute it.
func TestUsageTimelineDropsZeroTokenEvents(t *testing.T) {
	var tl usageTimeline
	tl.add(UsageEvent{TimestampMs: 1000})
	events, coalesced := tl.finish()
	if len(events) != 0 || coalesced != 0 {
		t.Fatalf("zero-token event retained: %+v", events)
	}
}
