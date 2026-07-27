package tokens

import (
	"math"
	"testing"
)

const floatEps = 1e-9

func approxEqual(a, b float64) bool { return math.Abs(a-b) < floatEps }

func TestEstimateCostUSD_KnownModel(t *testing.T) {
	// claude-opus-4-8: $5/MTok in, $25/MTok out, $0.50 cache read, $6.25 cache write.
	// 1,000,000 input + 1,000,000 output = $5 + $25 = $30.
	usd, priced := EstimateCostUSD("claude-opus-4-8", 1_000_000, 1_000_000, 0, 0)
	if !priced {
		t.Fatalf("expected claude-opus-4-8 to be priced")
	}
	if !approxEqual(usd, 30.0) {
		t.Fatalf("expected $30.00, got $%.6f", usd)
	}

	// Add cache: 1M cache read ($0.50) + 1M cache write ($6.25) = +$6.75 -> $36.75.
	usd, _ = EstimateCostUSD("claude-opus-4-8", 1_000_000, 1_000_000, 1_000_000, 1_000_000)
	if !approxEqual(usd, 36.75) {
		t.Fatalf("expected $36.75 with cache, got $%.6f", usd)
	}
}

func TestEstimateCostUSD_UnknownModel(t *testing.T) {
	// Unknown models are now tier-estimated, never $0: exact=false but usd>0.
	usd, exact := EstimateCostUSD("some-unknown-model-9000", 1_000_000, 1_000_000, 0, 0)
	if exact {
		t.Fatalf("expected unknown model to be a tier estimate (exact=false)")
	}
	if usd <= 0 {
		t.Fatalf("expected a non-zero tier estimate for an unknown model, got $%.6f", usd)
	}
}

// TestFallbackTierOrdering guards the "smaller/older cheaper, bigger/newer
// pricier" heuristic: a mini/flash model must estimate cheaper than an
// opus/flagship one for identical token counts.
func TestFallbackTierOrdering(t *testing.T) {
	cheap, _ := EstimateCostUSD("mystery-flash-mini", 1_000_000, 1_000_000, 0, 0)
	big, _ := EstimateCostUSD("mystery-opus-max", 1_000_000, 1_000_000, 0, 0)
	mid, _ := EstimateCostUSD("mystery-model-x", 1_000_000, 1_000_000, 0, 0)
	if !(cheap < mid && mid < big) {
		t.Fatalf("tier ordering broken: cheap=%.4f mid=%.4f big=%.4f (want cheap<mid<big)", cheap, mid, big)
	}
	if cheap <= 0 {
		t.Fatalf("cheap tier must still be non-zero, got %.6f", cheap)
	}
}

func TestEstimateCostUSD_FractionalTokens(t *testing.T) {
	// gpt-5-4: $1.25/MTok in. 500,000 input -> $0.625.
	usd, priced := EstimateCostUSD("gpt-5.4", 500_000, 0, 0, 0)
	if !priced {
		t.Fatalf("expected gpt-5.4 to be priced")
	}
	if !approxEqual(usd, 0.625) {
		t.Fatalf("expected $0.625, got $%.6f", usd)
	}
}

func TestNormalizeModelID(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-8":                   "claude-opus-4-8",
		"claude-opus-4.6":                   "claude-opus-4-6",
		"anthropic/claude-opus-4.8":         "claude-opus-4-8",
		"gpt-5.4":                           "gpt-5-4",
		"openai/gpt-5.4":                    "gpt-5-4",
		"deepseek/deepseek-chat:free":       "deepseek-chat",
		"  Claude-Opus-4-8  ":               "claude-opus-4-8",
		"meta-llama/Llama-3.3-70B-Instruct": "llama-3-3-70b-instruct",
	}
	for in, want := range cases {
		if got := normalizeModelID(in); got != want {
			t.Errorf("normalizeModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeVariantsResolveToSamePrice(t *testing.T) {
	// All three spellings must resolve to the same opus-4-8 price.
	variants := []string{"claude-opus-4-8", "claude-opus-4.8", "anthropic/claude-opus-4.8"}
	var want float64
	for i, v := range variants {
		usd, priced := EstimateCostUSD(v, 1_000_000, 0, 0, 0)
		if !priced {
			t.Fatalf("variant %q should be priced", v)
		}
		if i == 0 {
			want = usd
		} else if !approxEqual(usd, want) {
			t.Fatalf("variant %q priced $%.6f, want $%.6f", v, usd, want)
		}
	}
}

func TestEstimateFromSummary(t *testing.T) {
	agg := &AggregateSummary{
		ByModelDetail: map[string]*AgentModelBucket{
			"claude-opus-4-8": {Input: 1_000_000, Output: 1_000_000},
			"mystery-model":   {Input: 1_000_000, Output: 1_000_000},
		},
		ByAgentDetail: map[string]*AgentModelBucket{
			"scanner": {Input: 1_000_000, Output: 0},
		},
		Sessions: []SessionSummary{
			{Agent: "scanner", Model: "claude-opus-4-8", TotalTokens: 2_000_000},
		},
	}

	est := EstimateFromSummary(agg)

	// Both models contribute now: opus-4-8 1M+1M = $30 (exact) plus
	// mystery-model 1M+1M at the mid tier ($1.25 in + $10 out = $11.25).
	if !approxEqual(est.TotalUSD, 41.25) {
		t.Fatalf("expected total $41.25 (exact + tier estimate), got $%.6f", est.TotalUSD)
	}

	if mc := est.ByModel["claude-opus-4-8"]; !mc.Priced || !approxEqual(mc.USD, 30.0) {
		t.Fatalf("claude-opus-4-8 model cost wrong: %+v", mc)
	}
	// mystery-model: tier-estimated (Priced=false) but non-zero and included.
	if mc := est.ByModel["mystery-model"]; mc.Priced || !approxEqual(mc.USD, 11.25) {
		t.Fatalf("mystery-model should be tier-estimated ~$11.25, got %+v", mc)
	}

	// UnpricedModels now means "tier-estimated" (still surfaced so the UI can
	// mark it ~approximate), not "excluded".
	foundUnpriced := false
	for _, m := range est.UnpricedModels {
		if m == "mystery-model" {
			foundUnpriced = true
		}
	}
	if !foundUnpriced {
		t.Fatalf("expected mystery-model in UnpricedModels (tier-estimated list), got %v", est.UnpricedModels)
	}

	// Per-agent: scanner used opus-4-8 (1M input) -> $5.
	if ac := est.ByAgent["scanner"]; !ac.Priced || !approxEqual(ac.USD, 5.0) {
		t.Fatalf("scanner agent cost wrong: %+v", ac)
	}

	if est.PriceTableDate != PriceTableDate() {
		t.Fatalf("price table date mismatch: %q vs %q", est.PriceTableDate, PriceTableDate())
	}
}

func TestEstimateFromSummary_Nil(t *testing.T) {
	est := EstimateFromSummary(nil)
	if est.TotalUSD != 0 {
		t.Fatalf("nil summary should yield $0, got %v", est.TotalUSD)
	}
	if est.ByModel == nil || est.ByAgent == nil {
		t.Fatalf("nil summary should still return initialized maps")
	}
}

// TestGPT53CodexPriced guards the auto-mode routing target so it stops being
// excluded from the estimated total.
func TestGPT53CodexPriced(t *testing.T) {
	for _, id := range []string{"gpt-5.3-codex", "gpt-5-3-codex", "openai/gpt-5.3-codex"} {
		if _, priced := EstimateCostUSD(id, 1_000_000, 0, 0, 0); !priced {
			t.Errorf("%q should be priced (auto-mode routes here)", id)
		}
	}
}
