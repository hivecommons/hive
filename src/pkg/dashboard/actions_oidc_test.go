package dashboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/mention"
)

const actionsTestKid = "test-kid"

func TestActionsOIDCVerifyTable(t *testing.T) {
	key := actionsTestKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	s := actionsTestServer(t, key, now)
	cfg := config.GitHubActionsOIDCConfig{Enabled: true, Audience: "hive"}
	good := actionsTestClaims(now)
	for _, tc := range []struct {
		name   string
		claims actionsOIDCClaims
		key    *rsa.PrivateKey
		kid    string
		wantOK bool
	}{
		{name: "good", claims: good, key: key, kid: actionsTestKid, wantOK: true},
		{name: "bad sig", claims: good, key: actionsTestKey(t), kid: actionsTestKid},
		{name: "wrong aud", claims: withAud(good, "stranger"), key: key, kid: actionsTestKid},
		{name: "wrong iss", claims: withIss(good, "https://example.invalid"), key: key, kid: actionsTestKid},
		{name: "expired", claims: withExp(good, now.Add(-10*time.Minute)), key: key, kid: actionsTestKid},
		{name: "nbf future", claims: withNBF(good, now.Add(10*time.Minute)), key: key, kid: actionsTestKid},
		{name: "unknown kid", claims: good, key: key, kid: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := signActionsToken(t, tc.key, tc.kid, tc.claims)
			_, err := s.verifyActionsOIDC(context.Background(), tok, cfg)
			if (err == nil) != tc.wantOK {
				t.Fatalf("verify err=%v wantOK=%v", err, tc.wantOK)
			}
		})
	}
}

func TestActionsDispatchEndpointGuardsAndAccepts(t *testing.T) {
	key := actionsTestKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	base := actionsTestDeps(t, key, now)
	cases := []struct {
		name       string
		mutateDeps func(*Dependencies)
		mutateReq  func(*actionsDispatchRequest)
		claims     actionsOIDCClaims
		want       int
		wantKicks  int
	}{
		{name: "accepted", claims: actionsTestClaims(now), want: http.StatusAccepted, wantKicks: 1},
		{name: "ungoverned repo", claims: withRepo(actionsTestClaims(now), "evil/repo"), want: http.StatusForbidden},
		{name: "unmapped actor", claims: withActor(actionsTestClaims(now), "mallory"), want: http.StatusForbidden},
		{name: "bot actor unmapped", claims: withActor(actionsTestClaims(now), "github-actions[bot]"), want: http.StatusForbidden},
		{name: "disallowed command", claims: actionsTestClaims(now), mutateReq: func(r *actionsDispatchRequest) { r.Command = "kick" }, want: http.StatusForbidden},
		{name: "allow apply gated", claims: actionsTestClaims(now), mutateReq: func(r *actionsDispatchRequest) { r.Command = "kick" }, mutateDeps: func(d *Dependencies) { d.Config.GitHub.Actions.AllowedCommands = []string{"status", "review", "kick"} }, want: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := cloneActionsDeps(base)
			var kicks []string
			deps.ActionDispatchKick = func(agent, message, source string) error { kicks = append(kicks, source+":"+message); return nil }
			if tc.mutateDeps != nil {
				tc.mutateDeps(deps)
			}
			s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
			s.RegisterAPI(deps)
			reqBody := actionsDispatchRequest{Command: "review", Prompt: "please review", Issue: 42, RunID: "100", RunAttempt: "1"}
			if tc.mutateReq != nil {
				tc.mutateReq(&reqBody)
			}
			code, body := postActionsDispatch(t, s, key, tc.claims, reqBody)
			if code != tc.want {
				t.Fatalf("status=%d body=%s want=%d", code, body, tc.want)
			}
			if len(kicks) != tc.wantKicks {
				t.Fatalf("kicks=%d want=%d body=%s", len(kicks), tc.wantKicks, body)
			}
		})
	}
}

func TestActionsDispatchEndpointDedupeReplay(t *testing.T) {
	key := actionsTestKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	deps := actionsTestDeps(t, key, now)
	var kicks int
	deps.ActionDispatchKick = func(agent, message, source string) error { kicks++; return nil }
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.RegisterAPI(deps)
	body := actionsDispatchRequest{Command: "review", Prompt: "once", Issue: 42, RunID: "100", RunAttempt: "1"}
	for i := 0; i < 2; i++ {
		code, resp := postActionsDispatch(t, s, key, actionsTestClaims(now), body)
		if code != http.StatusAccepted {
			t.Fatalf("status=%d body=%s", code, resp)
		}
	}
	if kicks != 1 {
		t.Fatalf("dedupe replay kicked %d times", kicks)
	}
}

func actionsTestDeps(t *testing.T, key *rsa.PrivateKey, now time.Time) *Dependencies {
	t.Helper()
	store, err := mention.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	return &Dependencies{
		Config: &config.Config{
			Project:   config.ProjectConfig{Repos: []string{"org/repo"}},
			Dashboard: config.DashboardConfig{AuthorizedUsers: []string{"alice:owner"}},
			GitHub:    config.GitHubConfig{Mentions: config.GitHubMentionsConfig{Enabled: true, MinRole: config.RoleReadWrite}, Actions: config.GitHubActionsConfig{Enabled: true, AllowedCommands: []string{"status", "review"}, IdentityMap: map[string]string{"ci-bot": "alice"}, OIDC: config.GitHubActionsOIDCConfig{Enabled: true, Audience: "hive"}}},
		},
		MentionStore: store,
		ActionDispatchAgents: func() []mention.AgentInfo {
			return []mention.AgentInfo{{Name: "scanner", Enabled: true, Converse: true, Mention: true, GovernorKick: true}}
		},
		ActionsJWKSFetcher: func(context.Context, string) ([]byte, error) { return actionsJWKSJSON(t, &key.PublicKey), nil },
		ActionsClock:       func() time.Time { return now },
	}
}

func cloneActionsDeps(d *Dependencies) *Dependencies {
	cp := *d
	cfg := *d.Config
	gh := d.Config.GitHub
	gh.Actions.IdentityMap = map[string]string{"ci-bot": "alice"}
	cfg.GitHub = gh
	cp.Config = &cfg
	st, _ := mention.NewStore("")
	cp.MentionStore = st
	return &cp
}
func actionsTestServer(t *testing.T, key *rsa.PrivateKey, now time.Time) *Server {
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.deps = actionsTestDeps(t, key, now)
	return s
}

func postActionsDispatch(t *testing.T, s *Server, key *rsa.PrivateKey, claims actionsOIDCClaims, body actionsDispatchRequest) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/actions/dispatch", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+signActionsToken(t, key, actionsTestKid, claims))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func actionsTestClaims(now time.Time) actionsOIDCClaims {
	return actionsOIDCClaims{Repository: "org/repo", RepositoryOwner: "org", Actor: "ci-bot", Workflow: "Smoke", Ref: "refs/heads/v6", RegisteredClaims: jwt.RegisteredClaims{Issuer: githubActionsOIDCIssuer, Audience: []string{"hive"}, ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)), NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)), IssuedAt: jwt.NewNumericDate(now)}}
}
func withAud(c actionsOIDCClaims, aud string) actionsOIDCClaims { c.Audience = []string{aud}; return c }
func withIss(c actionsOIDCClaims, iss string) actionsOIDCClaims { c.Issuer = iss; return c }
func withExp(c actionsOIDCClaims, exp time.Time) actionsOIDCClaims {
	c.ExpiresAt = jwt.NewNumericDate(exp)
	return c
}
func withNBF(c actionsOIDCClaims, nbf time.Time) actionsOIDCClaims {
	c.NotBefore = jwt.NewNumericDate(nbf)
	return c
}
func withRepo(c actionsOIDCClaims, repo string) actionsOIDCClaims   { c.Repository = repo; return c }
func withActor(c actionsOIDCClaims, actor string) actionsOIDCClaims { c.Actor = actor; return c }

func actionsTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
func signActionsToken(t *testing.T, key *rsa.PrivateKey, kid string, claims actionsOIDCClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func actionsJWKSJSON(t *testing.T, pub *rsa.PublicKey) []byte {
	t.Helper()
	e := big.NewInt(int64(pub.E)).Bytes()
	jwks := jwksDocument{Keys: []jwkKey{{Kty: "RSA", Use: "sig", Kid: actionsTestKid, Alg: "RS256", N: base64.RawURLEncoding.EncodeToString(pub.N.Bytes()), E: base64.RawURLEncoding.EncodeToString(e)}}}
	b, err := json.Marshal(jwks)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestActionsDispatchNeverEchoesPromptOnRefusal(t *testing.T) {
	key := actionsTestKey(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	deps := actionsTestDeps(t, key, now)
	deps.Config.GitHub.Actions.OIDC.Audience = "expected"
	s := NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.RegisterAPI(deps)
	code, body := postActionsDispatch(t, s, key, actionsTestClaims(now), actionsDispatchRequest{Command: "review", Prompt: "SECRET PROMPT", Issue: 42, RunID: "1", RunAttempt: "1"})
	if code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if strings.Contains(body, "SECRET PROMPT") {
		t.Fatalf("refusal echoed prompt: %s", body)
	}
}
