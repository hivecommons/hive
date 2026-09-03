package dashboard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/tokens"
)

// TestEstimatedSessions_NilAndEmpty verifies the builder never returns a nil
// slice, matching the /api/cost contract (UI guards with (arr||[]) but the
// backend should still emit []).
func TestEstimatedSessions_NilAndEmpty(t *testing.T) {
	if got := estimatedSessions(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil summary: got %v, want empty non-nil slice", got)
	}
	empty := &tokens.AggregateSummary{}
	if got := estimatedSessions(empty); got == nil || len(got) != 0 {
		t.Fatalf("empty summary: got %v, want empty non-nil slice", got)
	}
}

// TestEstimatedSessions_PricesAndSorts verifies each session is priced from its
// own token splits, carries its sandbox (agent)/model/session id, and rows come
// back most-expensive first.
func TestEstimatedSessions_PricesAndSorts(t *testing.T) {
	summary := &tokens.AggregateSummary{
		Sessions: []tokens.SessionSummary{
			{SessionID: "sess-small", Agent: "scanner", Model: "claude-opus-4.7", InputTokens: 1_000, OutputTokens: 500, Messages: 3, FirstActive: 100, LastActive: 111},
			{SessionID: "sess-big", Agent: "quality", Model: "claude-opus-4.7", InputTokens: 5_000_000, OutputTokens: 2_000_000, Messages: 40, FirstActive: 200, LastActive: 222},
		},
	}

	got := estimatedSessions(summary)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	// Most-expensive first: the big session must lead.
	if got[0].SessionID != "sess-big" {
		t.Errorf("row[0] = %q, want sess-big (should sort most-expensive first)", got[0].SessionID)
	}
	if got[0].USD <= got[1].USD {
		t.Errorf("not sorted by cost desc: %v <= %v", got[0].USD, got[1].USD)
	}

	// Per-session pricing must equal EstimateCostUSD over that session's splits.
	wantUSD, exact := tokens.EstimateCostUSD("claude-opus-4.7", 5_000_000, 2_000_000, 0, 0)
	if got[0].USD != wantUSD {
		t.Errorf("big session USD = %v, want %v (priced from its own splits)", got[0].USD, wantUSD)
	}
	if got[0].Source != sourceForPriced(exact) {
		t.Errorf("big session source = %q, want %q", got[0].Source, sourceForPriced(exact))
	}

	// Identity fields survive.
	if got[0].Agent != "quality" || got[0].Model != "claude-opus-4.7" {
		t.Errorf("big session identity = %q/%q, want quality/claude-opus-4.7", got[0].Agent, got[0].Model)
	}
	if got[0].Messages != 40 || got[0].LastActive != 222 {
		t.Errorf("big session meta = msgs %d last %d, want 40/222", got[0].Messages, got[0].LastActive)
	}
	if got[0].Started != 200 {
		t.Errorf("big session started = %d, want 200 (FirstActive passed through)", got[0].Started)
	}
	if got[0].Input != 5_000_000 || got[0].Output != 2_000_000 {
		t.Errorf("big session token splits = %d/%d, want 5000000/2000000", got[0].Input, got[0].Output)
	}
}

// TestEstimatedSessions_UnpricedModelSource verifies a model absent from the
// exact price table is tagged "unpriced" (still tier-priced, non-zero) rather
// than silently dropped.
func TestEstimatedSessions_UnpricedModelSource(t *testing.T) {
	summary := &tokens.AggregateSummary{
		Sessions: []tokens.SessionSummary{
			{SessionID: "s1", Agent: "a", Model: "some-unknown-mid-model", InputTokens: 1000, OutputTokens: 1000},
		},
	}
	got := estimatedSessions(summary)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Source != "unpriced" {
		t.Errorf("source = %q, want unpriced for a model absent from the price table", got[0].Source)
	}
	if got[0].USD <= 0 {
		t.Errorf("USD = %v, want > 0 (tier fallback, never $0)", got[0].USD)
	}
}

// TestEstimatedSessions_CapsRows verifies the payload is bounded at
// maxCostSessions and keeps the newest-started rows.
func TestEstimatedSessions_CapsRows(t *testing.T) {
	over := maxCostSessions + 25
	sessions := make([]tokens.SessionSummary, over)
	for i := range sessions {
		// Larger i → newer start, so the top rows are the tail.
		sessions[i] = tokens.SessionSummary{
			SessionID:   "s",
			Agent:       "a",
			Model:       "claude-opus-4.7",
			InputTokens: int64(i+1) * 1000,
			FirstActive: int64(i + 1),
		}
	}
	summary := &tokens.AggregateSummary{Sessions: sessions}

	got := estimatedSessions(summary)
	if len(got) != maxCostSessions {
		t.Fatalf("len = %d, want cap %d", len(got), maxCostSessions)
	}
	if got[0].Started != int64(over) {
		t.Errorf("row[0] started = %d, want %d (newest kept after cap)", got[0].Started, over)
	}
}

// TestEstimatedSessions_StartEndTimestamps verifies the session time bracket
// (start = earliest event ts, end = latest event ts) survives aggregation into
// the /api/cost rows, and that an active/unknown-start session (FirstActive 0)
// serializes without a fake "started" field so the UI can render "—" / active.
func TestEstimatedSessions_StartEndTimestamps(t *testing.T) {
	const start, end = int64(1_755_800_000_000), int64(1_755_803_600_000) // 1h apart, unix ms
	summary := &tokens.AggregateSummary{
		Sessions: []tokens.SessionSummary{
			{SessionID: "sess-done", Agent: "scanner", Model: "claude-opus-4.7", InputTokens: 1000, FirstActive: start, LastActive: end},
			{SessionID: "sess-nostart", Agent: "quality", Model: "claude-opus-4.7", InputTokens: 500, LastActive: end},
		},
	}

	got := estimatedSessions(summary)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	byID := map[string]costSessionEntry{}
	for _, e := range got {
		byID[e.SessionID] = e
	}

	done := byID["sess-done"]
	if done.Started != start || done.LastActive != end {
		t.Errorf("sess-done bracket = %d→%d, want %d→%d", done.Started, done.LastActive, start, end)
	}
	if done.Started >= done.LastActive {
		t.Errorf("started %d should precede ended %d", done.Started, done.LastActive)
	}

	// Unknown start must serialize with no "started" key (omitempty) — the UI
	// shows "—" rather than a fabricated timestamp.
	noStart := byID["sess-nostart"]
	if noStart.Started != 0 {
		t.Errorf("sess-nostart started = %d, want 0", noStart.Started)
	}
	blob, err := json.Marshal(noStart)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), `"started"`) {
		t.Errorf("zero Started must be omitted from JSON, got %s", blob)
	}
	if !strings.Contains(string(blob), `"last_active"`) {
		t.Errorf("last_active missing from JSON: %s", blob)
	}
	full, err := json.Marshal(done)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(full), `"started":1755800000000`) {
		t.Errorf("started field missing/wrong in JSON: %s", full)
	}
}

// TestCostSessionTimeColumnWiring pins the "started → ended" column added to
// the cost-per-session table: the header, the active-session sentinel, and the
// constant the active heuristic relies on. Guards the renderAll()-style bug
// class where a referenced identifier silently disappears.
func TestCostSessionTimeColumnWiring(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)
	for _, snippet := range []string{
		"const COST_SESSION_ACTIVE_MS = 5 * 60 * 1000;",
		"session: { key: 'started', dir: 'desc' }",
		"costSortTh('session', 'started', 'started → ended'",
		"data-action=\"costSortTable\"",
		`cost-session-active">active</span>`,
		"sn.started ? fmtSessTs(sn.started) : '—'",
		"(Date.now() - sn.last_active) < COST_SESSION_ACTIVE_MS",
		`<td colspan="7"`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing cost session time snippet %q", snippet)
		}
	}
}
