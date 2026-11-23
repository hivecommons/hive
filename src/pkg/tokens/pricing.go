package tokens

import (
	"regexp"
	"sort"
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
// This constant and the numeric literals in modelPrices are rewritten by
// `go run ./internal/tools/pricetable`, which the weekly
// price-table-refresh workflow runs against OpenRouter's public model
// catalogue and turns into a reviewable PR. Keep every row as a plain
// key: {Field: literal, ...} entry so the tool can parse and edit it in place;
// rows OpenRouter does not list keep their hand-set value and are flagged in
// the refresh report. CI fails when this date is more than 60 days old.
//
// Sources (list prices as of this date):
//   - OpenRouter catalogue: https://openrouter.ai/api/v1/models (automated)
//   - Anthropic Claude:    https://platform.claude.com/docs/en/about-claude/models/overview
//   - OpenAI (GPT-5.x):    https://openai.com/api/pricing/
//   - DeepSeek:            https://api-docs.deepseek.com/quick_start/pricing
//   - Kimi:                https://platform.kimi.ai/docs/pricing/chat
//   - Meta Llama / Gemini: representative OpenRouter/provider list prices
const priceTableDate = "2026-11-17"

// PriceTableDate returns the date the price table was last reconciled with
// published list prices, so the UI can show "list prices as of <date>".
func PriceTableDate() string { return priceTableDate }

// tokensPerMillion is the denominator for per-MTok list prices. Prices in
// modelPrices are USD per 1,000,000 tokens; dividing token counts by this
// constant converts to millions before multiplying by the rate.
const tokensPerMillion = 1_000_000.0

var modelVersionSuffixRE = regexp.MustCompile(`-\d{8}(-v\d+)?$`)

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
//
// Kimi K3 uses the published default 5-minute cache-write rate ($3/MTok);
// its optional 1-hour tier ($6/MTok) is not tracked here. K2.7 Code publishes
// cached-input pricing but no separate write rate, so writes use input pricing.
//
// The literals are authoritative; the inline comments record the tier a row
// was added at and are not rewritten by the automated refresh.
var modelPrices = map[string]ModelPrice{
	// ---- Anthropic Claude (list prices per MTok) ----
	// Opus 5.5: $4 in / $20 out; cache read $0.20 (0.05×), cache write $5
	// (1.25×) — per the published Anthropic list price (pi.dev bundled
	// catalog for the anthropic provider, verified 2026-09-30, #9804).
	"claude-opus-5-5": {InputPerMTok: 4.00, OutputPerMTok: 20.00, CacheReadPerMTok: 0.20, CacheWritePerMTok: 5.00},
	// Opus tier: $5 in / $25 out; cache read $0.50, cache write $6.25.
	"claude-opus-5":   {InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},
	"claude-opus-4-8": {InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},
	"claude-opus-4-7": {InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},
	"claude-opus-4-6": {InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},
	"claude-opus-4-5": {InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},
	"claude-opus-4-1": {InputPerMTok: 15.00, OutputPerMTok: 75.00, CacheReadPerMTok: 1.50, CacheWritePerMTok: 18.75},
	// Fable 5: $10 in / $50 out; cache read $1.00, cache write $12.50.
	"claude-fable-5": {InputPerMTok: 10.00, OutputPerMTok: 50.00, CacheReadPerMTok: 1.00, CacheWritePerMTok: 12.50},
	// Fable 5.1 is the same list tier as Fable 5 (#13488); without this entry it
	// fell to tierBig at roughly half the real rate and was flagged estimated.
	"claude-fable-5-1": {InputPerMTok: 10.00, OutputPerMTok: 50.00, CacheReadPerMTok: 0.25, CacheWritePerMTok: 12.50},
	// Fable 5.2: $8 in / $40 out; cache read $0.20 (0.025×), cache write $10
	// (5-minute TTL) — per Anthropic's pricing page, 2026-11-17 (#18753).
	"claude-fable-5-2": {InputPerMTok: 8.00, OutputPerMTok: 40.00, CacheReadPerMTok: 0.20, CacheWritePerMTok: 10.00},
	// Sonnet 5.5: $2 in / $10 out; cache read $0.20, cache write $2.50 (1.25×)
	// — per the Claude Code 2.1.284 changelog ("$2/$10 per Mtok with
	// $0.20/Mtok cache reads", #9804).
	"claude-sonnet-5-5": {InputPerMTok: 2.00, OutputPerMTok: 10.00, CacheReadPerMTok: 0.10, CacheWritePerMTok: 2.50},
	// Sonnet tier: $3 in / $15 out; cache read $0.30, cache write $3.75.
	"claude-sonnet-5":   {InputPerMTok: 2.00, OutputPerMTok: 10.00, CacheReadPerMTok: 0.20, CacheWritePerMTok: 2.50},
	"claude-sonnet-4-6": {InputPerMTok: 3.00, OutputPerMTok: 15.00, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75},
	"claude-sonnet-4-5": {InputPerMTok: 3.00, OutputPerMTok: 15.00, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.75},
	// Haiku tier: $1 in / $5 out; cache read $0.10, cache write $1.25.
	"claude-haiku-4-5": {InputPerMTok: 1.00, OutputPerMTok: 5.00, CacheReadPerMTok: 0.10, CacheWritePerMTok: 1.25},
	// Haiku 5.5: base rate for prompts up to 100,000 tokens — $0.10 in /
	// $0.50 out; cache read $0.01, cache write $0.125 (5-minute TTL) — per
	// Anthropic's pricing page, 2026-11-17 (#18753). Prompts over 100,000
	// tokens bill at $0.50 / $2.50 (cache read $0.05, write $0.625); that
	// tier is not tracked here, so long-prompt Haiku 5.5 spend is undercounted.
	"claude-haiku-5-5": {InputPerMTok: 0.10, OutputPerMTok: 0.50, CacheReadPerMTok: 0.01, CacheWritePerMTok: 0.125},

	// ---- OpenAI GPT-5.x (Copilot/Codex model ids; representative list prices) ----
	// GPT-5.x flagship: $1.25 in / $10 out; cached input $0.125.
	"gpt-5-6": {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},
	"gpt-5-5": {InputPerMTok: 5.00, OutputPerMTok: 30.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 5.00},
	"gpt-5-4": {InputPerMTok: 2.50, OutputPerMTok: 15.00, CacheReadPerMTok: 0.25, CacheWritePerMTok: 2.50},
	"gpt-5-3": {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},
	"gpt-5-2": {InputPerMTok: 1.75, OutputPerMTok: 14.00, CacheReadPerMTok: 0.175, CacheWritePerMTok: 1.75},
	// GPT-5.x mini tier: $0.25 in / $2 out; cached input $0.025.
	"gpt-5-4-mini": {InputPerMTok: 0.75, OutputPerMTok: 4.50, CacheReadPerMTok: 0.075, CacheWritePerMTok: 0.75},
	"gpt-5-mini":   {InputPerMTok: 0.25, OutputPerMTok: 2.00, CacheReadPerMTok: 0.025, CacheWritePerMTok: 0.25},
	// Codex variants inherit the flagship GPT-5.x rate.
	"gpt-5-6-sol":         {InputPerMTok: 4.00, OutputPerMTok: 20.00, CacheReadPerMTok: 0.40, CacheWritePerMTok: 5.00},
	"gpt-5-6-terra":       {InputPerMTok: 2.00, OutputPerMTok: 12.00, CacheReadPerMTok: 0.20, CacheWritePerMTok: 2.50},
	"gpt-5-6-luna":        {InputPerMTok: 0.20, OutputPerMTok: 1.20, CacheReadPerMTok: 0.02, CacheWritePerMTok: 0.25},
	"gpt-5-3-codex-spark": {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},
	"gpt-5-3-codex":       {InputPerMTok: 1.75, OutputPerMTok: 14.00, CacheReadPerMTok: 0.175, CacheWritePerMTok: 1.75},

	// ---- OpenAI GPT-6 (codex 0.159.0 catalog; published list prices from the
	// pi.dev bundled catalog for the openai provider, verified 2026-09-30,
	// #9804). Unlike the 5.x rows above, OpenAI publishes explicit cache-read
	// AND cache-write rates for this family.
	"gpt-6-astra": {InputPerMTok: 10.00, OutputPerMTok: 50.00, CacheReadPerMTok: 1.00, CacheWritePerMTok: 12.50},
	"gpt-6-sol":   {InputPerMTok: 2.00, OutputPerMTok: 10.00, CacheReadPerMTok: 0.20, CacheWritePerMTok: 2.50},
	"gpt-6-luna":  {InputPerMTok: 0.10, OutputPerMTok: 0.50, CacheReadPerMTok: 0.01, CacheWritePerMTok: 0.125},

	// ---- Other OpenAI/Copilot ids offered by static CLI dropdowns (#9804) ----
	"gpt-4-1":                  {InputPerMTok: 2.00, OutputPerMTok: 8.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 2.00},
	"gpt-4o":                   {InputPerMTok: 2.50, OutputPerMTok: 10.00, CacheReadPerMTok: 1.25, CacheWritePerMTok: 2.50},
	"o3":                       {InputPerMTok: 2.00, OutputPerMTok: 8.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 2.00},
	"o4-mini":                  {InputPerMTok: 1.10, OutputPerMTok: 4.40, CacheReadPerMTok: 0.275, CacheWritePerMTok: 1.10},
	"gpt-daybreak-blue-latest": {InputPerMTok: 10.00, OutputPerMTok: 50.00, CacheReadPerMTok: 1.00, CacheWritePerMTok: 12.50},
	"gpt-daybreak-red-latest":  {InputPerMTok: 10.00, OutputPerMTok: 50.00, CacheReadPerMTok: 1.00, CacheWritePerMTok: 12.50},

	// ---- DeepSeek (common LiteLLM/OpenRouter routing target) ----
	// deepseek-chat: $0.32 in / $0.89 out (OpenRouter 2026-11-09); V3: $0.27 in / $1.10 out; cache hit $0.07.
	"deepseek-chat": {InputPerMTok: 0.32, OutputPerMTok: 0.89, CacheReadPerMTok: 0.07, CacheWritePerMTok: 0.32},
	"deepseek-v3":   {InputPerMTok: 0.27, OutputPerMTok: 1.10, CacheReadPerMTok: 0.07, CacheWritePerMTok: 0.27},
	// deepseek-reasoner / R1: $0.55 in / $2.19 out; cache hit $0.14.
	"deepseek-reasoner": {InputPerMTok: 0.55, OutputPerMTok: 2.19, CacheReadPerMTok: 0.14, CacheWritePerMTok: 0.55},
	"deepseek-r1":       {InputPerMTok: 0.55, OutputPerMTok: 2.19, CacheReadPerMTok: 0.14, CacheWritePerMTok: 0.55},

	// ---- Meta Llama 3.3 70B (representative hosted list price) ----
	"llama-3-3-70b":               {InputPerMTok: 0.59, OutputPerMTok: 0.79, CacheReadPerMTok: 0.59, CacheWritePerMTok: 0.59},
	"llama-3-3-70b-instruct":      {InputPerMTok: 0.59, OutputPerMTok: 0.79, CacheReadPerMTok: 0.295, CacheWritePerMTok: 0.59},
	"meta-llama-3-3-70b-instruct": {InputPerMTok: 0.59, OutputPerMTok: 0.79, CacheReadPerMTok: 0.59, CacheWritePerMTok: 0.59},

	// ---- Google Gemini Flash / Pro (representative list prices) ----
	"gemini-flash-3-5": {InputPerMTok: 0.075, OutputPerMTok: 0.30, CacheReadPerMTok: 0.01875, CacheWritePerMTok: 0.075},
	"gemini-2-5-flash": {InputPerMTok: 0.30, OutputPerMTok: 2.50, CacheReadPerMTok: 0.03, CacheWritePerMTok: 0.30},
	"gemini-2-0-flash": {InputPerMTok: 0.075, OutputPerMTok: 0.30, CacheReadPerMTok: 0.01875, CacheWritePerMTok: 0.075},
	"gemini-2-5-pro":   {InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25},
	// Gemini 3 previews (offered by geminiStaticModels/geminiPinnedCLIModels,
	// #9804). Pro: $2 in / $12 out (<=200K-token prompts; Google's published
	// list price, 2026-09), cached input at the standard 0.1× rate. Flash:
	// $0.50 in / $3 out with a published $0.05 cached-input rate (pi.dev
	// bundled catalog for the google provider, verified 2026-09-30). Cache
	// writes fall back to the input rate per the header convention.
	"gemini-3-pro-preview":    {InputPerMTok: 2.00, OutputPerMTok: 12.00, CacheReadPerMTok: 0.20, CacheWritePerMTok: 2.00},
	"gemini-3-flash-preview":  {InputPerMTok: 0.50, OutputPerMTok: 3.00, CacheReadPerMTok: 0.05, CacheWritePerMTok: 0.50},
	"gemini-3-7-flash":        {InputPerMTok: 0.75, OutputPerMTok: 3.75, CacheReadPerMTok: 0.075, CacheWritePerMTok: 0.75},
	"gemini-3-7-flash-high":   {InputPerMTok: 0.75, OutputPerMTok: 3.75, CacheReadPerMTok: 0.075, CacheWritePerMTok: 0.75},
	"gemini-3-7-flash-medium": {InputPerMTok: 0.75, OutputPerMTok: 3.75, CacheReadPerMTok: 0.075, CacheWritePerMTok: 0.75},
	"gemini-3-7-flash-low":    {InputPerMTok: 0.75, OutputPerMTok: 3.75, CacheReadPerMTok: 0.075, CacheWritePerMTok: 0.75},
	"gemini-3-6-flash":        {InputPerMTok: 0.75, OutputPerMTok: 3.75, CacheReadPerMTok: 0.075, CacheWritePerMTok: 0.75},
	"gemini-3-6-flash-high":   {InputPerMTok: 0.75, OutputPerMTok: 3.75, CacheReadPerMTok: 0.075, CacheWritePerMTok: 0.75},
	"gemini-3-6-flash-medium": {InputPerMTok: 0.75, OutputPerMTok: 3.75, CacheReadPerMTok: 0.075, CacheWritePerMTok: 0.75},
	"gemini-3-6-flash-low":    {InputPerMTok: 0.75, OutputPerMTok: 3.75, CacheReadPerMTok: 0.075, CacheWritePerMTok: 0.75},
	"gemini-3-5-flash":        {InputPerMTok: 1.50, OutputPerMTok: 9.00, CacheReadPerMTok: 0.15, CacheWritePerMTok: 1.50},
	"gemini-3-1-pro-preview":  {InputPerMTok: 2.00, OutputPerMTok: 12.00, CacheReadPerMTok: 0.20, CacheWritePerMTok: 2.00},
	"gemini-3-1-pro-high":     {InputPerMTok: 2.00, OutputPerMTok: 12.00, CacheReadPerMTok: 0.20, CacheWritePerMTok: 2.00},
	"gemini-3-1-pro-low":      {InputPerMTok: 2.00, OutputPerMTok: 12.00, CacheReadPerMTok: 0.20, CacheWritePerMTok: 2.00},
	// Gemini 3.8 Flash (offered by geminiStaticModels since #19520): OpenRouter
	// google/gemini-3.8-flash list price, 2026-11-23.
	"gemini-3-8-flash": {InputPerMTok: 0.825, OutputPerMTok: 4.125, CacheReadPerMTok: 0.0825, CacheWritePerMTok: 0.825},

	// ---- Other Copilot/AGY selectors with no separately published price ----
	// These reuse the closest sibling tier so every offered dropdown value has
	// a deterministic price entry instead of relying on the coarse fallback.
	"grok-4-5":                 {InputPerMTok: 2.00, OutputPerMTok: 6.00, CacheReadPerMTok: 0.30, CacheWritePerMTok: 2.00},
	"kimi-k3":                  {InputPerMTok: 3.00, OutputPerMTok: 15.00, CacheReadPerMTok: 0.30, CacheWritePerMTok: 3.00},
	"kimi-k2-7-code":           {InputPerMTok: 0.60, OutputPerMTok: 3.50, CacheReadPerMTok: 0.16, CacheWritePerMTok: 0.60},
	"mai-code-1-flash-picker":  {InputPerMTok: 0.25, OutputPerMTok: 2.00, CacheReadPerMTok: 0.025, CacheWritePerMTok: 0.25},
	"claude-opus-4-6-thinking": {InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25},
	"gpt-oss-120b-medium":      {InputPerMTok: 0.15, OutputPerMTok: 0.60, CacheReadPerMTok: 0.075, CacheWritePerMTok: 0.15},
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
// isRoutingAlias reports whether a NORMALIZED model id is a routing/selection
// mode ("auto", "default") rather than a real model. Tokens are attributed to
// the resolved model, so these carry ~no usage and should not appear as cost
// rows. Pass the output of normalizeModelID.
func isRoutingAlias(normalizedID string) bool {
	switch normalizedID {
	case "", "auto", "default":
		return true
	}
	return false
}

// NormalizeModelID is the exported form of normalizeModelID for callers that
// must match model ids the same way the price table does.
func NormalizeModelID(model string) string { return normalizeModelID(model) }

// IsRoutingAlias is the exported form of isRoutingAlias. Pass the output of
// NormalizeModelID.
func IsRoutingAlias(normalizedID string) bool { return isRoutingAlias(normalizedID) }

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
	id = strings.Trim(id, "-")

	// LiteLLM/Bedrock/Vertex routed models often arrive with provider and region
	// qualifiers embedded into the final segment, for example:
	//   bedrock/us.anthropic.claude-sonnet-4-6-20250514-v1:0
	// If we keep "us-anthropic-" (or the dated Bedrock suffix), the exact lookup
	// misses and falls through to a coarse tier price. Slice to the first real
	// model family marker before consulting the table.
	for _, marker := range []string{"claude-", "gpt-", "deepseek-", "llama-", "llama3-", "gemini-"} {
		if idx := strings.Index(id, marker); idx > 0 {
			id = id[idx:]
			break
		}
	}
	if strings.HasPrefix(id, "llama3-") {
		id = "llama-3-" + strings.TrimPrefix(id, "llama3-")
	}
	if strings.HasPrefix(id, "claude-") {
		id = modelVersionSuffixRE.ReplaceAllString(id, "")
	}
	return strings.Trim(id, "-")
}

// LookupPrice returns the list price for a model and whether it was found. The
// model id is normalized before lookup, so any of the stored spellings resolve.
func LookupPrice(model string) (ModelPrice, bool) {
	p, ok := modelPrices[normalizeModelID(model)]
	return p, ok
}

// Tier fallback prices (USD per MTok). Deliberately coarse — used only when a
// model is not in the exact table, so the cost estimate reflects activity
// instead of showing $0. The heuristic follows the obvious ordering: smaller /
// older / lighter models cost less than bigger / newer / flagship ones.
var (
	tierCheap = ModelPrice{InputPerMTok: 0.25, OutputPerMTok: 2.00, CacheReadPerMTok: 0.025, CacheWritePerMTok: 0.25}  // mini / flash / haiku / lite
	tierMid   = ModelPrice{InputPerMTok: 1.25, OutputPerMTok: 10.00, CacheReadPerMTok: 0.125, CacheWritePerMTok: 1.25} // gpt-5.x / sonnet / codex class
	tierBig   = ModelPrice{InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50, CacheWritePerMTok: 6.25}  // opus / pro / fable / flagship
)

// FallbackPrice returns a tier-based estimate for a model absent from the exact
// table, plus false to signal it's a fallback (callers may still flag the row
// as approximate). It never returns a zero price, so no active model shows $0.
func FallbackPrice(model string) (ModelPrice, bool) {
	id := normalizeModelID(model)
	switch {
	case id == "":
		return tierMid, false
	case strings.Contains(id, "mini") || strings.Contains(id, "flash") ||
		strings.Contains(id, "haiku") || strings.Contains(id, "lite") ||
		strings.Contains(id, "small") || strings.Contains(id, "nano"):
		return tierCheap, false
	case strings.Contains(id, "opus") || strings.Contains(id, "-pro") ||
		strings.Contains(id, "fable") || strings.Contains(id, "ultra") ||
		strings.Contains(id, "-max"):
		return tierBig, false
	default:
		// Unknown mid-size (most GPT-5.x/codex/sonnet-class ids land here).
		return tierMid, false
	}
}

// EstimateCostUSD computes the estimated dollar cost for the given token counts.
// It ALWAYS returns a dollar estimate: exact list price when the model is in the
// table, otherwise a coarse tier fallback (see FallbackPrice) so an active model
// never contributes $0 to the total. The `exact` return reports which was used —
// true = exact list price, false = tier estimate — so the UI can mark a row as
// approximate, but the number is real either way. (This whole card is an
// estimate; we do not depend on unavailable "actual" gateway spend.)
//
// A price a configured gateway publishes for the exact id (see
// SetGatewayPricer) wins over the table and counts as exact. A model a
// gateway serves without a price returns (0, false): see
// EstimateCostWithSource for callers that must say "price unknown".
func EstimateCostUSD(model string, inputTok, outputTok, cacheReadTok, cacheWriteTok int64) (usd float64, exact bool) {
	usd, src := EstimateCostWithSource(model, false, inputTok, outputTok, cacheReadTok, cacheWriteTok)
	return usd, src.Exact()
}

// ModelCost is the estimated dollar cost for one model or agent, plus whether
// it came from an exact table entry. Priced=false means the USD is a coarse
// tier estimate and must be labelled approximate.
type ModelCost struct {
	USD    float64 `json:"usd"`
	Priced bool    `json:"priced"`
	// PriceSource is where the price came from (gateway | table | tier |
	// unknown). For an agent spanning several models it is the least certain
	// source among them. unknown means USD excludes those tokens.
	PriceSource PriceSource `json:"price_source,omitempty"`
	// UnknownPriceModels lists, for an agent row, the models whose price is
	// unknown (their tokens are not in USD).
	UnknownPriceModels []string `json:"unknown_price_models,omitempty"`
	// Token splits are echoed so the UI can show the basis of the estimate.
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cache_read"`
	CacheCreate int64 `json:"cache_create"`
}

// EstimatedCost is the full estimated-cost breakdown derived from an
// AggregateSummary: a grand total plus per-model and per-agent detail. Every
// active model contributes to TotalUSD: exact table price when known, otherwise
// a coarse tier estimate. UnpricedModels lists model ids that had no exact
// price-table entry so operators know which rows are approximate.
type EstimatedCost struct {
	TotalUSD       float64              `json:"total_usd"`
	ByModel        map[string]ModelCost `json:"by_model"`
	ByAgent        map[string]ModelCost `json:"by_agent"`
	UnpricedModels []string             `json:"unpriced_models"`
	// UnknownPriceModels are gateway models with no published or configured
	// price. Their tokens are NOT in TotalUSD, so the total is a floor; the
	// UI says "price unknown" for them instead of showing $0.
	UnknownPriceModels []string `json:"unknown_price_models"`
	PriceTableDate     string   `json:"price_table_date"`
}

// EstimateFromSummary computes the total, per-model, and per-agent estimated
// dollar cost from an AggregateSummary using its ByModelDetail / ByAgentDetail
// token buckets. It is a pure function of the summary — no clock, no network.
//
// Per-agent cost is the sum of that agent's own sessions, each priced at its
// own session's model (via agg.Sessions), so sum(by_agent) stays additive
// with total_usd even when an agent's sessions span more than one model
// (hivecommons/hive#10025).
func EstimateFromSummary(agg *AggregateSummary) EstimatedCost {
	out := EstimatedCost{
		ByModel:            make(map[string]ModelCost),
		ByAgent:            make(map[string]ModelCost),
		UnpricedModels:     []string{},
		UnknownPriceModels: []string{},
		PriceTableDate:     priceTableDate,
	}
	if agg == nil {
		return out
	}

	unpricedSeen := make(map[string]bool)
	unknownSeen := make(map[string]bool)
	// Models seen on the inference (gateway) path are never tier-guessed:
	// they are priced from their gateway, the table, or reported unknown.
	viaGateway := make(map[string]bool)
	for _, s := range agg.Sessions {
		if s.Backend == BackendInference {
			viaGateway[normalizeModelID(s.Model)] = true
		}
	}

	// Merge raw per-model buckets by NORMALIZED model id first. The scanners key
	// tokens by the exact model string, so the same model shows up under both
	// dotted and dashed spellings (e.g. "claude-opus-4.7" from Copilot and
	// "claude-opus-4-7" from the Claude CLI). Those distinctions matter for
	// launching the CLI but NOT for cost — leaving them split double-lists one
	// model as two rows. Collapse them here, keeping the most-used raw spelling
	// as the display name.
	type mergedBucket struct {
		display                               string
		displayTok                            int64
		input, output, cacheRead, cacheCreate int64
	}
	merged := make(map[string]*mergedBucket)
	for model, b := range agg.ByModelDetail {
		if b == nil {
			continue
		}
		key := normalizeModelID(model)
		// Skip routing aliases — "auto"/"default" are selection MODES, not
		// models. The tokens are attributed to the resolved model, so these
		// buckets are always ~empty and just add a noise row to the cost table.
		if isRoutingAlias(key) {
			continue
		}
		m := merged[key]
		if m == nil {
			m = &mergedBucket{display: model}
			merged[key] = m
		}
		// Prefer the spelling that carries the most tokens as the display name.
		if tot := b.Input + b.Output; tot >= m.displayTok {
			m.display = model
			m.displayTok = tot
		}
		m.input += b.Input
		m.output += b.Output
		m.cacheRead += b.CacheRead
		m.cacheCreate += b.CacheCreate
	}

	// Per-model estimated cost — the authoritative source for the grand total.
	for key, m := range merged {
		usd, src := EstimateCostWithSource(m.display, viaGateway[key], m.input, m.output, m.cacheRead, m.cacheCreate)
		out.ByModel[m.display] = ModelCost{
			USD:         usd,
			Priced:      src.Exact(),
			PriceSource: src,
			Input:       m.input,
			Output:      m.output,
			CacheRead:   m.cacheRead,
			CacheCreate: m.cacheCreate,
		}
		// Every model with a price contributes to the total — exact where the
		// gateway or table publishes one, tier estimate otherwise.
		// UnpricedModels lists the tier-estimated ones so the UI can mark them
		// ~approximate. A gateway model with NO price is excluded and listed
		// in UnknownPriceModels: counting it as $0 or a tier guess would
		// silently misstate gateway spend.
		out.TotalUSD += usd
		switch {
		case src == PriceSourceUnknown && m.display != "" && !unknownSeen[m.display]:
			unknownSeen[m.display] = true
			out.UnknownPriceModels = append(out.UnknownPriceModels, m.display)
		case src == PriceSourceTier && m.display != "" && !unpricedSeen[m.display]:
			unpricedSeen[m.display] = true
			out.UnpricedModels = append(out.UnpricedModels, m.display)
		}
	}
	sort.Strings(out.UnknownPriceModels)

	// Price each agent by summing each of its sessions at that session's own
	// model, instead of pricing the whole agent bucket at one "dominant"
	// model (hivecommons/hive#10025). Once an agent's sessions span more than
	// one model, a single dominant-model rate disagrees with TotalUSD, which
	// prices every model at its own rate — this keeps sum(by_agent) additive
	// with total_usd.
	agentUSD := make(map[string]float64, len(agg.ByAgentDetail))
	agentExact := make(map[string]bool, len(agg.ByAgentDetail))
	agentSource := make(map[string]PriceSource, len(agg.ByAgentDetail))
	agentUnknown := make(map[string][]string)
	for agent := range agg.ByAgentDetail {
		agentExact[agent] = true
	}
	for _, s := range agg.Sessions {
		if _, ok := agg.ByAgentDetail[s.Agent]; !ok {
			continue
		}
		via := s.Backend == BackendInference || viaGateway[normalizeModelID(s.Model)]
		usd, src := EstimateCostWithSource(s.Model, via, s.InputTokens, s.OutputTokens, s.CacheRead, s.CacheCreate)
		agentUSD[s.Agent] += usd
		if !src.Exact() {
			agentExact[s.Agent] = false
		}
		agentSource[s.Agent] = leastCertainSource(agentSource[s.Agent], src)
		if src == PriceSourceUnknown && s.Model != "" && !containsString(agentUnknown[s.Agent], s.Model) {
			agentUnknown[s.Agent] = append(agentUnknown[s.Agent], s.Model)
		}
	}

	for agent, b := range agg.ByAgentDetail {
		if b == nil {
			continue
		}
		unknown := agentUnknown[agent]
		sort.Strings(unknown)
		out.ByAgent[agent] = ModelCost{
			USD:                agentUSD[agent],
			Priced:             agentExact[agent],
			PriceSource:        agentSource[agent],
			UnknownPriceModels: unknown,
			Input:              b.Input,
			Output:             b.Output,
			CacheRead:          b.CacheRead,
			CacheCreate:        b.CacheCreate,
		}
	}

	return out
}
