package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	hivegithub "github.com/hivecommons/hive/pkg/github"
)

// ============================================================
// lite_enrollment.go — the enrollment AUTH path, exercised for real
//
// Every existing test stubs verifyLiteRepoAccess, so the REAL
// verifyGitHubRepoAccess — the function that decides whether an enrolling
// user's GitHub token actually has admin/maintain on the repo — had 7.1%
// coverage, and discoverLiteInstallation (which resolves the App
// installation used for the new lite spoke) had 36.4%. These are
// security-relevant decisions: a regression that returns true for a
// push-only token silently over-grants enrollment.
//
// verifyGitHubRepoAccess builds hardcoded https://api.github.com (or GHE
// /api/v3) URLs and performs the request through a client with a nil
// Transport, i.e. http.DefaultTransport. Tests swap DefaultTransport for a
// rewrite transport targeting a local httptest server — the same pattern
// pkg/dashboard uses (import_test_transport_test.go) — so no real network
// call is made and every branch is deterministic. These tests must NOT call
// t.Parallel(): the swap is process-global for the test's duration.
// ============================================================

// defaultTransportRewrite rewrites every request to target and delegates to
// the ORIGINAL transport it displaced (never http.DefaultTransport itself,
// which would recurse while the swap is in effect).
type defaultTransportRewrite struct {
	target *url.URL
	next   http.RoundTripper
}

func (rt *defaultTransportRewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = rt.target.Scheme
	req.URL.Host = rt.target.Host
	return rt.next.RoundTrip(req)
}

// swapDefaultTransport reroutes ALL requests made through
// http.DefaultTransport to srv for the duration of one test.
func swapDefaultTransport(t *testing.T, srv *httptest.Server) {
	t.Helper()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	prev := http.DefaultTransport
	http.DefaultTransport = &defaultTransportRewrite{target: target, next: prev}
	t.Cleanup(func() { http.DefaultTransport = prev })
}

// repoAccessServer records the last request and answers with the given
// status and body.
func repoAccessServer(t *testing.T, status int, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	var got http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r.Clone(r.Context())
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestVerifyGitHubRepoAccess_PermissionOutcomes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"admin grants access", `{"permissions":{"admin":true,"maintain":false}}`, true},
		{"maintain grants access", `{"permissions":{"admin":false,"maintain":true}}`, true},
		{"push-only is NOT enough", `{"permissions":{"admin":false,"maintain":false,"push":true}}`, false},
		{"no permissions object", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := repoAccessServer(t, http.StatusOK, tc.body)
			swapDefaultTransport(t, srv)

			ok, err := verifyGitHubRepoAccess(context.Background(), "tok-123", "", "kubestellar", "hive")
			if err != nil {
				t.Fatalf("verifyGitHubRepoAccess: %v", err)
			}
			if ok != tc.want {
				t.Errorf("access = %v, want %v", ok, tc.want)
			}
			// The request must be the public-API shape with the caller's token.
			if got.URL == nil {
				t.Fatal("no request reached the test server")
			}
			if got.URL.Path != "/repos/kubestellar/hive" {
				t.Errorf("path = %q, want /repos/kubestellar/hive", got.URL.Path)
			}
			if auth := got.Header.Get("Authorization"); auth != "Bearer tok-123" {
				t.Errorf("Authorization = %q, want the enrolling user's bearer token", auth)
			}
			if accept := got.Header.Get("Accept"); accept != "application/vnd.github+json" {
				t.Errorf("Accept = %q, want application/vnd.github+json", accept)
			}
		})
	}
}

// A GHE github_host must be checked against ITS /api/v3 base, not
// api.github.com — otherwise the permission answer comes from the wrong
// forge entirely.
func TestVerifyGitHubRepoAccess_GHEHostUsesAPIV3(t *testing.T) {
	stubPrivateURLResolver(t, "ghe.example.com")
	srv, got := repoAccessServer(t, http.StatusOK, `{"permissions":{"admin":true}}`)
	swapDefaultTransport(t, srv)

	ok, err := verifyGitHubRepoAccess(context.Background(), "tok", "ghe.example.com", "open-source", "hive")
	if err != nil {
		t.Fatalf("verifyGitHubRepoAccess: %v", err)
	}
	if !ok {
		t.Error("admin on GHE reported no access")
	}
	if got.URL == nil {
		t.Fatal("no request reached the test server")
	}
	if got.URL.Path != "/api/v3/repos/open-source/hive" {
		t.Errorf("path = %q, want the GHE /api/v3 prefix", got.URL.Path)
	}
}

// 401/403/404 are all "this token does not have access", not transport
// failures: the caller distinguishes (false, nil) from (false, err) to give
// the enrolling user the right message.
func TestVerifyGitHubRepoAccess_DeniedStatusesAreNotErrors(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprintf("HTTP %d", status), func(t *testing.T) {
			srv, _ := repoAccessServer(t, status, `{"message":"nope"}`)
			swapDefaultTransport(t, srv)

			ok, err := verifyGitHubRepoAccess(context.Background(), "tok", "", "o", "r")
			if err != nil {
				t.Fatalf("HTTP %d must be (false, nil), got err: %v", status, err)
			}
			if ok {
				t.Errorf("HTTP %d reported access", status)
			}
		})
	}
}

func TestVerifyGitHubRepoAccess_UnexpectedStatusIsAnError(t *testing.T) {
	srv, _ := repoAccessServer(t, http.StatusBadGateway, "upstream sad")
	swapDefaultTransport(t, srv)

	ok, err := verifyGitHubRepoAccess(context.Background(), "tok", "", "o", "r")
	if err == nil {
		t.Fatal("HTTP 502 must surface as an error, got nil")
	}
	if ok {
		t.Error("failed check reported access")
	}
	if !strings.Contains(err.Error(), "HTTP 502") || !strings.Contains(err.Error(), "upstream sad") {
		t.Errorf("error %q must name the status and body snippet", err)
	}
}

func TestVerifyGitHubRepoAccess_MalformedJSONIsAnError(t *testing.T) {
	srv, _ := repoAccessServer(t, http.StatusOK, `{not json`)
	swapDefaultTransport(t, srv)

	ok, err := verifyGitHubRepoAccess(context.Background(), "tok", "", "o", "r")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("malformed body must be a decode error, got %v", err)
	}
	if ok {
		t.Error("undecodable response reported access")
	}
}

func TestVerifyGitHubRepoAccess_TransportFailureIsAnError(t *testing.T) {
	// A server that is already closed: the connection is refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	swapDefaultTransport(t, srv)
	srv.Close()

	ok, err := verifyGitHubRepoAccess(context.Background(), "tok", "", "o", "r")
	if err == nil || !strings.Contains(err.Error(), "GitHub repo access check failed") {
		t.Fatalf("transport failure must be reported, got %v", err)
	}
	if ok {
		t.Error("unreachable API reported access")
	}
}

// The SSRF gate: TestVerifyGitHubRepoAccessRejectsPrivateHost
// (reach_pr_source_test.go) asserts one private host errors; this
// strengthens it — NO request may leave the process for any private host,
// including the cloud metadata endpoint.
func TestVerifyGitHubRepoAccess_PrivateHostRejectedBeforeAnyRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a request was made despite the private-host rejection")
	}))
	t.Cleanup(srv.Close)
	swapDefaultTransport(t, srv)

	for _, host := range []string{"192.168.1.9", "127.0.0.1:8080", "169.254.169.254"} {
		ok, err := verifyGitHubRepoAccess(context.Background(), "tok", host, "o", "r")
		if err == nil || !strings.Contains(err.Error(), "private/internal") {
			t.Errorf("host %q: want private/internal rejection, got %v", host, err)
		}
		if ok {
			t.Errorf("host %q: private host reported access", host)
		}
	}
}

// TestValidateLiteGitHubHost (lite_enrollment_branches_test.go) covers the
// empty/public/private-literal cases; this covers the two resolution-driven
// branches it leaves untested: a genuinely public GHE hostname passes, and an
// unresolvable hostname fails CLOSED.
func TestValidateLiteGitHubHost_ResolutionBranches(t *testing.T) {
	stubPrivateURLResolver(t, "ghe.example.com")

	if err := validateLiteGitHubHost(context.Background(), "ghe.example.com"); err != nil {
		t.Errorf("public GHE host must pass: %v", err)
	}
	if err := validateLiteGitHubHost(context.Background(), "unregistered.example"); err == nil {
		t.Error("unresolvable host must fail closed (private)")
	}
}

// TestLiteRepoAccessHTTPClientRedirectPolicy (reach_pr_source_test.go)
// covers the two BLOCKING branches (private target, 11th hop). This covers
// the branch it leaves untested: a public redirect within the limit is
// allowed — the policy must not break legitimate GHE redirects.
func TestLiteRepoAccessHTTPClient_AllowsPublicRedirectWithinLimit(t *testing.T) {
	stubPrivateURLResolver(t, "public.example")
	client := liteRepoAccessHTTPClient()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://public.example/repos/o/r", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := client.CheckRedirect(req, make([]*http.Request, 3)); err != nil {
		t.Errorf("public redirect within the limit must be allowed: %v", err)
	}
}

// ============================================================
// discoverLiteInstallation — App-installation auto-discovery for a lite
// enrollment that supplied no installation_id.
// ============================================================

// liteDiscoveryHub builds a hub whose default cluster carries the given App
// key PEM and whose API URL points at apiURL.
func liteDiscoveryHub(t *testing.T, pemData, apiURL string) *HubServer {
	t.Helper()
	withTempAppKeyDir(t)
	if pemData != "" {
		if err := storeClusterAppKey(defaultClusterID, pemData); err != nil {
			t.Fatalf("store key: %v", err)
		}
	}
	return &HubServer{
		logger: appKeyTestLogger(),
		clusters: map[string]ClusterConfig{
			defaultClusterID: {
				ID:            defaultClusterID,
				GitHubAppID:   testGitHubComAppID,
				GitHubAppSlug: "kubestellar-hive",
				GitHubAPIURL:  apiURL,
			},
		},
	}
}

// TestDiscoverLiteInstallationRequiresAppIdentity covers the nil-identity
// case; this covers the OTHER half of the same guard — the cluster names an
// App (identity exists) but no key was ever uploaded (PrivateKey empty).
func TestDiscoverLiteInstallation_AppWithoutKey(t *testing.T) {
	s := liteDiscoveryHub(t, "", "")
	if _, err := s.discoverLiteInstallation(context.Background(), "some-org"); err == nil ||
		!strings.Contains(err.Error(), "installation_id is required") {
		t.Fatalf("keyless cluster: want the installation_id-required error, got %v", err)
	}
}

func TestDiscoverLiteInstallation_UnusableKey(t *testing.T) {
	// An EC key fingerprints fine (so the identity carries it) but a GitHub
	// App JWT needs RSA — NewAppAuthFromPEM must reject it and the caller
	// must hear "not usable", not a misleading not-found.
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		t.Fatalf("marshal EC key: %v", err)
	}
	ecPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))

	s := liteDiscoveryHub(t, ecPEM, "")
	if _, err := s.discoverLiteInstallation(context.Background(), "some-org"); err == nil ||
		!strings.Contains(err.Error(), "not usable for discovery") {
		t.Fatalf("want the unusable-key error, got %v", err)
	}
}

func TestDiscoverLiteInstallation_FoundAndNotFound(t *testing.T) {
	hivegithub.ResetInstallationDiscoveryCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/orgs/lite-org/"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id":      424242,
				"account": map[string]any{"login": "lite-org"},
			})
		case r.URL.Path == "/app/installations":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(srv.Close)

	s := liteDiscoveryHub(t, testAppKeyPEM(t), srv.URL)

	id, err := s.discoverLiteInstallation(context.Background(), "lite-org")
	if err != nil {
		t.Fatalf("discoverLiteInstallation: %v", err)
	}
	if id != 424242 {
		t.Errorf("installation id = %d, want 424242", id)
	}

	if _, err := s.discoverLiteInstallation(context.Background(), "org-without-install"); err == nil ||
		!strings.Contains(err.Error(), "GitHub App installation not found for org-without-install") {
		t.Fatalf("want the not-found error naming the org, got %v", err)
	}
}
