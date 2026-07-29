package hub

import (
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
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state=%2Fmy-hives", nil)
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
	if user, ok := verifyHubUserCookieValue(testHubSecret, sessionCookie.Value); !ok || user != "octocat" {
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
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=abc", nil)
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
