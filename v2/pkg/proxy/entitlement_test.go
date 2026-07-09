package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// The live 403 body verbatim from the field: a LiteLLM team-scope denial that
// lists the entitled set. parseTeam403Models must extract those ids verbatim,
// prefixes intact, so the dashboard can narrow the dropdown to them.
func TestParseTeam403Models_LiveBody(t *testing.T) {
	body := `inference backend returned 403: {"error":{"message":"team not allowed to access model. ` +
		`This team can only access models=['Azure/gpt-4.1','Azure/gpt-4o','Azure/gpt-5-2025-08-07',` +
		`'Azure/gpt-5-mini-2025-08-07','Azure/gpt-5-nano-2025-08-07']","type":"auth_error"}}`
	got := parseTeam403Models(body)
	want := []string{
		"Azure/gpt-4.1", "Azure/gpt-4o", "Azure/gpt-5-2025-08-07",
		"Azure/gpt-5-mini-2025-08-07", "Azure/gpt-5-nano-2025-08-07",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A double-quoted models list (some LiteLLM versions) must parse too, and
// provider prefixes for non-Azure providers must survive verbatim (no provider
// assumptions).
func TestParseTeam403Models_DoubleQuotedMixedProviders(t *testing.T) {
	body := `team not allowed to access model. models=["aws/us.claude-opus-4-7","gemini/gemini-2.5-pro","Azure/gpt-4o"]`
	got := parseTeam403Models(body)
	want := []string{"aws/us.claude-opus-4-7", "gemini/gemini-2.5-pro", "Azure/gpt-4o"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A 403 that is NOT a team-scope denial (e.g. a bad key) must yield no
// entitlement signal, so an unrelated 403 never narrows the dropdown.
func TestParseTeam403Models_NonTeamDenialIgnored(t *testing.T) {
	if got := parseTeam403Models(`{"error":{"message":"Invalid proxy server token passed"}}`); got != nil {
		t.Fatalf("expected nil for non-team 403, got %v", got)
	}
}

// Provider-side failures relayed by the gateway — an upstream 404 not-found
// (e.g. Vertex model missing) or 400 bad-request (e.g. Azure codex param
// rejection) — say NOTHING about the key's entitlements. Verified live: with
// verbatim model names, 24/28 advertised models worked for a scoped key and
// the 4 failures were provider-side, not entitlement. Their bodies must never
// be misread as an entitlement signal.
func TestParseTeam403Models_ProviderErrorsAreNotEntitlementSignals(t *testing.T) {
	bodies := []string{
		`{"error":{"message":"litellm.NotFoundError: VertexAIException - Publisher Model was not found","code":404}}`,
		`{"error":{"message":"litellm.BadRequestError: AzureException - Unsupported parameter","code":400}}`,
		`{"error":{"message":"upstream timeout"}}`,
	}
	for _, b := range bodies {
		if got := parseTeam403Models(b); got != nil {
			t.Fatalf("provider error body must yield no entitlement signal, got %v for %q", got, b)
		}
	}
}

// key-info payloads under "info.models" produce the entitled set; a wildcard
// key ("*") means unrestricted → nil (no subset to enforce).
func TestEntitledModelsFromKeyInfo(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"info.models", `{"info":{"models":["Azure/gpt-4o","aws/claude-opus-4-7"]}}`,
			[]string{"Azure/gpt-4o", "aws/claude-opus-4-7"}},
		{"top-level models", `{"models":["Azure/gpt-4o"]}`, []string{"Azure/gpt-4o"}},
		{"wildcard is unrestricted", `{"info":{"models":["*"]}}`, nil},
		{"all-proxy-models unrestricted", `{"info":{"models":["all-proxy-models"]}}`, nil},
		{"empty", `{"info":{"models":[]}}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := entitledModelsFromKeyInfo([]byte(tc.body))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// probeEntitledModels tries /key/info then /user/info and returns the first
// non-wildcard set.
func TestProbeEntitledModels_KeyInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == entitlementProbePathKeyInfo {
			if got := r.Header.Get("Authorization"); got != "Bearer sk-team" {
				t.Errorf("missing/incorrect auth header: %q", got)
			}
			fmt.Fprint(w, `{"info":{"models":["Azure/gpt-4o","Azure/gpt-4.1"]}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	got := probeEntitledModels(srv.URL, "sk-team", nil)
	want := []string{"Azure/gpt-4o", "Azure/gpt-4.1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A gateway with no key-info endpoint yields nil (the 403-parse path takes
// over).
func TestProbeEntitledModels_NoKeyInfoEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	if got := probeEntitledModels(srv.URL, "sk-team", nil); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// The store round-trips per endpoint, normalizes trailing slashes, and keeps
// SERVING a stale entry (stale-while-revalidate) so the filtered model list
// never flaps back to the full catalog for an unchanged key — it only flags
// staleness so the caller can refresh in the background.
func TestEntitlementStore_SetGetStale(t *testing.T) {
	s := newEntitlementStore()
	s.set("https://gw.example.com/", "sk-team", []string{"Azure/gpt-4o"}, "403")

	models, source, ok, stale := s.get("https://gw.example.com") // no trailing slash
	if !ok || stale || source != "403" || !reflect.DeepEqual(models, []string{"Azure/gpt-4o"}) {
		t.Fatalf("get = %v %q known=%v stale=%v", models, source, ok, stale)
	}

	// Setting an empty slice clears the entry (unrestricted key).
	s.set("https://gw.example.com", "sk-team", nil, "key-info")
	if _, _, ok, _ := s.get("https://gw.example.com"); ok {
		t.Fatalf("expected cleared entry")
	}

	// Stale entries are still served (stable list) but flagged for refresh.
	s.set("https://gw.example.com", "sk-team", []string{"Azure/gpt-4o"}, "403")
	s.mu.Lock()
	e := s.byEndpt["https://gw.example.com"]
	e.at = time.Now().Add(-entitlementTTL - time.Second)
	s.byEndpt["https://gw.example.com"] = e
	s.mu.Unlock()
	models, _, ok, stale = s.get("https://gw.example.com")
	if !ok || !stale || !reflect.DeepEqual(models, []string{"Azure/gpt-4o"}) {
		t.Fatalf("stale entry must still be served and flagged: %v known=%v stale=%v", models, ok, stale)
	}
}

// A key swap invalidates the previous key's cached scope: the old entitled set
// must not be served for the new key.
func TestEntitlementStore_KeySwapInvalidates(t *testing.T) {
	s := newEntitlementStore()
	s.set("https://gw.example.com", "sk-old", []string{"Azure/gpt-4o"}, "key-info")

	// Same key re-registered: entry survives.
	s.rememberProbe("https://gw.example.com", "sk-old", "")
	if _, _, ok, _ := s.get("https://gw.example.com"); !ok {
		t.Fatalf("same-key rememberProbe must keep the entry")
	}

	// New key: old scope dropped.
	s.rememberProbe("https://gw.example.com", "sk-new", "")
	if _, _, ok, _ := s.get("https://gw.example.com"); ok {
		t.Fatalf("key swap must invalidate the old entitled set")
	}
}

// beginProbe/endProbe collapse concurrent probe triggers for one endpoint into
// a single in-flight probe.
func TestEntitlementStore_ProbeInFlightGuard(t *testing.T) {
	s := newEntitlementStore()
	if !s.beginProbe("https://gw.example.com") {
		t.Fatalf("first beginProbe must succeed")
	}
	if s.beginProbe("https://gw.example.com/") { // normalized to same endpoint
		t.Fatalf("second beginProbe while in flight must fail")
	}
	s.endProbe("https://gw.example.com")
	if !s.beginProbe("https://gw.example.com") {
		t.Fatalf("beginProbe after endProbe must succeed")
	}
}

// recordInferenceError caches the entitled set from a team-scope 403 but does
// NOT switch the agent's route itself — model switching is governed by the
// dashboard reconcile policy (#1848: unpinned + governor suggestion, or
// confirmed genuine unavailability, once per list change), which the filtered
// list feeds.
func TestRecordInferenceError_CachesWithoutSwitchingRoute(t *testing.T) {
	p := &GitHubProxy{
		logger:       slog.Default(),
		inference:    newInferenceRouter(),
		entitlements: newEntitlementStore(),
	}
	route := &InferenceRoute{
		Backend:  "litellm",
		Endpoint: "https://gw.example.com",
		Model:    "aws/us.claude-opus-4-7", // NOT entitled
		APIKey:   "sk-team",
	}
	p.inference.Set("scanner", route)

	body := []byte(`team not allowed to access model. This team can only access ` +
		`models=['Azure/gpt-4.1','Azure/gpt-4o']`)
	p.recordInferenceError(route, "scanner", http.StatusForbidden, body)

	// Entitled set cached.
	models, source, ok, _ := p.entitlements.get("https://gw.example.com")
	if !ok || source != "403" || !reflect.DeepEqual(models, []string{"Azure/gpt-4.1", "Azure/gpt-4o"}) {
		t.Fatalf("entitlement not cached: %v %q %v", models, source, ok)
	}

	// The route is untouched: heals go through the dashboard policy, not here.
	if got := p.inference.Get("scanner").Model; got != "aws/us.claude-opus-4-7" {
		t.Fatalf("proxy must not switch routes itself, got %q", got)
	}
}

// recordInferenceError ignores non-403 statuses (provider-side 404/400/5xx
// relayed by the gateway) and non-litellm backends.
func TestRecordInferenceError_IgnoresProviderErrorsAndOtherBackends(t *testing.T) {
	p := &GitHubProxy{
		logger:       slog.Default(),
		inference:    newInferenceRouter(),
		entitlements: newEntitlementStore(),
	}
	route := &InferenceRoute{Backend: "litellm", Endpoint: "https://gw.example.com", Model: "Azure/gpt-4o", APIKey: "sk-team"}

	// Provider-side statuses must not record entitlements, even if the body
	// happened to contain a models=[...] list.
	for _, status := range []int{http.StatusNotFound, http.StatusBadRequest, http.StatusInternalServerError} {
		p.recordInferenceError(route, "a", status,
			[]byte(`team not allowed to access model. models=['Azure/gpt-4o']`))
		if _, _, ok, _ := p.entitlements.get("https://gw.example.com"); ok {
			t.Fatalf("status %d must not record entitlements", status)
		}
	}

	// Non-litellm backends never entitlement-filter.
	vllm := &InferenceRoute{Backend: "vllm", Endpoint: "https://vllm.example.com", Model: "m"}
	p.recordInferenceError(vllm, "a", http.StatusForbidden,
		[]byte(`team not allowed to access model. models=['x']`))
	if _, _, ok, _ := p.entitlements.get("https://vllm.example.com"); ok {
		t.Fatalf("vllm 403 must not record entitlements")
	}
}
