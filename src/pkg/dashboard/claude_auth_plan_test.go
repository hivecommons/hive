package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/claude"
)

// planSignIn runs one dashboard sign-in through handleClaudeAuthExchange with
// the real credentials writer (into a temp dir) and returns the response, the
// claudeAiOauth block written to disk, and the captured log.
func planSignIn(t *testing.T, tokens *claude.OAuthTokens, previous string, fetch func(context.Context, string) (claude.Profile, error)) (*httptest.ResponseRecorder, map[string]any, string) {
	t.Helper()
	stubClaudeSeams(t)
	claudeWriteCreds = claude.WriteCredentials
	if previous != "" {
		if err := os.WriteFile(claudeCredPath, []byte(previous), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	claudeExchange = func(code, verifier, redirect string) (*claude.OAuthTokens, error) {
		return tokens, nil
	}
	claudeFetchProfile = fetch

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s := NewServer(0, logger)
	s.deps = &Dependencies{Logger: logger}
	s.claudeOAuthFlow.codeVerifier = "verifier"
	s.claudeOAuthFlow.expiresAt = time.Now().Add(10 * time.Minute)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/claude-auth/exchange", strings.NewReader(`{"code":"good-code"}`))
	markOwnerRequest(req)
	s.handleClaudeAuthExchange(rec, req)

	var block map[string]any
	if raw, err := os.ReadFile(claudeCredPath); err == nil {
		var creds struct {
			ClaudeAIOAuth map[string]any `json:"claudeAiOauth"`
		}
		if err := json.Unmarshal(raw, &creds); err != nil {
			t.Fatalf("credentials file is not valid JSON: %v", err)
		}
		block = creds.ClaudeAIOAuth
	}
	return rec, block, logBuf.String()
}

func freshTokens() *claude.OAuthTokens {
	return &claude.OAuthTokens{
		AccessToken:   "at_new",
		RefreshToken:  "rt_new",
		ExpiresAt:     time.Now().Add(time.Hour).UnixMilli(),
		Scopes:        []string{"user:inference"},
		SignInAccount: &claude.SignInAccount{AccountUUID: "acct-a", OrganizationUUID: "org-1"},
	}
}

// After the exchange, the written credentials carry the fetched plan, so
// Claude Code shows the subscription rather than "Claude API". A stale plan
// already on file is replaced by the fetched one.
func TestClaudeSignInStoresFetchedPlan(t *testing.T) {
	previous := `{"claudeAiOauth":{"accessToken":"at_old","expiresAt":0,"scopes":[],"subscriptionType":"pro","rateLimitTier":"default_claude_pro"}}`
	var gotToken string
	rec, block, _ := planSignIn(t, freshTokens(), previous, func(_ context.Context, tok string) (claude.Profile, error) {
		gotToken = tok
		return claude.Profile{SubscriptionType: "max", RateLimitTier: "default_claude_max_20x"}, nil
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if block["subscriptionType"] != "max" || block["rateLimitTier"] != "default_claude_max_20x" {
		t.Fatalf("claudeAiOauth = %v, want subscriptionType=max rateLimitTier=default_claude_max_20x", block)
	}
	if block["accessToken"] != "at_new" {
		t.Fatalf("accessToken = %v, want at_new", block["accessToken"])
	}
	if gotToken != "at_new" {
		t.Fatalf("profile read with token %q, want the new access token", gotToken)
	}
}

// A failed profile read never blocks the sign-in: the token is written, valid,
// with no plan, and a warning says why.
func TestClaudeSignInProfileFailureStillSignsIn(t *testing.T) {
	rec, block, logs := planSignIn(t, freshTokens(), "", func(context.Context, string) (claude.Profile, error) {
		return claude.Profile{}, errors.New("profile request: HTTP 503")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if block["accessToken"] != "at_new" || block["refreshToken"] != "rt_new" {
		t.Fatalf("claudeAiOauth = %v, want the new token written", block)
	}
	if _, ok := block["subscriptionType"]; ok {
		t.Fatalf("subscriptionType written without a profile read: %v", block)
	}
	if _, ok := block["rateLimitTier"]; ok {
		t.Fatalf("rateLimitTier written without a profile read: %v", block)
	}
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "HTTP 503") {
		t.Fatalf("want a WARN naming the profile failure, got log %q", logs)
	}
}

// A failed read keeps the plan on file only when hive recorded it for the
// same account and organization as the new token.
func TestClaudeSignInProfileFailureKeepsSameAccountPlan(t *testing.T) {
	previous := `{"claudeAiOauth":{"accessToken":"at_old","expiresAt":0,"scopes":[],"subscriptionType":"max","rateLimitTier":"default_claude_max_20x",` +
		`"hiveSignInAccount":{"accountUuid":"acct-a","organizationUuid":"org-1"}}}`
	rec, block, _ := planSignIn(t, freshTokens(), previous, func(context.Context, string) (claude.Profile, error) {
		return claude.Profile{}, errors.New("profile request: HTTP 503")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if block["subscriptionType"] != "max" || block["rateLimitTier"] != "default_claude_max_20x" {
		t.Fatalf("claudeAiOauth = %v, want the same account's plan kept", block)
	}
}

func TestClaudeSignInProfileFailureDropsOtherAccountPlan(t *testing.T) {
	for name, previous := range map[string]string{
		"other account": `{"claudeAiOauth":{"accessToken":"at_old","expiresAt":0,"scopes":[],"subscriptionType":"max","rateLimitTier":"default_claude_max_20x",` +
			`"hiveSignInAccount":{"accountUuid":"acct-b","organizationUuid":"org-1"}}}`,
		"no recorded account": `{"claudeAiOauth":{"accessToken":"at_old","expiresAt":0,"scopes":[],"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec, block, _ := planSignIn(t, freshTokens(), previous, func(context.Context, string) (claude.Profile, error) {
				return claude.Profile{}, errors.New("profile request: HTTP 503")
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if _, ok := block["subscriptionType"]; ok {
				t.Fatalf("stale plan carried over: %v", block)
			}
			if _, ok := block["rateLimitTier"]; ok {
				t.Fatalf("stale tier carried over: %v", block)
			}
		})
	}
}
