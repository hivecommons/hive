// Package linearagent makes hive a first-class agent in a Linear workspace
// (RFC #4492, Part 2).
//
// Linear's agent platform (https://linear.app/developers/agents) turns an
// OAuth application into an assignable, mentionable workspace member. This
// package owns the outbound half of that integration:
//
//   - the OAuth2 `actor=app` install flow (this file),
//   - the per-workspace token + app-user identity store (store.go),
//   - the control-plane GraphQL client that emits AgentActivities (client.go),
//   - the AgentSessionEvent webhook receiver (webhook.go),
//   - the latency-critical session responder (responder.go), and
//   - the session tracker the dashboard reads (session.go).
//
// The OAuth client is hand-rolled on net/http rather than pulling in
// golang.org/x/oauth2: the tree carries no oauth2 dependency today (checked
// go.mod), pkg/auth is inbound OIDC only, and Linear's flow is the plain
// authorization-code exchange — two form POSTs. Adding a dependency to save
// two POSTs is the same trade linear_rules.go declined for a GraphQL parser.
//
// SECURITY: the client secret and webhook signing secret come from the
// environment (LINEAR_CLIENT_ID / LINEAR_CLIENT_SECRET / LINEAR_WEBHOOK_SECRET)
// and are never written to hive.yaml, logs, or API responses. Tokens are
// persisted owner-only on /data (store.go). The authorize flow carries a
// single-use, TTL-bounded state token, verified on callback.
package linearagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// AuthorizeURL is Linear's OAuth2 authorization endpoint.
	AuthorizeURL = "https://linear.app/oauth/authorize"

	// TokenURL is Linear's OAuth2 token endpoint (exchange and refresh).
	TokenURL = "https://api.linear.app/oauth/token"

	// GraphQLURL is Linear's single GraphQL endpoint.
	GraphQLURL = "https://api.linear.app/graphql"

	// Scopes is what the RFC names for an agent install: read/write plus the
	// two agent scopes that make the app assignable and mentionable. Linear
	// wants a comma-separated list (not the space-separated OAuth2 default).
	Scopes = "read,write,app:assignable,app:mentionable"

	// clientIDEnv / clientSecretEnv are the OAuth application credentials.
	// Environment-only, never config: hive.yaml is exported, backed up, and
	// diffed in dashboards — a client secret must not ride along.
	clientIDEnv     = "LINEAR_CLIENT_ID"
	clientSecretEnv = "LINEAR_CLIENT_SECRET"

	// StateTTL bounds how long an authorize redirect stays redeemable. Long
	// enough for a human to click through Linear's consent screen, short
	// enough that a leaked URL goes stale.
	StateTTL = 10 * time.Minute

	// tokenHTTPTimeout bounds the token-endpoint POSTs.
	tokenHTTPTimeout = 30 * time.Second

	// oauthResponseLimit caps how much of a token response is read.
	oauthResponseLimit = 1 << 20
)

// Credentials are the OAuth application's client id and secret.
type Credentials struct {
	ClientID     string
	ClientSecret string
}

// CredentialsFromEnv reads the OAuth application credentials from the
// environment. Configured() on the result reports whether both are present.
func CredentialsFromEnv() Credentials {
	return Credentials{
		ClientID:     strings.TrimSpace(os.Getenv(clientIDEnv)),
		ClientSecret: strings.TrimSpace(os.Getenv(clientSecretEnv)),
	}
}

// Configured reports whether both halves of the credential are present.
func (c Credentials) Configured() bool {
	return c.ClientID != "" && c.ClientSecret != ""
}

// BuildAuthorizeURL builds the `actor=app` authorization URL for this
// application. `prompt=consent` is always sent so an operator reconnecting a
// workspace (or connecting a second one after a reinstall) is shown the
// consent screen instead of being silently bounced through.
func BuildAuthorizeURL(clientID, redirectURI, state string) string {
	q := url.Values{
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {Scopes},
		"actor":         {"app"},
		"prompt":        {"consent"},
		"state":         {state},
	}
	return AuthorizeURL + "?" + q.Encode()
}

// Token is one workspace's OAuth grant. Linear access tokens expire (24h) and
// are paired with a refresh token; ExpiresAt is computed from expires_in at
// receipt so the client can refresh ahead of the deadline.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	Scope        string    `json:"scope,omitempty"`
}

// tokenResponse is the wire shape of Linear's token endpoint.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	// Scope is a string on current apps; apps created before Dec 2023 return
	// an array. RawMessage tolerates both without failing the exchange.
	Scope            json.RawMessage `json:"scope"`
	Error            string          `json:"error"`
	ErrorDescription string          `json:"error_description"`
}

// ExchangeCode exchanges an authorization code for a Token at tokenURL
// (TokenURL in production; a test server otherwise).
func ExchangeCode(ctx context.Context, hc *http.Client, tokenURL string, creds Credentials, code, redirectURI string) (Token, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {creds.ClientID},
		"client_secret": {creds.ClientSecret},
	}
	return postTokenForm(ctx, hc, tokenURL, form)
}

// RefreshToken redeems a refresh token for a fresh Token.
func RefreshToken(ctx context.Context, hc *http.Client, tokenURL string, creds Credentials, refreshToken string) (Token, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {creds.ClientID},
		"client_secret": {creds.ClientSecret},
	}
	return postTokenForm(ctx, hc, tokenURL, form)
}

// postTokenForm runs one form POST against the token endpoint and normalizes
// the response into a Token. Errors never include token material — only
// Linear's error code/description and the HTTP status.
func postTokenForm(ctx context.Context, hc *http.Client, tokenURL string, form url.Values) (Token, error) {
	if hc == nil {
		hc = &http.Client{Timeout: tokenHTTPTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := hc.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("token endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, oauthResponseLimit))
	if err != nil {
		return Token{}, fmt.Errorf("read token response: %w", err)
	}

	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return Token{}, fmt.Errorf("token endpoint status %d: unparseable response", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		msg := tr.Error
		if tr.ErrorDescription != "" {
			msg = tr.Error + ": " + tr.ErrorDescription
		}
		if msg == "" {
			msg = "no access token in response"
		}
		return Token{}, fmt.Errorf("token endpoint status %d: %s", resp.StatusCode, msg)
	}

	tok := Token{AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken, Scope: scopeString(tr.Scope)}
	if tr.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return tok, nil
}

// scopeString normalizes Linear's scope field, which is a string on current
// apps and a JSON array on pre-Dec-2023 apps.
func scopeString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return strings.Join(arr, ",")
	}
	return ""
}

// StateStore is an in-memory, single-use, TTL-bounded store of pending
// authorize states, modeled on pkg/openrouter's. The state token is the only
// thing binding a callback to a flow this server started, so it must be
// unguessable (crypto/rand), unexpired, and consumed exactly once.
type StateStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	pending map[string]time.Time
	clock   func() time.Time
}

// NewStateStore returns a StateStore with the given TTL (use StateTTL in
// production).
func NewStateStore(ttl time.Duration) *StateStore {
	return &StateStore{ttl: ttl, pending: make(map[string]time.Time), clock: time.Now}
}

// SetClock overrides the store's clock. Tests only.
func (s *StateStore) SetClock(f func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = f
}

// Create mints a new single-use state token.
func (s *StateStore) Create() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	state := hex.EncodeToString(buf)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	s.pending[state] = s.clock().Add(s.ttl)
	return state, nil
}

// Consume redeems a state token. It returns false for an unknown, expired, or
// already-consumed state — all three must be indistinguishable to the caller,
// and all three mean "do not trust this callback".
func (s *StateStore) Consume(state string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline, ok := s.pending[state]
	if !ok {
		return false
	}
	delete(s.pending, state)
	return s.clock().Before(deadline)
}

func (s *StateStore) evictExpiredLocked() {
	now := s.clock()
	for k, deadline := range s.pending {
		if !now.Before(deadline) {
			delete(s.pending, k)
		}
	}
}
