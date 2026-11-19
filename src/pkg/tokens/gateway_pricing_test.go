package tokens

import (
	"math"
	"testing"
)

const priceEps = 1e-9

func withGatewayPricer(t *testing.T, fn GatewayPriceFunc) {
	t.Helper()
	SetGatewayPricer(fn)
	t.Cleanup(func() { SetGatewayPricer(nil) })
}

func near(a, b float64) bool { return math.Abs(a-b) < priceEps }

// DeepSeek V4.1 Flash on OpenRouter, per 1M: $0.14 in, $0.0042 cached, $0.42 out.
var flashPrice = ModelPrice{InputPerMTok: 0.14, OutputPerMTok: 0.42, CacheReadPerMTok: 0.0042, CacheWritePerMTok: 0.14}

func gatewayStub(prices map[string]ModelPrice, servedUnpriced ...string) GatewayPriceFunc {
	unpriced := map[string]bool{}
	for _, m := range servedUnpriced {
		unpriced[m] = true
	}
	return func(model string) (ModelPrice, bool, bool) {
		if p, ok := prices[model]; ok {
			return p, true, true
		}
		return ModelPrice{}, false, unpriced[model]
	}
}

func TestGatewayPriceWinsAndCachedInputUsesCacheRate(t *testing.T) {
	withGatewayPricer(t, gatewayStub(map[string]ModelPrice{"deepseek/deepseek-v4.1-flash": flashPrice}))
	const in, cached, out = 1_000_000, 4_000_000, 1_000_000
	usd, src := EstimateCostWithSource("deepseek/deepseek-v4.1-flash", true, in, out, cached, 0)
	if src != PriceSourceGateway {
		t.Fatalf("source = %q, want gateway", src)
	}
	// 1M × 0.14 + 1M × 0.42 + 4M × 0.0042 — cached tokens at the cache rate,
	// not the input rate.
	if want := 0.14 + 0.42 + 4*0.0042; !near(usd, want) {
		t.Fatalf("usd = %v, want %v", usd, want)
	}
	if usd2, exact := EstimateCostUSD("deepseek/deepseek-v4.1-flash", in, out, cached, 0); !exact || !near(usd2, usd) {
		t.Fatalf("EstimateCostUSD = %v, %v", usd2, exact)
	}
}

func TestGatewayModelWithoutPriceIsUnknownNotCounted(t *testing.T) {
	withGatewayPricer(t, gatewayStub(nil, "Qwen/Qwen3-32B"))
	usd, src := EstimateCostWithSource("Qwen/Qwen3-32B", false, 1000, 1000, 0, 0)
	if src != PriceSourceUnknown || usd != 0 {
		t.Fatalf("served-but-unpriced = %v %q, want 0 unknown", usd, src)
	}
	// Seen on the inference path with no pricer entry at all: still unknown,
	// never a tier guess.
	SetGatewayPricer(nil)
	if _, src := EstimateCostWithSource("my-finetune-7b", true, 1000, 1000, 0, 0); src != PriceSourceUnknown {
		t.Fatalf("inference-path model source = %q, want unknown", src)
	}
	// A CLI model absent from the table keeps the tier estimate.
	if usd, src := EstimateCostWithSource("some-cli-model", false, 1_000_000, 0, 0, 0); src != PriceSourceTier || usd <= 0 {
		t.Fatalf("CLI fallback = %v %q, want tier > 0", usd, src)
	}
	// A gateway model that is in the static table is still priced from it.
	if _, src := EstimateCostWithSource("claude-sonnet-4-6", true, 1, 1, 0, 0); src != PriceSourceTable {
		t.Fatalf("table model via gateway source = %q", src)
	}
}

func TestEstimateFromSummaryListsUnknownAndExcludesFromTotal(t *testing.T) {
	withGatewayPricer(t, gatewayStub(map[string]ModelPrice{"deepseek/deepseek-v4.1-flash": flashPrice}))
	agg := &AggregateSummary{
		ByModelDetail: map[string]*AgentModelBucket{
			"deepseek/deepseek-v4.1-flash": {Input: 1_000_000, Output: 1_000_000},
			"local/qwen":                   {Input: 5_000_000, Output: 5_000_000},
		},
		ByAgentDetail: map[string]*AgentModelBucket{
			"scout":  {Input: 1_000_000, Output: 1_000_000},
			"worker": {Input: 5_000_000, Output: 5_000_000},
		},
		Sessions: []SessionSummary{
			{Agent: "scout", Model: "deepseek/deepseek-v4.1-flash", Backend: BackendInference, InputTokens: 1_000_000, OutputTokens: 1_000_000},
			{Agent: "worker", Model: "local/qwen", Backend: BackendInference, InputTokens: 5_000_000, OutputTokens: 5_000_000},
		},
	}
	est := EstimateFromSummary(agg)
	if want := 0.14 + 0.42; !near(est.TotalUSD, want) {
		t.Fatalf("total = %v, want %v (unknown-priced tokens excluded)", est.TotalUSD, want)
	}
	if len(est.UnknownPriceModels) != 1 || est.UnknownPriceModels[0] != "local/qwen" {
		t.Fatalf("UnknownPriceModels = %v", est.UnknownPriceModels)
	}
	if len(est.UnpricedModels) != 0 {
		t.Fatalf("an unknown gateway model must not be tier-estimated: %v", est.UnpricedModels)
	}
	if mc := est.ByModel["local/qwen"]; mc.PriceSource != PriceSourceUnknown || mc.USD != 0 || mc.Priced {
		t.Fatalf("local/qwen row = %+v", mc)
	}
	if mc := est.ByModel["deepseek/deepseek-v4.1-flash"]; mc.PriceSource != PriceSourceGateway || !mc.Priced {
		t.Fatalf("flash row = %+v", mc)
	}
	w := est.ByAgent["worker"]
	if w.PriceSource != PriceSourceUnknown || len(w.UnknownPriceModels) != 1 || w.UnknownPriceModels[0] != "local/qwen" {
		t.Fatalf("worker agent row = %+v", w)
	}
	if s := est.ByAgent["scout"]; s.PriceSource != PriceSourceGateway || len(s.UnknownPriceModels) != 0 {
		t.Fatalf("scout agent row = %+v", s)
	}
}

func TestLeastCertainSource(t *testing.T) {
	if got := leastCertainSource("", PriceSourceTable); got != PriceSourceTable {
		t.Errorf("empty start = %q", got)
	}
	if got := leastCertainSource(PriceSourceGateway, PriceSourceTier); got != PriceSourceTier {
		t.Errorf("gateway+tier = %q", got)
	}
	if got := leastCertainSource(PriceSourceUnknown, PriceSourceTable); got != PriceSourceUnknown {
		t.Errorf("unknown+table = %q", got)
	}
	if !PriceSourceTier.Counted() || PriceSourceUnknown.Counted() {
		t.Error("Counted")
	}
}
