package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The FMA shape from hivecommons/hive#9945: the legacy governor.litellm block
// still names the OLD gateway, the Model Gateways tab points gateway "litellm"
// at the NEW one. After the pkg/inference fix the route follows the explicit
// gateway, and the diagnostic must say so — naming the exact POST URL, the key
// fingerprint and the source — with NO mismatch warning when the tested
// endpoint is that gateway.
func TestInferenceRouteDiagnostic_ExplicitGatewayRouteMatchesGatewayTest(t *testing.T) {
	t.Setenv(config.LiteLLMEndpointEnv, "")
	t.Setenv("HIVE_TEST_LITELLM_KEY", "sk-newkeyvalue")
	cfg := &config.Config{Governor: config.GovernorConfig{
		AgentRuntimeGovernorConfig: config.AgentRuntimeGovernorConfig{
			LiteLLM: config.LiteLLMConfig{Endpoint: "https://old-gateway.example", DefaultModel: "old-model"},
			Gateways: []config.GatewayConfig{{
				Name: "litellm", Kind: config.GatewayKindLiteLLM,
				Endpoint: "https://litellm.cb.example", DefaultModel: "rits/zai-org/glm-5-3",
				APIKeyEnv: "HIVE_TEST_LITELLM_KEY", KeyName: "mspreitz-cb-1",
			}},
		},
	}}

	line := inferenceRouteDiagnostic(cfg, "litellm", "https://litellm.cb.example/", "sk-newkeyvalue")
	for _, want := range []string{
		`backend "litellm"`,
		"POST https://litellm.cb.example/v1/chat/completions",
		"model rits/zai-org/glm-5-3",
		"Authorization: Bearer",
		"SHA256 " + shortKeyHash(config.APIKeySHA256("sk-newkeyvalue")),
		`gateway "litellm" (Model Gateways tab)`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("diagnostic %q missing %q", line, want)
		}
	}
	if strings.Contains(line, "⚠") || strings.Contains(line, "old-gateway") {
		t.Errorf("route agrees with the tested gateway; no warning expected: %q", line)
	}
	if strings.Contains(line, "sk-newkeyvalue") {
		t.Errorf("diagnostic leaked the key value: %q", line)
	}
}

// The LiteLLM tab tests the LEGACY endpoint. When an explicit gateway named
// "litellm" exists, agents do not use that endpoint — the diagnostic must warn
// that a passing Test proves nothing about inference and say what to reconcile.
func TestInferenceRouteDiagnostic_LegacyTabWarnsWhenGatewayWinsRoute(t *testing.T) {
	t.Setenv(config.LiteLLMEndpointEnv, "")
	cfg := &config.Config{Governor: config.GovernorConfig{
		AgentRuntimeGovernorConfig: config.AgentRuntimeGovernorConfig{
			LiteLLM:  config.LiteLLMConfig{Endpoint: "https://old-gateway.example"},
			Gateways: []config.GatewayConfig{{Name: "litellm", Endpoint: "https://litellm.cb.example"}},
		},
	}}
	line := inferenceRouteDiagnostic(cfg, "litellm", "https://old-gateway.example", "")
	for _, want := range []string{
		"POST https://litellm.cb.example/v1/chat/completions",
		"⚠ That is NOT the endpoint this Test just exercised (https://old-gateway.example)",
		"does not prove agent inference works",
		"governor.litellm.endpoint",
		// #9945 reopen: the reporter could not find "the legacy endpoint";
		// the warning must say where in Settings it lives.
		"Settings → Model Gateways → Legacy LiteLLM endpoint",
		config.LiteLLMEndpointEnv,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("diagnostic %q missing %q", line, want)
		}
	}
	if !strings.Contains(line, "with no Authorization header") {
		t.Errorf("keyless route must say so: %q", line)
	}
}

// A tested key that differs from the key the route resolver hands agents is a
// second, independent way for "OK" to lie; both fingerprints are shown.
func TestInferenceRouteDiagnostic_KeyMismatchWarns(t *testing.T) {
	t.Setenv(config.LiteLLMEndpointEnv, "")
	t.Setenv("HIVE_TEST_LITELLM_KEY", "sk-routekey")
	cfg := &config.Config{Governor: config.GovernorConfig{
		AgentRuntimeGovernorConfig: config.AgentRuntimeGovernorConfig{
			LiteLLM: config.LiteLLMConfig{Endpoint: "https://gw.example", APIKeyEnv: "HIVE_TEST_LITELLM_KEY"},
		},
	}}
	line := inferenceRouteDiagnostic(cfg, "litellm", "https://gw.example", "sk-testedkey")
	wantRoute := shortKeyHash(config.APIKeySHA256("sk-routekey"))
	wantTested := shortKeyHash(config.APIKeySHA256("sk-testedkey"))
	if !strings.Contains(line, "⚠ The key agents send (SHA256 "+wantRoute+") is NOT the key this Test just used (SHA256 "+wantTested+")") {
		t.Errorf("expected key mismatch warning with both fingerprints, got %q", line)
	}
	if !strings.Contains(line, "legacy governor.litellm block (Settings → Model Gateways → Legacy LiteLLM endpoint)") {
		t.Errorf("no explicit gateway: source must be the legacy block and say where it is edited, got %q", line)
	}
}

// Source attribution must follow the resolver's precedence: local_proxy, then
// the explicit gateway, then env, then yaml; and no route at all is an
// explicit "NONE" with the 502 consequence spelled out.
func TestInferenceRouteDiagnostic_SourcesAndNoRoute(t *testing.T) {
	t.Setenv(config.LiteLLMEndpointEnv, "")
	if got := inferenceRouteDiagnostic(&config.Config{}, "litellm", "", ""); !strings.Contains(got, "NONE") || !strings.Contains(got, "502 no inference route") {
		t.Errorf("empty config: want NONE + 502 consequence, got %q", got)
	}
	if got := inferenceRouteDiagnostic(nil, "litellm", "", ""); got != "" {
		t.Errorf("nil config: want empty, got %q", got)
	}

	local := &config.Config{Governor: config.GovernorConfig{AgentRuntimeGovernorConfig: config.AgentRuntimeGovernorConfig{LiteLLM: config.LiteLLMConfig{LocalProxy: true}}}}
	if got := inferenceRouteDiagnostic(local, "", "", ""); !strings.Contains(got, "local_proxy") {
		t.Errorf("local proxy source not named: %q", got)
	}

	t.Setenv(config.LiteLLMEndpointEnv, "https://from-env.example")
	env := &config.Config{Governor: config.GovernorConfig{AgentRuntimeGovernorConfig: config.AgentRuntimeGovernorConfig{LiteLLM: config.LiteLLMConfig{Endpoint: "https://from-yaml.example"}}}}
	got := inferenceRouteDiagnostic(env, "litellm", "", "")
	if !strings.Contains(got, "POST https://from-env.example/v1/chat/completions") || !strings.Contains(got, config.LiteLLMEndpointEnv+" environment variable") {
		t.Errorf("env source not named: %q", got)
	}
}

// A gateway that is NOT named litellm is selected by agents as backend=<name>
// and routes straight through itself; unknown names describe nothing.
func TestInferenceRouteDiagnostic_NamedGatewayAndUnknownBackend(t *testing.T) {
	t.Setenv("HIVE_TEST_OR_KEY", "sk-or-key")
	cfg := &config.Config{Governor: config.GovernorConfig{
		AgentRuntimeGovernorConfig: config.AgentRuntimeGovernorConfig{
			Gateways: []config.GatewayConfig{{Name: "openrouter", Kind: "openrouter", Endpoint: "https://openrouter.example/api/v1", DefaultModel: "x/y", APIKeyEnv: "HIVE_TEST_OR_KEY"}},
		},
	}}
	got := inferenceRouteDiagnostic(cfg, "openrouter", "https://openrouter.example/api/v1", "sk-or-key")
	for _, want := range []string{`backend "openrouter"`, "POST https://openrouter.example/api/v1/chat/completions", "model x/y", `gateway "openrouter"`} {
		if !strings.Contains(got, want) {
			t.Errorf("named gateway diagnostic %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "⚠") {
		t.Errorf("no mismatch expected: %q", got)
	}
	if got := inferenceRouteDiagnostic(cfg, "nosuch", "", ""); got != "" {
		t.Errorf("unknown backend: want empty, got %q", got)
	}
}

// nginx-style HTML 403 vs LiteLLM-style JSON 401, through gatewayProbeResult:
// the gateway Test's diagnostic must classify them differently, report the
// request shape for the JSON case (method, path, bearer header, fingerprint,
// User-Agent, egress proxy) and the responder's Server header, and ALWAYS end
// with the inference route line.
func TestGatewayProbeResult_ClassifiesEdgeVsGatewayAndAppendsRoute(t *testing.T) {
	const key = "sk-livekeyvalue"
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    []string
		reject  []string
	}{
		{
			name: "nginx html 403 is a proxy refusal",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.Header().Set("Server", "nginx")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, "<html><head><title>403 Forbidden</title></head><body><center><h1>403 Forbidden</h1></center></body></html>")
			},
			want:   []string{"Connection failed:", "proxy in front of the gateway, not by the gateway API", `server "nginx"`, "403 Forbidden"},
			reject: []string{"gateway rejected the configured key"},
		},
		{
			name: "litellm json 401 is the gateway evaluating the key",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Server", "uvicorn")
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":{"message":"Authentication Error, Invalid proxy server token passed. Received API Key = sk-abc, Key Hash (Token) = 0123456789abcdef0123456789abcdef","type":"auth_error"}}`)
			},
			want: []string{
				"Connection failed:", "gateway rejected the configured key",
				"gateway API's own JSON auth error", `server "uvicorn"`, `content-type "application/json"`,
				"request sent: GET ", "/v1/models with Authorization: Bearer <key> (SHA256 " + shortKeyHash(config.APIKeySHA256(key)) + ")",
				"User-Agent Go-http-client/1.1", "egress:", "Invalid proxy server token",
			},
			reject: []string{"proxy in front of the gateway", "0123456789abcdef0123456789abcdef", key},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(tc.handler)
			defer upstream.Close()
			t.Setenv(config.LiteLLMEndpointEnv, "")
			t.Setenv("HIVE_TEST_LITELLM_KEY", key)
			origProxyFunc := probeProxyFunc
			probeProxyFunc = func(*http.Request) (*url.URL, error) { return nil, nil }
			t.Cleanup(func() { probeProxyFunc = origProxyFunc })

			gw := config.GatewayConfig{
				Name: "litellm", Kind: config.GatewayKindLiteLLM, Endpoint: upstream.URL,
				DefaultModel: "rits/zai-org/glm-5-3", APIKeyEnv: "HIVE_TEST_LITELLM_KEY", KeyName: "mspreitz-cb-1",
			}
			s := newFullServer(t)
			s.deps.Config.Governor.LiteLLM = config.LiteLLMConfig{Endpoint: "https://old-gateway.example"}
			s.deps.Config.Governor.Gateways = []config.GatewayConfig{gw}

			probe := s.gatewayProbeResult(gw, "")
			if probe == nil || probe["ok"] != false {
				t.Fatalf("expected failed probe, got %v", probe)
			}
			diag, _ := probe["diagnostic"].(string)
			for _, want := range append(tc.want,
				"Using key: mspreitz-cb-1",
				"Agents on backend \"litellm\" send inference to POST "+upstream.URL+"/v1/chat/completions",
				`route source: gateway "litellm"`) {
				if !strings.Contains(diag, want) {
					t.Errorf("diagnostic missing %q:\n%s", want, diag)
				}
			}
			for _, reject := range append(tc.reject, "old-gateway", "⚠") {
				if strings.Contains(diag, reject) {
					t.Errorf("diagnostic must not contain %q:\n%s", reject, diag)
				}
			}
			if route, _ := probe["route"].(string); !strings.HasSuffix(diag, route) || route == "" {
				t.Errorf("probe[\"route\"] = %q must be the diagnostic's last line:\n%s", route, diag)
			}
		})
	}
}

// The LiteLLM-tab probe (legacy block) carries the same route line, and warns
// when an explicit gateway named litellm means agents will not use the
// endpoint this tab just tested — on success as well as on failure.
func TestLiteLLMProbeResult_AttachesRouteAndWarnsOnDivergence(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"rits/zai-org/glm-5-3"}]}`)
	}))
	defer upstream.Close()
	t.Setenv(config.LiteLLMEndpointEnv, "")

	s := newFullServer(t)
	s.deps.Config.Governor.LiteLLM = config.LiteLLMConfig{Endpoint: upstream.URL, DefaultModel: "rits/zai-org/glm-5-3"}
	s.deps.Config.Governor.Gateways = []config.GatewayConfig{{Name: "litellm", Endpoint: "https://litellm.cb.example"}}
	lc := s.deps.Config.Governor.LiteLLM

	probe := s.liteLLMProbeResult(&lc, "sk-livekeyvalue")
	if probe == nil || probe["ok"] != true {
		t.Fatalf("expected ok probe, got %v", probe)
	}
	route, _ := probe["route"].(string)
	for _, want := range []string{
		"POST https://litellm.cb.example/v1/chat/completions",
		"⚠ That is NOT the endpoint this Test just exercised (" + upstream.URL + ")",
	} {
		if !strings.Contains(route, want) {
			t.Errorf("route %q missing %q", route, want)
		}
	}

	// Without the explicit gateway the tab's endpoint IS the route: no warning.
	s.deps.Config.Governor.Gateways = nil
	probe = s.liteLLMProbeResult(&lc, "sk-livekeyvalue")
	route, _ = probe["route"].(string)
	if !strings.Contains(route, "POST "+upstream.URL+"/v1/chat/completions") || strings.Contains(route, "⚠") {
		t.Errorf("legacy-only route should name the tested endpoint with no warning, got %q", route)
	}

	// A Server without deps (older test seams) is tolerated: no route, no panic.
	bare := &Server{}
	if p := bare.liteLLMProbeResult(&lc, "sk-livekeyvalue"); p == nil || p["route"] != nil {
		t.Errorf("bare server must probe without a route line, got %v", p)
	}
}

// probeLiteLLMInference distinguishes a JSON 401 from the gateway (reports the
// request shape + Server) from an HTML 403 at the edge, and names the Server
// header on other statuses.
func TestProbeLiteLLMInference_ErrorClassification(t *testing.T) {
	origProxyFunc := probeProxyFunc
	probeProxyFunc = func(*http.Request) (*url.URL, error) { return nil, nil }
	t.Cleanup(func() { probeProxyFunc = origProxyFunc })

	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		want        []string
		reject      []string
	}{
		{
			name: "json 401", status: http.StatusUnauthorized, contentType: "application/json",
			body:   `{"error":{"message":"Invalid proxy server token passed"}}`,
			want:   []string{"rejected by the gateway API itself with HTTP 401", `server "uvicorn"`, "request sent: POST ", "/v1/chat/completions with Authorization: Bearer <key> (SHA256 ", "Content-Type application/json", "User-Agent Go-http-client/1.1", "Invalid proxy server token"},
			reject: []string{"proxy in front of the gateway", "sk-livekeyvalue"},
		},
		{
			name: "html 403", status: http.StatusForbidden, contentType: "text/html",
			body:   "<html><body>403 Forbidden</body></html>",
			want:   []string{"refused with HTTP 403 by a proxy in front of the gateway", `server "uvicorn"`},
			reject: []string{"gateway API itself"},
		},
		{
			name: "other 500", status: http.StatusInternalServerError, contentType: "application/json",
			body: `{"error":"boom"}`,
			want: []string{"returned HTTP 500", `server "uvicorn"`, `content-type "application/json"`, "boom"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Server", "uvicorn")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer upstream.Close()
			err := probeLiteLLMInference(upstream.URL, "sk-livekeyvalue", "rits/zai-org/glm-5-3")
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err.Error(), want)
				}
			}
			for _, reject := range tc.reject {
				if strings.Contains(err.Error(), reject) {
					t.Errorf("error %q must not contain %q", err.Error(), reject)
				}
			}
		})
	}
}

// probeRequestShape never prints the key, says when no header was sent, and
// reports the egress proxy host when one applies.
func TestProbeRequestShape(t *testing.T) {
	origProxyFunc := probeProxyFunc
	probeProxyFunc = func(*http.Request) (*url.URL, error) { return url.Parse("http://egress.corp:3128") }
	t.Cleanup(func() { probeProxyFunc = origProxyFunc })

	req, _ := http.NewRequest(http.MethodGet, "https://gw.example/v1/models", nil)
	got := probeRequestShape(req, "")
	if !strings.Contains(got, "GET https://gw.example/v1/models with no Authorization header") || !strings.Contains(got, "egress.corp:3128") {
		t.Errorf("keyless shape = %q", got)
	}
	got = probeRequestShape(req, "sk-secret")
	if strings.Contains(got, "sk-secret") || !strings.Contains(got, "Authorization: Bearer <key> (SHA256 "+shortKeyHash(config.APIKeySHA256("sk-secret"))+")") {
		t.Errorf("keyed shape = %q", got)
	}
	if shortKeyHash("abc") != "abc" {
		t.Errorf("short hashes pass through unchanged")
	}
}
