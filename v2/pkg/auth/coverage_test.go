package auth

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// --- registry helpers ---

func TestNewRegistry_OrderAndNilSkip(t *testing.T) {
	a := &Provider{Name: "github", DisplayName: "GitHub"}
	b := &Provider{Name: "Google", DisplayName: "Google", IsOIDC: true}
	reg := NewRegistry(a, nil, b) // nil is skipped
	if reg.Count() != 2 {
		t.Fatalf("Count = %d, want 2", reg.Count())
	}
	ps := reg.Providers()
	if len(ps) != 2 || ps[0].Name != "github" || ps[1].Name != "Google" {
		t.Errorf("order = %v", providerNames(ps))
	}
	// Get is case-insensitive on the registered name.
	if reg.Get("google") == nil {
		t.Error("Get(google) should resolve the Google provider case-insensitively")
	}
	if reg.Get("nope") != nil {
		t.Error("Get(nope) should be nil")
	}
}

func TestRegistry_NilReceiverSafe(t *testing.T) {
	var reg *Registry
	if reg.Get("x") != nil {
		t.Error("nil Registry Get must be nil")
	}
	if reg.Providers() != nil {
		t.Error("nil Registry Providers must be nil")
	}
	if reg.Count() != 0 {
		t.Error("nil Registry Count must be 0")
	}
	if len(reg.OIDCProviders()) != 0 {
		t.Error("nil Registry OIDCProviders must be empty")
	}
}

// --- claim helpers ---

func TestClaimNameOverrideAndDefault(t *testing.T) {
	p := &Provider{}
	if got := p.claimName("", "sub"); got != "sub" {
		t.Errorf("default = %q", got)
	}
	if got := p.claimName("custom_sub", "sub"); got != "custom_sub" {
		t.Errorf("override = %q", got)
	}
}

func TestStringClaim_MissingAndWrongType(t *testing.T) {
	m := jwt.MapClaims{"s": "v", "n": 42}
	if stringClaim(m, "s") != "v" {
		t.Error("string claim not read")
	}
	if stringClaim(m, "n") != "" {
		t.Error("non-string claim must yield empty")
	}
	if stringClaim(m, "absent") != "" {
		t.Error("absent claim must yield empty")
	}
}

// --- non-OIDC guardrails ---

func TestExchange_NonOIDCErrors(t *testing.T) {
	p := &Provider{Name: "github", IsOIDC: false}
	if _, err := p.Exchange(context.Background(), "code", "cb", "nonce"); err == nil {
		t.Fatal("Exchange on a non-OIDC provider must error")
	}
}

func TestAuthCodeURL_NonOIDCOmitsNonce(t *testing.T) {
	// A non-OIDC provider needs no discovery and must not add a nonce param.
	p := &Provider{
		Name: "github", IsOIDC: false,
		AuthorizeURL: "https://github.com/login/oauth/authorize",
		ClientID:     "cid", Scopes: []string{""},
	}
	u, err := p.AuthCodeURL("https://h/cb", "state1", "nonce1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "client_id=cid"; !contains(u, want) {
		t.Errorf("url %q missing %q", u, want)
	}
	if contains(u, "nonce=") {
		t.Errorf("non-OIDC url must not carry a nonce: %s", u)
	}
}

func TestAuthCodeURL_AppendsWithExistingQuery(t *testing.T) {
	// AuthorizeURL already containing a query must get "&", not "?".
	p := &Provider{
		Name: "github", IsOIDC: false,
		AuthorizeURL: "https://github.com/login/oauth/authorize?foo=bar",
		ClientID:     "cid", Scopes: []string{""},
	}
	u, err := p.AuthCodeURL("https://h/cb", "s", "")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(u, "?foo=bar&") {
		t.Errorf("expected & separator on existing query: %s", u)
	}
}

// --- discovery / JWKS error paths ---

func TestFetchDiscovery_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := fetchDiscovery(context.Background(), srv.URL); err == nil {
		t.Fatal("non-200 discovery must error")
	}
}

func TestFetchDiscovery_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()
	if _, err := fetchDiscovery(context.Background(), srv.URL); err == nil {
		t.Fatal("malformed discovery must error")
	}
}

func TestFetchDiscovery_MissingEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": "https://x"}) // no endpoints
	}))
	defer srv.Close()
	if _, err := fetchDiscovery(context.Background(), srv.URL); err == nil {
		t.Fatal("discovery missing endpoints must error")
	}
}

func TestFetchJWKS_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	if _, err := fetchJWKS(context.Background(), srv.URL); err == nil {
		t.Fatal("non-200 JWKS must error")
	}
}

func TestFetchJWKS_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{"))
	}))
	defer srv.Close()
	if _, err := fetchJWKS(context.Background(), srv.URL); err == nil {
		t.Fatal("malformed JWKS must error")
	}
}

func TestFetchJWKS_NoUsableKeys(t *testing.T) {
	// A JWKS with only a non-RSA key (or a malformed RSA key) yields no usable keys.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{
				{"kty": "EC", "kid": "ec1"},                          // skipped: not RSA
				{"kty": "RSA", "kid": "bad", "n": "!!", "e": "AQAB"}, // skipped: bad n
			},
		})
	}))
	defer srv.Close()
	if _, err := fetchJWKS(context.Background(), srv.URL); err == nil {
		t.Fatal("JWKS with no usable RSA keys must error")
	}
}

func TestRSAPublicKeyFromJWK_Errors(t *testing.T) {
	if _, err := rsaPublicKeyFromJWK(jwkKey{N: "!!bad", E: "AQAB"}); err == nil {
		t.Error("bad modulus must error")
	}
	if _, err := rsaPublicKeyFromJWK(jwkKey{N: "AQAB", E: "!!bad"}); err == nil {
		t.Error("bad exponent must error")
	}
	if _, err := rsaPublicKeyFromJWK(jwkKey{N: "AQAB", E: "AQ"}); err == nil {
		// E="AQ" -> 1, which is < 2 -> invalid exponent
		t.Error("exponent < 2 must error")
	}
}

func TestRefreshJWKS_NoURL(t *testing.T) {
	p := &Provider{Name: "google", IsOIDC: true}
	if err := p.refreshJWKS(context.Background()); err == nil {
		t.Fatal("refreshJWKS with no URL must error")
	}
}

func TestEnsureDiscovered_NoIssuer(t *testing.T) {
	p := &Provider{Name: "google", IsOIDC: true} // no Issuer
	if err := p.ensureDiscovered(context.Background()); err == nil {
		t.Fatal("OIDC provider with no issuer must error on discovery")
	}
}

func TestEnsureDiscovered_NonOIDCNoop(t *testing.T) {
	p := &Provider{Name: "github", IsOIDC: false}
	if err := p.ensureDiscovered(context.Background()); err != nil {
		t.Fatalf("non-OIDC ensureDiscovered must be a no-op, got %v", err)
	}
}

func TestKeyForKID_ThrottledUnknown(t *testing.T) {
	// A provider whose JWKS was just refreshed (jwksSetAt=now) but does not carry
	// the requested kid must REFUSE rather than immediately re-fetch — the
	// throttle that stops an unknown-kid flood from hammering the JWKS endpoint.
	p := &Provider{Name: "google", IsOIDC: true}
	p.jwks = map[string]*rsa.PublicKey{"known": {}}
	p.jwksSetAt = nowUTC() // fresh → within jwksRefreshMinInterval
	if _, err := p.keyForKID(context.Background(), "unknown-kid"); err == nil {
		t.Fatal("keyForKID must refuse an unknown kid right after a refresh (throttle)")
	}
}

func TestFetchIDToken_ErrorPaths(t *testing.T) {
	// non-200 from the token endpoint
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv1.Close()
	p1 := &Provider{Name: "google", IsOIDC: true, TokenURL: srv1.URL, ClientID: "c"}
	if _, err := p1.fetchIDToken(context.Background(), "code", "cb"); err == nil {
		t.Error("non-200 token endpoint must error")
	}

	// token endpoint returns an OAuth error object
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "bad code"})
	}))
	defer srv2.Close()
	p2 := &Provider{Name: "google", IsOIDC: true, TokenURL: srv2.URL, ClientID: "c"}
	if _, err := p2.fetchIDToken(context.Background(), "code", "cb"); err == nil {
		t.Error("token endpoint error object must error")
	}

	// 200 but no id_token in the response
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"access_token": "at-only"})
	}))
	defer srv3.Close()
	p3 := &Provider{Name: "google", IsOIDC: true, TokenURL: srv3.URL, ClientID: "c"}
	if _, err := p3.fetchIDToken(context.Background(), "code", "cb"); err == nil {
		t.Error("missing id_token must error")
	}

	// malformed JSON body
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("nope"))
	}))
	defer srv4.Close()
	p4 := &Provider{Name: "google", IsOIDC: true, TokenURL: srv4.URL, ClientID: "c"}
	if _, err := p4.fetchIDToken(context.Background(), "code", "cb"); err == nil {
		t.Error("malformed token response must error")
	}
}

func TestKeyForKID_RefreshFindsRotatedKey(t *testing.T) {
	// keyForKID with a stale JWKS (jwksSetAt zero → older than the throttle) must
	// re-fetch and resolve a rotated kid it didn't previously hold.
	o := newOIDCTestServer(t)
	defer o.close()
	p := &Provider{Name: "google", IsOIDC: true, Issuer: o.issuer, JWKSURL: o.issuer + "/jwks", ClientID: "c"}
	// jwks empty and jwksSetAt zero-value → treated as stale, triggers a refresh.
	key, err := p.keyForKID(context.Background(), o.kid)
	if err != nil {
		t.Fatalf("keyForKID should resolve after refresh: %v", err)
	}
	if key == nil {
		t.Fatal("resolved key is nil")
	}
	// A second call hits the cache (kid now present).
	if _, err := p.keyForKID(context.Background(), o.kid); err != nil {
		t.Fatalf("cached keyForKID: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
