package tokens

import (
	"strings"
)

// Unified cost ($) estimation for hive.
//
// This file provides an ESTIMATED dollar cost for token usage, computed as
// (per-model list price × hive's existing token counts). It is the fallback
// cost source for backends that do NOT report a native dollar figure
// (Claude/Copilot/Codex/vLLM). Backends that DO report real spend
// (OpenRouter, LiteLLM) surface that separately as "native" cost in the
// dashboard; the estimate here is only a proxy and is always labeled as such
// in the UI.
//
// IMPORTANT — these are LIST PRICES, not billed amounts. Subscription plans
// (Claude Max, Copilot premium requests), self-hosted inference (vLLM), and
// negotiated rates all differ from list price. Treat the output as an
// order-of-magnitude estimate, never as an invoice.

// priceTableDate documents when the price table below was last reconciled with
// published API list prices. Passed in as a const (never time.Now) so the
// "as of" date is deterministic and travels with the code. Update this string
// whenever any price in modelPrices changes.
//
// Sources (list prices as of this date):
//   - Anthropic Claude:    https://platform.claude.com/docs/en/about-claude/models/overview
//   - OpenAI (GPT-5.x):    https://openai.com/api/pricing/
//   - DeepSeek:            https://api-docs.deepseek.com/quick_start/pricing
//   - Meta Llama / Gemini: representative OpenRouter/provider list prices
const priceTableDate = "2026-07-24"

// PriceTableDate returns the date the price table was last reconciled with
// published list prices, so the UI can show "list prices as of <date>".
func PriceTableDate() string { return priceTableDate }

// tokensPerMillion is the denominator for per-MTok list prices. Prices in
// modelPrices are USD per 1,000,000 tokens; dividing token counts by this
// constant converts to millions before multiplying by the rate.
const tokensPerMillion = 1_000_000.0

// ModelPrice is the USD-per-1,000,000-tokens list price for one model, split by
// token kind. A zero value for a field means "priced the same as input" is NOT
// assumed — callers use the explicit fields. CacheRead/CacheWrite default to
// input-derived multiples only where the provider does not publish a separate
// figure (documented per entry below).
type ModelPrice struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheReadPerMTok  float64
	CacheWritePerMTok float64
}

// modelPrices maps a NORMALIZED model id (see normalizeModelID) to its list
// price. Keys are already normalized: lowercased, provider prefix stripped,
// separators unified to dashes. Add new models here — this is the single place
// prices live.
//
// Claude cache pricing convention: cache reads bill at ~0.1× input, cache
// writes (5-minute TTL) at ~1.25× input. Those multiples are encoded directly
// as published $/MTok values so the table stays self-documenting.
//
// GPT / DeepSeek / Llama / Gemini: cache-read prices use each provider's
// published cached-input rate where one exists; cache writes fall back to the
// input rate (these providers do not charge a separate cache-write premium the
// way Anthropic does).
var modelPrices = map[string]ModelPrice{
	// ---- Anthropic Claude (list prices per MTok) ----
	// Opus tier: $5 in / $25 out; cache read $0.50, cache write $6.25.
	"claude-opus-4-8": {InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},
	"claude-opus-4-7": {InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},
	"claude-opus-4-6": {InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},
	"claude-opus-4-5": {InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},
	// Fable 5: $10 in / $50 out; cache read $1.00, cache write $12.50.
	"claude-fable-5": {InputPerMTok: 10.00, OutputPerMTok: 50.00, CacheReadPerMTok: 1.00, CacheWritePerMTok: 12.50},
	// Sonnet tier: $3 in / $15 out; cache read $0.30, cache write $3.75.
	"claude-sonnet-5":   {InputPerMTok: 3.00, OutputPerMTok: 15.00, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75},
	"claude-sonnet-4-6": {InputPerMTok: 3.00, OutputPerMTok: 15.00, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75},
	"claude-sonnet-4-5": {InputPerMTok: 3.00, OutputPerMTok: 15.00, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75},
	// Haiku tier: $1 in / $5 out; cache read $0.10, cache write $1.25.
	"claude-haiku-4-5": {InputPerMTok: 1.00, OutputPerMTok: 5.00, CacheReadPerMTok: 0.10, CacheWritePerMTok: 1.25},

	// ---- OpenAI GPT-5.x (Copilot/Codex model ids; representative list prices) ----
	// GPT-5.x flagship: $1.25 in / $10 out; cached input $0.125.
	"gpt-5-6": {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},
	"gpt-5-5": {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},
	"gpt-5-4": {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},
	"gpt-5-3": {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},
	"gpt-5-2": {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},
	// GPT-5.x mini tier: $0.25 in / $2 out; cached input $0.025.
	"gpt-5-4-mini": {InputPerMTok: 0.25, OutputPerMTok: 2.00, CacheReadPerMTok: 0.025, CacheWritePerMTok: 0.25},
	// Codex variants inherit the flagship GPT-5.x rate.
	"gpt-5-6-sol":         {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},
	"gpt-5-6-terra":       {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},
	"gpt-5-6-luna":        {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},
	"gpt-5-3-codex-spark": {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},
	"gpt-5-3-codex":       {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},

	// ---- DeepSeek (common LiteLLM/OpenRouter routing target) ----
	// deepseek-chat / V3: $0.27 in / $1.10 out; cache hit $0.07.
	"deepseek-chat": {InputPerMTok: 0.27, OutputPerMTok: 1.10, CacheReadPerMTok: 0.07, CacheWritePerMTok: 0.27},
	"deepseek-v3":   {InputPerMTok: 0.27, OutputPerMTok: 1.10, CacheReadPerMTok: 0.07, CacheWritePerMTok: 0.27},
	// deepseek-reasoner / R1: $0.55 in / $2.19 out; cache hit $0.14.
	"deepseek-reasoner": {InputPerMTok: 0.55, OutputPerMTok: 2.19, CacheReadPerMTok: 0.14, CacheWritePerMTok: 0.55},
	"deepseek-r1":       {InputPerMTok: 0.55, OutputPerMTok: 2.19, CacheReadPerMTok: 0.14, CacheWritePerMTok: 0.55},

	// ---- Meta Llama 3.3 70B (representative hosted list price) ----
	"llama-3-3-70b":               {InputPerMTok: 0.59, OutputPerMTok: 0.79, CacheReadPerMTok: 0.59, CacheWritePerMTok: 0.59},
	"llama-3-3-70b-instruct":      {InputPerMTok: 0.59, OutputPerMTok: 0.79, CacheReadPerMTok: 0.59, CacheWritePerMTok: 0.59},
	"meta-llama-3-3-70b-instruct": {InputPerMTok: 0.59, OutputPerMTok: 0.79, CacheReadPerMTok: 0.59, CacheWritePerMTok: 0.59},

	// ---- Google Gemini Flash (representative list price) ----
	"gemini-flash-3-5": {InputPerMTok: 0.075, OutputPerMTok: 0.30, CacheReadPerMTok: 0.01875, CacheWritePerMTok: 0.075},
	"gemini-2-5-flash": {InputPerMTok: 0.075, OutputPerMTok: 0.30, CacheReadPerMTok: 0.01875, CacheWritePerMTok: 0.075},
	"gemini-2-0-flash": {InputPerMTok: 0.075, OutputPerMTok: 0.30, CacheReadPerMTok: 0.01875, CacheWritePerMTok: 0.075},
}

// normalizeModelID canonicalizes a model id so the price table can be keyed
// consistently across the many spellings the scanners store:
//   - "anthropic/claude-opus-4.8" (OpenRouter provider prefix + dots)
//   - "claude-opus-4-8"           (Claude CLI dashes)
//   - "claude-opus-4.6"           (Copilot dots)
//   - "gpt-5.4"                   (Copilot/Codex dots)
//
// It strips a leading provider prefix ("anthropic/", "openai/", …), lowercases,
// unifies '.' and '_' separators to '-', and collapses repeated dashes. The
// result is a table-lookup key, not a display string.
func normalizeModelID(model string) string {
	id := strings.TrimSpace(strings.ToLower(model))
	if id == "" {
		return ""
	}
	// Strip provider prefix ("anthropic/claude-opus-4.8" -> "claude-opus-4.8").
	// Take the last path segment so any provider prefix is removed.
	if idx := strings.LastIndex(id, "/"); idx >= 0 {
		id = id[idx+1:]
	}
	// Some routers use ':' to append a variant tag ("...:free"); drop it.
	if idx := strings.Index(id, ":"); idx >= 0 {
		id = id[:idx]
	}
	// Unify separators.
	id = strings.ReplaceAll(id, ".", "-")
	id = strings.ReplaceAll(id, "_", "-")
	id = strings.ReplaceAll(id, " ", "-")
	// Collapse repeated dashes and trim edges.
	for strings.Contains(id, "--") {
		id = strings.ReplaceAll(id, "--", "-")
	}
	return strings.Trim(id, "-")
}

// LookupPrice returns the list price for a model and whether it was found. The
// model id is normalized before lookup, so any of the stored spellings resolve.
func LookupPrice(model string) (ModelPrice, bool) {
	p, ok := modelPrices[normalizeModelID(model)]
	return p, ok
}

// EstimateCostUSD computes the estimated dollar cost for the given token counts
// at the model's list price. priced is false when the model is not in the price
// table (unknown model) — in that case usd is 0 and the UI should render "—"
// rather than "$0.00" so an unknown model is never mistaken for a free one.
func EstimateCostUSD(model string, inputTok, outputTok, cacheReadTok, cacheWriteTok int64) (usd float64, priced bool) {
	p, ok := LookupPrice(model)
	if !ok {
		return 0, false
	}
	usd = float64(inputTok)/tokensPerMillion*p.InputPerMTok +
		float64(outputTok)/tokensPerMillion*p.OutputPerMTok +
		float64(cacheReadTok)/tokensPerMillion*p.CacheReadPerMTok +
		float64(cacheWriteTok)/tokensPerMillion*p.CacheWritePerMTok
	return usd, true
}

// ModelCost is the estimated dollar cost for one model or agent, plus whether
// the model was priced. Unpriced entries carry Priced=false and USD=0 so the UI
// can distinguish "unknown model" from "genuinely $0".
type ModelCost struct {
	USD    float64 `json:"usd"`
	Priced bool    `json:"priced"`
	// Token splits are echoed so the UI can show the basis of the estimate.
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cache_read"`
	CacheCreate int64 `json:"cache_create"`
}

// EstimatedCost is the full estimated-cost breakdown derived from an
// AggregateSummary: a grand total plus per-model and per-agent detail. Only
// priced models contribute to TotalUSD; UnpricedModels lists model ids that
// were seen with token usage but have no price-table entry, so operators know
// the total is a lower bound.
type EstimatedCost struct {
	TotalUSD       float64              `json:"total_usd"`
	ByModel        map[string]ModelCost `json:"by_model"`
	ByAgent        map[string]ModelCost `json:"by_agent"`
	UnpricedModels []string             `json:"unpriced_models"`
	PriceTableDate string               `json:"price_table_date"`
}

// EstimateFromSummary computes the total, per-model, and per-agent estimated
// dollar cost from an AggregateSummary using its ByModelDetail / ByAgentDetail
// token buckets. It is a pure function of the summary — no clock, no network.
//
// Per-agent cost is estimated by pricing each agent's aggregated token bucket
// at the price of the model that agent used most (the agent buckets do not
// carry a model id, so this is a best-effort attribution). When an agent's
// model can't be determined, its bucket is priced only if a single model
// dominates the summary; otherwise it is marked unpriced.
func EstimateFromSummary(agg *AggregateSummary) EstimatedCost {
	out := EstimatedCost{
		ByModel:        make(map[string]ModelCost),
		ByAgent:        make(map[string]ModelCost),
		UnpricedModels: []string{},
		PriceTableDate: priceTableDate,
	}
	if agg == nil {
		return out
	}

	unpricedSeen := make(map[string]bool)

	// Per-model estimated cost — the authoritative source for the grand total.
	for model, b := range agg.ByModelDetail {
		if b == nil {
			continue
		}
		usd, priced := EstimateCostUSD(model, b.Input, b.Output, b.CacheRead, b.CacheCreate)
		out.ByModel[model] = ModelCost{
			USD:         usd,
			Priced:      priced,
			Input:       b.Input,
			Output:      b.Output,
			CacheRead:   b.CacheRead,
			CacheCreate: b.CacheCreate,
		}
		if priced {
			out.TotalUSD += usd
		} else if model != "" && !unpricedSeen[model] {
			unpricedSeen[model] = true
			out.UnpricedModels = append(out.UnpricedModels, model)
		}
	}

	// Determine each agent's dominant model from the session list so per-agent
	// cost can be priced. Agent buckets don't carry a model id, so we attribute
	// via the agent's sessions.
	agentModel := dominantModelByAgent(agg)

	for agent, b := range agg.ByAgentDetail {
		if b == nil {
			continue
		}
		model := agentModel[agent]
		usd, priced := EstimateCostUSD(model, b.Input, b.Output, b.CacheRead, b.CacheCreate)
		out.ByAgent[agent] = ModelCost{
			USD:         usd,
			Priced:      priced,
			Input:       b.Input,
			Output:      b.Output,
			CacheRead:   b.CacheRead,
			CacheCreate: b.CacheCreate,
		}
	}

	return out
}

// dominantModelByAgent returns, per agent, the model id that accounts for the
// most total tokens across that agent's sessions. Agents with no attributable
// session model map to "".
func dominantModelByAgent(agg *AggregateSummary) map[string]string {
	// agent -> model -> total tokens
	tally := make(map[string]map[string]int64)
	for _, s := range agg.Sessions {
		if s.Agent == "" || s.Model == "" {
			continue
		}
		if tally[s.Agent] == nil {
			tally[s.Agent] = make(map[string]int64)
		}
		tally[s.Agent][s.Model] += s.TotalTokens
	}
	out := make(map[string]string, len(tally))
	for agent, models := range tally {
		var best string
		var bestTok int64
		for model, tok := range models {
			if tok > bestTok {
				bestTok = tok
				best = model
			}
		}
		out[agent] = best
	}
	return out
}
