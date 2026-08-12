package hub

import (
	"net/url"
	"strings"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ============================================================
// oauth.go — handleOAuthCallback
// ============================================================

func TestHandleOAuthCallbackMissingCode_Cov(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback", nil)
	s.handleOAuthCallback(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing code status = %d, want 400", rec.Code)
	}
}

func TestHandleOAuthCallbackTokenExchangeFails(t *testing.T) {
	// Token endpoint returns non-JSON so parse fails -> 502.
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not-json`))
	}))
	defer tok.Close()
	oldTok := defaultGHTokenURL
	defaultGHTokenURL = tok.URL
	defer func() { defaultGHTokenURL = oldTok }()

	s := &HubServer{logger: slog.Default()}
	rec := httptest.NewRecorder()
	req := newCallbackRequestWithState(t, "abc")
	s.handleOAuthCallback(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("bad token json status = %d, want 502", rec.Code)
	}
}

func TestHandleOAuthCallbackNoAccessToken_Cov(t *testing.T) {
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":""}`))
	}))
	defer tok.Close()
	oldTok := defaultGHTokenURL
	defaultGHTokenURL = tok.URL
	defer func() { defaultGHTokenURL = oldTok }()

	s := &HubServer{logger: slog.Default()}
	rec := httptest.NewRecorder()
	req := newCallbackRequestWithState(t, "abc")
	s.handleOAuthCallback(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("empty token status = %d, want 502", rec.Code)
	}
}

func TestHandleOAuthCallbackSuccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"gho_test"}`))
	}))
	defer tok.Close()
	usr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"login":"octocat","avatar_url":"https://x/a.png"}`))
	}))
	defer usr.Close()

	oldTok, oldUsr := defaultGHTokenURL, defaultGHUserURL
	defaultGHTokenURL = tok.URL
	defaultGHUserURL = usr.URL
	defer func() { defaultGHTokenURL, defaultGHUserURL = oldTok, oldUsr }()

	// A configured secret lets the login path mint a signed session cookie.
	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	rec := httptest.NewRecorder()
	// A safe relative redirect state should be honored.
	nonce, err := oauthStateNonce()
	if err != nil {
		t.Fatalf("mint nonce: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state="+url.QueryEscape(nonce+oauthStateSeparator+"/my-hives"), nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: nonce})
	s.handleOAuthCallback(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("success status = %d, want 307 (body=%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/my-hives" {
		t.Errorf("redirect = %q, want /my-hives", loc)
	}
	// User should have been created.
	if loadSaaSUser("octocat") == nil {
		t.Error("expected octocat user created")
	}
	// The login path must re-mint the NEW signed cookie so returning users get a
	// verifiable session (this is what lets legacy unsigned-cookie users recover).
	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("expected a hive_hub_user session cookie to be set")
	}
	if sessionCookie.Value == "octocat" {
		t.Error("session cookie must be signed, not the raw username")
	}
	// Verify through the SAME production verifier the hub uses on every cookie
	// read path (handleAuthUser / getRealAuthUser), not the raw master directly:
	// since #2775 the login path mints with the derived session key
	// (s.sessionCookieKey) so a v4 spoke's proxy can verify the cookie too, and
	// verifyHubUserDual is what accepts that derived-key signature (plus legacy
	// raw-master cookies). Asserting against the raw secret here would re-pin the
	// test to a key the hub no longer mints with.
	if user, ok := s.verifyHubUserDual(sessionCookie.Value); !ok || user != "octocat" {
		t.Errorf("minted cookie did not verify: (%q, %v)", user, ok)
	}
}

func TestHandleOAuthCallbackInvalidLogin(t *testing.T) {
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"gho_test"}`))
	}))
	defer tok.Close()
	usr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"login":"bad login!"}`)) // invalid name
	}))
	defer usr.Close()

	oldTok, oldUsr := defaultGHTokenURL, defaultGHUserURL
	defaultGHTokenURL = tok.URL
	defaultGHUserURL = usr.URL
	defer func() { defaultGHTokenURL, defaultGHUserURL = oldTok, oldUsr }()

	s := &HubServer{logger: slog.Default()}
	rec := httptest.NewRecorder()
	req := newCallbackRequestWithState(t, "abc")
	s.handleOAuthCallback(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("invalid login status = %d, want 502", rec.Code)
	}
}

// ============================================================
// saas.go — validateGitHubToken
// ============================================================

func TestValidateGitHubToken(t *testing.T) {
	// Empty token -> "".
	s := &HubServer{logger: slog.Default()}
	if got := s.validateGitHubToken(""); got != "" {
		t.Errorf("empty token -> %q", got)
	}

	// Valid token against a fake user endpoint.
	usr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"login":"validuser"}`))
	}))
	defer usr.Close()
	oldUsr := defaultGHUserURL
	defaultGHUserURL = usr.URL
	defer func() { defaultGHUserURL = oldUsr }()

	// Clear any cached entry for determinism.
	ghTokenCacheMu.Lock()
	delete(ghTokenCache, "good-token")
	delete(ghTokenCache, "bad-token")
	ghTokenCacheMu.Unlock()

	if got := s.validateGitHubToken("good-token"); got != "validuser" {
		t.Errorf("valid token -> %q, want validuser", got)
	}
	// Cached path (second call returns from cache).
	if got := s.validateGitHubToken("good-token"); got != "validuser" {
		t.Errorf("cached token -> %q, want validuser", got)
	}
	// Invalid token -> 401 -> "".
	if got := s.validateGitHubToken("bad-token"); got != "" {
		t.Errorf("bad token -> %q, want empty", got)
	}
}

// ============================================================
// saas.go — encrypt/decrypt round-trip
// ============================================================

func TestEncryptDecryptRoundTrip(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	enc, err := encryptToken("secret-value")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	dec, err := decryptToken(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if dec != "secret-value" {
		t.Errorf("round-trip = %q, want secret-value", dec)
	}

	// Corrupt base64 -> error.
	if _, err := decryptToken("!!!not base64!!!"); err == nil {
		t.Error("expected error decrypting invalid base64")
	}
	// Too short -> error.
	if _, err := decryptToken("YQ=="); err == nil {
		t.Error("expected error decrypting too-short ciphertext")
	}
}

// ============================================================
// saas_provision.go — pure helpers
// ============================================================

func TestReadSAToken(t *testing.T) {
	// SA token file absent in test env -> "".
	if got := readSAToken(); got != "" {
		t.Errorf("readSAToken = %q, want empty in test env", got)
	}
}

func TestEffectiveGitHubURLs(t *testing.T) {
	cluster := &ClusterConfig{GitHubBaseURL: "https://ghe.example.com", GitHubAPIURL: "https://ghe.example.com/api/v3"}

	// Empty hive override -> cluster default.
	h := &SaaSHive{}
	if got := effectiveGitHubBaseURL(h, cluster); got != "https://ghe.example.com" {
		t.Errorf("base default -> %q", got)
	}
	if got := effectiveGitHubAPIURL(h, cluster); got != "https://ghe.example.com/api/v3" {
		t.Errorf("api default -> %q", got)
	}

	// "public" -> empty (public github.com).
	pub := &SaaSHive{GitHubBaseURL: githubHostPublic}
	if got := effectiveGitHubBaseURL(pub, cluster); got != "" {
		t.Errorf("public base -> %q, want empty", got)
	}
	if got := effectiveGitHubAPIURL(pub, cluster); got != "" {
		t.Errorf("public api -> %q, want empty", got)
	}

	// Explicit hive override -> used as-is.
	ovr := &SaaSHive{GitHubBaseURL: "https://custom.gh", GitHubAPIURL: "https://custom.gh/api"}
	if got := effectiveGitHubBaseURL(ovr, cluster); got != "https://custom.gh" {
		t.Errorf("override base -> %q", got)
	}
	if got := effectiveGitHubAPIURL(ovr, cluster); got != "https://custom.gh/api" {
		t.Errorf("override api -> %q", got)
	}
}

func TestClusterForHiveAndHubCluster(t *testing.T) {
	s := &HubServer{clusters: map[string]ClusterConfig{
		defaultClusterID: {ID: defaultClusterID, Name: "default"},
		"gpu":            {ID: "gpu", Name: "gpu"},
	}}

	if c := s.clusterForHive(&SaaSHive{ClusterID: "gpu"}); c == nil || c.ID != "gpu" {
		t.Errorf("clusterForHive gpu -> %+v", c)
	}
	// Empty -> default.
	if c := s.clusterForHive(&SaaSHive{}); c == nil || c.ID != defaultClusterID {
		t.Errorf("clusterForHive default -> %+v", c)
	}
	// Unknown -> falls back to default.
	if c := s.clusterForHive(&SaaSHive{ClusterID: "nope"}); c == nil || c.ID != defaultClusterID {
		t.Errorf("clusterForHive unknown -> %+v", c)
	}
	if c := s.hubCluster(); c == nil || c.ID != defaultClusterID {
		t.Errorf("hubCluster -> %+v", c)
	}

	// hubCluster synth fallback when default not present.
	empty := &HubServer{clusters: map[string]ClusterConfig{}}
	if c := empty.hubCluster(); c == nil || !c.InCluster {
		t.Errorf("hubCluster synth -> %+v", c)
	}
	if c := empty.clusterForHive(&SaaSHive{}); c != nil {
		t.Errorf("clusterForHive with no clusters -> %+v, want nil", c)
	}
}

func TestClusterIDForSaaSHive_Cov(t *testing.T) {
	if got := clusterIDForSaaSHive(SaaSHive{}); got != defaultClusterID {
		t.Errorf("empty -> %q", got)
	}
	if got := clusterIDForSaaSHive(SaaSHive{ClusterID: "x"}); got != "x" {
		t.Errorf("explicit -> %q", got)
	}
}

func TestClusterNameForID_Cov(t *testing.T) {
	s := &HubServer{clusters: map[string]ClusterConfig{"c1": {ID: "c1", Name: "Cluster One"}}}
	if got := s.clusterNameForID("c1"); got != "Cluster One" {
		t.Errorf("known -> %q", got)
	}
	if got := s.clusterNameForID("missing"); got != "" {
		t.Errorf("unknown -> %q, want empty", got)
	}
}

// newCallbackRequestWithState builds an OAuth callback carrying a matching state
// nonce in both the query and the cookie — i.e. a login this browser genuinely
// started (audit F11). Tests exercising the token-exchange path need this or
// they are rejected at the state gate and never reach it.
func newCallbackRequestWithState(t *testing.T, code string) *http.Request {
	t.Helper()
	nonce, err := oauthStateNonce()
	if err != nil {
		t.Fatalf("mint nonce: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code="+code+"&state="+url.QueryEscape(nonce+oauthStateSeparator+"/dashboard"), nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: nonce})
	return req
}

// Audit F11 (CWE-352) — ported from v4 (#3435). `state` used to be nothing but
// the redirect target, so it proved only that the callback carried a URL, never
// that this browser had STARTED a login.
//
// The attack: complete an OAuth flow against your own GitHub account, capture
// the callback URL, hand it to a victim. Their browser completes it and they are
// silently logged into the ATTACKER's account — every issue they file and repo
// they connect afterwards lands there. Redirect validation does nothing about
// it: the URL is valid; it is the SESSION that is forged.
func TestF11_CallbackWithoutStateNonceIsRejected(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	var exchanged bool
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanged = true
		w.Write([]byte(`{"access_token":"gho_attacker"}`))
	}))
	defer tok.Close()
	usr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"login":"attacker","avatar_url":"https://x/a.png"}`))
	}))
	defer usr.Close()
	oldTok, oldUsr := defaultGHTokenURL, defaultGHUserURL
	defaultGHTokenURL, defaultGHUserURL = tok.URL, usr.URL
	defer func() { defaultGHTokenURL, defaultGHUserURL = oldTok, oldUsr }()

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	rec := httptest.NewRecorder()
	// The victim's browser holds NO state cookie: it never started this login.
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=attacker-code&state=%2Fdashboard", nil)
	s.handleOAuthCallback(rec, req)

	if rec.Code == http.StatusTemporaryRedirect {
		t.Fatal("F11: a callback the browser never initiated completed a login — an attacker can log a victim into the attacker's account")
	}
	if exchanged {
		t.Error("F11: the forged callback reached the token exchange; the state gate must reject it before burning a code")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" && c.Value != "" {
			t.Fatalf("F11: a session cookie was minted for a forged callback: %q", c.Value)
		}
	}
}

func TestF11_CallbackWithMismatchedStateNonceIsRejected(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: slog.Default(), hubSecret: testHubSecret}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state="+url.QueryEscape("attacker-nonce"+oauthStateSeparator+"/dashboard"), nil)
	req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: "victim-nonce"})
	s.handleOAuthCallback(rec, req)

	if rec.Code == http.StatusTemporaryRedirect {
		t.Fatal("F11: a callback whose state did not match the browser's cookie completed a login")
	}
}

// /login must actually issue the nonce — the callback gate is worth nothing if
// the login side never mints one.
func TestF11_LoginIssuesStateNonceCookieAndParameter(t *testing.T) {
	t.Setenv("HIVE_HUB_OAUTH_CLIENT_ID", "test-client-id")
	s := &HubServer{logger: slog.Default()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login?redirect=/my-hives", nil)
	s.handleLogin(rec, req)

	var nonce string
	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthStateCookieName {
			nonce = c.Value
			if !c.HttpOnly || !c.Secure {
				t.Error("F11: the state cookie must be HttpOnly and Secure")
			}
			if c.Domain != "" {
				t.Errorf("F11: the state cookie must be host-scoped, got Domain=%q — a sibling tenant would receive it", c.Domain)
			}
		}
	}
	if nonce == "" {
		t.Fatal("F11: /login issued no state nonce cookie, so the callback gate can never pass")
	}

	parsed, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	state := parsed.Query().Get("state")
	if !strings.HasPrefix(state, nonce+oauthStateSeparator) {
		t.Fatalf("F11: state %q does not carry the issued nonce", state)
	}
	if !strings.HasSuffix(state, "/my-hives") {
		t.Errorf("F11: the redirect target was lost from state: %q", state)
	}
}

// Audit F4 (CWE-352/942) — ported from v4 (#3499). A sibling tenant origin must
// not be able to author a state change, and must not receive a credentialed
// CORS reflection (which would be a scripted cross-tenant READ).
func TestF4_SiblingOriginCannotAuthorMutation(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/saas/hives/x/visibility", nil)
	req.Header.Set("Origin", "https://evil.hive.kubestellar.io")
	req.Header.Set("Content-Type", "application/json")
	if isCSRFSafe(req) {
		t.Fatal("F4: a sibling tenant Origin was accepted as CSRF-safe — it can drive cross-tenant mutations")
	}
}

// POSITIVE CONTROL: the hub's own origin must still be able to author, or the
// fix is just a gate broken shut and every legitimate mutation breaks.
func TestF4_HubOriginStillAuthorsMutation(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/saas/hives/x/visibility", nil)
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	req.Header.Set("Content-Type", "application/json")
	if !isCSRFSafe(req) {
		t.Fatal("the hub's own origin must remain CSRF-safe — rejecting it breaks every legitimate mutation")
	}
}
