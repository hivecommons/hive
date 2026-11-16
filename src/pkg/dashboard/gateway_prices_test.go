package dashboard

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/tokens"
)

const gwPriceEps = 1e-9

func f64(v float64) *float64 { return &v }

func approx(a, b float64) bool { return math.Abs(a-b) < gwPriceEps }

// syncGatewayPrices makes the background price refresh synchronous and
// clears the cache for the test.
func syncGatewayPrices(t *testing.T) {
	t.Helper()
	prev := gatewayPriceRefreshAsync
	gatewayPriceRefreshAsync = false
	gatewayPriceCache.Lock()
	gatewayPriceCache.entries = map[string]*gatewayPriceEntry{}
	gatewayPriceCache.Unlock()
	t.Cleanup(func() {
		gatewayPriceRefreshAsync = prev
		gatewayPriceCache.Lock()
		gatewayPriceCache.entries = map[string]*gatewayPriceEntry{}
		gatewayPriceCache.Unlock()
	})
}

// The OpenRouter catalog's per-token prices (recorded shape) become per-1M
// prices, with the published cache-read rate; unknown prices are skipped.
func TestOpenRouterPriceTableFromCatalog(t *testing.T) {
	table := openRouterPriceTable([]OpenRouterModel{
		{ID: "deepseek/deepseek-v4.1-flash", Pricing: OpenRouterPricing{Prompt: f64(0.00000014), Completion: f64(0.00000042), InputCacheRead: f64(0.0000000042)}},
		{ID: "z-ai/glm-5.3-flash", Pricing: OpenRouterPricing{Prompt: f64(0.0000000399), Completion: f64(0.0000000784)}},
		{ID: "openrouter/auto"},
	})
	p, ok := table["deepseek/deepseek-v4.1-flash"]
	if !ok || !approx(p.InputPerMTok, 0.14) || !approx(p.OutputPerMTok, 0.42) || !approx(p.CacheReadPerMTok, 0.0042) {
		t.Fatalf("flash = %+v %v", p, ok)
	}
	// No published cache-read rate: cached input bills at the input rate.
	if g := table["z-ai/glm-5.3-flash"]; !approx(g.CacheReadPerMTok, g.InputPerMTok) {
		t.Fatalf("glm flash cache read = %+v", g)
	}
	if _, ok := table["openrouter/auto"]; ok {
		t.Fatal("an unpriced model must be left out, not priced at $0")
	}
}

func TestParseLiteLLMModelInfo(t *testing.T) {
	body := `{"data":[
	  {"model_name":"claude_sonnet_5.5","model_info":{"input_cost_per_token":0.000002,"output_cost_per_token":0.00001,"cache_read_input_token_cost":0.0000002}},
	  {"model_name":"granite","model_info":{"input_cost_per_token":0.0000001,"output_cost_per_token":0.0000003}},
	  {"model_name":"no-price","model_info":{}}
	]}`
	table, err := parseLiteLLMModelInfo(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if p := table["claude_sonnet_5.5"]; !approx(p.InputPerMTok, 2) || !approx(p.OutputPerMTok, 10) || !approx(p.CacheReadPerMTok, 0.2) {
		t.Fatalf("sonnet = %+v", p)
	}
	if p := table["granite"]; !approx(p.CacheReadPerMTok, p.InputPerMTok) {
		t.Fatalf("granite cache default = %+v", p)
	}
	if _, ok := table["no-price"]; ok {
		t.Fatal("no-price must be skipped")
	}
	if _, err := parseLiteLLMModelInfo(strings.NewReader("nope")); err == nil {
		t.Fatal("bad json must error")
	}
}

func TestLiteLLMModelInfoURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://litellm.example.com":     "https://litellm.example.com/model/info",
		"https://litellm.example.com/v1/": "https://litellm.example.com/model/info",
	} {
		if got := liteLLMModelInfoURL(in); got != want {
			t.Errorf("liteLLMModelInfoURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConfiguredGatewayPriceOverride(t *testing.T) {
	prices := &config.GatewayPrices{
		Input: f64(0.5), Output: f64(1.5),
		Models: map[string]config.GatewayModelPrice{"big": {Input: f64(3), CachedInput: f64(0.3), Output: f64(9)}},
	}
	if p, ok := configuredGatewayPrice(prices, "small"); !ok || p.InputPerMTok != 0.5 || p.OutputPerMTok != 1.5 || p.CacheReadPerMTok != 0.5 {
		t.Fatalf("flat = %+v %v (cached input defaults to input)", p, ok)
	}
	if p, ok := configuredGatewayPrice(prices, "big"); !ok || p.InputPerMTok != 3 || p.CacheReadPerMTok != 0.3 || p.OutputPerMTok != 9 {
		t.Fatalf("per-model = %+v %v", p, ok)
	}
	if _, ok := configuredGatewayPrice(&config.GatewayPrices{Input: f64(1)}, "x"); ok {
		t.Fatal("input without output must not count as a price")
	}
	if _, ok := configuredGatewayPrice(nil, "x"); ok {
		t.Fatal("nil prices")
	}
}

func gatewayPriceServer(t *testing.T, gws []config.GatewayConfig, agents map[string]config.AgentConfig) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Agents = agents
	cfg.Governor.Gateways = gws
	return &Server{deps: &Dependencies{Config: cfg}}
}

// vllm publishes no price: its served models are UNKNOWN unless prices: is set.
func TestGatewayModelPriceUnknownAndOverride(t *testing.T) {
	syncGatewayPrices(t)
	agents := map[string]config.AgentConfig{
		"worker": {Enabled: true, Backend: "local-vllm", Model: "Qwen/Qwen3-32B"},
	}
	s := gatewayPriceServer(t, []config.GatewayConfig{{Name: "local-vllm", Kind: "vllm", Endpoint: "http://vllm:8000"}}, agents)
	if _, known, served := s.gatewayModelPrice("Qwen/Qwen3-32B"); known || !served {
		t.Fatalf("vllm model: known=%v served=%v, want unknown but served", known, served)
	}
	if _, _, served := s.gatewayModelPrice("claude-opus-5"); served {
		t.Fatal("a model no gateway serves must not be claimed")
	}

	s = gatewayPriceServer(t, []config.GatewayConfig{{Name: "local-vllm", Kind: "vllm", Endpoint: "http://vllm:8000",
		Prices: &config.GatewayPrices{Input: f64(0.2), CachedInput: f64(0.02), Output: f64(0.6)}}}, agents)
	p, known, served := s.gatewayModelPrice("Qwen/Qwen3-32B")
	if !known || !served || p.InputPerMTok != 0.2 || p.CacheReadPerMTok != 0.02 || p.OutputPerMTok != 0.6 {
		t.Fatalf("override = %+v known=%v served=%v", p, known, served)
	}

	// Installed as the process pricer, the cost estimate uses it.
	tokens.SetGatewayPricer(s.gatewayModelPrice)
	t.Cleanup(func() { tokens.SetGatewayPricer(nil) })
	usd, src := tokens.EstimateCostWithSource("Qwen/Qwen3-32B", true, 1_000_000, 1_000_000, 1_000_000, 0)
	if src != tokens.PriceSourceGateway || !approx(usd, 0.2+0.6+0.02) {
		t.Fatalf("estimate = %v %q", usd, src)
	}
}

// LiteLLM prices come from /model/info, read with the gateway key.
func TestGatewayModelPriceFromLiteLLMModelInfo(t *testing.T) {
	syncGatewayPrices(t)
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/info" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"model_name":"granite","model_info":{"input_cost_per_token":0.0000001,"output_cost_per_token":0.0000003}}]}`))
	}))
	defer srv.Close()
	t.Setenv("TEST_LITELLM_PRICE_KEY", "sk-test")
	s := gatewayPriceServer(t, []config.GatewayConfig{{Name: "corp", Kind: "litellm", Endpoint: srv.URL, APIKeyEnv: "TEST_LITELLM_PRICE_KEY"}}, nil)
	p, known, served := s.gatewayModelPrice("granite")
	if !known || !served || !approx(p.InputPerMTok, 0.1) || !approx(p.OutputPerMTok, 0.3) {
		t.Fatalf("litellm price = %+v known=%v served=%v", p, known, served)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth = %q", gotAuth)
	}
}

func TestGatewaySpendNilWithoutTokens(t *testing.T) {
	s := gatewayPriceServer(t, []config.GatewayConfig{{Name: "openrouter", Kind: "openrouter"}}, nil)
	if rows := s.gatewaySpend(); rows != nil {
		t.Fatalf("rows = %+v, want nil without a token collector", rows)
	}
}
