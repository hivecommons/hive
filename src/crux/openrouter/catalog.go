package openrouter

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// modelsListTimeout bounds the public /v1/models fetch used by the funding
// picker and the gateway form. The catalog is ~1 MB (hundreds of models with
// descriptions), so it gets more headroom than the small key/credit calls; it
// is still a best-effort enrichment, never on an inference path.
const modelsListTimeout = 10 * time.Second

// maxCatalogBody caps how much of the public catalog we read. The live
// response was ~850 KB with ~470 models in Nov 2026; the cap leaves room for
// the catalog to keep growing without letting a hostile or broken upstream
// stream an unbounded body into memory.
const maxCatalogBody = 8 << 20

// catalogCacheTTL is how long a successfully fetched catalog is reused.
// OpenRouter adds and retires models on a scale of days, so a few minutes of
// staleness is harmless and keeps the dashboard from re-downloading the
// catalog on every form open.
const catalogCacheTTL = 15 * time.Minute

// tokensPerMillion converts OpenRouter's per-token USD prices into the
// per-1M-token figures people compare.
const tokensPerMillion = 1_000_000

// Pricing is a model's USD price PER TOKEN, as OpenRouter publishes it in the
// /models `pricing` block. A field is nil when the catalog omits it or marks
// it variable (OpenRouter's routers publish "-1"), so a caller can tell
// "unknown" from a genuine zero (free models publish "0").
type Pricing struct {
	Prompt         *float64 `json:"prompt,omitempty"`
	Completion     *float64 `json:"completion,omitempty"`
	InputCacheRead *float64 `json:"input_cache_read,omitempty"`
}

// Known reports whether both the input and output price are published. A
// model with only one side known cannot be costed honestly.
func (p Pricing) Known() bool { return p.Prompt != nil && p.Completion != nil }

// PerMillion returns a per-token price as USD per 1M tokens, or nil.
func PerMillion(perToken *float64) *float64 {
	if perToken == nil {
		return nil
	}
	v := *perToken * tokensPerMillion
	return &v
}

// Model is one entry of the public OpenRouter catalog: the id agents send,
// its display name, its context window, and its published prices.
type Model struct {
	ID            string  `json:"id"`
	Name          string  `json:"name,omitempty"`
	ContextLength int     `json:"context_length,omitempty"`
	Pricing       Pricing `json:"pricing"`
}

// rawModel mirrors the catalog wire shape: prices are decimal STRINGS.
type rawModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
	Pricing       struct {
		Prompt         string `json:"prompt"`
		Completion     string `json:"completion"`
		InputCacheRead string `json:"input_cache_read"`
	} `json:"pricing"`
}

// parsePrice turns a catalog price string into a per-token float. Empty,
// unparsable and negative ("-1" = variable, used by the auto routers) all
// mean unknown.
func parsePrice(s string) *float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return nil
	}
	return &v
}

// ParseModels decodes an OpenRouter /models body. Items with an empty id are
// skipped; unknown fields are ignored. Order is preserved.
func ParseModels(r io.Reader) ([]Model, error) {
	var out struct {
		Data []rawModel `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(r, maxCatalogBody)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	models := make([]Model, 0, len(out.Data))
	for _, m := range out.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		models = append(models, Model{
			ID:            id,
			Name:          m.Name,
			ContextLength: m.ContextLength,
			Pricing: Pricing{
				Prompt:         parsePrice(m.Pricing.Prompt),
				Completion:     parsePrice(m.Pricing.Completion),
				InputCacheRead: parsePrice(m.Pricing.InputCacheRead),
			},
		})
	}
	return models, nil
}

// FetchPublicModels GETs the public catalog (no key required) from BaseURL.
// It is uncached; most callers want CachedPublicModels.
func FetchPublicModels() ([]Model, error) {
	req, err := http.NewRequest(http.MethodGet, BaseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build models request: %w", err)
	}
	client := &http.Client{Timeout: modelsListTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter /models returned HTTP %d", resp.StatusCode)
	}
	return ParseModels(resp.Body)
}

// catalogCache holds the last good catalog per BaseURL. Keyed by URL so a
// test that points BaseURL at an httptest server never sees production data.
var catalogCache = struct {
	sync.Mutex
	entries map[string]catalogEntry
}{entries: map[string]catalogEntry{}}

type catalogEntry struct {
	models    []Model
	fetchedAt time.Time
}

// catalogNow is the cache clock; tests may swap it.
var catalogNow = time.Now

// CachedPublicModels returns the public catalog, re-fetching at most once per
// catalogCacheTTL. A failed refresh keeps serving the last good catalog (a
// transient outage must not blank the picker or the price table); with no
// good catalog yet it returns nil.
func CachedPublicModels() []Model {
	key := BaseURL
	catalogCache.Lock()
	entry, ok := catalogCache.entries[key]
	catalogCache.Unlock()
	if ok && catalogNow().Sub(entry.fetchedAt) < catalogCacheTTL {
		return entry.models
	}
	models, err := FetchPublicModels()
	if err != nil || len(models) == 0 {
		if ok {
			return entry.models
		}
		return nil
	}
	catalogCache.Lock()
	catalogCache.entries[key] = catalogEntry{models: models, fetchedAt: catalogNow()}
	catalogCache.Unlock()
	return models
}

// ModelIDs projects a catalog onto its ids.
func ModelIDs(models []Model) []string {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

// AvailableSuggested returns the curated SuggestedModels that are present in
// the live catalog ids, in curated order. When live is empty the catalog could
// not be read, so nothing can be disproved and the full curated list is
// returned. A curated id OpenRouter has retired is therefore hidden instead of
// offered as a dead default.
func AvailableSuggested(live []string) []SuggestedModel {
	if len(live) == 0 {
		return append([]SuggestedModel(nil), SuggestedModels...)
	}
	present := make(map[string]bool, len(live))
	for _, id := range live {
		present[id] = true
	}
	out := make([]SuggestedModel, 0, len(SuggestedModels))
	for _, m := range SuggestedModels {
		if present[m.ID] {
			out = append(out, m)
		}
	}
	return out
}

// AvailableDefault returns DefaultModel when the live catalog still lists it
// (or cannot be read), else the first available suggested model, else
// DefaultModel unchanged (a manual entry is still possible).
func AvailableDefault(live []string) string {
	avail := AvailableSuggested(live)
	for _, m := range avail {
		if m.ID == DefaultModel {
			return DefaultModel
		}
	}
	if len(live) == 0 {
		return DefaultModel
	}
	for _, id := range live {
		if id == DefaultModel {
			return DefaultModel
		}
	}
	if len(avail) > 0 {
		return avail[0].ID
	}
	return DefaultModel
}
