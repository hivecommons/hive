package dashboard

// Regression tests for audit F13: the gateway DISCOVER endpoint exfiltrated a
// stored API key to a caller-supplied endpoint and was a full SSRF.
//
// F13 is the same defect as N8 (2026-08-07) and F6 (2026-08-11). Those were
// fixed on the gateway UPSERT handler and missed on this sibling, so it survived
// three audits. These tests pin the invariant on the discover path specifically:
//
//	a credential the caller did not supply must never be sent to an endpoint the
//	caller chose.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// storedKeyGateway registers a gateway named "stored" whose key lives in a file
// (the production shape — hive.yaml records the PATH, never the value) pointing
// at endpoint. Returns the key value the test should never see leave.
func storedKeyGateway(t *testing.T, deps *Dependencies, endpoint string) string {
	t.Helper()
	dir := t.TempDir()
	restore := setGatewaySecretsDirForTest(dir)
	t.Cleanup(restore)

	const secret = "sk-stored-must-not-leak"
	path := filepath.Join(dir, "gateway_stored_api_key")
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	deps.Config.Governor.Gateways = []config.GatewayConfig{{
		Name:       "stored",
		Kind:       config.GatewayKindCustom,
		Endpoint:   endpoint,
		APIKeyFile: path,
	}}
	// Positive control on the fixture itself: if resolution silently returned ""
	// every "the key did not leak" assertion below would pass for the wrong
	// reason.
	if got := deps.Config.Governor.Gateways[0].ResolveAPIKey(); got != secret {
		t.Fatalf("fixture: stored key resolved to %q, want %q — the leak assertions "+
			"would be vacuous", got, secret)
	}
	return secret
}

// treatAsPublic exempts the given httptest server URLs' host:port from the
// private-address guard for the duration of the test. httptest always binds
// 127.0.0.1, which the guard correctly rejects; without this seam the SSRF
// tests below would pass because the request was blocked as loopback rather
// than because the credential logic is right — passing for the wrong reason.
func treatAsPublic(t *testing.T, urls ...string) {
	t.Helper()
	prev := privateURLTestExemptHostPorts
	exempt := make(map[string]struct{}, len(urls))
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		exempt[strings.ToLower(u.Host)] = struct{}{}
	}
	privateURLTestExemptHostPorts = exempt
	t.Cleanup(func() { privateURLTestExemptHostPorts = prev })
}

// captureAuthServer records the Authorization header of every request it
// receives and answers with an empty OpenAI-style model list.
func captureAuthServer(t *testing.T, got *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = append(*got, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestF13_DiscoverRequiresOwnerRole pins sub-fix 1: discover drives an outbound
// request with a stored credential attached, so it must be owner-gated like the
// other gateway routes. Before the fix it had no gate at all, so a read-write
// member — or an unauthenticated caller on an open spoke — could reach it.
func TestF13_DiscoverRequiresOwnerRole(t *testing.T) {
	s, _ := apiServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/governor/gateways/discover",
		strings.NewReader(`{"endpoint":"https://public.example/v1"}`))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately NOT markOwnerRequest: a non-owner caller.
	req.Header.Set("X-Hive-Role", "member")
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("discover as non-owner = %d, want 403 — the endpoint is ungated: %s",
			rec.Code, rec.Body.String())
	}
}

// TestF13_DiscoverDoesNotSendStoredKeyToCallerEndpoint is the core exfiltration
// regression. The caller names an existing gateway but points the endpoint at a
// server they control and supplies no key. The stored key must NOT be attached.
//
// This asserts on what the outbound request actually CARRIED, not on the handler
// response — the leak happens on the wire, and a handler that returns an error
// after already sending the key has still lost it.
func TestF13_DiscoverDoesNotSendStoredKeyToCallerEndpoint(t *testing.T) {
	s, deps := apiServer(t)

	var attackerSaw []string
	attacker := captureAuthServer(t, &attackerSaw)

	// The gateway's real endpoint is somewhere else entirely.
	secret := storedKeyGateway(t, deps, "https://real-gateway.example/v1")

	treatAsPublic(t, attacker.URL)

	rec := doPost(s, "/api/config/governor/gateways/discover", map[string]interface{}{
		"name":     "stored", // an EXISTING gateway with a stored key
		"endpoint": attacker.URL,
		// no api_key: this is the whole attack — make the server supply one.
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("discover = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	for _, auth := range attackerSaw {
		if strings.Contains(auth, secret) {
			t.Fatalf("F13 EXFILTRATION: the stored gateway key was sent to a "+
				"caller-supplied endpoint (Authorization: %q)", auth)
		}
		if auth != "" {
			t.Errorf("unexpected credential sent to caller endpoint: %q", auth)
		}
	}
}

// TestF13_DiscoverUsesStoredKeyForOwnEndpoint is the POSITIVE CONTROL for the
// test above. "Never send the stored key" is trivially satisfiable by never
// sending it at all, which would silently break the legitimate feature (
// re-opening an existing gateway populates its model dropdown without retyping
// the key). When the caller names the gateway's OWN persisted endpoint — an
// address that already receives this key on every agent request — the stored key
// SHOULD still be used.
func TestF13_DiscoverUsesStoredKeyForOwnEndpoint(t *testing.T) {
	s, deps := apiServer(t)

	var upstreamSaw []string
	upstream := captureAuthServer(t, &upstreamSaw)

	// The gateway's persisted endpoint IS the server we probe.
	secret := storedKeyGateway(t, deps, upstream.URL)
	treatAsPublic(t, upstream.URL)

	rec := doPost(s, "/api/config/governor/gateways/discover", map[string]interface{}{
		"name":     "stored",
		"endpoint": upstream.URL,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("discover = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	found := false
	for _, auth := range upstreamSaw {
		if auth == "Bearer "+secret {
			found = true
		}
	}
	if !found {
		t.Errorf("stored key was NOT used for the gateway's own endpoint (saw %q) — "+
			"the fix over-blocked and broke model discovery for saved gateways", upstreamSaw)
	}
}

// TestF13_SameGatewayEndpoint covers the canonicalisation that decides whether
// the stored key may be reused. Innocent spelling differences must match;
// anything else must not.
func TestF13_SameGatewayEndpoint(t *testing.T) {
	same := []struct{ a, b, why string }{
		{"https://gw.example/v1", "https://gw.example/v1", "identical"},
		{"https://gw.example/v1/", "https://gw.example/v1", "trailing slash"},
		{"https://GW.Example/v1", "https://gw.example/v1", "host case"},
		{"HTTPS://gw.example/v1", "https://gw.example/v1", "scheme case"},
		{"https://gw.example:443/v1", "https://gw.example/v1", "explicit default port"},
		{"http://gw.example:80/v1", "http://gw.example/v1", "explicit default port (http)"},
	}
	for _, c := range same {
		if !sameGatewayEndpoint(c.a, c.b) {
			t.Errorf("sameGatewayEndpoint(%q,%q) = false, want true (%s)", c.a, c.b, c.why)
		}
	}

	diff := []struct{ a, b, why string }{
		{"https://evil.example/v1", "https://gw.example/v1", "different host"},
		{"http://gw.example/v1", "https://gw.example/v1", "different scheme"},
		{"https://gw.example:8443/v1", "https://gw.example/v1", "different port"},
		{"https://gw.example/other", "https://gw.example/v1", "different path"},
		{"https://gw.example.evil.com/v1", "https://gw.example/v1", "suffix-extended host"},
		{"https://evil@gw.example/v1", "https://gw.example/v1", "injected userinfo"},
		{"notaurl", "https://gw.example/v1", "unparseable supplied"},
		{"", "https://gw.example/v1", "empty supplied"},
		{"https://gw.example/v1", "", "empty stored (unconfigured gateway)"},
	}
	for _, c := range diff {
		if sameGatewayEndpoint(c.a, c.b) {
			t.Errorf("sameGatewayEndpoint(%q,%q) = true, want false (%s)", c.a, c.b, c.why)
		}
	}
}

// TestF13_DiscoverRejectsInternalEndpoints pins sub-fix 3: the discover probe
// must refuse internal address space so it cannot be used to reach cloud
// metadata or port-scan the pod's own loopback.
//
// Note this guard is on DISCOVER only, not on validateGatewayEndpoint — upsert
// must keep accepting in-cluster gateways (the bundled litellm proxy listens on
// 127.0.0.1:18445 and vllm/llm-d default to *.svc.cluster.local).
func TestF13_DiscoverRejectsInternalEndpoints(t *testing.T) {
	s, _ := apiServer(t)

	blocked := []struct{ endpoint, why string }{
		{"http://127.0.0.1/v1", "loopback"},
		{"http://127.0.0.1:18445/v1", "loopback, bundled litellm proxy port"},
		{"http://localhost/v1", "loopback by name"},
		{"http://169.254.169.254/v1", "cloud metadata endpoint"},
		{"http://10.0.0.5/v1", "RFC1918 10/8"},
		{"http://192.168.1.1/v1", "RFC1918 192.168/16"},
		{"http://172.16.0.1/v1", "RFC1918 172.16/12"},
		{"http://[::1]/v1", "IPv6 loopback"},
		{"http://0.0.0.0/v1", "unspecified address"},
	}
	for _, c := range blocked {
		rec := doPost(s, "/api/config/governor/gateways/discover", map[string]interface{}{
			"endpoint": c.endpoint,
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("discover %s (%s) = %d, want 400 — SSRF target reachable",
				c.endpoint, c.why, rec.Code)
		}
	}
}

// TestF13_DiscoverAcceptsPublicEndpoint is the POSITIVE CONTROL for the table
// above: without it, a guard that rejected EVERY endpoint would pass.
func TestF13_DiscoverAcceptsPublicEndpoint(t *testing.T) {
	s, _ := apiServer(t)
	allowPrivateURLHostsForTest(t, "public.example")

	rec := doPost(s, "/api/config/governor/gateways/discover", map[string]interface{}{
		"endpoint": "https://public.example/v1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("discover public endpoint = %d, want 200 — the guard over-blocks "+
			"and no gateway can be discovered at all: %s", rec.Code, rec.Body.String())
	}
}

// TestF13_UpsertStillAcceptsInClusterEndpoint guards the scope decision above.
// If someone later "hardens" validateGatewayEndpoint itself, in-cluster gateway
// configuration breaks — this test says so explicitly rather than letting a
// real deployment discover it.
func TestF13_UpsertStillAcceptsInClusterEndpoint(t *testing.T) {
	for _, ep := range []string{
		"http://127.0.0.1:18445",                                     // bundled local litellm proxy
		"http://hive-vllm-svc.hive-inference.svc.cluster.local:8000", // explicit in-cluster vLLM endpoint
		"http://10.0.0.5:4000",                                       // in-cluster LiteLLM
	} {
		if err := validateGatewayEndpoint(ep); err != nil {
			t.Errorf("validateGatewayEndpoint(%q) = %v, want nil — in-cluster gateways "+
				"are a supported configuration and must stay configurable", ep, err)
		}
	}
}

// TestF13_RedirectDropsAuthorizationCrossHost pins sub-fix 4. Go's default
// redirect policy preserves Authorization across hops it considers same-host and
// follows up to 10 of them, so an upstream answering /v1/models with a 302 could
// walk the gateway key onward. fetchModelsWithHeaders had no CheckRedirect at
// all.
func TestF13_RedirectDropsAuthorizationCrossHost(t *testing.T) {
	var finalSaw []string
	final := captureAuthServer(t, &finalSaw)

	// The first hop redirects to a DIFFERENT host.
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/v1/models", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	treatAsPublic(t, redirector.URL, final.URL)

	const secret = "sk-redirect-must-not-follow"
	if _, err := fetchModelsWithHeaders(redirector.URL, secret, nil); err != nil {
		// A refusal to follow is an equally acceptable outcome; the assertion
		// that matters is that the key never reached the second host.
		t.Logf("fetch returned error (redirect refused): %v", err)
	}

	for _, auth := range finalSaw {
		if strings.Contains(auth, secret) {
			t.Fatalf("F13: Authorization survived a cross-host redirect and leaked "+
				"the key to another host (got %q)", auth)
		}
	}
}

// TestF13_RedirectKeepsAuthorizationSameHost is the POSITIVE CONTROL for the
// redirect fix: stripping credentials on EVERY redirect would pass the test
// above while breaking upstreams that legitimately redirect in-place (trailing
// slash, path normalization).
func TestF13_RedirectKeepsAuthorizationSameHost(t *testing.T) {
	var saw []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saw = append(saw, r.Header.Get("Authorization"))
		if r.URL.Path == "/v1/models" {
			// Same-host redirect to a normalized path.
			http.Redirect(w, r, "/models-final", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"}]}`))
	}))
	t.Cleanup(srv.Close)

	treatAsPublic(t, srv.URL)

	const secret = "sk-same-host-ok"
	models, err := fetchModelsWithHeaders(srv.URL, secret, nil)
	if err != nil {
		t.Fatalf("same-host redirect should still succeed: %v", err)
	}
	if len(models) != 1 || models[0] != "m1" {
		t.Errorf("models = %v, want [m1]", models)
	}
	if len(saw) < 2 || saw[len(saw)-1] != "Bearer "+secret {
		t.Errorf("Authorization was dropped on a SAME-host redirect (saw %q) — "+
			"legitimate in-place redirects would break", saw)
	}
}

// TestF13_DiscoverErrorDoesNotEchoStoredKey checks the error path does not turn
// into a slower oracle for the same secret.
func TestF13_DiscoverErrorDoesNotEchoStoredKey(t *testing.T) {
	s, deps := apiServer(t)

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized: "+r.Header.Get("Authorization"), http.StatusUnauthorized)
	}))
	t.Cleanup(failing.Close)

	secret := storedKeyGateway(t, deps, failing.URL)
	treatAsPublic(t, failing.URL)

	rec := doPost(s, "/api/config/governor/gateways/discover", map[string]interface{}{
		"name":     "stored",
		"endpoint": failing.URL,
	})
	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("discover error response echoed the stored key: %s", body)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		if ok, _ := parsed["ok"].(bool); ok {
			t.Logf("discover reported ok on an upstream 401 (informational)")
		}
	}
}
