package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/tokens"
)

// Gateway pricing: where a gateway model's dollars come from.
//
//   - Any kind: an optional `prices:` block on the gateway (USD per 1M
//     tokens, flat or per model). It wins over everything below and is the
//     only source for vllm, llm-d, watsonx and custom gateways.
//   - openrouter: the public catalog's per-token `pricing` block (the same
//     cached catalog model discovery reads).
//   - litellm: the proxy's /model/info (input_cost_per_token,
//     output_cost_per_token, cache_read_input_token_cost).
//
// A model a gateway serves with none of these is priced UNKNOWN: spend
// reports say "price unknown" instead of counting it as $0 or a tier guess.
//
// The remote sources are read in the background and cached, never on the
// request path: an offline hive must not stall /api/cost on a 10-second
// catalog fetch.

const (
	// gatewayPriceTTL is how long a fetched price table is reused, and how
	// long a failed fetch waits before it is retried.
	gatewayPriceTTL = 15 * time.Minute
	// liteLLMModelInfoTimeout bounds one /model/info request.
	liteLLMModelInfoTimeout = 10 * time.Second
	// maxModelInfoBody caps the /model/info body read.
	maxModelInfoBody = 8 << 20
	// usdPerTokenToPerMillion converts per-token prices to per-1M.
	usdPerTokenToPerMillion = 1_000_000.0
	// liteLLMModelInfoPath is LiteLLM's model metadata route (prices included).
	liteLLMModelInfoPath = "/model/info"
)

// gatewayPriceTable is one gateway's remote price list: model id → price.
type gatewayPriceTable map[string]tokens.ModelPrice

type gatewayPriceEntry struct {
	table       gatewayPriceTable
	attemptedAt time.Time
	inFlight    bool
}

// gatewayPriceCache holds the remote price tables, keyed by gateway
// kind+name+endpoint. Package-level because tokens.SetGatewayPricer is
// process-wide too.
var gatewayPriceCache = struct {
	sync.Mutex
	entries map[string]*gatewayPriceEntry
}{entries: map[string]*gatewayPriceEntry{}}

// gatewayPriceRefreshAsync is a test seam: tests refresh synchronously.
var gatewayPriceRefreshAsync = true

// gatewayPriceNow is the cache clock; tests may swap it.
var gatewayPriceNow = time.Now

// cachedGatewayPrices returns the last good table for key, starting a
// refresh when it is older than gatewayPriceTTL. A failed refresh keeps the
// previous table.
func cachedGatewayPrices(key string, fetch func() (gatewayPriceTable, error)) gatewayPriceTable {
	gatewayPriceCache.Lock()
	e := gatewayPriceCache.entries[key]
	if e == nil {
		e = &gatewayPriceEntry{}
		gatewayPriceCache.entries[key] = e
	}
	stale := !e.inFlight && (e.attemptedAt.IsZero() || gatewayPriceNow().Sub(e.attemptedAt) >= gatewayPriceTTL)
	if stale {
		e.inFlight = true
		e.attemptedAt = gatewayPriceNow()
	}
	table := e.table
	gatewayPriceCache.Unlock()
	if !stale {
		return table
	}
	refresh := func() {
		t, err := fetch()
		gatewayPriceCache.Lock()
		defer gatewayPriceCache.Unlock()
		e.inFlight = false
		if err == nil && len(t) > 0 {
			e.table = t
		}
	}
	if gatewayPriceRefreshAsync {
		go refresh()
		return table
	}
	refresh()
	gatewayPriceCache.Lock()
	defer gatewayPriceCache.Unlock()
	return e.table
}

// perMillion converts a per-token USD price to per-1M.
func perMillion(perToken float64) float64 { return perToken * usdPerTokenToPerMillion }

// openRouterPriceTable converts the catalog into a price table. Models with
// unknown input or output price are left out (unknown, not free). Without a
// published cache-read rate, cached input bills at the input rate.
func openRouterPriceTable(catalog []OpenRouterModel) gatewayPriceTable {
	t := gatewayPriceTable{}
	for _, m := range catalog {
		p := m.Pricing
		if p.Prompt == nil || p.Completion == nil {
			continue
		}
		price := tokens.ModelPrice{
			InputPerMTok:      perMillion(*p.Prompt),
			OutputPerMTok:     perMillion(*p.Completion),
			CacheReadPerMTok:  perMillion(*p.Prompt),
			CacheWritePerMTok: perMillion(*p.Prompt),
		}
		if p.InputCacheRead != nil {
			price.CacheReadPerMTok = perMillion(*p.InputCacheRead)
		}
		t[m.ID] = price
	}
	return t
}

// parseLiteLLMModelInfo extracts per-model prices from a LiteLLM /model/info
// body: {data:[{model_name, model_info:{input_cost_per_token,
// output_cost_per_token, cache_read_input_token_cost,
// cache_creation_input_token_cost}}]}. Entries without both an input and an
// output cost are skipped (unknown, not free).
func parseLiteLLMModelInfo(r io.Reader) (gatewayPriceTable, error) {
	var body struct {
		Data []struct {
			ModelName string `json:"model_name"`
			ModelInfo struct {
				Input      *float64 `json:"input_cost_per_token"`
				Output     *float64 `json:"output_cost_per_token"`
				CacheRead  *float64 `json:"cache_read_input_token_cost"`
				CacheWrite *float64 `json:"cache_creation_input_token_cost"`
			} `json:"model_info"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(r, maxModelInfoBody)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode model info: %w", err)
	}
	t := gatewayPriceTable{}
	for _, d := range body.Data {
		mi := d.ModelInfo
		name := strings.TrimSpace(d.ModelName)
		if name == "" || mi.Input == nil || mi.Output == nil {
			continue
		}
		p := tokens.ModelPrice{
			InputPerMTok:      perMillion(*mi.Input),
			OutputPerMTok:     perMillion(*mi.Output),
			CacheReadPerMTok:  perMillion(*mi.Input),
			CacheWritePerMTok: perMillion(*mi.Input),
		}
		if mi.CacheRead != nil {
			p.CacheReadPerMTok = perMillion(*mi.CacheRead)
		}
		if mi.CacheWrite != nil {
			p.CacheWritePerMTok = perMillion(*mi.CacheWrite)
		}
		t[name] = p
	}
	return t, nil
}

// liteLLMModelInfoURL is the gateway's /model/info URL. LiteLLM serves it at
// the proxy root, so a trailing /v1 on the endpoint is dropped.
func liteLLMModelInfoURL(endpoint string) string {
	base := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	base = strings.TrimSuffix(base, "/v1")
	return base + liteLLMModelInfoPath
}

// fetchLiteLLMPrices reads a LiteLLM gateway's /model/info with its key.
func fetchLiteLLMPrices(gw config.GatewayConfig) (gatewayPriceTable, error) {
	req, err := http.NewRequest(http.MethodGet, liteLLMModelInfoURL(gw.Endpoint), nil)
	if err != nil {
		return nil, fmt.Errorf("build model info request: %w", err)
	}
	if key := gw.ResolveAPIKey(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := costHTTPClient(gw.CABundle)
	client.Timeout = liteLLMModelInfoTimeout
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model info request failed: %w", err)
	}
	defer closeHTTPBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model info returned HTTP %d", resp.StatusCode)
	}
	return parseLiteLLMModelInfo(resp.Body)
}

// configuredGatewayPrice returns the gateway's prices: override for model:
// the per-model entry first, then the flat rate. Both input and output must
// be set; cached input defaults to input.
func configuredGatewayPrice(p *config.GatewayPrices, model string) (tokens.ModelPrice, bool) {
	if p == nil {
		return tokens.ModelPrice{}, false
	}
	in, cached, out := p.Input, p.CachedInput, p.Output
	if m, ok := p.Models[model]; ok {
		if m.Input != nil {
			in = m.Input
		}
		if m.CachedInput != nil {
			cached = m.CachedInput
		}
		if m.Output != nil {
			out = m.Output
		}
	}
	if in == nil || out == nil {
		return tokens.ModelPrice{}, false
	}
	price := tokens.ModelPrice{InputPerMTok: *in, OutputPerMTok: *out, CacheReadPerMTok: *in, CacheWritePerMTok: *in}
	if cached != nil {
		price.CacheReadPerMTok = *cached
	}
	return price, true
}

// remoteGatewayPrices is the cached remote table for a gateway (OpenRouter
// catalog or LiteLLM /model/info); nil for kinds that publish no prices.
func (s *Server) remoteGatewayPrices(gw config.GatewayConfig) gatewayPriceTable {
	key := strings.ToLower(gw.Kind) + "|" + gw.Name + "|" + gw.Endpoint
	switch {
	case isOpenRouterGateway(gw.Kind, gw.Endpoint):
		or := s.openRouter()
		if or == nil {
			return nil
		}
		return cachedGatewayPrices(key, func() (gatewayPriceTable, error) {
			return openRouterPriceTable(or.PublicModels()), nil
		})
	case strings.EqualFold(gw.Kind, config.GatewayKindLiteLLM):
		return cachedGatewayPrices(key, func() (gatewayPriceTable, error) {
			return fetchLiteLLMPrices(gw)
		})
	}
	return nil
}

// gatewayServesModel reports whether model is routed through gw: it is the
// gateway's default, an enabled agent on the gateway selects it, or the
// gateway's price list or prices: block names it.
func (s *Server) gatewayServesModel(gw config.GatewayConfig, model string, remote gatewayPriceTable) bool {
	if model == strings.TrimSpace(gw.DefaultModel) {
		return true
	}
	if _, ok := remote[model]; ok {
		return true
	}
	if gw.Prices != nil {
		if _, ok := gw.Prices.Models[model]; ok {
			return true
		}
	}
	for _, agent := range s.deps.Config.EnabledAgents() {
		if strings.EqualFold(agent.Backend, gw.Name) && agent.Model == model {
			return true
		}
	}
	return false
}

// gatewayModelPrice is the tokens.GatewayPriceFunc the dashboard installs:
// the price of model on the first configured gateway that serves it.
// known=false with served=true means that gateway publishes no price.
func (s *Server) gatewayModelPrice(model string) (tokens.ModelPrice, bool, bool) {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return tokens.ModelPrice{}, false, false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return tokens.ModelPrice{}, false, false
	}
	for _, gw := range s.deps.Config.Governor.ResolvedGateways() {
		remote := s.remoteGatewayPrices(gw)
		if !s.gatewayServesModel(gw, model, remote) {
			continue
		}
		if p, ok := configuredGatewayPrice(gw.Prices, model); ok {
			return p, true, true
		}
		if p, ok := remote[model]; ok {
			return p, true, true
		}
		return tokens.ModelPrice{}, false, true
	}
	return tokens.ModelPrice{}, false, false
}

// InstallGatewayPricer makes every cost estimate in this process consult the
// configured gateways' prices (OpenRouter catalog, LiteLLM /model/info,
// prices: overrides). The pricer is process-wide, so cmd/hive installs it
// once at boot; unit tests that build servers keep the static price table.
func (s *Server) InstallGatewayPricer() {
	tokens.SetGatewayPricer(s.gatewayModelPrice)
}

// gatewaySpendRow is one gateway's estimated spend for the usage gauge: the
// enabled agents routed through it and what their tokens cost at the
// gateway's prices. UnknownPriceModels are models whose tokens are NOT in USD.
type gatewaySpendRow struct {
	Gateway            string   `json:"gateway"`
	Kind               string   `json:"kind,omitempty"`
	Agents             []string `json:"agents"`
	USD                float64  `json:"usd"`
	Tokens             int64    `json:"tokens"`
	UnknownPriceModels []string `json:"unknown_price_models"`
}

// gatewaySpend groups enabled agents by the gateway their backend names and
// sums their estimated cost over the token window the cost card uses.
// Gateways with no enabled agent are left out; nil when nothing applies.
func (s *Server) gatewaySpend() []gatewaySpendRow {
	if s.deps == nil || s.deps.Config == nil || s.deps.Tokens == nil {
		return nil
	}
	gws := s.deps.Config.Governor.ResolvedGateways()
	if len(gws) == 0 {
		return nil
	}
	agentsBy := map[string][]string{}
	for name, agent := range s.deps.Config.EnabledAgents() {
		for _, gw := range gws {
			if strings.EqualFold(agent.Backend, gw.Name) {
				agentsBy[gw.Name] = append(agentsBy[gw.Name], name)
				break
			}
		}
	}
	if len(agentsBy) == 0 {
		return nil
	}
	summary := s.deps.Tokens.Summary()
	if summary == nil {
		return nil
	}
	est := tokens.EstimateFromSummary(summary)
	var rows []gatewaySpendRow
	for _, gw := range gws {
		agents := agentsBy[gw.Name]
		if len(agents) == 0 {
			continue
		}
		sort.Strings(agents)
		row := gatewaySpendRow{Gateway: gw.Name, Kind: gw.Kind, Agents: agents, UnknownPriceModels: []string{}}
		seen := map[string]bool{}
		for _, a := range agents {
			mc, ok := est.ByAgent[a]
			if !ok {
				continue
			}
			row.USD += mc.USD
			row.Tokens += mc.Input + mc.Output + mc.CacheRead + mc.CacheCreate
			for _, m := range mc.UnknownPriceModels {
				if !seen[m] {
					seen[m] = true
					row.UnknownPriceModels = append(row.UnknownPriceModels, m)
				}
			}
		}
		sort.Strings(row.UnknownPriceModels)
		rows = append(rows, row)
	}
	return rows
}
