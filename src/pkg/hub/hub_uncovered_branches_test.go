package hub

// Branch coverage for two previously-untested seams in pkg/hub (the third
// seam of the v4 original, discoverSpokeServedHost, lives in pkg/hub/spoke on
// v5 and is covered there by spoke_boundary_coverage_test.go):
//
//   - startProviderLogin's OIDC arm (oauth.go): the replay-nonce cookie and the
//     discovery-driven authorize redirect, plus the 502 when the provider's
//     authorize URL cannot be built.
//   - handleContributeProxy's post-selection branches (server.go): the 500 on an
//     unparseable DashboardURL and the reverse-proxy dispatch itself.
//
// Everything here is hermetic: DNS goes through the privateURLResolver seam,
// and the OIDC provider is the package's fakeOIDCProvider.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/auth"
)

// ============================================================
// startProviderLogin — OIDC arm
// ============================================================

// A single OIDC provider must send the browser to the discovered authorize
// endpoint carrying BOTH nonces: the CSRF state nonce (cookie + state) and the
// OIDC replay nonce (cookie + nonce param). Missing either one reopens the
// login-CSRF (audit F11) or id_token-replay hole the two cookies exist to close.
func TestStartProviderLoginOIDCRedirectsWithBothNonces(t *testing.T) {
	f := newFakeOIDCProvider(t)
	defer f.close()

	s := &HubServer{logger: slog.Default()}
	s.authProviders = auth.NewRegistry(&auth.Provider{
		Name:        "google",
		DisplayName: "Google",
		IsOIDC:      true,
		Issuer:      f.issuer,
		ClientID:    "hub-oidc-client",
		Scopes:      []string{"openid", "email", "profile"},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/auth/login/google", nil)
	req.SetPathValue("provider", "google")
	rec := httptest.NewRecorder()
	s.handleProviderLogin(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307 (body=%s)", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location did not parse: %v", err)
	}
	if got, want := loc.Scheme+"://"+loc.Host+loc.Path, f.issuer+"/authorize"; got != want {
		t.Errorf("redirected to %q, want the discovered authorize endpoint %q", got, want)
	}

	cookies := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		cookies[c.Name] = c
	}
	state := cookies[oauthStateCookieName]
	if state == nil {
		t.Fatalf("no %s cookie set", oauthStateCookieName)
	}
	oidcNonce := cookies[oidcNonceCookieName]
	if oidcNonce == nil {
		t.Fatalf("no %s cookie set — the OIDC replay nonce was not minted", oidcNonceCookieName)
	}
	for name, c := range map[string]*http.Cookie{"state": state, "oidc nonce": oidcNonce} {
		if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
			t.Errorf("%s cookie must be HttpOnly+Secure+Lax, got HttpOnly=%v Secure=%v SameSite=%v",
				name, c.HttpOnly, c.Secure, c.SameSite)
		}
	}

	// The authorize request must echo the same nonces the cookies carry.
	q := loc.Query()
	if q.Get("nonce") != oidcNonce.Value {
		t.Errorf("authorize nonce param %q != %s cookie %q", q.Get("nonce"), oidcNonceCookieName, oidcNonce.Value)
	}
	// startProviderLogin QueryEscapes the state once and AuthCodeURL's
	// url.Values.Encode escapes it again, so one Query() decode still leaves the
	// inner escaping (the encoding contract pinned by oauth_state_encoding_test.go).
	if !strings.HasPrefix(q.Get("state"), url.QueryEscape(state.Value+oauthStateSeparator+"google"+oauthStateSeparator)) {
		t.Errorf("state param %q does not start with the escaped <state cookie>:google:", q.Get("state"))
	}
	if q.Get("client_id") != "hub-oidc-client" {
		t.Errorf("client_id = %q, want hub-oidc-client", q.Get("client_id"))
	}
}

// An OIDC provider whose authorize URL cannot be built (here: no issuer, so
// discovery is impossible) must answer 502 — "provider not reachable" — rather
// than 500 or a redirect to nowhere.
func TestStartProviderLoginOIDCDiscoveryFailureIs502(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	s.authProviders = auth.NewRegistry(&auth.Provider{
		Name:        "ibmid",
		DisplayName: "IBMid",
		IsOIDC:      true,
		// No Issuer: AuthCodeURL's ensureDiscovered fails without touching the network.
		ClientID: "hub-oidc-client",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/auth/login/ibmid", nil)
	req.SetPathValue("provider", "ibmid")
	rec := httptest.NewRecorder()
	s.handleProviderLogin(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 when the authorize URL cannot be built (body=%s)",
			rec.Code, rec.Body.String())
	}
}

// ============================================================
// handleContributeProxy — post-selection branches
// ============================================================

// stubPublicResolver makes every hostname resolve to a public address so
// findContributeHive's SSRF guard admits the fixture hive without real DNS.
func stubPublicResolver(t *testing.T) {
	t.Helper()
	orig := privateURLResolver
	privateURLResolver = func(ctx context.Context, host string) ([]string, error) {
		return []string{"93.184.216.34"}, nil
	}
	t.Cleanup(func() { privateURLResolver = orig })
}

// A hive that passes the public-URL admission check but whose DashboardURL does
// not parse must be a 500, not a proxy attempt. "http://[::1" survives
// isPrivateURL's prefix scan (the host slice stops at the first ':') yet fails
// url.Parse — exactly the shape that reaches this branch.
func TestHandleContributeProxyUnparseableDashboardURLIs500(t *testing.T) {
	stubPublicResolver(t)

	srv := newHubServerForTest(t)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "bad-url", Online: true, IsPublic: true, DashboardURL: "http://[::1", Owner: "user1"},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.handleContributeProxy(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for an unparseable hive dashboard URL", rec.Code)
	}
}

// The happy path builds a single-host reverse proxy and dispatches the request
// to the selected hive. The inbound request context is already cancelled, so
// the proxy's upstream round-trip fails immediately and hermetically — the
// handler must surface that as the reverse proxy's 502, proving the request
// reached the proxy dispatch rather than an earlier error branch.
func TestHandleContributeProxyDispatchesToSelectedHive(t *testing.T) {
	stubPublicResolver(t)

	srv := newHubServerForTest(t)
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "pub-hive", Online: true, IsPublic: true, DashboardURL: "http://hub-contribute.example.com", Owner: "user1"},
	}
	srv.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", strings.NewReader(`{}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleContributeProxy(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want the reverse proxy's 502 for a failed upstream round-trip", rec.Code)
	}
}
