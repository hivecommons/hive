package claude

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withProfileEndpoint points FetchProfile at srv for one test.
func withProfileEndpoint(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := profileEndpointOverride
	profileEndpointOverride = srv.URL + "/api/oauth/profile"
	t.Cleanup(func() { profileEndpointOverride = prev })
}

// A fake profile body in the shape Claude Code reads: organization carries
// organization_type and rate_limit_tier.
func fakeProfileBody(orgType, tier string) string {
	b, _ := json.Marshal(map[string]any{
		"account": map[string]any{"uuid": "acct-1", "display_name": "Test"},
		"organization": map[string]any{
			"uuid":              "org-1",
			"organization_type": orgType,
			"rate_limit_tier":   tier,
		},
	})
	return string(b)
}

// FetchProfile sends the access token as a bearer token to the profile path
// and maps organization_type the way Claude Code does.
func TestFetchProfileMapsOrganizationTypeAndTier(t *testing.T) {
	cases := []struct {
		orgType, tier, wantType string
	}{
		{"claude_max", "default_claude_max_20x", "max"},
		{"claude_pro", "default_claude_pro", "pro"},
		{"claude_team", "default_claude_team", "team"},
		{"claude_enterprise", "default_claude_enterprise", "enterprise"},
		// An organization type outside Claude Code's table stores no
		// subscriptionType; the tier is still recorded.
		{"api_console", "default_api", ""},
	}
	for _, tc := range cases {
		t.Run(tc.orgType, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/oauth/profile" {
					t.Errorf("got %s %s, want GET /api/oauth/profile", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer at_fake" {
					t.Errorf("Authorization = %q, want the bearer access token", got)
				}
				_, _ = w.Write([]byte(fakeProfileBody(tc.orgType, tc.tier)))
			}))
			defer srv.Close()
			withProfileEndpoint(t, srv)

			prof, err := FetchProfile(context.Background(), "at_fake")
			if err != nil {
				t.Fatalf("FetchProfile: %v", err)
			}
			if prof.SubscriptionType != tc.wantType || prof.RateLimitTier != tc.tier {
				t.Fatalf("profile = %+v, want {%q %q}", prof, tc.wantType, tc.tier)
			}
		})
	}
}

// Every unusable answer is an error, and no error repeats the token.
func TestFetchProfileFailures(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"unauthorized": func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"error":"x"}`, http.StatusUnauthorized) },
		"server error": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) },
		"malformed":    func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{not json")) },
		"no organization": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"account":{"uuid":"acct-1"}}`))
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			withProfileEndpoint(t, srv)
			_, err := FetchProfile(context.Background(), "at_secret_value")
			if err == nil {
				t.Fatal("FetchProfile returned no error")
			}
			if strings.Contains(err.Error(), "at_secret_value") {
				t.Fatalf("error leaks the token: %v", err)
			}
		})
	}
}

// The token endpoint names the account and organization; ExchangeCode keeps
// them so a later sign-in can tell whether a stored plan is the same account's.
func TestExchangeCodeRecordsSignInAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600,"scope":"user:inference",` +
			`"account":{"uuid":"acct-1","email_address":"a@example.invalid"},"organization":{"uuid":"org-1","name":"Org"}}`))
	}))
	defer srv.Close()
	withTokenEndpoint(t, srv)

	tok, err := ExchangeCode("code#state", "verifier", "https://example.invalid/cb")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	want := SignInAccount{AccountUUID: "acct-1", OrganizationUUID: "org-1"}
	if tok.SignInAccount == nil || *tok.SignInAccount != want {
		t.Fatalf("SignInAccount = %+v, want %+v", tok.SignInAccount, want)
	}
}

func TestExchangeCodeWithoutAccountLeavesIdentityUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"at","expires_in":3600}`))
	}))
	defer srv.Close()
	withTokenEndpoint(t, srv)

	tok, err := ExchangeCode("code", "verifier", "https://example.invalid/cb")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.SignInAccount != nil {
		t.Fatalf("SignInAccount = %+v, want nil", tok.SignInAccount)
	}
}

func TestApplyProfile(t *testing.T) {
	acctA := &SignInAccount{AccountUUID: "acct-a", OrganizationUUID: "org-1"}
	acctAOtherOrg := &SignInAccount{AccountUUID: "acct-a", OrganizationUUID: "org-2"}
	acctB := &SignInAccount{AccountUUID: "acct-b", OrganizationUUID: "org-1"}
	stale := func(id *SignInAccount) *OAuthTokens {
		return &OAuthTokens{AccessToken: "old", SubscriptionType: "pro", RateLimitTier: "default_claude_pro", SignInAccount: id}
	}
	failed := errors.New("profile request: HTTP 503")

	cases := []struct {
		name               string
		id                 *SignInAccount
		prof               Profile
		err                error
		prev               *OAuthTokens
		wantType, wantTier string
		wantKept           bool
	}{
		{"fetched plan wins over stored plan", acctA, Profile{"max", "default_claude_max_20x"}, nil, stale(acctA), "max", "default_claude_max_20x", false},
		{"fetched plan with no type clears a stale one", acctA, Profile{"", "default_api"}, nil, stale(acctA), "", "default_api", false},
		{"failed fetch, same account keeps plan", acctA, Profile{}, failed, stale(acctA), "pro", "default_claude_pro", true},
		{"failed fetch, other account drops plan", acctA, Profile{}, failed, stale(acctB), "", "", false},
		{"failed fetch, same account other org drops plan", acctA, Profile{}, failed, stale(acctAOtherOrg), "", "", false},
		{"failed fetch, stored file has no identity", acctA, Profile{}, failed, stale(nil), "", "", false},
		{"failed fetch, new token has no identity", nil, Profile{}, failed, stale(nil), "", "", false},
		{"failed fetch, nothing on file", acctA, Profile{}, failed, nil, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := &OAuthTokens{AccessToken: "new", SignInAccount: tc.id}
			kept := ApplyProfile(tok, tc.prof, tc.err, tc.prev)
			if tok.SubscriptionType != tc.wantType || tok.RateLimitTier != tc.wantTier || kept != tc.wantKept {
				t.Fatalf("got {%q %q kept=%v}, want {%q %q kept=%v}",
					tok.SubscriptionType, tok.RateLimitTier, kept, tc.wantType, tc.wantTier, tc.wantKept)
			}
		})
	}
}

// The plan lands inside claudeAiOauth under the key names Claude Code reads.
func TestWriteCredentialsStoresPlanUnderClaudeKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	tok := &OAuthTokens{
		AccessToken:      "at",
		Scopes:           []string{"user:inference"},
		SubscriptionType: "max",
		RateLimitTier:    "default_claude_max_20x",
		SignInAccount:    &SignInAccount{AccountUUID: "acct-1", OrganizationUUID: "org-1"},
	}
	if err := WriteCredentials(tok, path); err != nil {
		t.Fatalf("WriteCredentials: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		ClaudeAIOAuth map[string]any `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.ClaudeAIOAuth["subscriptionType"] != "max" || onDisk.ClaudeAIOAuth["rateLimitTier"] != "default_claude_max_20x" {
		t.Fatalf("claudeAiOauth = %v, want subscriptionType/rateLimitTier", onDisk.ClaudeAIOAuth)
	}
	back := LoadTokens(path)
	if back == nil || back.SignInAccount == nil || back.SignInAccount.AccountUUID != "acct-1" {
		t.Fatalf("LoadTokens lost the sign-in account: %+v", back)
	}
}
