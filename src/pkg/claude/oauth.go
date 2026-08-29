package claude

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// AuthorizeURL is the Claude OAuth authorization endpoint.
	// Uses claude.com/cai/oauth path which supports the hosted code callback.
	AuthorizeURL = "https://claude.com/cai/oauth/authorize"

	// TokenURL is the Claude OAuth token endpoint, on the same host
	// as the hosted code callback page.
	TokenURL = "https://platform.claude.com/v1/oauth/token"

	// ClientID is the public Claude Code OAuth client identifier (UUID).
	ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	// DefaultScopes are the scopes requested for agent authentication.
	DefaultScopes = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"

	// CredentialsPath is where Claude Code stores its credentials on the pod.
	CredentialsPath = "/data/home/.claude/.credentials.json"

	// PKCEVerifierLength is the byte length of the PKCE code verifier (produces 43-char base64url).
	PKCEVerifierLength = 32

	// TokenExchangeTimeout limits how long we wait for the token endpoint,
	// which can take 40-60s to respond.
	TokenExchangeTimeout = 120 * time.Second
)

// tokenEndpointOverride redirects the token exchange away from the real Claude
// endpoint. TEST SEAM ONLY: it is never set in production, where every exchange
// goes to TokenURL. It exists so ExchangeCode's response handling — error
// payloads, empty tokens, scope defaulting — can be exercised without a live
// OAuth round trip.
var tokenEndpointOverride string

func tokenEndpoint() string {
	if tokenEndpointOverride != "" {
		return tokenEndpointOverride
	}
	return TokenURL
}

// OAuthState holds the server-side state for an in-progress PKCE authorization flow.
type OAuthState struct {
	CodeVerifier string    `json:"-"`
	AuthorizeURL string    `json:"authorize_url"`
	ExpiresAt    time.Time `json:"-"`
}

// Credentials is the on-disk format Claude Code expects in .credentials.json.
type Credentials struct {
	ClaudeAIOAuth *OAuthTokens `json:"claudeAiOauth,omitempty"`
}

// OAuthTokens holds the token set stored inside the credentials file.
type OAuthTokens struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken,omitempty"`
	ExpiresAt        int64    `json:"expiresAt"`
	Scopes           []string `json:"scopes"`
	SubscriptionType string   `json:"subscriptionType,omitempty"`
	RateLimitTier    string   `json:"rateLimitTier,omitempty"`
}

// tokenResponse is the raw response from the Claude token endpoint.
type tokenResponse struct {
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresIn    int             `json:"expires_in"`
	TokenType    string          `json:"token_type"`
	Scope        string          `json:"scope"`
	Error        json.RawMessage `json:"error"`
	ErrorDesc    string          `json:"error_description"`
}

func (t *tokenResponse) ErrorString() string {
	if len(t.Error) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(t.Error, &s) == nil {
		return s
	}
	return string(t.Error)
}

// GeneratePKCE creates a code_verifier and its S256 code_challenge.
func GeneratePKCE() (verifier, challenge string, err error) {
	buf := make([]byte, PKCEVerifierLength)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

// BuildAuthorizeURL constructs the full authorization URL with PKCE parameters.
func BuildAuthorizeURL(codeChallenge, redirectURI, state string) string {
	v := url.Values{
		"code":                  {"true"},
		"client_id":             {ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"scope":                 {DefaultScopes},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return AuthorizeURL + "?" + v.Encode()
}

// ExchangeCode trades an authorization code for access + refresh tokens using PKCE.
// The hosted callback page displays the code as "<code>#<state>" — split it
// and send both parts, matching what the Claude CLI does on paste.
func ExchangeCode(code, codeVerifier, redirectURI string) (*OAuthTokens, error) {
	state := ""
	if idx := strings.Index(code, "#"); idx >= 0 {
		state = code[idx+1:]
		code = code[:idx]
	}
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"state":         {state},
		"redirect_uri":  {redirectURI},
		"client_id":     {ClientID},
		"code_verifier": {codeVerifier},
	}

	client := &http.Client{Timeout: TokenExchangeTimeout}
	resp, err := client.PostForm(tokenEndpoint(), data)
	if err != nil {
		return nil, fmt.Errorf("token exchange request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}

	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	if errStr := tok.ErrorString(); errStr != "" {
		return nil, fmt.Errorf("token error: %s — %s", errStr, tok.ErrorDesc)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("empty access token in response")
	}

	scopes := strings.Fields(tok.Scope)
	if len(scopes) == 0 {
		scopes = strings.Fields(DefaultScopes)
	}

	return &OAuthTokens{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().UnixMilli() + int64(tok.ExpiresIn)*1000,
		Scopes:       scopes,
	}, nil
}

// WriteCredentials atomically writes tokens to the credentials file.
func WriteCredentials(tokens *OAuthTokens, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o2775); err != nil {
		return fmt.Errorf("create credentials dir: %w", err)
	}

	creds := Credentials{ClaudeAIOAuth: tokens}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o660); err != nil {
		return fmt.Errorf("write temp credentials: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename credentials: %w", err)
	}
	return nil
}

// ReadAccessToken reads the access token from the credentials file.
// Returns empty string if the file doesn't exist or is malformed.
func ReadAccessToken(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return ""
	}
	if creds.ClaudeAIOAuth == nil {
		return ""
	}
	if creds.ClaudeAIOAuth.ExpiresAt > 0 && creds.ClaudeAIOAuth.ExpiresAt < time.Now().UnixMilli() {
		return ""
	}
	return creds.ClaudeAIOAuth.AccessToken
}

// HasValidToken returns true if a valid, non-expired Claude token exists.
func HasValidToken(path string) bool {
	return ReadAccessToken(path) != ""
}
