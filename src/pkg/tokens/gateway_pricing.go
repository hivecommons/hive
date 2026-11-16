package tokens

import "sync/atomic"

// PriceSource says where a model's price came from, so a spend figure can be
// labelled honestly.
type PriceSource string

const (
	// PriceSourceGateway: the price the model's gateway publishes — the
	// OpenRouter catalog, LiteLLM /model/info, or a gateway's configured
	// prices: override. Preferred over the static table because it is the
	// rate the gateway actually bills for that exact id.
	PriceSourceGateway PriceSource = "gateway"
	// PriceSourceTable: an exact list price from modelPrices.
	PriceSourceTable PriceSource = "table"
	// PriceSourceTier: a coarse tier estimate (FallbackPrice) for a CLI model
	// absent from the table. Counted, but marked approximate.
	PriceSourceTier PriceSource = "tier"
	// PriceSourceUnknown: the model runs through a gateway that publishes no
	// price and is not in the table. Its cost is NOT counted (and not shown as
	// $0): a tier guess for a self-hosted or custom model is meaningless.
	PriceSourceUnknown PriceSource = "unknown"
)

// Counted reports whether a cost from this source is included in totals.
func (s PriceSource) Counted() bool { return s != PriceSourceUnknown }

// Exact reports whether the price is a published figure (gateway or table)
// rather than an estimate or unknown.
func (s PriceSource) Exact() bool { return s == PriceSourceGateway || s == PriceSourceTable }

// GatewayPriceFunc resolves a model's price from the configured gateways.
// served reports whether any configured gateway serves model at all; known
// reports whether that gateway publishes (or is configured with) a price.
type GatewayPriceFunc func(model string) (price ModelPrice, known, served bool)

var gatewayPricer atomic.Pointer[GatewayPriceFunc]

// SetGatewayPricer installs (or, with nil, removes) the host's gateway price
// source. pkg/tokens stays free of config and network: the dashboard wires
// a function that reads the OpenRouter catalog, LiteLLM /model/info and
// gateway prices: overrides.
func SetGatewayPricer(fn GatewayPriceFunc) {
	if fn == nil {
		gatewayPricer.Store(nil)
		return
	}
	gatewayPricer.Store(&fn)
}

func lookupGatewayPrice(model string) (ModelPrice, bool, bool) {
	p := gatewayPricer.Load()
	if p == nil || model == "" {
		return ModelPrice{}, false, false
	}
	return (*p)(model)
}

// ResolvePrice returns the price for model and its source. viaGateway marks a
// model observed on the inference (gateway) path, which is never tier-guessed:
// gateway price, then the static table, else unknown. Other models resolve
// gateway price (when a configured gateway serves that exact id), then table,
// then tier.
func ResolvePrice(model string, viaGateway bool) (ModelPrice, PriceSource) {
	gp, known, served := lookupGatewayPrice(model)
	if known {
		return gp, PriceSourceGateway
	}
	if p, ok := LookupPrice(model); ok {
		return p, PriceSourceTable
	}
	if viaGateway || served {
		return ModelPrice{}, PriceSourceUnknown
	}
	p, _ := FallbackPrice(model)
	return p, PriceSourceTier
}

// costAt prices token counts at p.
func costAt(p ModelPrice, inputTok, outputTok, cacheReadTok, cacheWriteTok int64) float64 {
	return float64(inputTok)/tokensPerMillion*p.InputPerMTok +
		float64(outputTok)/tokensPerMillion*p.OutputPerMTok +
		float64(cacheReadTok)/tokensPerMillion*p.CacheReadPerMTok +
		float64(cacheWriteTok)/tokensPerMillion*p.CacheWritePerMTok
}

// EstimateCostWithSource is EstimateCostUSD plus the price source. An unknown
// price returns 0 with PriceSourceUnknown; callers must show "price unknown",
// never $0.
func EstimateCostWithSource(model string, viaGateway bool, inputTok, outputTok, cacheReadTok, cacheWriteTok int64) (float64, PriceSource) {
	p, src := ResolvePrice(model, viaGateway)
	if src == PriceSourceUnknown {
		return 0, src
	}
	return costAt(p, inputTok, outputTok, cacheReadTok, cacheWriteTok), src
}

// priceSourceRank orders sources from most to least certain.
var priceSourceRank = map[PriceSource]int{
	PriceSourceGateway: 1,
	PriceSourceTable:   1,
	PriceSourceTier:    2,
	PriceSourceUnknown: 3,
}

// leastCertainSource returns whichever of a and b is less certain ("" is
// treated as no source yet).
func leastCertainSource(a, b PriceSource) PriceSource {
	if a == "" {
		return b
	}
	if priceSourceRank[b] > priceSourceRank[a] {
		return b
	}
	return a
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
