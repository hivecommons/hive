package claude

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// CredentialVerdict is what the provider said about a stored credential when
// asked directly (hivecommons/hive#18732).
//
// It exists because a credential FILE that is well-formed and unexpired is not
// a credential the provider still accepts. A revoked OAuth grant leaves the
// file byte-for-byte identical to a healthy one — access token, refresh token,
// future expiry — so every on-disk predicate (HasUsableToken, TokensUsable)
// keeps answering "valid" while the CLI shows `Please run /login`. Only a live
// call can tell the two apart.
type CredentialVerdict int

const (
	// CredentialInconclusive: nothing could be concluded. The access token has
	// aged out (the CLI redeems the refresh grant on its next start, so a stale
	// token would 401 whether or not the grant is alive — #5165), the provider
	// rate-limited or errored, or the network failed. Callers keep whatever
	// behaviour they had before asking.
	CredentialInconclusive CredentialVerdict = iota
	// CredentialAccepted: the provider served a request with the live access
	// token.
	CredentialAccepted
	// CredentialRejected: the provider answered HTTP 401 to a live, unexpired
	// access token. The grant is revoked or otherwise dead; no restart can
	// recover it and only an operator sign-in will.
	CredentialRejected
)

func (v CredentialVerdict) String() string {
	switch v {
	case CredentialAccepted:
		return "accepted"
	case CredentialRejected:
		return "rejected"
	default:
		return "inconclusive"
	}
}

const (
	// verifyBaseURL / verifyUsagePath / verifyOAuthBeta mirror the headroom
	// probe in pkg/rotation (ClaudeProber): the same read-only usage endpoint
	// the CLI's own OAuth session calls, which answers 401 to a dead grant.
	verifyBaseURL   = "https://api.anthropic.com"
	verifyUsagePath = "/api/oauth/usage"
	verifyOAuthBeta = "oauth-2025-04-20"
	// verifyTimeout bounds one live check; the caller polls every few seconds
	// and must never block on a hung provider.
	verifyTimeout = 10 * time.Second
)

// Verifier asks the provider whether the live access token in a credentials
// file is still accepted. The zero value talks to the production endpoint.
type Verifier struct {
	// BaseURL overrides the provider origin (tests).
	BaseURL string
	// Client overrides the HTTP client (tests); nil uses a bounded default.
	Client *http.Client
}

// Verify loads the credential at path and checks it against the provider.
// The returned detail is a short, redacted explanation safe to log: it names
// an HTTP status or a condition, never token material.
func (v Verifier) Verify(ctx context.Context, path string) (CredentialVerdict, string) {
	return v.verifyAt(ctx, path, time.Now())
}

func (v Verifier) verifyAt(ctx context.Context, path string, now time.Time) (CredentialVerdict, string) {
	tokens := LoadTokens(path)
	if tokens == nil || tokens.AccessToken == "" {
		return CredentialInconclusive, "no access token on disk"
	}
	if expired(tokens.ExpiresAt, now) {
		// Sending a stale access token manufactures a 401 that says nothing
		// about the refresh grant behind it (#5165). Inconclusive, on purpose.
		return CredentialInconclusive, "access token aged out; the CLI refreshes it on start"
	}
	base := v.BaseURL
	if base == "" {
		base = verifyBaseURL
	}
	client := v.Client
	if client == nil {
		client = &http.Client{Timeout: verifyTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+verifyUsagePath, nil)
	if err != nil {
		return CredentialInconclusive, "request build failed"
	}
	req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	req.Header.Set("anthropic-beta", verifyOAuthBeta)
	resp, err := client.Do(req)
	if err != nil {
		// The transport error may embed the URL but never the bearer header;
		// still, keep the detail to a fixed phrase so nothing unexpected is
		// ever spliced into a log line.
		return CredentialInconclusive, "provider unreachable"
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusOK:
		return CredentialAccepted, "claude usage HTTP 200"
	case resp.StatusCode == http.StatusUnauthorized:
		return CredentialRejected, fmt.Sprintf("claude usage HTTP %d", resp.StatusCode)
	default:
		// 403 is authorization, not authentication (see the troubleshooting
		// entry on "Please run /login" with API Error 403); 429/5xx are the
		// provider's problem. None of them convicts the credential.
		return CredentialInconclusive, fmt.Sprintf("claude usage HTTP %d", resp.StatusCode)
	}
}
