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

// These tests pin the browser contract already available on v6. The verifier
// rejection matrix and S256 challenge assertions belong alongside these once
// the PKCE implementation (#14633) lands; do not weaken the state checks then.
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanup := helperSetupTempDirs(t)
			defer cleanup()
			t.Setenv("HIVE_HUB_OAUTH_CLIENT_ID", clientID)
			t.Setenv("HIVE_HUB_OAUTH_CLIENT_SECRET", secret)
			var logs bytes.Buffer
			s := &HubServer{logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), hubSecret: testHubSecret}
			var tokenCalls, userCalls atomic.Int32
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Inspect the URL at both endpoints, not just the token exchange.
				if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.User != nil {
					t.Error("GitHub endpoint request must not carry query or URL credentials")
				}
				for _, value := range []string{secret, code, token} {
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
					for key, want := range map[string]string{"client_id": clientID, "client_secret": secret, "code": code, "redirect_uri": oauthRedirectURI()} {
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

			state, cookies := beginGitHubBrowserContract(t, s)
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
			for _, value := range []string{secret, code, token} {
				if strings.Contains(logs.String(), value) {
					t.Error("OAuth logger exposed a secret")
				}
			}
		})
	}
}

func beginGitHubBrowserContract(t *testing.T, s *HubServer) (string, []*http.Cookie) {
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
	for _, c := range rec.Result().Cookies() {
		if c.Name == oauthStateCookieName {
			if c.Value == "" || !strings.HasPrefix(q.Get("state"), c.Value+oauthStateSeparator) || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge <= 0 {
				t.Fatal("authorize state must be bound to a short-lived secure browser cookie")
			}
			return q.Get("state"), rec.Result().Cookies()
		}
	}
	t.Fatal("authorize did not set a state cookie")
	return "", nil
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
