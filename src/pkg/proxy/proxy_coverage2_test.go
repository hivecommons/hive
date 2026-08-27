package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/tokens"
)

// newInferenceTestProxy builds a GitHubProxy with the maps/routers the
// inference-side helpers require, plus a self-signed CA for MITM paths.
//
// proxyAdvisoryOK is set because several tests here identify their synthetic
// CONNECT requests via a Proxy-Authorization header rather than building a
// real /proc/net/tcp + UID map fixture (see identifyAgentFromConn/N7, #3841):
// with no UID map, that header is trusted ONLY in advisory mode. Production
// never sets this from a test helper — it reads HIVE_PROXY_ADVISORY_OK.
func newInferenceTestProxy() *GitHubProxy {
	caCert, caX509, _ := generateCA()
	return &GitHubProxy{
		caCert:          caCert,
		caX509:          caX509,
		logger:          slog.Default(),
		violations:      make(map[string]int),
		certCache:       make(map[string]cachedCert),
		inference:       newInferenceRouter(),
		entitlements:    newEntitlementStore(),
		proxyAdvisoryOK: true,
	}
}

// ---------- entitlementStore.probeParams ----------

// probeParams returns the credentials remembered by rememberProbe, normalizing
// the endpoint, and reports absence for an unknown endpoint.
func TestEntitlementStore_ProbeParams(t *testing.T) {
	s := newEntitlementStore()

	if _, _, _, ok := s.probeParams("https://gw.example.com"); ok {
		t.Fatal("expected no probe params before rememberProbe")
	}

	s.rememberProbe("https://gw.example.com/", "sk-team", "/etc/ca.pem")
	ep, apiKey, caBundle, ok := s.probeParams("https://gw.example.com") // trailing slash normalized away
	if !ok {
		t.Fatal("expected remembered probe params")
	}
	if ep != "https://gw.example.com" || apiKey != "sk-team" || caBundle != "/etc/ca.pem" {
		t.Fatalf("probeParams = %q %q %q", ep, apiKey, caBundle)
	}
}

// ---------- SetTokenSink ----------

// SetTokenSink wires the sink so later Record calls are attributed. A nil sink
// stays a safe no-op.
func TestSetTokenSink(t *testing.T) {
	p := newInferenceTestProxy()
	if p.tokenSink != nil {
		t.Fatal("expected nil sink before SetTokenSink")
	}
	sink := tokens.NewInferenceSink("", slog.Default())
	p.SetTokenSink(sink)
	if p.tokenSink != sink {
		t.Fatal("SetTokenSink did not wire the sink")
	}
}

// ---------- EntitledModels (accessor + stale re-probe trigger) ----------

// EntitledModels returns a cached set and, when the entry is stale, kicks off a
// background re-probe using the remembered credentials.
func TestEntitledModels_KnownAndStaleReprobe(t *testing.T) {
	p := newInferenceTestProxy()

	// Unknown endpoint: nothing known.
	if models, _, known := p.EntitledModels("https://gw.example.com"); known || models != nil {
		t.Fatalf("expected unknown endpoint, got %v known=%v", models, known)
	}

	// Fresh entry: returned, no re-probe needed.
	p.entitlements.set("https://gw.example.com", "sk-team", []string{"Azure/gpt-4o"}, "403")
	models, source, known := p.EntitledModels("https://gw.example.com")
	if !known || source != "403" || !reflect.DeepEqual(models, []string{"Azure/gpt-4o"}) {
		t.Fatalf("fresh entry = %v %q known=%v", models, source, known)
	}

	// Make the entry stale and register re-probe credentials pointing at a
	// mock key-info server. EntitledModels must still return the stale set and
	// trigger a background probe that (eventually) overwrites it.
	probed := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == entitlementProbePathKeyInfo {
			select {
			case probed <- struct{}{}:
			default:
			}
			fmt.Fprint(w, `{"info":{"models":["Azure/gpt-4.1"]}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	p.entitlements.set(srv.URL, "sk-team", []string{"Azure/gpt-4o"}, "403")
	p.entitlements.rememberProbe(srv.URL, "sk-team", "")
	// Force staleness.
	p.entitlements.mu.Lock()
	e := p.entitlements.byEndpt[normalizeEndpoint(srv.URL)]
	e.at = time.Now().Add(-entitlementTTL - time.Second)
	p.entitlements.byEndpt[normalizeEndpoint(srv.URL)] = e
	p.entitlements.mu.Unlock()

	models, _, known = p.EntitledModels(srv.URL)
	if !known || !reflect.DeepEqual(models, []string{"Azure/gpt-4o"}) {
		t.Fatalf("stale entry must still be served: %v known=%v", models, known)
	}
	select {
	case <-probed:
	case <-time.After(3 * time.Second):
		t.Fatal("stale EntitledModels did not trigger a background re-probe")
	}
}

// ---------- probeEntitlements ----------

// probeEntitlements caches the entitled set discovered via key-info, and a
// concurrent call for the same endpoint is collapsed by the in-flight guard.
func TestProbeEntitlements_CachesAndGuards(t *testing.T) {
	p := newInferenceTestProxy()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == entitlementProbePathKeyInfo {
			fmt.Fprint(w, `{"info":{"models":["Azure/gpt-4o","Azure/gpt-4.1"]}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	p.probeEntitlements(srv.URL, "sk-team", "")
	models, source, known, _ := p.entitlements.get(srv.URL)
	if !known || source != "key-info" || !reflect.DeepEqual(models, []string{"Azure/gpt-4o", "Azure/gpt-4.1"}) {
		t.Fatalf("probeEntitlements did not cache: %v %q known=%v", models, source, known)
	}

	// With a probe already marked in-flight, probeEntitlements is a no-op.
	p.entitlements.beginProbe(srv.URL)
	p.probeEntitlements(srv.URL, "sk-team", "") // returns immediately (guard)
	p.entitlements.endProbe(srv.URL)
}

// probeEntitlements with a bad CA bundle path falls back to a nil transport and
// still probes over the default transport.
func TestProbeEntitlements_BadCABundleFallsBack(t *testing.T) {
	p := newInferenceTestProxy()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == entitlementProbePathKeyInfo {
			fmt.Fprint(w, `{"info":{"models":["Azure/gpt-4o"]}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// Nonexistent CA bundle → inferenceTransport errors → transport=nil path.
	p.probeEntitlements(srv.URL, "sk-team", "/nonexistent/ca-bundle.pem")
	if _, _, known, _ := p.entitlements.get(srv.URL); !known {
		t.Fatal("expected entitled set cached even with bad CA bundle (nil-transport fallback)")
	}
}

// ---------- SetInferenceRoute ----------

// SetInferenceRoute stores the route immediately, resolves max-context-len in
// the background, and (for litellm+key) probes entitlements.
func TestSetInferenceRoute_LitellmProbesAndResolvesContext(t *testing.T) {
	p := newInferenceTestProxy()

	hitModels := make(chan struct{}, 1)
	hitKeyInfo := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			select {
			case hitModels <- struct{}{}:
			default:
			}
			fmt.Fprint(w, `{"data":[{"id":"Azure/gpt-4o","max_model_len":128000}]}`)
		case entitlementProbePathKeyInfo:
			select {
			case hitKeyInfo <- struct{}{}:
			default:
			}
			fmt.Fprint(w, `{"info":{"models":["Azure/gpt-4o"]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	route := &InferenceRoute{
		Backend:  "litellm",
		Endpoint: srv.URL,
		Model:    "Azure/gpt-4o",
		APIKey:   "sk-team",
	}
	p.SetInferenceRoute("scanner", route)

	// Route stored synchronously.
	if got := p.inference.Get("scanner"); got == nil || got.Model != "Azure/gpt-4o" {
		t.Fatalf("route not stored: %+v", got)
	}

	// Background max-context-len probe resolves the value.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if p.inference.Get("scanner").MaxContextLen == 128000 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := p.inference.Get("scanner").MaxContextLen; got != 128000 {
		t.Fatalf("max context len not resolved, got %d", got)
	}

	// Entitlement key-info probe fired and cached.
	select {
	case <-hitKeyInfo:
	case <-time.After(3 * time.Second):
		t.Fatal("litellm route did not trigger key-info probe")
	}
}

// SetInferenceRoute for a non-litellm backend with a preset MaxContextLen skips
// both the async context probe and the entitlement probe.
func TestSetInferenceRoute_VLLMNoProbe(t *testing.T) {
	p := newInferenceTestProxy()
	route := &InferenceRoute{
		Backend:       "vllm",
		Endpoint:      "http://127.0.0.1:1",
		Model:         "Qwen/Qwen2.5",
		MaxContextLen: 4096,
	}
	p.SetInferenceRoute("worker", route)
	if got := p.inference.Get("worker"); got == nil || got.MaxContextLen != 4096 {
		t.Fatalf("route not stored as-is: %+v", got)
	}
}

// ---------- capMaxTokensForInput ----------

func TestCapMaxTokensForInput(t *testing.T) {
	cases := []struct {
		name          string
		maxTokens     int
		maxContextLen int
		inputChars    int
		want          int
	}{
		// No cap when context length unknown.
		{"unknown context", 1000, 0, 5000, 1000},
		// No cap when maxTokens unset.
		{"no max tokens", 0, 8000, 100, 0},
		// Fits comfortably: maxTokens returned unchanged.
		{"fits", 500, 100000, 200, 500},
		// Large input squeezes available budget below the request.
		{"capped", 100000, 1000, 1000, 350}, // 1000 - 500(input) - 150(margin) = 350
		// Input so large that available floors at 1.
		{"floor at one", 5000, 100, 100000, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := capMaxTokensForInput(tc.maxTokens, tc.maxContextLen, tc.inputChars); got != tc.want {
				t.Fatalf("capMaxTokensForInput(%d,%d,%d) = %d, want %d",
					tc.maxTokens, tc.maxContextLen, tc.inputChars, got, tc.want)
			}
		})
	}
}

// ---------- translateAnthropicToolChoice ----------

func TestTranslateAnthropicToolChoice(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty defaults to auto", ``, `"auto"`},
		{"malformed defaults to auto", `not-json`, `"auto"`},
		{"auto explicit", `{"type":"auto"}`, `"auto"`},
		{"any becomes required", `{"type":"any"}`, `"required"`},
		{"tool becomes function", `{"type":"tool","name":"Bash"}`,
			`{"type":"function","function":{"name":"Bash"}}`},
		{"unknown type defaults to auto", `{"type":"weird"}`, `"auto"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(translateAnthropicToolChoice(json.RawMessage(tc.raw)))
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// ---------- resolveInferencePreamble ----------

func TestResolveInferencePreamble(t *testing.T) {
	if got := resolveInferencePreamble(nil, "any"); got != "" {
		t.Fatalf("nil route must yield empty preamble, got %q", got)
	}
	// Explicit preamble wins.
	if got := resolveInferencePreamble(&InferenceRoute{Preamble: "CUSTOM"}, "supervisor"); got != "CUSTOM" {
		t.Fatalf("explicit preamble must win, got %q", got)
	}
	// litellm opts out by default.
	if got := resolveInferencePreamble(&InferenceRoute{Backend: "litellm"}, "worker"); got != "" {
		t.Fatalf("litellm default must be empty, got %q", got)
	}
	// supervisor gets the supervisor preamble.
	if got := resolveInferencePreamble(&InferenceRoute{Backend: "vllm"}, "supervisor"); got != SupervisorInferencePreamble {
		t.Fatal("supervisor must get supervisor preamble")
	}
	// default vllm agent gets the default preamble.
	if got := resolveInferencePreamble(&InferenceRoute{Backend: "vllm"}, "worker"); got != DefaultInferencePreamble {
		t.Fatal("default vllm agent must get default preamble")
	}
}

// ---------- queryMaxModelLen ----------

func TestQueryMaxModelLen(t *testing.T) {
	// Exact model id match.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"other","max_model_len":100},{"id":"m","max_model_len":4096}]}`)
	}))
	defer srv.Close()
	if got := queryMaxModelLen(srv.URL, "m", "", ""); got != 4096 {
		t.Fatalf("exact match = %d, want 4096", got)
	}

	// No id match → falls back to first entry's max_model_len.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"only","max_model_len":8192}]}`)
	}))
	defer srv2.Close()
	if got := queryMaxModelLen(srv2.URL, "absent", "", ""); got != 8192 {
		t.Fatalf("fallback = %d, want 8192", got)
	}

	// Non-200 → 0.
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv3.Close()
	if got := queryMaxModelLen(srv3.URL, "m", "", ""); got != 0 {
		t.Fatalf("non-200 = %d, want 0", got)
	}

	// Unreachable endpoint → 0.
	if got := queryMaxModelLen("http://127.0.0.1:1", "m", "", ""); got != 0 {
		t.Fatalf("unreachable = %d, want 0", got)
	}

	// Bad CA bundle path → transport error → 0.
	if got := queryMaxModelLen("https://example.invalid", "m", "", "/nonexistent/ca.pem"); got != 0 {
		t.Fatalf("bad CA bundle = %d, want 0", got)
	}

	// Malformed JSON body → 0.
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `not json`)
	}))
	defer srv4.Close()
	if got := queryMaxModelLen(srv4.URL, "m", "", ""); got != 0 {
		t.Fatalf("malformed json = %d, want 0", got)
	}

	// Empty data array → 0.
	srv5 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv5.Close()
	if got := queryMaxModelLen(srv5.URL, "m", "", ""); got != 0 {
		t.Fatalf("empty data = %d, want 0", got)
	}
}

// ---------- FindEndpointForModel ----------

func TestFindEndpointForModel(t *testing.T) {
	// Server that serves model "target".
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-x" {
			t.Errorf("missing auth header: %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"data":[{"id":"other"},{"id":"target"}]}`)
	}))
	defer good.Close()

	// Server that returns a non-200.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer bad.Close()

	// Unreachable first, non-200 second, match third.
	endpoints := []string{"http://127.0.0.1:1", bad.URL, good.URL}
	if got := FindEndpointForModel(endpoints, "target", "sk-x", ""); got != good.URL {
		t.Fatalf("FindEndpointForModel = %q, want %q", got, good.URL)
	}

	// No endpoint serves the model.
	if got := FindEndpointForModel([]string{good.URL}, "missing", "sk-x", ""); got != "" {
		t.Fatalf("expected empty for missing model, got %q", got)
	}

	// Bad CA bundle → transport error → "".
	if got := FindEndpointForModel([]string{good.URL}, "target", "", "/nonexistent/ca.pem"); got != "" {
		t.Fatalf("bad CA bundle must yield empty, got %q", got)
	}

	// Malformed JSON body is skipped.
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `not json`)
	}))
	defer junk.Close()
	if got := FindEndpointForModel([]string{junk.URL}, "target", "", ""); got != "" {
		t.Fatalf("malformed json must be skipped, got %q", got)
	}
}

// ---------- newModelsRequest ----------

func TestNewModelsRequest(t *testing.T) {
	req, err := newModelsRequest("http://gw.example.com/", "sk-x")
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.String() != "http://gw.example.com/v1/models" {
		t.Fatalf("url = %q", req.URL.String())
	}
	if req.Header.Get("Authorization") != "Bearer sk-x" {
		t.Fatalf("auth = %q", req.Header.Get("Authorization"))
	}

	// No API key → no auth header.
	req2, err := newModelsRequest("http://gw.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if req2.Header.Get("Authorization") != "" {
		t.Fatal("expected no auth header without key")
	}

	// Invalid method target (control char in URL) → error.
	if _, err := newModelsRequest("http://exa\x00mple", "x"); err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

// ---------- inferenceTransport / inferenceHTTPClient ----------

func TestInferenceTransport(t *testing.T) {
	// Empty bundle → default transport.
	rt, err := inferenceTransport("")
	if err != nil || rt != http.DefaultTransport {
		t.Fatalf("empty bundle must return default transport, got %v err=%v", rt, err)
	}

	// Missing file → error.
	if _, err := inferenceTransport("/nonexistent/ca-bundle.pem"); err == nil {
		t.Fatal("expected error for missing CA bundle")
	}

	// File with no PEM certs → error.
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, []byte("garbage not a cert"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inferenceTransport(empty); err == nil {
		t.Fatal("expected error for CA bundle with no certs")
	}

	// Valid CA bundle → custom transport, cached (second call returns same).
	caPEM := caCertPEMForTest(t)
	valid := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(valid, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	rt1, err := inferenceTransport(valid)
	if err != nil {
		t.Fatal(err)
	}
	rt2, err := inferenceTransport(valid)
	if err != nil {
		t.Fatal(err)
	}
	if rt1 != rt2 {
		t.Fatal("expected cached transport on second call")
	}
	if rt1 == http.DefaultTransport {
		t.Fatal("custom CA bundle must not be the default transport")
	}
}

func TestInferenceHTTPClient(t *testing.T) {
	// No CA bundle → default client.
	c, err := inferenceHTTPClient(&InferenceRoute{})
	if err != nil || c != http.DefaultClient {
		t.Fatalf("expected default client, got %v err=%v", c, err)
	}

	// Bad CA bundle → error.
	if _, err := inferenceHTTPClient(&InferenceRoute{CABundle: "/nonexistent/ca.pem"}); err == nil {
		t.Fatal("expected error for bad CA bundle")
	}

	// Valid CA bundle → non-default client with custom transport.
	dir := t.TempDir()
	valid := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(valid, caCertPEMForTest(t), 0600); err != nil {
		t.Fatal(err)
	}
	c2, err := inferenceHTTPClient(&InferenceRoute{CABundle: valid})
	if err != nil {
		t.Fatal(err)
	}
	if c2 == http.DefaultClient {
		t.Fatal("valid CA bundle must yield a non-default client")
	}
}

// caCertPEMForTest generates a throwaway CA cert PEM for CA-bundle parsing tests.
func caCertPEMForTest(t *testing.T) []byte {
	t.Helper()
	_, x509Cert, err := generateCA()
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: x509Cert.Raw})
}

// ---------- extractOpenAIUsage ----------

func TestExtractOpenAIUsage(t *testing.T) {
	in, out := extractOpenAIUsage([]byte(`{"usage":{"prompt_tokens":12,"completion_tokens":7}}`))
	if in != 12 || out != 7 {
		t.Fatalf("usage = %d/%d, want 12/7", in, out)
	}
	// No usage block → 0/0.
	if in, out := extractOpenAIUsage([]byte(`{"id":"x"}`)); in != 0 || out != 0 {
		t.Fatalf("no usage = %d/%d, want 0/0", in, out)
	}
	// Malformed JSON → 0/0.
	if in, out := extractOpenAIUsage([]byte(`not json`)); in != 0 || out != 0 {
		t.Fatalf("malformed = %d/%d, want 0/0", in, out)
	}
}

// ---------- jsonEscape ----------

func TestJSONEscape(t *testing.T) {
	if got := jsonEscape(`he said "hi"`); got != `he said \"hi\"` {
		t.Fatalf("jsonEscape quotes = %q", got)
	}
	if got := jsonEscape("line1\nline2\tend"); got != `line1\nline2\tend` {
		t.Fatalf("jsonEscape controls = %q", got)
	}
	if got := jsonEscape(`c:\path`); got != `c:\\path` {
		t.Fatalf("jsonEscape backslash = %q", got)
	}
}

// ---------- forwardToInference error branches ----------

// forwardToInference surfaces an upstream non-2xx as an Anthropic api_error
// envelope with the same status code.
func TestForwardToInference_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `team not allowed`, http.StatusForbidden)
	}))
	defer srv.Close()

	route := &InferenceRoute{Backend: "litellm", Endpoint: srv.URL, Model: "m"}
	body := `{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()

	if err := forwardToInference(req, []byte(body), w, route, "agent"); err != nil {
		t.Fatal(err)
	}
	if w.Result().StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Result().StatusCode)
	}
	if !strings.Contains(w.Body.String(), "inference backend returned 403") {
		t.Fatalf("body = %q", w.Body.String())
	}
}

// forwardToInference returns an error when the upstream endpoint is unreachable.
func TestForwardToInference_Unreachable(t *testing.T) {
	route := &InferenceRoute{Backend: "vllm", Endpoint: "http://127.0.0.1:1", Model: "m"}
	body := `{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	if err := forwardToInference(req, []byte(body), w, route, "agent"); err == nil {
		t.Fatal("expected error for unreachable upstream")
	}
}

// forwardToInference returns an error when the request body cannot be translated.
func TestForwardToInference_BadRequestBody(t *testing.T) {
	route := &InferenceRoute{Backend: "vllm", Endpoint: "http://127.0.0.1:1", Model: "m"}
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("x"))
	w := httptest.NewRecorder()
	if err := forwardToInference(req, []byte(`not json`), w, route, "agent"); err == nil {
		t.Fatal("expected translate error for malformed body")
	}
}

// forwardToInference with a bad CA bundle fails at client setup.
func TestForwardToInference_BadCABundle(t *testing.T) {
	route := &InferenceRoute{Backend: "litellm", Endpoint: "https://example.invalid", Model: "m", CABundle: "/nonexistent/ca.pem"}
	body := `{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	if err := forwardToInference(req, []byte(body), w, route, "agent"); err == nil {
		t.Fatal("expected client-setup error for bad CA bundle")
	}
}

// ---------- StartInferenceTranslator (over the real fixed-port listener) ----------

// TestStartInferenceTranslator_HandlerBranches drives the translator HTTP
// handler across its no-route (502), routed (200), translate-error (502), and
// unreachable-upstream (502) branches by starting the real server on its fixed
// port and issuing requests against it.
func TestStartInferenceTranslator_HandlerBranches(t *testing.T) {
	p := newInferenceTestProxy()
	p.SetTokenSink(tokens.NewInferenceSink("", slog.Default()))

	// Drive the real handler without binding the fixed production port.
	// Mock upstream vLLM.
	upstream := startMockVLLM(t)
	defer upstream.Close()

	translator := httptest.NewServer(p.inferenceTranslatorHandler())
	defer translator.Close()
	base := translator.URL

	client := translator.Client()

	// (1) No route for agent → 502.
	req1, _ := http.NewRequest("POST", base+"/v1/messages", strings.NewReader(`{"model":"claude","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req1.Header.Set("x-api-key", "sk-hive-ghost")
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusBadGateway {
		t.Fatalf("no-route status = %d, want 502", resp1.StatusCode)
	}

	// (2) Configure a route → 200 translated response.
	p.inference.Set("real", &InferenceRoute{Backend: "vllm", Endpoint: upstream.URL, Model: "test-model"})
	req2, _ := http.NewRequest("POST", base+"/v1/messages", strings.NewReader(`{"model":"claude","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("x-api-key", "sk-hive-real")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("routed status = %d, want 200", resp2.StatusCode)
	}
	var ar anthropicResponse
	if err := json.NewDecoder(resp2.Body).Decode(&ar); err != nil {
		t.Fatal(err)
	}
	if ar.Type != "message" {
		t.Fatalf("type = %q, want message", ar.Type)
	}

	// (3) Malformed body → translate error → 502.
	p.inference.Set("bad", &InferenceRoute{Backend: "vllm", Endpoint: upstream.URL, Model: "m"})
	req3, _ := http.NewRequest("POST", base+"/v1/messages", strings.NewReader(`not json`))
	req3.Header.Set("x-api-key", "sk-hive-bad")
	resp3, err := client.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadGateway {
		t.Fatalf("malformed status = %d, want 502", resp3.StatusCode)
	}

	// (4) Unreachable upstream → 502.
	p.inference.Set("down", &InferenceRoute{Backend: "vllm", Endpoint: "http://127.0.0.1:1", Model: "m"})
	req4, _ := http.NewRequest("POST", base+"/v1/messages", strings.NewReader(`{"model":"claude","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req4.Header.Set("x-api-key", "sk-hive-down")
	resp4, err := client.Do(req4)
	if err != nil {
		t.Fatal(err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusBadGateway {
		t.Fatalf("unreachable status = %d, want 502", resp4.StatusCode)
	}
}

// ---------- handleInferenceRequest / handleAnthropicReroute over MITM ----------

// mitmDialClient builds an HTTP client that CONNECTs through the proxy and does
// a MITM TLS handshake trusting the proxy CA, so requests to api.anthropic.com
// are rerouted through handleAnthropicReroute → handleInferenceRequest.
func TestHandleAnthropicReroute_NonStreaming(t *testing.T) {
	p := newInferenceTestProxy()
	p.SetTokenSink(tokens.NewInferenceSink("", slog.Default()))

	upstream := startMockVLLM(t)
	defer upstream.Close()
	p.inference.Set("agent", &InferenceRoute{Backend: "vllm", Endpoint: upstream.URL, Model: "test-model"})

	// Proxy listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go p.handleConn(conn)
		}
	}()

	// CA pool trusting the proxy CA.
	pool := x509.NewCertPool()
	pool.AddCert(p.caX509)

	// Manual CONNECT then MITM TLS.
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Proxy-Authorization identifies the agent (no UID map in tests) so the
	// CONNECT is matched to the "agent" inference route and rerouted.
	fmt.Fprintf(raw, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\nProxy-Authorization: hive agent\r\n\r\n")
	br := bufio.NewReader(raw)
	statusLine, err := br.ReadString('\n')
	if err != nil || !strings.Contains(statusLine, "200") {
		t.Fatalf("CONNECT response = %q err=%v", statusLine, err)
	}
	// Drain to end of headers.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	tlsConn := tls.Client(raw, &tls.Config{ServerName: "api.anthropic.com", RootCAs: pool})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("MITM handshake: %v", err)
	}
	defer tlsConn.Close()

	reqBody := `{"model":"claude","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	fmt.Fprintf(tlsConn, "POST /v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\nContent-Length: %d\r\nContent-Type: application/json\r\n\r\n%s",
		len(reqBody), reqBody)

	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		t.Fatalf("read MITM response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reroute status = %d, want 200", resp.StatusCode)
	}
	var ar anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		t.Fatal(err)
	}
	if ar.Type != "message" || len(ar.Content) == 0 {
		t.Fatalf("unexpected reroute response: %+v", ar)
	}
}

// handleInferenceRequest surfaces upstream failure as an api_error envelope
// written directly to the connection.
func TestHandleInferenceRequest_UpstreamUnreachable(t *testing.T) {
	p := newInferenceTestProxy()
	route := &InferenceRoute{Backend: "vllm", Endpoint: "http://127.0.0.1:1", Model: "m"}

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	go func() {
		reqBody := `{"model":"claude","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
		req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		p.handleInferenceRequest(serverConn, req, "agent", route)
		serverConn.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// handleInferenceRequest surfaces a translate error (malformed body) as a 502.
func TestHandleInferenceRequest_TranslateError(t *testing.T) {
	p := newInferenceTestProxy()
	route := &InferenceRoute{Backend: "vllm", Endpoint: "http://127.0.0.1:1", Model: "m"}

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	go func() {
		req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader("not json"))
		p.handleInferenceRequest(serverConn, req, "agent", route)
		serverConn.Close()
	}()

	resp, err := http.ReadResponse(bufio.NewReader(clientConn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// ---------- loadOrGenerateCA / NewGitHubProxy (CA-path seam) ----------

// redirectCAPaths points the package CA read/write locations at a temp dir for
// the duration of a test, restoring the production defaults afterwards. The
// CACertPath/caKeyPath vars exist only for this test seam; production never
// reassigns them.
func redirectCAPaths(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origCert, origKey := CACertPath, caKeyPath
	CACertPath = filepath.Join(dir, "proxy-ca.pem")
	caKeyPath = filepath.Join(dir, "proxy-ca-key.pem")
	t.Cleanup(func() { CACertPath, caKeyPath = origCert, origKey })
	return dir
}

// loadOrGenerateCA generates a fresh CA when none is persisted, persists it,
// and reuses it on the next call rather than regenerating.
func TestLoadOrGenerateCA_GenerateThenReuse(t *testing.T) {
	redirectCAPaths(t)

	cert1, x1, err := loadOrGenerateCA(slog.Default())
	if err != nil {
		t.Fatalf("first loadOrGenerateCA: %v", err)
	}
	if _, statErr := os.Stat(CACertPath); statErr != nil {
		t.Fatalf("CA cert not persisted: %v", statErr)
	}
	if _, statErr := os.Stat(caKeyPath); statErr != nil {
		t.Fatalf("CA key not persisted: %v", statErr)
	}

	// Second call must REUSE the persisted CA (same serial), not regenerate.
	_, x2, err := loadOrGenerateCA(slog.Default())
	if err != nil {
		t.Fatalf("second loadOrGenerateCA: %v", err)
	}
	if x1.SerialNumber.Cmp(x2.SerialNumber) != 0 {
		t.Fatal("persisted CA must be reused (serial mismatch)")
	}
	if len(cert1.Certificate) == 0 {
		t.Fatal("expected a usable tls.Certificate")
	}
}

// A persisted CA that is unparseable is discarded and a fresh one generated.
func TestLoadOrGenerateCA_UnusablePersistedRegenerates(t *testing.T) {
	redirectCAPaths(t)

	// Write garbage in place of a valid key pair.
	if err := os.WriteFile(CACertPath, []byte("not a cert"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caKeyPath, []byte("not a key"), 0600); err != nil {
		t.Fatal(err)
	}

	_, x509Cert, err := loadOrGenerateCA(slog.Default())
	if err != nil {
		t.Fatalf("loadOrGenerateCA with garbage persisted: %v", err)
	}
	if x509Cert == nil || x509Cert.Subject.CommonName != "Hive ACMM Proxy CA" {
		t.Fatalf("expected a freshly generated CA, got %+v", x509Cert)
	}
}

// handleInferenceRequest streams an SSE upstream response back over the raw
// connection (streaming branch), recording usage into the sink.
func TestHandleInferenceRequest_Streaming(t *testing.T) {
	p := newInferenceTestProxy()
	p.SetTokenSink(tokens.NewInferenceSink("", slog.Default()))

	upstream := startMockVLLM(t)
	defer upstream.Close()
	route := &InferenceRoute{Backend: "vllm", Endpoint: upstream.URL, Model: "test-model"}

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	go func() {
		reqBody := `{"model":"claude","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
		req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		p.handleInferenceRequest(serverConn, req, "agent", route)
		serverConn.Close()
	}()

	buf := make([]byte, 4096)
	var got strings.Builder
	for {
		n, err := clientConn.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	out := got.String()
	if !strings.Contains(out, "text/event-stream") {
		t.Fatalf("expected SSE headers, got %q", out)
	}
	if !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop") {
		t.Fatalf("expected Anthropic SSE events, got %q", out)
	}
}

// NewGitHubProxy wires the allowed-repo set and constructs a usable proxy,
// persisting a fresh CA via the redirected paths.
func TestNewGitHubProxy_Constructs(t *testing.T) {
	redirectCAPaths(t)

	p, err := NewGitHubProxy(slog.Default(), "myorg", []string{"console", "docs"})
	if err != nil {
		t.Fatalf("NewGitHubProxy: %v", err)
	}
	if p.ListenAddr() != "127.0.0.1:18443" {
		t.Fatalf("listen addr = %q", p.ListenAddr())
	}
	if !p.allowedRepos["myorg/console"] || !p.allowedRepos["myorg/docs"] {
		t.Fatal("expected myorg/console and myorg/docs to be allowed")
	}
	if p.allowedRepos["myorg/other"] {
		t.Fatal("myorg/other must not be allowed")
	}
	// Pre-warm forged a cert for at least one known GitHub host.
	if len(p.certCache) == 0 {
		t.Fatal("expected pre-warmed cert cache")
	}
}
