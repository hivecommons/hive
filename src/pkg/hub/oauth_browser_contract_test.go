package hub

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// These tests pin the end-to-end browser contract of the Hub's GitHub login
// (#14598): state cookie plus S256 PKCE at authorize, a form-POST exchange that
// carries the exact redirect_uri and the cookie's code_verifier, and fail-closed
// rejection before any exchange or session. oauth_pkce_test.go pins the PKCE
// helpers in isolation; do not weaken either set.
func TestGitHubBrowserLoginContract(t *testing.T) {
	const (
		clientID = "browser-contract-client"
		secret   = "browser-contract-secret"
		code     = "browser-contract-code"
		token    = "browser-contract-access-token"
	)
	for _, tc := range []struct {
		name, tokenBody, userBody string
		status, userCalls         int
	}{
		{"success", `{"access_token":"` + token + `"}`, `{"login":"octocat"}`, http.StatusTemporaryRedirect, 1},
		{"malformed token", `{"access_token":"` + token, "", http.StatusBadGateway, 0},
		{"missing token", `{}`, "", http.StatusBadGateway, 0},
		{"malformed user", `{"access_token":"` + token + `"}`, `{"login":`, http.StatusBadGateway, 1},
		{"empty identity", `{"access_token":"` + token + `"}`, `{"login":""}`, http.StatusBadGateway, 1},
		{"invalid identity", `{"access_token":"` + token + `"}`, `{"login":"bad login!"}`, http.StatusBadGateway, 1},
		// GitHub's own rejection of the exchange (e.g. an expired code) must
		// still fail closed. A verifier that does not hash to this login's
		// challenge never gets this far: TestGitHubBrowserVerifierFailsBeforeExchange
		// pins that it is rejected locally, before any token request.
		{"verifier rejected", `{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`, "", http.StatusBadGateway, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanup := helperSetupTempDirs(t)
			defer cleanup()
			t.Setenv("HIVE_HUB_OAUTH_CLIENT_ID", clientID)
			t.Setenv("HIVE_HUB_OAUTH_CLIENT_SECRET", secret)
			var logs bytes.Buffer
			s := &HubServer{logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), hubSecret: testHubSecret}
			var tokenCalls, userCalls atomic.Int32
			var verifier string
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Inspect the URL at both endpoints, not just the token exchange.
				if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.User != nil {
					t.Error("GitHub endpoint request must not carry query or URL credentials")
				}
				for _, value := range []string{secret, code, token, verifier} {
					if strings.Contains(r.URL.String(), value) {
						t.Error("secret found in GitHub request URL")
					}
				}
				switch r.URL.Path {
				case "/token":
					tokenCalls.Add(1)
					if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Accept") != "application/json" {
						t.Error("token exchange must be a form POST accepting JSON")
					}
					if err := r.ParseForm(); err != nil {
						t.Errorf("parse form: %v", err)
					}
					for key, want := range map[string]string{"client_id": clientID, "client_secret": secret, "code": code, "redirect_uri": oauthRedirectURI(), "code_verifier": verifier} {
						if values := r.PostForm[key]; len(values) != 1 || values[0] != want {
							t.Errorf("missing or incorrect token form field %s", key)
						}
					}
					_, _ = w.Write([]byte(tc.tokenBody))
				case "/user":
					userCalls.Add(1)
					if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+token {
						t.Error("user fetch must send the access token only as a bearer header")
					}
					_, _ = w.Write([]byte(tc.userBody))
				default:
					t.Error("unexpected GitHub endpoint")
					http.NotFound(w, r)
				}
			}))
			defer fake.Close()
			oldToken, oldUser := defaultGHTokenURL, defaultGHUserURL
			defaultGHTokenURL, defaultGHUserURL = fake.URL+"/token", fake.URL+"/user"
			defer func() { defaultGHTokenURL, defaultGHUserURL = oldToken, oldUser }()

			var state string
			var cookies []*http.Cookie
			state, verifier, cookies = beginGitHubBrowserContract(t, s)
			callbackURL := "/api/auth/callback?" + url.Values{"code": {code}, "state": {state}}.Encode()
			req := httptest.NewRequest(http.MethodGet, callbackURL, nil)
			for _, cookie := range cookies {
				req.AddCookie(cookie)
			}
			rec := httptest.NewRecorder()
			s.handleOAuthCallback(rec, req)
			if rec.Code != tc.status || tokenCalls.Load() != 1 || userCalls.Load() != int32(tc.userCalls) {
				t.Fatalf("callback status/calls = %d/%d/%d, want %d/1/%d", rec.Code, tokenCalls.Load(), userCalls.Load(), tc.status, tc.userCalls)
			}
			assertBrowserContractSession(t, rec, tc.status == http.StatusTemporaryRedirect)
			if !pkceCookieCleared(rec) {
				t.Error("callback must consume the PKCE verifier cookie on every outcome")
			}
			if tc.status != http.StatusTemporaryRedirect && loadSaaSUser("octocat") != nil {
				t.Error("a failed login must not write a user record")
			}
			if tc.status == http.StatusTemporaryRedirect {
				u := loadSaaSUser("octocat")
				if u == nil || u.EncryptedToken == "" || u.EncryptedToken == token {
					t.Fatal("successful login must persist an encrypted token")
				}
				plain, err := decryptToken(u.EncryptedToken)
				if err != nil || plain != token {
					t.Fatal("persisted token did not decrypt to the exchanged token")
				}
				// Simulate the browser applying the state-cookie deletion. Reusing
				// the callback URL without that cookie must not exchange again.
				cleared := false
				for _, c := range rec.Result().Cookies() {
					if c.Name == oauthStateCookieName && c.MaxAge < 0 {
						cleared = true
					}
				}
				if !cleared {
					t.Fatal("callback did not consume the browser state cookie")
				}
				replay := httptest.NewRecorder()
				s.handleOAuthCallback(replay, httptest.NewRequest(http.MethodGet, callbackURL, nil))
				if replay.Code != http.StatusBadRequest || tokenCalls.Load() != 1 {
					t.Fatal("callback replay after cookie deletion reached the token endpoint")
				}
				assertBrowserContractSession(t, replay, false)
			}
			for _, value := range []string{secret, code, token, verifier} {
				if strings.Contains(logs.String(), value) {
					t.Error("OAuth logger exposed a secret")
				}
			}
		})
	}
}

func beginGitHubBrowserContract(t *testing.T, s *HubServer) (state, verifier string, cookies []*http.Cookie) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.startProviderLogin(rec, httptest.NewRequest(http.MethodGet, "/login", nil), s.resolveProvider("github"), "/dashboard")
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("authorize status = %d", rec.Code)
	}
	authorize, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := authorize.Query()
	if authorize.Scheme+"://"+authorize.Host+authorize.Path != defaultGHAuthorizeURL || !q.Has("scope") || q.Get("scope") != "" || q.Get("redirect_uri") != oauthRedirectURI() || q.Get("client_id") != s.resolveProvider("github").ClientID {
		t.Fatal("incorrect authorize endpoint, scopes, client or redirect URI")
	}
	cookies = rec.Result().Cookies()
	var boundChallenge string
	for _, c := range cookies {
		switch c.Name {
		case oauthStateCookieName:
			nonce, challenge := splitOAuthStateCookie(c.Value)
			if nonce == "" || !strings.HasPrefix(q.Get("state"), nonce+oauthStateSeparator) || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge <= 0 {
				t.Fatal("authorize state must be bound to a short-lived secure browser cookie")
			}
			state, boundChallenge = q.Get("state"), challenge
		case oauthPKCECookieName:
			if !validPKCEVerifier(c.Value) || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge <= 0 || c.MaxAge > int(oauthStateTTL.Seconds()) {
				t.Fatal("PKCE verifier must live in a short-lived secure browser cookie")
			}
			verifier = c.Value
		}
	}
	if state == "" {
		t.Fatal("authorize did not set a state cookie")
	}
	if verifier == "" {
		t.Fatal("authorize did not set a PKCE verifier cookie")
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != pkceS256Challenge(verifier) {
		t.Fatal("authorize must send the S256 challenge of the cookie verifier")
	}
	if boundChallenge != q.Get("code_challenge") {
		t.Fatal("the issued code_challenge must be bound to the login's state cookie")
	}
	if strings.Contains(q.Get("state"), boundChallenge) {
		t.Fatal("the state parameter format must not change; only the cookie carries the challenge")
	}
	if strings.Contains(authorize.RawQuery, verifier) {
		t.Fatal("the PKCE verifier must never appear in the authorize URL")
	}
	return state, verifier, cookies
}

func assertBrowserContractSession(t *testing.T, rec *httptest.ResponseRecorder, want bool) {
	t.Helper()
	found := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hive_hub_user" && c.MaxAge >= 0 && c.Value != "" {
			found = true
		}
	}
	if found != want {
		t.Errorf("session cookie present = %v, want %v", found, want)
	}
}

func TestGitHubBrowserStateFailsBeforeExchange(t *testing.T) {
	var calls atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "unexpected-token"})
	}))
	defer fake.Close()
	old := defaultGHTokenURL
	defaultGHTokenURL = fake.URL
	defer func() { defaultGHTokenURL = old }()
	for _, tc := range []struct {
		name, state, cookie string
	}{
		{"no state cookie", "nonce:/dashboard", ""},
		{"state mismatch", "other:/dashboard", "nonce"},
		{"missing state", "", "nonce"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			s := &HubServer{logger: slog.New(slog.NewTextHandler(&logs, nil)), hubSecret: testHubSecret}
			req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?"+url.Values{"code": {"rejected-secret-code"}, "state": {tc.state}}.Encode(), nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: oauthStateCookieName, Value: tc.cookie})
			}
			rec := httptest.NewRecorder()
			s.handleOAuthCallback(rec, req)
			if rec.Code != http.StatusBadRequest || calls.Load() != 0 {
				t.Fatal("invalid state must fail before token exchange")
			}
			assertBrowserContractSession(t, rec, false)
			if strings.Contains(logs.String(), "rejected-secret-code") {
				t.Error("rejected callback code leaked to logger")
			}
		})
	}
}

// TestGitHubBrowserVerifierFailsBeforeExchange starts a real login, so the state
// nonce is valid, then presents the callback with a verifier that does not
// belong to that login: missing (an expired cookie is dropped by the browser),
// malformed, a syntactically valid but substituted verifier, or a state cookie
// whose bound challenge was replaced or stripped. Each case must be rejected
// locally — the fake GitHub fails the test if the token or user endpoint is
// contacted at all.
func TestGitHubBrowserVerifierFailsBeforeExchange(t *testing.T) {
	const code = "verifier-rejected-secret-code"
	substitute := func(t *testing.T) string {
		t.Helper()
		v, err := oauthPKCEVerifier()
		if err != nil {
			t.Fatalf("mint substitute verifier: %v", err)
		}
		return v
	}
	for _, tc := range []struct {
		name string
		// verifier returns the PKCE cookie to present (present=false omits it).
		verifier func(t *testing.T, issued string) (string, bool)
		// stateCookie rewrites the issued state cookie value; nil keeps it.
		stateCookie func(t *testing.T, issued string) string
	}{
		{name: "missing or expired", verifier: func(*testing.T, string) (string, bool) { return "", false }},
		{name: "empty", verifier: func(*testing.T, string) (string, bool) { return "", true }},
		{name: "malformed", verifier: func(*testing.T, string) (string, bool) { return "not a verifier", true }},
		{name: "truncated", verifier: func(_ *testing.T, v string) (string, bool) { return v[:42], true }},
		{name: "substituted valid verifier", verifier: func(t *testing.T, _ string) (string, bool) { return substitute(t), true }},
		{name: "substituted max-length verifier", verifier: func(*testing.T, string) (string, bool) {
			return strings.Repeat("Ab9-._~x", 16), true
		}},
		{name: "issued verifier with one character changed", verifier: func(_ *testing.T, v string) (string, bool) {
			last := byte('A')
			if v[len(v)-1] == 'A' {
				last = 'B'
			}
			return v[:len(v)-1] + string(last), true
		}},
		{
			name:     "state-bound challenge replaced",
			verifier: func(_ *testing.T, v string) (string, bool) { return v, true },
			stateCookie: func(t *testing.T, issued string) string {
				nonce, _ := splitOAuthStateCookie(issued)
				return nonce + oauthStateChallengeSeparator + pkceS256Challenge(substitute(t))
			},
		},
		{
			name:     "state-bound challenge stripped",
			verifier: func(_ *testing.T, v string) (string, bool) { return v, true },
			stateCookie: func(_ *testing.T, issued string) string {
				nonce, _ := splitOAuthStateCookie(issued)
				return nonce
			},
		},
		{
			name:     "state-bound challenge emptied",
			verifier: func(_ *testing.T, v string) (string, bool) { return v, true },
			stateCookie: func(_ *testing.T, issued string) string {
				nonce, _ := splitOAuthStateCookie(issued)
				return nonce + oauthStateChallengeSeparator
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanup := helperSetupTempDirs(t)
			defer cleanup()
			var calls atomic.Int32
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				t.Errorf("GitHub %s contacted for a callback that must be rejected locally", r.URL.Path)
				http.Error(w, "unexpected", http.StatusInternalServerError)
			}))
			defer fake.Close()
			oldToken, oldUser := defaultGHTokenURL, defaultGHUserURL
			defaultGHTokenURL, defaultGHUserURL = fake.URL+"/token", fake.URL+"/user"
			defer func() { defaultGHTokenURL, defaultGHUserURL = oldToken, oldUser }()

			var logs bytes.Buffer
			s := &HubServer{logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), hubSecret: testHubSecret}
			state, issued, cookies := beginGitHubBrowserContract(t, s)
			req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?"+url.Values{"code": {code}, "state": {state}}.Encode(), nil)
			for _, c := range cookies {
				switch {
				case c.Name == oauthPKCECookieName:
				case c.Name == oauthStateCookieName && tc.stateCookie != nil:
					req.AddCookie(&http.Cookie{Name: c.Name, Value: tc.stateCookie(t, c.Value)})
				default:
					req.AddCookie(c)
				}
			}
			presented, present := tc.verifier(t, issued)
			if present {
				if presented == issued && tc.stateCookie == nil {
					t.Fatal("fixture must not present the issued verifier against the issued challenge")
				}
				req.AddCookie(&http.Cookie{Name: oauthPKCECookieName, Value: presented})
			}
			rec := httptest.NewRecorder()
			s.handleOAuthCallback(rec, req)
			if rec.Code != http.StatusBadRequest || calls.Load() != 0 {
				t.Fatalf("status/calls = %d/%d, want 400/0: a verifier not bound to this login must fail before token exchange", rec.Code, calls.Load())
			}
			assertBrowserContractSession(t, rec, false)
			if !pkceCookieCleared(rec) {
				t.Error("rejected callback must still clear the PKCE cookie")
			}
			stateCleared := false
			for _, c := range rec.Result().Cookies() {
				if c.Name == oauthStateCookieName && c.MaxAge < 0 && c.Value == "" {
					stateCleared = true
				}
			}
			if !stateCleared {
				t.Error("rejected callback must clear the state cookie")
			}
			if loadSaaSUser("octocat") != nil {
				t.Error("rejected callback must not write a user record")
			}
			for _, value := range []string{code, issued, presented} {
				if value != "" && strings.Contains(logs.String(), value) {
					t.Error("rejected callback leaked the code or verifier to the logger")
				}
			}
		})
	}
}
