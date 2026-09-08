package hub

// Branch coverage for three previously-untested seams in pkg/hub:
//
//   - startProviderLogin's OIDC arm (oauth.go): the replay-nonce cookie and the
//     discovery-driven authorize redirect, plus the 502 when the provider's
//     authorize URL cannot be built.
//   - handleContributeProxy's post-selection branches (server.go): the 500 on an
//     unparseable DashboardURL and the reverse-proxy dispatch itself.
//   - discoverSpokeServedHost (heartbeat.go): the uncached in-cluster read —
//     client-build failure, the Ingress-host answer, and the Route fallback.
//
// Everything here is hermetic: DNS goes through the privateURLResolver seam,
// the Kubernetes API is a local TLS httptest server whose certificate is the
// test's ca.crt, and the OIDC provider is the package's fakeOIDCProvider.

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
		return []string{"203.0.113.10"}, nil
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

// ============================================================
// discoverSpokeServedHost — the uncached in-cluster read
// ============================================================

// pointDiscoveryAtFakeCluster wires the in-cluster seams (env-var funcs,
// serviceaccount dir) at a TLS httptest server standing in for the Kubernetes
// API, writing the server's own certificate as the serviceaccount CA so
// inClusterHTTPClient's verification passes. Restores everything on cleanup.
func pointDiscoveryAtFakeCluster(t *testing.T, api *fakeKubeAPI) {
	t.Helper()
	srv := httptest.NewTLSServer(api.handler())
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing fake API URL: %v", err)
	}

	dir := t.TempDir()
	writeServiceAccountFile(t, dir, "token", "test-sa-token")
	writeServiceAccountFile(t, dir, "namespace", "hive-hosted-test")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	writeServiceAccountFile(t, dir, "ca.crt", string(caPEM))

	// Sanity: the PEM we just wrote really is a certificate the client can load.
	if _, err := x509.ParseCertificate(srv.Certificate().Raw); err != nil {
		t.Fatalf("fake API certificate did not round-trip: %v", err)
	}

	overrideInClusterSeams(t, dir, u.Hostname(), u.Port())
}

func writeServiceAccountFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// overrideInClusterSeams swaps the package-level in-cluster discovery seams and
// restores them on cleanup. Not for parallel tests.
func overrideInClusterSeams(t *testing.T, dir, host, port string) {
	t.Helper()
	oldHost, oldPort, oldDir := kubernetesAPIHost, kubernetesAPIPort, serviceAccountDir
	kubernetesAPIHost = func() string { return host }
	kubernetesAPIPort = func() string { return port }
	serviceAccountDir = dir
	t.Cleanup(func() {
		kubernetesAPIHost = oldHost
		kubernetesAPIPort = oldPort
		serviceAccountDir = oldDir
	})
}

// A serviceaccount with token and namespace but NO ca.crt means the HTTP client
// cannot be built (the CA is required — fail closed, never an unverified
// probe). Discovery must answer "" so callers fall back rather than trust a
// fabricated host.
func TestDiscoverSpokeServedHostClientBuildFailureReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	writeServiceAccountFile(t, dir, "token", "test-sa-token")
	writeServiceAccountFile(t, dir, "namespace", "hive-hosted-test")
	// deliberately no ca.crt
	overrideInClusterSeams(t, dir, "10.96.0.1", "443")

	if got := discoverSpokeServedHost(t.Context()); got != "" {
		t.Errorf("discoverSpokeServedHost = %q, want \"\" when the k8s client cannot be built", got)
	}
}

// End to end through the real config, client, and TLS handshake: an Ingress
// rule host is the answer, and the Route API must not even be needed.
func TestDiscoverSpokeServedHostReturnsIngressHost(t *testing.T) {
	want := "hosted-spoke." + servedHostOKEDomain
	pointDiscoveryAtFakeCluster(t, &fakeKubeAPI{
		ingressHosts: []string{want},
		// A Route API answering 404 (vanilla Kubernetes) must not matter when the
		// Ingress already answered.
		routeStatus: http.StatusNotFound,
	})

	if got := discoverSpokeServedHost(t.Context()); got != want {
		t.Errorf("discoverSpokeServedHost = %q, want the Ingress host %q", got, want)
	}
}

// With no Ingress hosts, discovery must fall through to the OpenShift Route —
// the dashboard Route selected by name, per routeServedHost's contract.
func TestDiscoverSpokeServedHostFallsBackToRoute(t *testing.T) {
	want := "hosted-spoke." + servedHostOpenShiftDomain
	pointDiscoveryAtFakeCluster(t, &fakeKubeAPI{
		ingressHosts: nil,
		routes:       []fakeRoute{{name: routeBaseDashboard, host: want}},
	})

	if got := discoverSpokeServedHost(t.Context()); got != want {
		t.Errorf("discoverSpokeServedHost = %q, want the Route host %q", got, want)
	}
}
