package mint

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tests for the Kubernetes ServiceAccount caller backend (#3915).
//
// The finding's remaining gap was that /mint could only prove "the caller holds
// a secret". These tests are about what the mint now proves instead, and — more
// importantly — about the ways this backend could LOOK like it proves it while
// not doing so. Each of the following would leave the mint no better off than
// the shared secret, and each has a test:
//
//   - accepting a token the API server authenticated but did NOT validate for
//     the mint's audience (the API-server-token replay),
//   - accepting a username that is not a ServiceAccount,
//   - forwarding the caller's Authorization header (the shared secret) to the
//     API server as the token to review,
//   - failing OPEN when the API server is unreachable or refuses the mint's own
//     TokenReview call.

const (
	testAudience = "hive-mint"
	testSAName   = "system:serviceaccount:hive:hive-spoke"
)

// fakeAPIServer stands in for the Kubernetes API server. It records what the
// mint sent so the tests can assert on the request, not only the outcome.
type fakeAPIServer struct {
	srv *httptest.Server

	// captured request
	gotPath          string
	gotAuthorization string
	gotSpec          tokenReviewRequestSpec

	// canned response
	status     tokenReviewStatus
	httpStatus int
}

func newFakeAPIServer(t *testing.T, status tokenReviewStatus) *fakeAPIServer {
	t.Helper()
	f := &fakeAPIServer{status: status, httpStatus: http.StatusCreated}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.gotPath = r.URL.Path
		f.gotAuthorization = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var req tokenReviewRequest
		_ = json.Unmarshal(body, &req)
		f.gotSpec = req.Spec
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.httpStatus)
		_ = json.NewEncoder(w).Encode(tokenReviewResponse{Status: f.status})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// authenticatorFor wires a TokenReviewAuthenticator at the fake API server.
func authenticatorFor(t *testing.T, f *fakeAPIServer) *TokenReviewAuthenticator {
	t.Helper()
	a, err := NewTokenReviewAuthenticator(f.srv.URL, testAudience, nil,
		WithReviewHTTPClient(f.srv.Client()),
		WithReviewerToken(func() (string, error) { return "mint-own-token", nil }),
	)
	if err != nil {
		t.Fatalf("NewTokenReviewAuthenticator: %v", err)
	}
	return a
}

func requestWithSAToken(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, MintPath, strings.NewReader("{}"))
	if token != "" {
		r.Header.Set(ServiceAccountTokenHeader, token)
	}
	return r
}

func authenticatedStatus() tokenReviewStatus {
	return tokenReviewStatus{
		Authenticated: true,
		User:          tokenReviewUser{Username: testSAName, UID: "uid-1"},
		Audiences:     []string{testAudience},
	}
}

// --- the happy path, and what it establishes ---------------------------------

func TestTokenReviewYieldsTheServiceAccountIdentity(t *testing.T) {
	f := newFakeAPIServer(t, authenticatedStatus())
	a := authenticatorFor(t, f)

	id, err := a.Authenticate(requestWithSAToken("caller-projected-token"))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Kind != KindServiceAccount {
		t.Errorf("Kind = %q, want %q", id.Kind, KindServiceAccount)
	}
	if id.Name != testSAName {
		t.Errorf("Name = %q, want %q", id.Name, testSAName)
	}
	// The whole point of the finding: this is a name, not "someone held a
	// secret". It must be distinguishable from the shared-secret identity.
	if id.Name == SharedSecretIdentityName {
		t.Error("TokenReview produced the indistinguishable shared-secret identity")
	}
	if id.String() != KindServiceAccount+":"+testSAName {
		t.Errorf("audit rendering = %q", id.String())
	}
}

func TestTokenReviewSendsAnAudienceScopedReview(t *testing.T) {
	f := newFakeAPIServer(t, authenticatedStatus())
	a := authenticatorFor(t, f)

	if _, err := a.Authenticate(requestWithSAToken("caller-projected-token")); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if f.gotPath != tokenReviewPath {
		t.Errorf("posted to %q, want %q", f.gotPath, tokenReviewPath)
	}
	if f.gotSpec.Token != "caller-projected-token" {
		t.Errorf("reviewed token = %q, want the caller's token", f.gotSpec.Token)
	}
	if len(f.gotSpec.Audiences) != 1 || f.gotSpec.Audiences[0] != testAudience {
		t.Errorf("review audiences = %v, want [%s] — an unscoped review would accept an API-server token", f.gotSpec.Audiences, testAudience)
	}
	// The mint authenticates ITSELF with its own token, never the caller's.
	if f.gotAuthorization != authScheme+"mint-own-token" {
		t.Errorf("reviewer Authorization = %q, want the mint's own token", f.gotAuthorization)
	}
}

func TestTokenReviewAcceptsABearerPrefixedHeader(t *testing.T) {
	f := newFakeAPIServer(t, authenticatedStatus())
	a := authenticatorFor(t, f)

	if _, err := a.Authenticate(requestWithSAToken("Bearer caller-projected-token")); err != nil {
		t.Fatalf("Authenticate with Bearer prefix: %v", err)
	}
	if f.gotSpec.Token != "caller-projected-token" {
		t.Errorf("reviewed token = %q — the Bearer prefix should be stripped, not reviewed", f.gotSpec.Token)
	}
}

// --- the ways this could be wrong --------------------------------------------

// TestTokenReviewRefusesUnvalidatedAudience is the most important test here.
//
// An API server whose authenticators do not implement audience validation
// answers `authenticated: true` with the audiences field absent. A backend that
// checked only `authenticated` would accept a token minted for the API SERVER —
// which every pod already has at a well-known path — and audience scoping would
// be decorative.
func TestTokenReviewRefusesUnvalidatedAudience(t *testing.T) {
	cases := []struct {
		name      string
		audiences []string
	}{
		{"absent audiences (API server did not validate)", nil},
		{"empty audiences (no intersection)", []string{}},
		{"a different audience", []string{"https://kubernetes.default.svc"}},
		{"our audience only as a prefix", []string{testAudience + "-other"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := authenticatedStatus()
			st.Audiences = tc.audiences
			f := newFakeAPIServer(t, st)
			a := authenticatorFor(t, f)

			if _, err := a.Authenticate(requestWithSAToken("some-token")); err == nil {
				t.Fatal("accepted a token the API server did not validate for the mint's audience")
			}
		})
	}

	// And the case that must still pass: our audience among several.
	st := authenticatedStatus()
	st.Audiences = []string{"other", testAudience}
	f := newFakeAPIServer(t, st)
	if _, err := authenticatorFor(t, f).Authenticate(requestWithSAToken("some-token")); err != nil {
		t.Fatalf("refused a token whose returned audiences include ours: %v", err)
	}
}

func TestTokenReviewRefusesNonServiceAccountUsernames(t *testing.T) {
	cases := []string{
		"alice",                       // a human with a kubeconfig
		"system:node:worker-1",        // a kubelet
		"system:anonymous",            //
		"system:serviceaccount:",      // malformed: no namespace or name
		"system:serviceaccount:hive",  // malformed: no name
		"system:serviceaccount::name", // malformed: empty namespace
		"system:serviceaccount:hive:", // malformed: empty name
		"",                            // nothing at all
	}
	for _, username := range cases {
		t.Run(username, func(t *testing.T) {
			st := authenticatedStatus()
			st.User.Username = username
			f := newFakeAPIServer(t, st)
			if _, err := authenticatorFor(t, f).Authenticate(requestWithSAToken("some-token")); err == nil {
				t.Fatalf("accepted username %q as a ServiceAccount identity", username)
			}
		})
	}
}

func TestTokenReviewRefusesUnauthenticatedAndErrored(t *testing.T) {
	st := authenticatedStatus()
	st.Authenticated = false
	f := newFakeAPIServer(t, st)
	if _, err := authenticatorFor(t, f).Authenticate(requestWithSAToken("bad")); err == nil {
		t.Error("accepted a token the API server said was not authenticated")
	}

	// authenticated:true WITH an error string set is contradictory; refuse.
	st = authenticatedStatus()
	st.Error = "token expired"
	f = newFakeAPIServer(t, st)
	if _, err := authenticatorFor(t, f).Authenticate(requestWithSAToken("expired")); err == nil {
		t.Error("accepted a review that carried an error string")
	}
}

func TestTokenReviewRefusesAMissingHeader(t *testing.T) {
	f := newFakeAPIServer(t, authenticatedStatus())
	a := authenticatorFor(t, f)

	if _, err := a.Authenticate(requestWithSAToken("")); err == nil {
		t.Error("accepted a request with no ServiceAccount token")
	}
	if f.gotPath != "" {
		t.Error("called the API server for a request that carried no token at all")
	}
}

// TestTokenReviewNeverReviewsTheAuthorizationHeader guards a leak that would be
// invisible in behaviour: in a dual-accept deployment Authorization carries the
// SHARED SECRET, and reviewing it would write the mint's secret into the
// Kubernetes audit log.
func TestTokenReviewNeverReviewsTheAuthorizationHeader(t *testing.T) {
	f := newFakeAPIServer(t, authenticatedStatus())
	a := authenticatorFor(t, f)

	r := requestWithSAToken("caller-projected-token")
	r.Header.Set("Authorization", authScheme+"the-shared-secret")
	if _, err := a.Authenticate(r); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if strings.Contains(f.gotSpec.Token, "the-shared-secret") {
		t.Fatal("the caller's Authorization header was forwarded to the API server as a token to review")
	}
	if f.gotAuthorization == authScheme+"the-shared-secret" {
		t.Fatal("the caller's Authorization header was replayed as the mint's own reviewer credential")
	}
}

// TestTokenReviewFailsClosed: every infrastructure failure refuses the caller.
// A TokenReview that failed open would be strictly worse than the shared secret
// it replaces, because operators would believe identity was being checked.
func TestTokenReviewFailsClosed(t *testing.T) {
	t.Run("API server refuses the mint's own review call", func(t *testing.T) {
		f := newFakeAPIServer(t, authenticatedStatus())
		f.httpStatus = http.StatusForbidden // mint's SA lacks create on tokenreviews
		if _, err := authenticatorFor(t, f).Authenticate(requestWithSAToken("good-token")); err == nil {
			t.Error("authenticated a caller although the review call itself was refused")
		}
	})

	t.Run("API server unreachable", func(t *testing.T) {
		f := newFakeAPIServer(t, authenticatedStatus())
		a := authenticatorFor(t, f)
		f.srv.Close() // dial will fail
		if _, err := a.Authenticate(requestWithSAToken("good-token")); err == nil {
			t.Error("authenticated a caller with the API server down")
		}
	})

	t.Run("reviewer token unreadable", func(t *testing.T) {
		f := newFakeAPIServer(t, authenticatedStatus())
		a, err := NewTokenReviewAuthenticator(f.srv.URL, testAudience, nil,
			WithReviewHTTPClient(f.srv.Client()),
			WithReviewerToken(func() (string, error) { return "", nil }),
		)
		if err != nil {
			t.Fatalf("NewTokenReviewAuthenticator: %v", err)
		}
		if _, err := a.Authenticate(requestWithSAToken("good-token")); err == nil {
			t.Error("authenticated a caller although the mint has no credential to review with")
		}
	})

	t.Run("garbage response body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()
		a, err := NewTokenReviewAuthenticator(srv.URL, testAudience, nil,
			WithReviewHTTPClient(srv.Client()),
			WithReviewerToken(func() (string, error) { return "t", nil }),
		)
		if err != nil {
			t.Fatalf("NewTokenReviewAuthenticator: %v", err)
		}
		if _, err := a.Authenticate(requestWithSAToken("good-token")); err == nil {
			t.Error("authenticated a caller from an unparseable review response")
		}
	})
}

// --- construction ------------------------------------------------------------

func TestNewTokenReviewAuthenticatorValidatesItsInputs(t *testing.T) {
	if _, err := NewTokenReviewAuthenticator("", testAudience, nil); err == nil {
		t.Error("accepted an empty API server URL")
	}
	// An empty audience is the silent downgrade this backend exists to prevent:
	// the review would carry no audience and verifyAudience nothing to check.
	if _, err := NewTokenReviewAuthenticator("https://api", "", nil); err == nil {
		t.Error("accepted an empty audience — an unscoped review accepts API-server tokens")
	}
	if _, err := NewTokenReviewAuthenticator("https://api", testAudience, []byte("not a certificate")); err == nil {
		t.Error("accepted a CA bundle with no usable certificate")
	}
	a, err := NewTokenReviewAuthenticator("https://api/", testAudience, nil)
	if err != nil {
		t.Fatalf("NewTokenReviewAuthenticator: %v", err)
	}
	if a.apiURL != "https://api" {
		t.Errorf("trailing slash not trimmed: %q", a.apiURL)
	}
	if a.Name() != KindServiceAccount {
		t.Errorf("Name() = %q", a.Name())
	}
	// Nil options must not be able to clear the verified defaults.
	before := a.client
	WithReviewHTTPClient(nil)(a)
	WithReviewerToken(nil)(a)
	if a.client != before || a.reviewerToken == nil {
		t.Error("a nil option cleared a default it should have left alone")
	}
}

func TestNewInClusterTokenReviewAuthenticatorRefusesOutsideACluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if _, err := NewInClusterTokenReviewAuthenticator(testAudience); err == nil {
		t.Error("built an in-cluster authenticator outside a cluster — it must fail rather than degrade")
	}
}

// --- dual-accept --------------------------------------------------------------

func TestMultiAuthenticatorPrefersTheRealIdentity(t *testing.T) {
	f := newFakeAPIServer(t, authenticatedStatus())
	tr := authenticatorFor(t, f)
	secret, err := NewSharedSecretAuthenticator(testSecret)
	if err != nil {
		t.Fatalf("NewSharedSecretAuthenticator: %v", err)
	}
	multi, err := NewMultiAuthenticator(tr, secret)
	if err != nil {
		t.Fatalf("NewMultiAuthenticator: %v", err)
	}

	if got, want := multi.Name(), KindServiceAccount+"+"+KindSharedSecret; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}

	// A caller presenting BOTH must be recorded under its real identity — an
	// audit log that still says "any-holder" cannot show a migration finishing.
	r := requestWithSAToken("caller-projected-token")
	r.Header.Set("Authorization", authScheme+testSecret)
	id, err := multi.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Kind != KindServiceAccount {
		t.Errorf("a caller with both credentials was recorded as %q, want the ServiceAccount", id.Kind)
	}

	// A legacy caller with only the secret still works — that is the whole
	// point of dual-accept.
	legacy := httptest.NewRequest(http.MethodPost, MintPath, strings.NewReader("{}"))
	legacy.Header.Set("Authorization", authScheme+testSecret)
	id, err = multi.Authenticate(legacy)
	if err != nil {
		t.Fatalf("legacy caller refused: %v", err)
	}
	if id.Kind != KindSharedSecret {
		t.Errorf("legacy caller recorded as %q", id.Kind)
	}

	// A caller with neither is refused.
	if _, err := multi.Authenticate(httptest.NewRequest(http.MethodPost, MintPath, strings.NewReader("{}"))); err == nil {
		t.Error("authenticated a caller presenting no credential at all")
	}
}

func TestNewMultiAuthenticatorNeedsABackend(t *testing.T) {
	if _, err := NewMultiAuthenticator(); err == nil {
		t.Error("built an authenticator with no backends — it would 401 everything and read as an outage")
	}
	if _, err := NewMultiAuthenticator(nil, nil); err == nil {
		t.Error("built an authenticator from nothing but nils")
	}
	secret, _ := NewSharedSecretAuthenticator(testSecret)
	m, err := NewMultiAuthenticator(nil, secret, nil)
	if err != nil {
		t.Fatalf("nil backends should be dropped, not fatal: %v", err)
	}
	if m.Name() != KindSharedSecret {
		t.Errorf("Name() = %q, want the surviving backend", m.Name())
	}
}

// --- end to end through the server -------------------------------------------

// TestServerMintsUnderServiceAccountEntitlement is the payoff: an entitlement
// map keyed on a real ServiceAccount name, enforced deny-by-default. Under the
// shared secret this could not be written down at all, because there was only
// ever one identity to key on.
func TestServerMintsUnderServiceAccountEntitlement(t *testing.T) {
	f := newFakeAPIServer(t, authenticatedStatus())
	m, _ := newTestMinter(t)
	srv, err := NewServer(m, testSecret, nil,
		WithAuthenticator(authenticatorFor(t, f)),
		WithEntitlements(Entitlements{
			testSAName: {
				Subjects:  []string{testSub},
				Audiences: []string{testAud},
				Scopes:    []string{"registry:pull"},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	post := func(req MintRequest) int {
		body, _ := json.Marshal(req)
		r := httptest.NewRequest(http.MethodPost, MintPath, strings.NewReader(string(body)))
		r.Header.Set(ServiceAccountTokenHeader, "caller-projected-token")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w.Code
	}

	if got := post(MintRequest{Subject: testSub, Audience: testAud, Scopes: []string{"registry:pull"}}); got != http.StatusOK {
		t.Errorf("entitled mint returned %d, want 200", got)
	}
	// Outside the entitlement: authenticated, but not permitted. 403, not 401.
	if got := post(MintRequest{Subject: "hub-admin", Audience: testAud}); got != http.StatusForbidden {
		t.Errorf("unentitled subject returned %d, want 403", got)
	}
	if got := post(MintRequest{Subject: testSub, Audience: testAud, Scopes: []string{"registry:push"}}); got != http.StatusForbidden {
		t.Errorf("unentitled scope returned %d, want 403", got)
	}

	// A caller with the shared secret but no SA token is now refused outright:
	// the server was built with the TokenReview authenticator alone.
	body, _ := json.Marshal(MintRequest{Subject: testSub, Audience: testAud})
	r := httptest.NewRequest(http.MethodPost, MintPath, strings.NewReader(string(body)))
	r.Header.Set("Authorization", authScheme+testSecret)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("shared-secret caller returned %d against a TokenReview-only server, want 401", w.Code)
	}
}
