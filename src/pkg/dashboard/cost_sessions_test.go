package dashboard

import (
	"testing"

	"github.com/kubestellar/hive/pkg/tokens"
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
			{SessionID: "sess-small", Agent: "scanner", Model: "claude-opus-4.7", InputTokens: 1_000, OutputTokens: 500, Messages: 3, LastActive: 111},
			{SessionID: "sess-big", Agent: "quality", Model: "claude-opus-4.7", InputTokens: 5_000_000, OutputTokens: 2_000_000, Messages: 40, LastActive: 222},
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
// maxCostSessions and keeps the most-expensive rows.
func TestEstimatedSessions_CapsRows(t *testing.T) {
	over := maxCostSessions + 25
	sessions := make([]tokens.SessionSummary, over)
	for i := range sessions {
		// Larger i → more tokens → higher cost, so the top rows are the tail.
		sessions[i] = tokens.SessionSummary{
			SessionID:   "s",
			Agent:       "a",
			Model:       "claude-opus-4.7",
			InputTokens: int64(i+1) * 1000,
		}
	}
	summary := &tokens.AggregateSummary{Sessions: sessions}

	got := estimatedSessions(summary)
	if len(got) != maxCostSessions {
		t.Fatalf("len = %d, want cap %d", len(got), maxCostSessions)
	}
	// The single most-expensive session (largest i) must be present at row 0.
	wantTop, _ := tokens.EstimateCostUSD("claude-opus-4.7", int64(over)*1000, 0, 0, 0)
	if got[0].USD != wantTop {
		t.Errorf("row[0] USD = %v, want %v (most-expensive kept after cap)", got[0].USD, wantTop)
	}
}
