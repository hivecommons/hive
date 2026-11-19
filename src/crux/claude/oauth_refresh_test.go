package claude

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// refreshTestServer answers the token endpoint like Anthropic's does for a
// refresh grant and records what the request carried.
func refreshTestServer(t *testing.T, reply map[string]any) (*httptest.Server, *url.Values) {
	t.Helper()
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		got = r.PostForm
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	withTokenEndpoint(t, srv)
	return srv, &got
}

func TestRefreshTokensPostsRefreshGrantWithClaudeCodeClient(t *testing.T) {
	_, got := refreshTestServer(t, map[string]any{
		"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 28800,
	})
	prev := &OAuthTokens{
		AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: 1,
		Scopes: []string{"user:inference", "user:profile"},
	}
	before := time.Now().UnixMilli()
	tok, err := RefreshTokens(prev)
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("grant_type") != "refresh_token" {
		t.Errorf("grant_type = %q, want refresh_token", got.Get("grant_type"))
	}
	if got.Get("refresh_token") != "old-refresh" {
		t.Errorf("refresh_token presented = %q, want the previous refresh token", got.Get("refresh_token"))
	}
	if got.Get("client_id") != ClientID {
		t.Errorf("client_id = %q, want the public Claude Code client id", got.Get("client_id"))
	}
	if got.Get("scope") != "user:inference user:profile" {
		t.Errorf("scope = %q, want the previous scopes", got.Get("scope"))
	}
	if tok.AccessToken != "new-access" || tok.RefreshToken != "new-refresh" {
		t.Errorf("tokens not taken from the response: %+v", tok)
	}
	wantExpiry := before + 28800*1000
	if tok.ExpiresAt < wantExpiry || tok.ExpiresAt > wantExpiry+5000 {
		t.Errorf("expiresAt = %d, want about %d", tok.ExpiresAt, wantExpiry)
	}
}

// Fields the endpoint does not return are carried over so the file reads back
// exactly as one Claude Code refreshed.
func TestRefreshTokensCarriesOverFieldsTheEndpointDoesNotReturn(t *testing.T) {
	refreshTestServer(t, map[string]any{
		"access_token": "new-access", "expires_in": 3600,
	})
	prev := &OAuthTokens{
		AccessToken: "old", RefreshToken: "keep-me", ExpiresAt: 1,
		RefreshTokenExpiresAt: 99, Scopes: []string{"user:inference"},
		SubscriptionType: "max", RateLimitTier: "tier-x",
	}
	tok, err := RefreshTokens(prev)
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "keep-me" {
		t.Errorf("a response without refresh_token must keep the previous one, got %q", tok.RefreshToken)
	}
	if tok.RefreshTokenExpiresAt != 99 || tok.SubscriptionType != "max" || tok.RateLimitTier != "tier-x" {
		t.Errorf("carried-over fields lost: %+v", tok)
	}
	if strings.Join(tok.Scopes, " ") != "user:inference" {
		t.Errorf("scopes should be carried over when the response names none, got %v", tok.Scopes)
	}
}

func TestRefreshTokensInvalidGrantIsErrRefreshGrantRejected(t *testing.T) {
	refreshTestServer(t, map[string]any{
		"error": "invalid_grant", "error_description": "refresh token revoked",
	})
	_, err := RefreshTokens(&OAuthTokens{RefreshToken: "dead"})
	if !errors.Is(err, ErrRefreshGrantRejected) {
		t.Fatalf("want ErrRefreshGrantRejected, got %v", err)
	}
	if strings.Contains(err.Error(), "dead") {
		t.Errorf("error must not carry the refresh token: %v", err)
	}
}

func TestRefreshTokensOtherOAuthErrorIsNotGrantRejection(t *testing.T) {
	refreshTestServer(t, map[string]any{
		"error": "server_error", "error_description": "try later",
	})
	_, err := RefreshTokens(&OAuthTokens{RefreshToken: "r"})
	if err == nil || errors.Is(err, ErrRefreshGrantRejected) {
		t.Fatalf("a transient server error must not read as a dead grant, got %v", err)
	}
}

func TestRefreshTokensRejectsMissingRefreshTokenAndBadResponses(t *testing.T) {
	if _, err := RefreshTokens(nil); !errors.Is(err, ErrNoRefreshToken) {
		t.Errorf("nil: want ErrNoRefreshToken, got %v", err)
	}
	if _, err := RefreshTokens(&OAuthTokens{AccessToken: "a"}); !errors.Is(err, ErrNoRefreshToken) {
		t.Errorf("no refresh token: want ErrNoRefreshToken, got %v", err)
	}
	refreshTestServer(t, map[string]any{"access_token": "x"})
	if _, err := RefreshTokens(&OAuthTokens{RefreshToken: "r"}); err == nil {
		t.Error("a response with no expires_in must be rejected")
	}
}
