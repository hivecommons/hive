package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/tokens"
)

// Unit tests for the pure cost-formatting functions: flattenEstimated,
// sourceForPriced, and estimatedSessions. These convert internal token data
// to the /api/cost JSON response and previously had no direct unit coverage
// (only exercised indirectly through the full handleCost handler).

func TestSourceForPriced(t *testing.T) {
	if got := sourceForPriced(true); got != "estimated" {
		t.Errorf("sourceForPriced(true) = %q, want %q", got, "estimated")
	}
	if got := sourceForPriced(false); got != "unpriced" {
		t.Errorf("sourceForPriced(false) = %q, want %q", got, "unpriced")
	}
}

func TestFlattenEstimated_Empty(t *testing.T) {
	est := tokens.EstimatedCost{
		TotalUSD:       0,
		ByModel:        map[string]tokens.ModelCost{},
		ByAgent:        map[string]tokens.ModelCost{},
		UnpricedModels: []string{},
	}
	out := flattenEstimated(est)
	if out.TotalUSD != 0 {
		t.Errorf("TotalUSD = %f, want 0", out.TotalUSD)
	}
	if len(out.ByModel) != 0 {
		t.Errorf("ByModel len = %d, want 0", len(out.ByModel))
	}
	if len(out.ByAgent) != 0 {
		t.Errorf("ByAgent len = %d, want 0", len(out.ByAgent))
	}
	if out.ByModel == nil {
		t.Error("ByModel must be non-nil (empty slice, not null in JSON)")
	}
	if out.ByAgent == nil {
		t.Error("ByAgent must be non-nil (empty slice, not null in JSON)")
	}
}

func TestFlattenEstimated_WithModelsAndAgents(t *testing.T) {
	est := tokens.EstimatedCost{
		TotalUSD: 1.23,
		ByModel: map[string]tokens.ModelCost{
			"claude-sonnet-4-20250514": {
				USD:         0.80,
				Priced:      true,
				Input:       1000,
				Output:      500,
				CacheRead:   200,
				CacheCreate: 50,
			},
			"unknown-model": {
				USD:    0,
				Priced: false,
				Input:  300,
				Output: 100,
			},
		},
		ByAgent: map[string]tokens.ModelCost{
			"quality": {
				USD:    0.43,
				Priced: true,
				Input:  700,
				Output: 400,
			},
		},
		UnpricedModels: []string{"unknown-model"},
	}

	out := flattenEstimated(est)

	if out.TotalUSD != 1.23 {
		t.Errorf("TotalUSD = %f, want 1.23", out.TotalUSD)
	}
	if len(out.ByModel) != 2 {
		t.Fatalf("ByModel len = %d, want 2", len(out.ByModel))
	}
	if len(out.ByAgent) != 1 {
		t.Fatalf("ByAgent len = %d, want 1", len(out.ByAgent))
	}

	// Verify model entries carry correct source tags.
	foundPriced, foundUnpriced := false, false
	for _, m := range out.ByModel {
		if m.Name == "claude-sonnet-4-20250514" {
			foundPriced = true
			if m.Source != "estimated" {
				t.Errorf("priced model source = %q, want %q", m.Source, "estimated")
			}
			if m.USD != 0.80 {
				t.Errorf("priced model USD = %f, want 0.80", m.USD)
			}
			if m.Input != 1000 || m.Output != 500 {
				t.Errorf("token counts = %d/%d, want 1000/500", m.Input, m.Output)
			}
		}
		if m.Name == "unknown-model" {
			foundUnpriced = true
			if m.Source != "unpriced" {
				t.Errorf("unpriced model source = %q, want %q", m.Source, "unpriced")
			}
		}
	}
	if !foundPriced {
		t.Error("priced model 'claude-sonnet-4-20250514' not found in ByModel")
	}
	if !foundUnpriced {
		t.Error("unpriced model 'unknown-model' not found in ByModel")
	}

	// Verify agent entry.
	agent := out.ByAgent[0]
	if agent.Name != "quality" {
		t.Errorf("agent name = %q, want %q", agent.Name, "quality")
	}
	if agent.Source != "estimated" {
		t.Errorf("agent source = %q, want %q", agent.Source, "estimated")
	}

	// Verify unpriced models list is passed through.
	if len(out.UnpricedModels) != 1 || out.UnpricedModels[0] != "unknown-model" {
		t.Errorf("UnpricedModels = %v, want [unknown-model]", out.UnpricedModels)
	}
}

func TestEstimatedSessions_NilSummary(t *testing.T) {
	out := estimatedSessions(nil)
	if out == nil {
		t.Fatal("estimatedSessions(nil) must return non-nil empty slice")
	}
	if len(out) != 0 {
		t.Errorf("len = %d, want 0", len(out))
	}
}

func TestEstimatedSessions_EmptySessions(t *testing.T) {
	agg := &tokens.AggregateSummary{}
	out := estimatedSessions(agg)
	if out == nil {
		t.Fatal("must return non-nil empty slice")
	}
	if len(out) != 0 {
		t.Errorf("len = %d, want 0", len(out))
	}
}

func TestEstimatedSessions_PricesAndSortsByStartThenAgent(t *testing.T) {
	agg := &tokens.AggregateSummary{
		Sessions: []tokens.SessionSummary{
			{
				SessionID:    "sess-cheap",
				Agent:        "z-quality",
				Model:        "claude-haiku-3-20240307",
				InputTokens:  100,
				OutputTokens: 50,
				FirstActive:  200,
			},
			{
				SessionID:    "sess-expensive",
				Agent:        "a-sec-check",
				Model:        "claude-sonnet-4-20250514",
				InputTokens:  10000,
				OutputTokens: 5000,
				CacheRead:    1000,
				FirstActive:  200,
			},
		},
	}

	out := estimatedSessions(agg)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}

	// Same start time: agent name ascending, not cost descending.
	if out[0].SessionID != "sess-expensive" {
		t.Errorf("first session = %q, want sess-expensive (agent name tie-break)", out[0].SessionID)
	}
	if out[1].SessionID != "sess-cheap" {
		t.Errorf("second session = %q, want sess-cheap", out[1].SessionID)
	}

	// Agent and model are preserved.
	if out[0].Agent != "a-sec-check" || out[0].Model != "claude-sonnet-4-20250514" {
		t.Errorf("expensive session agent/model = %q/%q", out[0].Agent, out[0].Model)
	}

	// Token counts are passed through.
	if out[0].Input != 10000 || out[0].Output != 5000 || out[0].CacheRead != 1000 {
		t.Errorf("token counts not preserved: input=%d output=%d cache=%d",
			out[0].Input, out[0].Output, out[0].CacheRead)
	}
}

func TestEstimatedSessions_CapsAtMaxCostSessions(t *testing.T) {
	// Build more sessions than maxCostSessions.
	sessions := make([]tokens.SessionSummary, maxCostSessions+10)
	for i := range sessions {
		sessions[i] = tokens.SessionSummary{
			SessionID:    "sess-" + sourceForPriced(i%2 == 0), // reuse for unique-ish ids
			Agent:        "agent",
			Model:        "claude-sonnet-4-20250514",
			InputTokens:  int64(i * 100),
			OutputTokens: int64(i * 50),
		}
	}
	agg := &tokens.AggregateSummary{Sessions: sessions}

	out := estimatedSessions(agg)
	if len(out) > maxCostSessions {
		t.Errorf("len = %d, exceeds maxCostSessions = %d", len(out), maxCostSessions)
	}
}
