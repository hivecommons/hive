package claude

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hivecommons/hive/crux/sharedfile"
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

	// ProfileURL is the OAuth profile endpoint Claude Code itself reads right
	// after its own token exchange to learn the plan behind the new token
	// (organization.organization_type and organization.rate_limit_tier). See
	// FetchProfile.
	ProfileURL = "https://api.anthropic.com/api/oauth/profile"

	// ProfileFetchTimeout matches the 10s Claude Code allows the same call.
	ProfileFetchTimeout = 10 * time.Second
)

// profileEndpointOverride redirects FetchProfile away from ProfileURL. TEST
// SEAM ONLY, like tokenEndpointOverride: never set in production.
var profileEndpointOverride string

func profileEndpoint() string {
	if profileEndpointOverride != "" {
		return profileEndpointOverride
	}
	return ProfileURL
}

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
//
// RefreshTokenExpiresAt is carried even though hive never mints it: Claude
// Code writes it, and a struct that dropped the field would silently delete
// the only evidence that distinguishes "this login is over" from "this access
// token aged out and the next CLI start will mint a new one" — see
// HasUsableToken.
type OAuthTokens struct {
	AccessToken           string   `json:"accessToken"`
	RefreshToken          string   `json:"refreshToken,omitempty"`
	ExpiresAt             int64    `json:"expiresAt"`
	RefreshTokenExpiresAt int64    `json:"refreshTokenExpiresAt,omitempty"`
	Scopes                []string `json:"scopes"`
	SubscriptionType      string   `json:"subscriptionType,omitempty"`
	RateLimitTier         string   `json:"rateLimitTier,omitempty"`

	// SignInAccount records which account and organization the dashboard
	// sign-in minted this token for, as the token endpoint reported them. It
	// is hive's own key, read only by ApplyProfile: it lets a later sign-in
	// whose profile read fails tell whether the plan already on file belongs
	// to the same account. Claude Code ignores it, and drops it when it
	// rewrites the token block on its own refresh or sign-in, so a file the
	// CLI rewrote never vouches for a plan.
	SignInAccount *SignInAccount `json:"hiveSignInAccount,omitempty"`
}

// SignInAccount identifies the account and organization behind a token.
type SignInAccount struct {
	AccountUUID      string `json:"accountUuid"`
	OrganizationUUID string `json:"organizationUuid,omitempty"`
}

// sameAccount reports whether a and b positively name the same account and
// organization. A missing identity on either side is not a match.
func sameAccount(a, b *SignInAccount) bool {
	if a == nil || b == nil || a.AccountUUID == "" {
		return false
	}
	return a.AccountUUID == b.AccountUUID && a.OrganizationUUID == b.OrganizationUUID
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
	// Account and Organization identify who the token was minted for. Claude
	// Code reads the same two objects from this response (account.uuid,
	// organization.uuid) to record the signed-in account.
	Account *struct {
		UUID string `json:"uuid"`
	} `json:"account"`
	Organization *struct {
		UUID string `json:"uuid"`
	} `json:"organization"`
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

	out := &OAuthTokens{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().UnixMilli() + int64(tok.ExpiresIn)*1000,
		Scopes:       scopes,
	}
	if tok.Account != nil && tok.Account.UUID != "" {
		out.SignInAccount = &SignInAccount{AccountUUID: tok.Account.UUID}
		if tok.Organization != nil {
			out.SignInAccount.OrganizationUUID = tok.Organization.UUID
		}
	}
	return out, nil
}

// subscriptionTypes maps the profile's organization.organization_type to the
// subscriptionType value Claude Code stores in .credentials.json. It is the
// CLI's own table; any other organization type stores no subscriptionType.
var subscriptionTypes = map[string]string{
	"claude_max":        "max",
	"claude_pro":        "pro",
	"claude_enterprise": "enterprise",
	"claude_team":       "team",
}

// Profile is the plan behind an OAuth token, in the form Claude Code stores it.
type Profile struct {
	SubscriptionType string
	RateLimitTier    string
}

type profileResponse struct {
	Organization *struct {
		OrganizationType string `json:"organization_type"`
		RateLimitTier    string `json:"rate_limit_tier"`
	} `json:"organization"`
}

// FetchProfile reads the plan behind accessToken from the OAuth profile
// endpoint, the same call Claude Code makes after its own sign-in:
//
//	GET https://api.anthropic.com/api/oauth/profile
//	Authorization: Bearer <access token>
//
// subscriptionType is organization.organization_type mapped through the CLI's
// table (claude_max → max, …); rateLimitTier is organization.rate_limit_tier
// as returned. Without this, a dashboard sign-in writes a token with no plan
// and Claude Code labels the session "Claude API" / "API Usage Billing" even
// on a subscription. Errors never carry the token or the response body.
func FetchProfile(ctx context.Context, accessToken string) (Profile, error) {
	ctx, cancel := context.WithTimeout(ctx, ProfileFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, profileEndpoint(), nil)
	if err != nil {
		return Profile{}, fmt.Errorf("build profile request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Profile{}, fmt.Errorf("profile request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Profile{}, fmt.Errorf("profile request: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Profile{}, fmt.Errorf("read profile response: %w", err)
	}
	var pr profileResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return Profile{}, fmt.Errorf("parse profile response: %w", err)
	}
	if pr.Organization == nil {
		return Profile{}, fmt.Errorf("profile response has no organization")
	}
	return Profile{
		SubscriptionType: subscriptionTypes[pr.Organization.OrganizationType],
		RateLimitTier:    pr.Organization.RateLimitTier,
	}, nil
}

// ApplyProfile stamps the plan onto freshly exchanged tokens before they are
// written over previous (the token block currently on file, or nil).
//
// A successful read always wins. When the read failed, the plan on file is
// kept only if previous was signed in by hive for the same account and
// organization as tokens; otherwise the token is written with no plan, which
// is what a sign-in wrote before hive read the profile at all. It reports
// whether the previous plan was kept.
func ApplyProfile(tokens *OAuthTokens, prof Profile, fetchErr error, previous *OAuthTokens) bool {
	if tokens == nil {
		return false
	}
	if fetchErr == nil {
		tokens.SubscriptionType = prof.SubscriptionType
		tokens.RateLimitTier = prof.RateLimitTier
		return false
	}
	tokens.SubscriptionType, tokens.RateLimitTier = "", ""
	if previous == nil || !sameAccount(previous.SignInAccount, tokens.SignInAccount) {
		return false
	}
	tokens.SubscriptionType = previous.SubscriptionType
	tokens.RateLimitTier = previous.RateLimitTier
	return tokens.SubscriptionType != "" || tokens.RateLimitTier != ""
}

// ErrNoAccessToken is returned by WriteCredentials for a token set with no
// access token. Writing one would replace whatever login is on file with a
// signed-out credential.
var ErrNoAccessToken = errors.New("refusing to write a Claude credential with no access token")

// CredentialsFileMode is the mode of the shared credentials file: owner and
// group read/write, so every agent uid reaches it through the shared group.
const CredentialsFileMode = 0o660

// WriteCredentials atomically writes tokens to the credentials file. It
// refuses a nil token set or one with an empty access token
// (ErrNoAccessToken), so no hive caller can replace the shared login with a
// signed-out credential. The write is sharedfile.Replace: the credentials
// directory is shared with every agent uid, so the temporary name is random,
// metadata is set by descriptor, and a non-regular file at the destination or
// a directory that is not this process's own is refused rather than written
// through.
func WriteCredentials(tokens *OAuthTokens, path string) error {
	if tokens == nil || tokens.AccessToken == "" {
		return ErrNoAccessToken
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o2775); err != nil {
		return fmt.Errorf("create credentials dir: %w", err)
	}

	creds := Credentials{ClaudeAIOAuth: tokens}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	if err := removeDeadCredentialLink(path); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := sharedfile.Replace(path, data, CredentialsFileMode, sharedfile.Options{PreserveOwner: true}); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	return nil
}

// removeDeadCredentialLink unlinks a symlink at path that resolves to nothing
// (dangling, or a loop such as a link to itself, which an older bridge repair
// once left on live hives) so a sign-in can recover the shared login. A link
// that resolves to an existing file is left alone: sharedfile.Replace refuses
// it, because nothing hive writes may land through a link another uid planted.
func removeDeadCredentialLink(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	if _, err := os.Stat(path); err == nil || !(errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP)) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove dead credential link: %w", err)
	}
	return nil
}

// LoadTokens parses the credentials file and returns the stored token set, or
// nil when the file is absent, malformed, or carries no token block. It never
// applies an expiry rule — callers decide what a stale token means to them.
func LoadTokens(path string) *OAuthTokens {
	tokens, err := ReadTokens(path)
	if err != nil {
		return nil
	}
	return tokens
}

// ReadTokens is LoadTokens with the read error kept, for callers that must
// tell "this process cannot read the file" (fs.ErrNotExist, or
// fs.ErrPermission on another uid's 0600 credential) apart from "the file
// holds no usable token". A file that was read but is malformed, or carries
// no token block, returns (nil, nil). The error never carries file content.
func ReadTokens(path string) (*OAuthTokens, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseTokens(data), nil
}

// ParseTokens decodes a credentials file already in memory. Nil for malformed
// bytes or a file with no token block, like LoadTokens.
func ParseTokens(data []byte) *OAuthTokens {
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil
	}
	return creds.ClaudeAIOAuth
}

// expired reports whether a millisecond epoch stamp is in the past. A zero or
// negative stamp is "no expiry recorded", which is never treated as expired —
// fabricating an expiry from a missing one is how a working credential gets
// declared dead.
func expired(unixMilli int64, now time.Time) bool {
	return unixMilli > 0 && unixMilli < now.UnixMilli()
}

// ReadAccessToken reads the access token from the credentials file.
// Returns empty string if the file doesn't exist or is malformed.
//
// An EXPIRED access token reads as absent, deliberately: every caller of this
// function wants a string to put in an Authorization header, and sending an
// expired one manufactures a 401. Callers asking the different question — "can
// this credential still get an agent working?" — must use HasUsableToken.
func ReadAccessToken(path string) string {
	tokens := LoadTokens(path)
	if tokens == nil {
		return ""
	}
	if expired(tokens.ExpiresAt, time.Now()) {
		return ""
	}
	return tokens.AccessToken
}

// HasValidToken returns true if a valid, non-expired Claude token exists.
func HasValidToken(path string) bool {
	return ReadAccessToken(path) != ""
}

// HasUsableToken reports whether this credential can put an agent back to work
// WITHOUT a human completing an OAuth flow — either the access token is still
// live, or it has aged out but the refresh grant behind it has not.
//
// The distinction is not academic; it is the difference between the two
// recoveries hive can prescribe, and they are nothing alike:
//
//   - refreshable → RESTART THE CLI. Claude Code redeems the refresh token
//     when a process starts, so a relaunch mints a new access token from the
//     file already on disk. Measured on a live hive (2026-09-01): a credential
//     whose access token had expired eight hours earlier produced a working
//     agent on the first restart, with a refresh grant still 28 days from its
//     own expiry.
//   - not refreshable → OPERATOR LOGIN. Nothing on disk can mint a token; a
//     human has to authenticate.
//
// Claude access tokens live 8 hours (measured: mint-to-expiresAt on a live
// credential), so the refreshable state is the ROUTINE
// one — a hive whose agents run longer than half a day enters it daily. Every
// hive decision that reads "expired" as "logged out" therefore pages a human
// once a day for a credential that would have healed itself on a restart.
//
// Positive evidence only: an unreadable or absent file returns false, leaving
// callers exactly where they were before they asked.
func HasUsableToken(path string) bool {
	return TokensUsable(LoadTokens(path), time.Now())
}

// TokensUsable is HasUsableToken over an already-loaded token set, for callers
// that must judge two credentials side by side (the per-agent credential
// bridge repair compares an agent's private copy with the shared file). A nil
// token set is not usable.
func TokensUsable(tokens *OAuthTokens, now time.Time) bool {
	if tokens == nil {
		return false
	}
	if tokens.AccessToken != "" && !expired(tokens.ExpiresAt, now) {
		return true
	}
	return tokens.RefreshToken != "" && !expired(tokens.RefreshTokenExpiresAt, now)
}
