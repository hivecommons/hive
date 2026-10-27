package claude

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withTokenEndpoint points the token exchange at srv for one test.
func withTokenEndpoint(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := tokenEndpointOverride
	tokenEndpointOverride = srv.URL
	t.Cleanup(func() { tokenEndpointOverride = prev })
}

// ---- GeneratePKCE ----

// The challenge must be the S256 hash of the verifier, or the authorization
// server rejects the exchange.
func TestGeneratePKCEChallengeMatchesVerifier(t *testing.T) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Fatalf("challenge is not S256(verifier): got %q want %q", challenge, want)
	}
}

// The verifier must be base64url with no padding — '+', '/' and '=' are invalid
// in a PKCE verifier and would be rejected or mangled in a URL.
func TestGeneratePKCEVerifierIsURLSafe(t *testing.T) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{verifier, challenge} {
		if strings.ContainsAny(s, "+/=") {
			t.Fatalf("%q contains characters invalid for base64url PKCE", s)
		}
	}
	// 32 random bytes encode to 43 base64url characters.
	if len(verifier) != 43 {
		t.Fatalf("verifier length = %d, want 43", len(verifier))
	}
}

// Two calls must not produce the same verifier — a reused verifier would let
// one flow's code be replayed against another.
func TestGeneratePKCEIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		v, _, err := GeneratePKCE()
		if err != nil {
			t.Fatal(err)
		}
		if seen[v] {
			t.Fatal("GeneratePKCE returned a duplicate verifier")
		}
		seen[v] = true
	}
}

// ---- BuildAuthorizeURL ----

func TestBuildAuthorizeURLCarriesPKCEParams(t *testing.T) {
	raw := BuildAuthorizeURL("my-challenge", "https://hive.example/callback", "my-state")

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("BuildAuthorizeURL produced an unparseable URL: %v", err)
	}
	if u.Scheme+"://"+u.Host+u.Path != AuthorizeURL {
		t.Fatalf("authorize endpoint = %q, want %q", u.Scheme+"://"+u.Host+u.Path, AuthorizeURL)
	}

	q := u.Query()
	checks := map[string]string{
		"client_id":             ClientID,
		"response_type":         "code",
		"redirect_uri":          "https://hive.example/callback",
		"code_challenge":        "my-challenge",
		"code_challenge_method": "S256",
		"state":                 "my-state",
		"scope":                 DefaultScopes,
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("query %q = %q, want %q", k, got, want)
		}
	}
}

// The redirect URI must survive encoding intact, or the callback lands nowhere.
func TestBuildAuthorizeURLEncodesRedirect(t *testing.T) {
	redirect := "https://hive.example/cb?x=1&y=2"
	raw := BuildAuthorizeURL("c", redirect, "s")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("redirect_uri"); got != redirect {
		t.Fatalf("redirect_uri round trip failed: got %q", got)
	}
}

// ---- tokenResponse.ErrorString ----

func TestTokenResponseErrorString(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"absent", `{}`, ""},
		{"string error", `{"error":"invalid_grant"}`, "invalid_grant"},
		// Some providers return a structured error; it must still surface rather
		// than being swallowed into "".
		{"object error", `{"error":{"code":"bad"}}`, `{"code":"bad"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tr tokenResponse
			if err := json.Unmarshal([]byte(tc.raw), &tr); err != nil {
				t.Fatal(err)
			}
			if got := tr.ErrorString(); got != tc.want {
				t.Fatalf("ErrorString() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---- ExchangeCode ----

// The hosted callback shows the code as "<code>#<state>"; both halves must be
// sent separately or the server rejects the exchange.
func TestExchangeCodeSplitsCodeAndState(t *testing.T) {
	var gotCode, gotState, gotVerifier, gotGrant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotCode = r.FormValue("code")
		gotState = r.FormValue("state")
		gotVerifier = r.FormValue("code_verifier")
		gotGrant = r.FormValue("grant_type")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at_123", "refresh_token": "rt_456",
			"expires_in": 3600, "scope": "user:profile user:inference",
		})
	}))
	defer srv.Close()
	withTokenEndpoint(t, srv)

	tokens, err := ExchangeCode("thecode#thestate", "theverifier", "https://hive.example/cb")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}

	if gotCode != "thecode" {
		t.Errorf("code = %q, want the part before '#'", gotCode)
	}
	if gotState != "thestate" {
		t.Errorf("state = %q, want the part after '#'", gotState)
	}
	if gotVerifier != "theverifier" {
		t.Errorf("code_verifier = %q", gotVerifier)
	}
	if gotGrant != "authorization_code" {
		t.Errorf("grant_type = %q", gotGrant)
	}
	if tokens.AccessToken != "at_123" || tokens.RefreshToken != "rt_456" {
		t.Fatalf("tokens = %+v", tokens)
	}
	if len(tokens.Scopes) != 2 {
		t.Errorf("scopes = %v", tokens.Scopes)
	}
}

// A code with no '#' is used whole, with an empty state.
func TestExchangeCodeWithoutStateFragment(t *testing.T) {
	var gotCode, gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotCode, gotState = r.FormValue("code"), r.FormValue("state")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "expires_in": 60})
	}))
	defer srv.Close()
	withTokenEndpoint(t, srv)

	if _, err := ExchangeCode("plaincode", "v", "https://hive.example/cb"); err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if gotCode != "plaincode" || gotState != "" {
		t.Fatalf("code=%q state=%q", gotCode, gotState)
	}
}

// ExpiresAt must be an absolute epoch-millis deadline derived from expires_in,
// or every stored credential looks permanently expired (or never expires).
func TestExchangeCodeComputesAbsoluteExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "expires_in": 3600})
	}))
	defer srv.Close()
	withTokenEndpoint(t, srv)

	before := time.Now().UnixMilli()
	tokens, err := ExchangeCode("c", "v", "https://hive.example/cb")
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().UnixMilli()

	if tokens.ExpiresAt < before+3600*1000 || tokens.ExpiresAt > after+3600*1000 {
		t.Fatalf("ExpiresAt = %d, want roughly now+3600s in millis", tokens.ExpiresAt)
	}
}

// When the server returns no scope, the default set is recorded — an empty
// scope list would make the credential look unusable.
func TestExchangeCodeDefaultsScopes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "expires_in": 60})
	}))
	defer srv.Close()
	withTokenEndpoint(t, srv)

	tokens, err := ExchangeCode("c", "v", "https://hive.example/cb")
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens.Scopes) != len(strings.Fields(DefaultScopes)) {
		t.Fatalf("scopes = %v, want the default set", tokens.Scopes)
	}
}

// An OAuth error payload must surface as an error, never as empty credentials
// that would be written to disk and fail mysteriously later.
func TestExchangeCodeSurfacesOAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"error": "invalid_grant", "error_description": "code already used",
		})
	}))
	defer srv.Close()
	withTokenEndpoint(t, srv)

	_, err := ExchangeCode("c", "v", "https://hive.example/cb")
	if err == nil {
		t.Fatal("an OAuth error payload must be returned as an error")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error should name the OAuth code, got %v", err)
	}
	if !strings.Contains(err.Error(), "code already used") {
		t.Errorf("error should include the description, got %v", err)
	}
}

// A 200 with no access token is still a failure.
func TestExchangeCodeRejectsEmptyAccessToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"expires_in": 60})
	}))
	defer srv.Close()
	withTokenEndpoint(t, srv)

	if _, err := ExchangeCode("c", "v", "https://hive.example/cb"); err == nil {
		t.Fatal("an empty access token must be an error")
	}
}

func TestExchangeCodeRejectsMalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{not json"))
	}))
	defer srv.Close()
	withTokenEndpoint(t, srv)

	if _, err := ExchangeCode("c", "v", "https://hive.example/cb"); err == nil {
		t.Fatal("a malformed token response must be an error")
	}
}

// A transport failure must be reported, not treated as an empty token set.
func TestExchangeCodeReportsTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed immediately: the endpoint is unreachable
	withTokenEndpoint(t, srv)

	if _, err := ExchangeCode("c", "v", "https://hive.example/cb"); err == nil {
		t.Fatal("an unreachable token endpoint must be an error")
	}
}

// ---- WriteCredentials / ReadAccessToken ----

// The written file must be exactly the shape Claude Code expects, and it must
// round trip through ReadAccessToken.
func TestWriteAndReadCredentialsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", ".credentials.json")
	tokens := &OAuthTokens{
		AccessToken:  "at_abc",
		RefreshToken: "rt_def",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		Scopes:       []string{"user:profile"},
	}
	if err := WriteCredentials(tokens, path); err != nil {
		t.Fatalf("WriteCredentials: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("credentials not written: %v", err)
	}
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("credentials file is not JSON: %v", err)
	}
	// The key name is load-bearing: Claude Code reads claudeAiOauth.
	if _, ok := onDisk["claudeAiOauth"]; !ok {
		t.Fatalf("credentials must be nested under claudeAiOauth, got %v", onDisk)
	}

	if got := ReadAccessToken(path); got != "at_abc" {
		t.Fatalf("ReadAccessToken = %q, want the written token", got)
	}
	if !HasValidToken(path) {
		t.Fatal("HasValidToken must be true for a fresh token")
	}
}

// The write must be atomic — no .tmp file may survive.
func TestWriteCredentialsIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := WriteCredentials(&OAuthTokens{AccessToken: "at"}, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("the temp file must be renamed away, not left behind")
	}
}

// An expired token must read as absent: handing an expired token to an agent
// produces confusing 401s instead of a clean re-auth prompt.
func TestReadAccessTokenRejectsExpiredToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	tokens := &OAuthTokens{
		AccessToken: "at_expired",
		ExpiresAt:   time.Now().Add(-time.Minute).UnixMilli(),
	}
	if err := WriteCredentials(tokens, path); err != nil {
		t.Fatal(err)
	}
	if got := ReadAccessToken(path); got != "" {
		t.Fatalf("an expired token must read as empty, got %q", got)
	}
	if HasValidToken(path) {
		t.Fatal("HasValidToken must be false for an expired token")
	}
}

// ExpiresAt == 0 means "no expiry recorded" and must NOT be treated as expired.
func TestReadAccessTokenAcceptsZeroExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := WriteCredentials(&OAuthTokens{AccessToken: "at_noexp"}, path); err != nil {
		t.Fatal(err)
	}
	if got := ReadAccessToken(path); got != "at_noexp" {
		t.Fatalf("a zero ExpiresAt must not count as expired, got %q", got)
	}
}

// Missing, malformed, and structurally-empty credential files must all read as
// "no token" rather than erroring or panicking.
func TestReadAccessTokenFailsClosed(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		if got := ReadAccessToken(filepath.Join(dir, "absent.json")); got != "" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		p := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := ReadAccessToken(p); got != "" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("no oauth block", func(t *testing.T) {
		p := filepath.Join(dir, "empty.json")
		if err := os.WriteFile(p, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := ReadAccessToken(p); got != "" {
			t.Fatalf("got %q", got)
		}
		if HasValidToken(p) {
			t.Fatal("HasValidToken must be false without an oauth block")
		}
	})
}

// An unwritable destination must be reported rather than silently losing the
// credential the user just authorized.
func TestWriteCredentialsReportsUnwritablePath(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The parent is a file, so MkdirAll fails.
	err := WriteCredentials(&OAuthTokens{AccessToken: "at"}, filepath.Join(blocker, "sub", "c.json"))
	if err == nil {
		t.Fatal("an unwritable credentials path must be reported")
	}
}
