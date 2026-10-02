package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The Test Connection probe must accept the exact OpenAI-shaped /v1/models
// payload the live LiteLLM gateway returns — including per-item object /
// created / owned_by fields and provider-prefixed, hyphenated model ids — and
// report the real model count. Before the fix the probe read only a truncated
// prefix of the body, so a large valid list decoded as invalid JSON and
// produced a false "non-OpenAI response" negative.
func TestProbeLiteLLMModels_AcceptsFullOpenAIPayload(t *testing.T) {
	// Captured-shape payload: top-level {"data":[...],"object":"list"}, each
	// item id/object/created/owned_by, ids with Azure/ aws/ azure/ prefixes
	// and hyphenated versions. Padded well past the old 512-byte read cap.
	const modelCount = 40
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var b strings.Builder
		b.WriteString(`{"data":[`)
		for i := 0; i < modelCount; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b,
				`{"id":"Azure/gpt-5.1-codex-2025-11-13-variant-%02d","object":"model","created":1677610602,"owned_by":"openai"}`, i)
		}
		b.WriteString(`],"object":"list"}`)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, b.String())
	}))
	defer srv.Close()

	n, err := probeLiteLLMModels(srv.URL, "sk-livekeyvalue")
	if err != nil {
		t.Fatalf("probe failed on a valid OpenAI payload: %v", err)
	}
	if n != modelCount {
		t.Errorf("probe reported %d models, want %d", n, modelCount)
	}
}

// The probe must not require top-level object=="list" (some gateways omit it)
// nor a specific field order, and must tolerate unknown fields — exactly what
// the discovery dropdown parser accepts.
func TestProbeLiteLLMModels_LenientShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No top-level "object"; extra unknown top-level + per-item fields;
		// provider-prefixed and hyphenated ids.
		fmt.Fprint(w, `{"unexpected_top":true,"data":[`+
			`{"owned_by":"aws","id":"aws/claude-opus-4-7","object":"model","extra":123},`+
			`{"id":"azure/gemini-2.5-flash"}`+
			`]}`)
	}))
	defer srv.Close()

	n, err := probeLiteLLMModels(srv.URL, "")
	if err != nil {
		t.Fatalf("probe rejected a lenient-but-valid payload: %v", err)
	}
	if n != 2 {
		t.Errorf("probe reported %d models, want 2", n)
	}
}

func TestProbeLiteLLMModelsRejectsRedirectToPrivateHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest/meta-data/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"stolen-token-sink"}]}`)
			return
		}
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/v1/models", http.StatusFound)
	}))
	defer srv.Close()
	routeImportFetchesToServer(t, srv)

	if _, err := probeLiteLLMModels("https://gateway.example.invalid", "sk-livekeyvalue"); err == nil {
		t.Fatal("expected redirect to private host to be blocked")
	} else if !strings.Contains(err.Error(), "redirect to private/internal host blocked") {
		t.Fatalf("expected private redirect error, got %v", err)
	}
}

// parseModelsResponse is the shared source of truth; verify it returns
// provider-prefixed ids verbatim and skips id-less entries.
func TestParseModelsResponse_VerbatimIDs(t *testing.T) {
	body := `{"object":"list","data":[` +
		`{"id":"Azure/gpt-5.1-codex-2025-11-13"},` +
		`{"id":"claude-opus-4-7"},` +
		`{"object":"model"}` + // no id — skipped
		`]}`
	models, err := parseModelsResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"Azure/gpt-5.1-codex-2025-11-13", "claude-opus-4-7"}
	if len(models) != len(want) {
		t.Fatalf("got %v, want %v", models, want)
	}
	for i, m := range models {
		if m != want[i] {
			t.Errorf("models[%d] = %q, want %q", i, m, want[i])
		}
	}
}

// An HTML "403 Forbidden" page comes from an ingress/WAF/VPN proxy in front of
// the gateway, not from the gateway's auth layer: the probe must say so —
// naming the URL it tried — instead of claiming the configured key was
// rejected, which sent a reporter chasing a key that worked fine for
// /v1/completions (hivecommons/hive#9945).
func TestProbeLiteLLMModels_HTMLForbiddenBlamesProxyNotKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Server", "nginx")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html>\n<head><title>403 Forbidden</title></head>\n<body><center><h1>403 Forbidden</h1></center></body>\n</html>")
	}))
	defer srv.Close()

	_, err := probeLiteLLMModels(srv.URL, "sk-livekeyvalue")
	if err == nil {
		t.Fatal("expected an error for an HTTP 403 probe response")
	}
	msg := err.Error()
	if strings.Contains(msg, "gateway rejected the configured key") {
		t.Errorf("HTML 403 must not be attributed to the key: %v", err)
	}
	for _, want := range []string{"proxy in front of the gateway", srv.URL + "/v1/models", "nginx", "403 Forbidden"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// A JSON auth error IS the gateway speaking, so the key-specific message (and
// the probed URL) must survive.
func TestProbeLiteLLMModels_JSONForbiddenStillBlamesKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"message":"token not found","type":"auth_error"}}`)
	}))
	defer srv.Close()

	_, err := probeLiteLLMModels(srv.URL, "sk-livekeyvalue")
	if err == nil {
		t.Fatal("expected an error for an HTTP 403 probe response")
	}
	for _, want := range []string{"gateway rejected the configured key", srv.URL + "/v1/models", "token not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// When the hub's egress would route the probe through an environment proxy
// (HTTPS_PROXY/https_proxy, honoured by http.ProxyFromEnvironment via the
// probeProxyFunc seam), an HTML/empty 401/403 must name that proxy — the
// reporter's laptop has no such proxy and reaches LiteLLM directly, so the
// proxy identity is the one fact that tells them the paths differ
// (hivecommons/hive#9945).
func TestProbeLiteLLMModels_HTMLForbiddenNamesEgressProxyWhenConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Server", "nginx")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html><body>403 Forbidden</body></html>")
	}))
	defer srv.Close()

	proxyURL, err := url.Parse("http://user:secret@proxy.example:3128")
	if err != nil {
		t.Fatalf("parsing test proxy URL: %v", err)
	}
	origProbeProxyFunc := probeProxyFunc
	probeProxyFunc = func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	defer func() { probeProxyFunc = origProbeProxyFunc }()

	_, probeErr := probeLiteLLMModels(srv.URL, "sk-livekeyvalue")
	if probeErr == nil {
		t.Fatal("expected an error for an HTTP 403 probe response")
	}
	msg := probeErr.Error()
	if !strings.Contains(msg, "proxy.example:3128") {
		t.Errorf("error %q does not name the configured egress proxy", msg)
	}
	if !strings.Contains(msg, "HTTPS_PROXY") {
		t.Errorf("error %q does not mention HTTPS_PROXY", msg)
	}
	if strings.Contains(msg, "secret") || strings.Contains(msg, "user:secret") {
		t.Errorf("error %q leaks proxy userinfo", msg)
	}
}

// When no egress proxy applies, the message must not claim one does, but
// should note that the hive's own network path (not the laptop's VPN) is
// what the gateway saw.
func TestProbeLiteLLMModels_HTMLForbiddenNoProxyConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Server", "nginx")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html><body>403 Forbidden</body></html>")
	}))
	defer srv.Close()

	origProbeProxyFunc := probeProxyFunc
	probeProxyFunc = func(*http.Request) (*url.URL, error) { return nil, nil }
	defer func() { probeProxyFunc = origProbeProxyFunc }()

	_, err := probeLiteLLMModels(srv.URL, "sk-livekeyvalue")
	if err == nil {
		t.Fatal("expected an error for an HTTP 403 probe response")
	}
	msg := err.Error()
	if strings.Contains(msg, "HTTPS_PROXY egress proxy was configured") == false {
		t.Errorf("error %q does not note the absence of an egress proxy: %v", msg, err)
	}
	if strings.Contains(msg, "://") && strings.Contains(msg, "egress proxy http") {
		t.Errorf("error %q wrongly names a proxy URL when none is configured", msg)
	}
}

func TestLooksLikeIntermediaryRejection(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		want        bool
	}{
		{"html page", "text/html", "<html><title>403 Forbidden</title></html>", true},
		{"html body without content type", "", "<html>403</html>", true},
		{"empty body", "", "", true},
		{"plain text refusal", "text/plain", "access denied", false},
		{"gateway json", "application/json", `{"error":"token not found"}`, false},
		{"json body without content type", "", `{"error":"token not found"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeIntermediaryRejection(tc.contentType, tc.body); got != tc.want {
				t.Errorf("looksLikeIntermediaryRejection(%q, %q) = %v, want %v", tc.contentType, tc.body, got, tc.want)
			}
		})
	}
}

// An edge proxy that blocks GET /v1/models but lets inference through must not
// fail Test Connection: the probe falls back to a 1-token chat completion with
// the default model on the same path the inference translator uses, and
// reports success with a model-listing warning (hivecommons/hive#9945).
func TestLiteLLMProbeResult_ModelsBlockedByEdgeFallsBackToInference(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Server", "nginx")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "<html><body>403 Forbidden</body></html>")
		case "/v1/chat/completions":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer sk-livekeyvalue" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var body struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotModel = body.Model
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"p"}}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := &Server{}
	lc := &config.LiteLLMConfig{Endpoint: srv.URL, DefaultModel: "rits/zai-org/glm-5-3"}
	probe := s.liteLLMProbeResult(lc, "sk-livekeyvalue")
	if probe == nil || probe["ok"] != true {
		t.Fatalf("expected ok probe via inference fallback, got %v", probe)
	}
	if probe["modelsListBlocked"] != true || probe["inferenceModel"] != "rits/zai-org/glm-5-3" {
		t.Errorf("probe should flag blocked model listing and name the model: %v", probe)
	}
	if w, _ := probe["warning"].(string); !strings.Contains(w, "/v1/models") {
		t.Errorf("warning %q should name the blocked /v1/models URL", w)
	}
	if gotModel != "rits/zai-org/glm-5-3" {
		t.Errorf("inference fallback sent model %q, want the default model", gotModel)
	}
}

// When the edge blocks inference too, the probe still fails, and the error
// says both paths were refused so the operator knows the hive's network path
// (not the key) is the problem.
func TestLiteLLMProbeResult_EdgeBlocksInferenceTooFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Server", "nginx")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html><body>403 Forbidden</body></html>")
	}))
	defer srv.Close()

	s := &Server{}
	lc := &config.LiteLLMConfig{Endpoint: srv.URL, DefaultModel: "m1"}
	probe := s.liteLLMProbeResult(lc, "sk-livekeyvalue")
	if probe == nil || probe["ok"] != false {
		t.Fatalf("expected failed probe, got %v", probe)
	}
	msg, _ := probe["error"].(string)
	for _, want := range []string{srv.URL + "/v1/models", srv.URL + "/v1/chat/completions", "inference from the hive is blocked too"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "sk-livekeyvalue") {
		t.Errorf("error %q leaks the key", msg)
	}
}

// A JSON 403 is the gateway itself rejecting the key: no inference fallback.
func TestLiteLLMProbeResult_GatewayKeyRejectionSkipsFallback(t *testing.T) {
	var chatCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			chatCalls++
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"message":"token not found"}}`)
	}))
	defer srv.Close()

	s := &Server{}
	lc := &config.LiteLLMConfig{Endpoint: srv.URL, DefaultModel: "m1"}
	probe := s.liteLLMProbeResult(lc, "sk-livekeyvalue")
	if probe == nil || probe["ok"] != false {
		t.Fatalf("expected failed probe, got %v", probe)
	}
	if chatCalls != 0 {
		t.Errorf("gateway key rejection must not trigger the inference fallback (got %d chat calls)", chatCalls)
	}
}

// A bare 401 with an empty body and no key configured is the gateway asking
// for credentials, not an edge refusal: keep the actionable "no key is
// configured" message instead of sending the operator to the ingress rules.
func TestProbeLiteLLMModels_EmptyUnauthorizedWithoutKeyBlamesMissingKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := probeLiteLLMModels(srv.URL, "")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "requires an API key and none is configured") {
		t.Errorf("error %q should blame the missing key", err)
	}
	var edgeErr *probeEdgeRejectedError
	if errors.As(err, &edgeErr) {
		t.Errorf("error %q must not be classified as an edge rejection", err)
	}
}

// An empty 401 WITH a key configured is still ambiguous enough to be an edge
// refusal, so that classification is unchanged.
func TestProbeLiteLLMModels_EmptyUnauthorizedWithKeyStillBlamesProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := probeLiteLLMModels(srv.URL, "sk-livekeyvalue")
	if err == nil {
		t.Fatal("expected an error")
	}
	var edgeErr *probeEdgeRejectedError
	if !errors.As(err, &edgeErr) {
		t.Errorf("error %q should be classified as an edge rejection", err)
	}
}

// When the edge blocks GET /v1/models and no default model is configured,
// there is nothing to POST — the probe must say why it could not run the
// inference fallback instead of only reporting the proxy refusal.
func TestLiteLLMProbeResult_EdgeBlockedWithoutDefaultModelExplainsSkip(t *testing.T) {
	var chatCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			chatCalls++
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Server", "nginx")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html><body>403 Forbidden</body></html>")
	}))
	defer srv.Close()

	s := &Server{}
	lc := &config.LiteLLMConfig{Endpoint: srv.URL}
	probe := s.liteLLMProbeResult(lc, "sk-livekeyvalue")
	if probe == nil || probe["ok"] != false {
		t.Fatalf("expected failed probe, got %v", probe)
	}
	msg, _ := probe["error"].(string)
	for _, want := range []string{srv.URL + "/v1/models", "no default model is configured"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if chatCalls != 0 {
		t.Errorf("no default model must not trigger an inference POST (got %d chat calls)", chatCalls)
	}
}
