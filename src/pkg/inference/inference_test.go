package inference

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/watsonx"
)

// discardLogger keeps the auth/route tests silent without losing the calls.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func cfgWith(lc config.LiteLLMConfig, gws []config.GatewayConfig) *config.Config {
	c := &config.Config{}
	c.Governor.LiteLLM = lc
	c.Governor.Gateways = gws
	return c
}

// TestLocalProxyURL pins the loopback URL to the reserved port. The Go
// translator forwards here, so a drift between the URL and the port the
// supervisor actually binds is a silent 502.
func TestLocalProxyURL(t *testing.T) {
	got := LocalProxyURL()
	if !strings.Contains(got, strconv.Itoa(LocalProxyPort)) {
		t.Fatalf("LocalProxyURL %q does not carry LocalProxyPort %d", got, LocalProxyPort)
	}
	if got != "http://127.0.0.1:18445" {
		t.Fatalf("LocalProxyURL = %q, want the loopback reserved port", got)
	}
}

// TestResolveLiteLLMRoute is the route-install decision tree for the built-in
// "litellm" backend. The `gateway fallback` cases pin the #5393 fix: a hive
// configured ONLY through the Model Gateways tab resolved its key and CA
// bundle from that gateway but its ENDPOINT from the empty legacy block, so no
// route was installed and every agent call died "502 no inference route".
func TestResolveLiteLLMRoute(t *testing.T) {
	localProxy := LocalProxyURL()

	cases := []struct {
		name         string
		litellm      config.LiteLLMConfig
		gateways     []config.GatewayConfig
		backend      string
		reqModel     string
		wantEndpoint string
		wantModel    string
		wantOK       bool
	}{
		{
			name:         "legacy endpoint with requested model",
			litellm:      config.LiteLLMConfig{Endpoint: "https://legacy", DefaultModel: "legacy-default"},
			backend:      "litellm",
			reqModel:     "asked-for",
			wantEndpoint: "https://legacy",
			wantModel:    "asked-for",
			wantOK:       true,
		},
		{
			name:         "legacy endpoint falls back to the legacy default model",
			litellm:      config.LiteLLMConfig{Endpoint: "https://legacy", DefaultModel: "legacy-default"},
			backend:      "litellm",
			wantEndpoint: "https://legacy",
			wantModel:    "legacy-default",
			wantOK:       true,
		},
		{
			name:         "local proxy overrides the configured endpoint",
			litellm:      config.LiteLLMConfig{Endpoint: "https://legacy", LocalProxy: true, DefaultModel: "legacy-default"},
			backend:      "litellm",
			wantEndpoint: localProxy,
			wantModel:    "legacy-default",
			wantOK:       true,
		},
		{
			name:    "gateway fallback supplies endpoint and model (#5393)",
			litellm: config.LiteLLMConfig{},
			gateways: []config.GatewayConfig{
				{Name: "litellm", Kind: "litellm", Endpoint: "https://gw", DefaultModel: "gw-default"},
			},
			backend:      "litellm",
			wantEndpoint: "https://gw",
			wantModel:    "gw-default",
			wantOK:       true,
		},
		{
			name:    "gateway fallback keeps an explicitly requested model",
			litellm: config.LiteLLMConfig{},
			gateways: []config.GatewayConfig{
				{Name: "litellm", Kind: "litellm", Endpoint: "https://gw", DefaultModel: "gw-default"},
			},
			backend:      "litellm",
			reqModel:     "asked-for",
			wantEndpoint: "https://gw",
			wantModel:    "asked-for",
			wantOK:       true,
		},
		{
			name:         "no endpoint anywhere installs no route",
			litellm:      config.LiteLLMConfig{DefaultModel: "legacy-default"},
			backend:      "litellm",
			reqModel:     "asked-for",
			wantEndpoint: "",
			wantModel:    "asked-for",
			wantOK:       false,
		},
		{
			name:    "gateway with an empty endpoint is not a route",
			litellm: config.LiteLLMConfig{},
			gateways: []config.GatewayConfig{
				{Name: "litellm", Kind: "litellm", DefaultModel: "gw-default"},
			},
			backend:      "litellm",
			wantEndpoint: "",
			wantModel:    "",
			wantOK:       false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep, model, ok := ResolveLiteLLMRoute(cfgWith(tc.litellm, tc.gateways), tc.backend, tc.reqModel)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ep != tc.wantEndpoint {
				t.Errorf("endpoint = %q, want %q", ep, tc.wantEndpoint)
			}
			if model != tc.wantModel {
				t.Errorf("model = %q, want %q", model, tc.wantModel)
			}
			if ok && ep == "" {
				t.Error("returned ok with an empty endpoint — this is the 502 the guard exists to prevent")
			}
		})
	}
}

// TestResolveWatsonxGateway must prefer the gateway BOTH named and kinded
// "watsonx" over a merely watsonx-kinded gateway with another name, fall back
// to kind-only when no canonical slot exists, and return nil when none is
// configured — resolving the wrong gateway would route the built-in watsonx
// backend through someone else's endpoint and key.
func TestResolveWatsonxGateway(t *testing.T) {
	canonical := config.GatewayConfig{Name: "watsonx", Kind: config.GatewayKindWatsonx, Endpoint: "https://canonical"}
	kindOnly := config.GatewayConfig{Name: "ibm-granite", Kind: config.GatewayKindWatsonx, Endpoint: "https://kind-only"}
	other := config.GatewayConfig{Name: "openrouter", Kind: "litellm", Endpoint: "https://other"}

	cases := []struct {
		name     string
		gateways []config.GatewayConfig
		want     string // expected Endpoint, "" for nil
	}{
		{"no gateways", nil, ""},
		{"no watsonx gateway", []config.GatewayConfig{other}, ""},
		{"kind-only fallback", []config.GatewayConfig{other, kindOnly}, "https://kind-only"},
		{"canonical preferred over kind-only", []config.GatewayConfig{kindOnly, canonical}, "https://canonical"},
		{"canonical alone", []config.GatewayConfig{canonical}, "https://canonical"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveWatsonxGateway(cfgWith(config.LiteLLMConfig{}, tc.gateways))
			if tc.want == "" {
				if got != nil {
					t.Fatalf("got gateway %q, want nil", got.Endpoint)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want gateway %q", tc.want)
			}
			if got.Endpoint != tc.want {
				t.Errorf("endpoint = %q, want %q", got.Endpoint, tc.want)
			}
		})
	}
}

// swapDefaultMinter points watsonx.DefaultMinter at endpoint for the duration
// of the test, restoring the shared process-wide minter afterwards, so the
// suite never mints against the real IBM IAM host.
func swapDefaultMinter(t *testing.T, endpoint string) {
	t.Helper()
	orig := watsonx.DefaultMinter
	watsonx.DefaultMinter = watsonx.NewTokenMinterForTest(endpoint, nil)
	t.Cleanup(func() { watsonx.DefaultMinter = orig })
}

// TestResolveGatewayAuthNonWatsonxPassesKeyThrough pins that every other kind
// gets the resolved key verbatim and no extra headers — minting or header
// injection on a non-watsonx gateway would corrupt an otherwise valid route.
func TestResolveGatewayAuthNonWatsonxPassesKeyThrough(t *testing.T) {
	t.Setenv("HIVE_TEST_GW_KEY", "sk-plain")
	gw := &config.GatewayConfig{Name: "openrouter", Kind: "litellm", APIKeyEnv: "HIVE_TEST_GW_KEY", ProjectID: "ignored"}

	key, headers := ResolveGatewayAuth(gw, "scanner", "litellm", discardLogger())
	if key != "sk-plain" {
		t.Errorf("key = %q, want the resolved key verbatim", key)
	}
	if headers != nil {
		t.Errorf("headers = %v, want none for a non-watsonx gateway", headers)
	}
}

// TestResolveGatewayAuthWatsonxMintsBearer pins that the watsonx path hands
// the agent the MINTED IAM bearer — never the raw IBM Cloud API key — and
// attaches the project header. Leaking the raw key upstream would both fail
// auth and put the long-lived key on the wire per request.
func TestResolveGatewayAuthWatsonxMintsBearer(t *testing.T) {
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"minted-bearer","expires_in":3600}`))
	}))
	defer iam.Close()
	swapDefaultMinter(t, iam.URL)

	t.Setenv("HIVE_TEST_GW_KEY", "ibm-raw-key")
	gw := &config.GatewayConfig{
		Name: "watsonx", Kind: config.GatewayKindWatsonx,
		APIKeyEnv: "HIVE_TEST_GW_KEY", ProjectID: "proj-123",
	}

	key, headers := ResolveGatewayAuth(gw, "scanner", "watsonx", discardLogger())
	if key == "ibm-raw-key" {
		t.Fatal("raw IBM Cloud API key handed to the agent instead of a minted bearer")
	}
	if key != "minted-bearer" {
		t.Errorf("key = %q, want the minted IAM bearer", key)
	}
	if got := headers[watsonx.ProjectIDHeader]; got != "proj-123" {
		t.Errorf("%s = %q, want the configured project id", watsonx.ProjectIDHeader, got)
	}
}

// TestResolveGatewayAuthWatsonxWithoutProjectSendsNoHeader pins that an
// unconfigured project yields NO header rather than an empty one — an empty
// X-IBM-Project-ID is rejected upstream differently from an absent one.
func TestResolveGatewayAuthWatsonxWithoutProjectSendsNoHeader(t *testing.T) {
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"minted-bearer","expires_in":3600}`))
	}))
	defer iam.Close()
	swapDefaultMinter(t, iam.URL)

	t.Setenv("HIVE_TEST_GW_KEY", "ibm-raw-key")
	gw := &config.GatewayConfig{Name: "watsonx", Kind: config.GatewayKindWatsonx, APIKeyEnv: "HIVE_TEST_GW_KEY"}

	_, headers := ResolveGatewayAuth(gw, "scanner", "watsonx", discardLogger())
	if _, ok := headers[watsonx.ProjectIDHeader]; ok {
		t.Errorf("project header present with no project configured: %v", headers)
	}
}

// TestResolveGatewayAuthWatsonxMintFailureKeepsRawKey pins the deliberate
// fallback: when minting fails the raw key is returned so watsonx answers a
// clear upstream 401, rather than dropping the route and leaving the agent
// with no endpoint at all.
func TestResolveGatewayAuthWatsonxMintFailureKeepsRawKey(t *testing.T) {
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer iam.Close()
	swapDefaultMinter(t, iam.URL)

	t.Setenv("HIVE_TEST_GW_KEY", "ibm-raw-key")
	gw := &config.GatewayConfig{Name: "watsonx", Kind: config.GatewayKindWatsonx, APIKeyEnv: "HIVE_TEST_GW_KEY"}

	key, _ := ResolveGatewayAuth(gw, "scanner", "watsonx", discardLogger())
	if key != "ibm-raw-key" {
		t.Errorf("key = %q, want the raw key preserved so the 401 surfaces upstream", key)
	}
}

// TestResolveGatewayAuthNeverLogsSecrets pins that neither the raw key nor the
// minted bearer reaches the log sink on the failure path, which is the branch
// that actually logs.
func TestResolveGatewayAuthNeverLogsSecrets(t *testing.T) {
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer iam.Close()
	swapDefaultMinter(t, iam.URL)

	var sink strings.Builder
	logger := slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Setenv("HIVE_TEST_GW_KEY", "ibm-raw-secret-key")
	gw := &config.GatewayConfig{Name: "watsonx", Kind: config.GatewayKindWatsonx, APIKeyEnv: "HIVE_TEST_GW_KEY"}

	ResolveGatewayAuth(gw, "scanner", "watsonx", logger)

	if strings.Contains(sink.String(), "ibm-raw-secret-key") {
		t.Errorf("API key leaked into logs:\n%s", sink.String())
	}
}

// TestParseEndpointList pins the comma-splitting used for the vllm/llm-d
// endpoint env vars, including the nil-not-empty-slice contract.
func TestParseEndpointList(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty string", "", nil},
		{"only separators and space", " , , ", nil},
		{"single url", "http://a", []string{"http://a"}},
		{"multiple urls", "http://a,http://b", []string{"http://a", "http://b"}},
		{"trims surrounding space", " http://a , http://b ", []string{"http://a", "http://b"}},
		{"drops empty entries", "http://a,,http://b,", []string{"http://a", "http://b"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseEndpointList(tc.raw)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil (not an empty slice)", got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
