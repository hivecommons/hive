package hub

import (
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/auth"
)

// addPKCECookie gives a callback request the PKCE verifier cookie a real
// browser would carry after startProviderLogin, and returns the verifier. Like
// startProviderLogin, it binds the verifier's S256 challenge into any state
// cookie already on the request, so the pair passes the pre-exchange check.
func addPKCECookie(t *testing.T, req *http.Request) string {
	t.Helper()
	v, err := oauthPKCEVerifier()
	if err != nil {
		t.Fatalf("mint PKCE verifier: %v", err)
	}
	cookies := req.Cookies()
	req.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name == oauthStateCookieName && c.Value != "" && !strings.Contains(c.Value, oauthStateChallengeSeparator) {
			c = &http.Cookie{Name: c.Name, Value: c.Value + oauthStateChallengeSeparator + pkceS256Challenge(v)}
		}
		req.AddCookie(c)
	}
	req.AddCookie(&http.Cookie{Name: oauthPKCECookieName, Value: v})
	return v
}

// pkceCookieCleared reports whether the response expires the PKCE cookie.
func pkceCookieCleared(rec *httptest.ResponseRecorder) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthPKCECookieName && c.MaxAge < 0 && c.Value == "" {
			return true
		}
	}
	return false
}

// fakeGitHubTokenEndpoint points the token and user endpoints at local
// servers and counts token-exchange requests. onToken inspects each one.
func fakeGitHubTokenEndpoint(t *testing.T, onToken func(r *http.Request)) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if onToken != nil {
			onToken(r)
		}
		_, _ = w.Write([]byte(`{"access_token":"gho_pkce"}`))
	}))
	usr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"login":"octocat","avatar_url":"https://x/a.png"}`))
	}))
	oldTok, oldUsr := defaultGHTokenURL, defaultGHUserURL
	defaultGHTokenURL, defaultGHUserURL = tok.URL, usr.URL
	t.Cleanup(func() {
		defaultGHTokenURL, defaultGHUserURL = oldTok, oldUsr
		tok.Close()
		usr.Close()
	})
	return &hits
}

func TestPKCE_VerifierShapeAndChallenge(t *testing.T) {
	v, err := oauthPKCEVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 43 || !validPKCEVerifier(v) {
		t.Fatalf("verifier %q: want a valid 43-char RFC 7636 verifier", v)
	}
	sum := sha256.Sum256([]byte(v))
	if got, want := pkceS256Challenge(v), base64.RawURLEncoding.EncodeToString(sum[:]); got != want {
		t.Fatalf("challenge = %q, want %q", got, want)
	}
	// RFC 7636 appendix B test vector.
	if got := pkceS256Challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("RFC 7636 vector challenge = %q", got)
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 42), strings.Repeat("a", 129), strings.Repeat("a", 42) + "!", strings.Repeat("a", 42) + "%"} {
		if validPKCEVerifier(bad) {
			t.Errorf("validPKCEVerifier(%q) = true, want false", bad)
		}
	}
}

func TestPKCE_GitHubAuthorizeRedirectCarriesS256Challenge(t *testing.T) {
	s := &HubServer{
		authProviders: auth.NewRegistry(&auth.Provider{
			Name:         "github",
			DisplayName:  "GitHub",
			AuthorizeURL: "https://github.com/login/oauth/authorize",
			ClientID:     "test-client-id",
		}),
	}
	req := httptest.NewRequest(http.MethodGet, "/login/github", nil)
	req.SetPathValue("provider", "github")
	rec := httptest.NewRecorder()
	s.handleProviderLogin(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", rec.Code)
	}
	var pkce *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthPKCECookieName {
			pkce = c
		}
	}
	if pkce == nil {
		t.Fatalf("no %s cookie set", oauthPKCECookieName)
	}
	if !pkce.HttpOnly || !pkce.Secure || pkce.SameSite != http.SameSiteLaxMode || pkce.Domain != "" || pkce.Path != "/" {
		t.Errorf("PKCE cookie must be HttpOnly+Secure+Lax, host-scoped, Path=/; got %+v", pkce)
	}
	if pkce.MaxAge != int(oauthStateTTL.Seconds()) {
		t.Errorf("PKCE cookie MaxAge = %d, want %d", pkce.MaxAge, int(oauthStateTTL.Seconds()))
	}
	if !validPKCEVerifier(pkce.Value) {
		t.Fatalf("PKCE cookie value %q is not a valid verifier", pkce.Value)
	}

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if got := q.Get("code_challenge"); got != pkceS256Challenge(pkce.Value) {
		t.Errorf("code_challenge = %q, want S256 of the cookie verifier", got)
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", got)
	}
	if _, ok := q["scope"]; !ok || q.Get("scope") != "" {
		t.Errorf("scope must stay present and empty, got %q (present=%v)", q.Get("scope"), ok)
	}
	if strings.Contains(loc.RawQuery, pkce.Value) {
		t.Error("the verifier itself must never appear in the authorize URL")
	}
}

func TestPKCE_OIDCAuthorizeHasNoPKCECookie(t *testing.T) {
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
		t.Fatalf("status = %d, want 307", rec.Code)
	}
	if strings.Contains(rec.Header().Get("Location"), "code_challenge") {
		t.Error("OIDC authorize URL must be unchanged: no code_challenge")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthPKCECookieName {
			t.Fatal("OIDC login must be unchanged: no PKCE cookie")
		}
	}
}

func TestPKCE_CallbackSendsVerifierInFormBody(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	var gotMethod, gotCT, gotVerifier, gotRedirectURI, gotRawQuery string
	hits := fakeGitHubTokenEndpoint(t, func(r *http.Request) {
		gotMethod, gotCT, gotRawQuery = r.Method, r.Header.Get("Content-Type"), r.URL.RawQuery
		_ = r.ParseForm()
		gotVerifier = r.PostForm.Get("code_verifier")
		gotRedirectURI = r.PostForm.Get("redirect_uri")
	})

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	req := newCallbackRequestWithState(t, "abc")
	verifier := s.oauthPKCEVerifierFromCookie(req)
	rec := httptest.NewRecorder()
	s.handleOAuthCallback(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307 (body=%s)", rec.Code, rec.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("token endpoint hits = %d, want 1", hits.Load())
	}
	if gotMethod != http.MethodPost || !strings.HasPrefix(gotCT, "application/x-www-form-urlencoded") {
		t.Errorf("token request = %s %q, want form-encoded POST", gotMethod, gotCT)
	}
	if gotVerifier != verifier {
		t.Errorf("code_verifier = %q, want the cookie verifier %q", gotVerifier, verifier)
	}
	if gotRedirectURI != oauthRedirectURI() || gotRedirectURI == "" {
		t.Errorf("redirect_uri = %q, want %q", gotRedirectURI, oauthRedirectURI())
	}
	if gotRawQuery != "" {
		t.Errorf("token request URL must carry no parameters, got %q", gotRawQuery)
	}
	if !pkceCookieCleared(rec) {
		t.Error("PKCE cookie must be cleared on success")
	}
}

func TestPKCE_CallbackRejectsBadVerifierBeforeExchange(t *testing.T) {
	cases := map[string]func(*http.Request){
		"missing":   func(*http.Request) {},
		"empty":     func(r *http.Request) { r.AddCookie(&http.Cookie{Name: oauthPKCECookieName, Value: ""}) },
		"malformed": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: oauthPKCECookieName, Value: "not a verifier"}) },
		"too short": func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: oauthPKCECookieName, Value: strings.Repeat("a", 42)})
		},
	}
	for name, addCookie := range cases {
		t.Run(name, func(t *testing.T) {
			cleanup := helperSetupTempDirs(t)
			defer cleanup()
			hits := fakeGitHubTokenEndpoint(t, nil)

			nonce, _ := oauthStateNonce()
			req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state="+url.QueryEscape(nonce+oauthStateSeparator+"/dashboard"), nil)
			req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: nonce})
			addCookie(req)

			s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
			rec := httptest.NewRecorder()
			s.handleOAuthCallback(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if hits.Load() != 0 {
				t.Fatalf("token endpoint hits = %d, want 0", hits.Load())
			}
			for _, c := range rec.Result().Cookies() {
				if c.Name == "hive_hub_user" && c.Value != "" {
					t.Fatal("no session may be minted")
				}
			}
			if loadSaaSUser("octocat") != nil {
				t.Fatal("no user record may be written")
			}
			if !pkceCookieCleared(rec) {
				t.Error("PKCE cookie must be cleared on failure")
			}
		})
	}
}

func TestPKCE_CallbackRejectsBadStateBeforeExchangeAndClearsVerifier(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	hits := fakeGitHubTokenEndpoint(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state="+url.QueryEscape("attacker-nonce"+oauthStateSeparator+"/dashboard"), nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: "victim-nonce"})
	addPKCECookie(t, req)

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	rec := httptest.NewRecorder()
	s.handleOAuthCallback(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if hits.Load() != 0 {
		t.Fatalf("token endpoint hits = %d, want 0", hits.Load())
	}
	if !pkceCookieCleared(rec) {
		t.Error("PKCE cookie must be cleared when state is rejected")
	}
}

// TestPKCE_ReplayedCallbackIsRejected replays a completed callback the way a
// browser would after the first response: the state and PKCE cookies have been
// expired, so the second attempt carries neither and must not reach the exchange.
func TestPKCE_ReplayedCallbackIsRejected(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	hits := fakeGitHubTokenEndpoint(t, nil)

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	first := newCallbackRequestWithState(t, "abc")
	rec := httptest.NewRecorder()
	s.handleOAuthCallback(rec, first)
	if rec.Code != http.StatusTemporaryRedirect || hits.Load() != 1 {
		t.Fatalf("first callback: status=%d hits=%d, want 307 and 1", rec.Code, hits.Load())
	}

	// Same URL; the state cookie is resent (a stale copy) but the PKCE cookie
	// was cleared by the first response.
	replay := httptest.NewRequest(http.MethodGet, first.URL.String(), nil)
	for _, c := range first.Cookies() {
		if c.Name != oauthPKCECookieName {
			replay.AddCookie(c)
		}
	}
	rec2 := httptest.NewRecorder()
	s.handleOAuthCallback(rec2, replay)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("replay status = %d, want 400", rec2.Code)
	}
	if hits.Load() != 1 {
		t.Fatalf("replay reached the token endpoint (hits=%d)", hits.Load())
	}
}
