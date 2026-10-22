package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/tokens"
)

// openRouterResponse is the subset of GET /api/v1/models the tool reads.
type openRouterResponse struct {
	Data []openRouterModel `json:"data"`
}

// openRouterModel is one catalogue row. Pricing values are USD per token,
// serialised as decimal strings.
type openRouterModel struct {
	ID      string `json:"id"`
	Pricing struct {
		Prompt          string `json:"prompt"`
		Completion      string `json:"completion"`
		InputCacheRead  string `json:"input_cache_read"`
		InputCacheWrite string `json:"input_cache_write"`
	} `json:"pricing"`
}

// perTokenToPerMTok scales OpenRouter's per-token rates to the table's
// per-million-token unit.
var perTokenToPerMTok = big.NewRat(1_000_000, 1)

// quote is a model's list price in USD per MTok as published by the source.
// CacheRead/CacheWrite are nil when the source does not publish them.
type quote struct {
	SourceID   string
	Input      *big.Rat
	Output     *big.Rat
	CacheRead  *big.Rat
	CacheWrite *big.Rat
}

// catalogue maps a normalised model id to its quote.
type catalogue struct {
	Quotes map[string]quote
	// Ambiguous lists normalised ids that several distinct catalogue ids map
	// to; those are skipped rather than guessed.
	Ambiguous map[string][]string
}

// parseOpenRouter decodes the models endpoint and keys every priced model by
// the same normalised id the price table uses for lookups.
//
// Variant ids ("...:free", "...:batch", "...:thinking") normalise onto their
// base model and would otherwise overwrite its price with the variant's, so
// they are dropped before normalisation; so are "~vendor/model-latest" router
// aliases and rows priced at zero (free tiers are not list prices).
func parseOpenRouter(raw []byte) (*catalogue, error) {
	var resp openRouterResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, errors.New("catalogue has no models")
	}
	byKey := make(map[string][]openRouterModel)
	for _, m := range resp.Data {
		if m.ID == "" || strings.Contains(m.ID, ":") || strings.HasPrefix(m.ID, "~") {
			continue
		}
		key := tokens.NormalizeModelID(m.ID)
		if tokens.IsRoutingAlias(key) {
			continue
		}
		byKey[key] = append(byKey[key], m)
	}
	c := &catalogue{Quotes: make(map[string]quote), Ambiguous: make(map[string][]string)}
	for key, models := range byKey {
		if len(models) > 1 {
			ids := make([]string, 0, len(models))
			for _, m := range models {
				ids = append(ids, m.ID)
			}
			sort.Strings(ids)
			c.Ambiguous[key] = ids
			continue
		}
		q, err := quoteFromModel(models[0])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", models[0].ID, err)
		}
		if q == nil || (q.Input.Sign() <= 0 && q.Output.Sign() <= 0) {
			continue
		}
		c.Quotes[key] = *q
	}
	return c, nil
}

// quoteFromModel converts one catalogue row's per-token strings to per-MTok.
// It returns nil for rows without a published prompt and completion rate.
func quoteFromModel(m openRouterModel) (*quote, error) {
	q := &quote{SourceID: m.ID}
	var err error
	if q.Input, err = perMTok(m.Pricing.Prompt); err != nil {
		return nil, fmt.Errorf("prompt: %w", err)
	}
	if q.Output, err = perMTok(m.Pricing.Completion); err != nil {
		return nil, fmt.Errorf("completion: %w", err)
	}
	if q.CacheRead, err = perMTok(m.Pricing.InputCacheRead); err != nil {
		return nil, fmt.Errorf("input_cache_read: %w", err)
	}
	if q.CacheWrite, err = perMTok(m.Pricing.InputCacheWrite); err != nil {
		return nil, fmt.Errorf("input_cache_write: %w", err)
	}
	if q.Input == nil || q.Output == nil {
		return nil, nil
	}
	return q, nil
}

// perMTok parses a per-token decimal string into a per-MTok rational. An
// empty or negative string yields nil (not published).
func perMTok(s string) (*big.Rat, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return nil, fmt.Errorf("cannot parse %q", s)
	}
	// OpenRouter writes "-1" for models with variable (per-request) pricing;
	// that is "not published", not a rate.
	if r.Sign() < 0 {
		return nil, nil
	}
	return roundPrice(r.Mul(r, perTokenToPerMTok)), nil
}
