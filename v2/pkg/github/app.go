package github

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	gh "github.com/google/go-github/v72/github"
)

const (
	jwtExpiry          = 10 * time.Minute
	tokenRefreshBuffer = 20 * time.Minute
	TokenCachePath     = "/var/run/hive-metrics/gh-app-token.cache"
	DocsTokenCachePath = "/var/run/hive-metrics/gh-app-token-docs.cache"
	tokenCachePerms    = 0o640

	// Per-agent scoped token caches — only the owning agent UID can read.
	agentTokenCacheDir   = "/var/run/hive-metrics/agent-tokens"
	agentTokenCachePerms = 0o600
)

type AppAuth struct {
	appID          int64
	installationID int64
	key            *rsa.PrivateKey
	logger         *slog.Logger
	cachePath      string
	apiURL         string // custom GitHub API URL for GHE; empty means default (github.com)

	mu          sync.RWMutex
	cachedToken string
	tokenExpiry time.Time
}

func NewAppAuth(appID, installationID int64, keyFile string, logger *slog.Logger, apiURL string) (*AppAuth, error) {
	return NewAppAuthWithCache(appID, installationID, keyFile, TokenCachePath, logger, apiURL)
}

func NewAppAuthWithCache(appID, installationID int64, keyFile, cachePath string, logger *slog.Logger, apiURL string) (*AppAuth, error) {
	keyData, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("reading app key %s: %w", keyFile, err)
	}

	block, _ := pem.Decode(keyData)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in %s", keyFile)
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		pkcs8Key, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, fmt.Errorf("parsing private key: PKCS1 error: %w, PKCS8 error: %w", err, err2)
		}
		var ok bool
		key, ok = pkcs8Key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS8 key is not RSA")
		}
	}

	return &AppAuth{
		appID:          appID,
		installationID: installationID,
		key:            key,
		logger:         logger,
		cachePath:      cachePath,
		apiURL:         apiURL,
	}, nil
}

func (a *AppAuth) generateJWT() (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(jwtExpiry)),
		Issuer:    fmt.Sprintf("%d", a.appID),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(a.key)
}

func (a *AppAuth) Token(ctx context.Context) (string, error) {
	a.mu.RLock()
	if a.cachedToken != "" && time.Now().Before(a.tokenExpiry.Add(-tokenRefreshBuffer)) {
		token := a.cachedToken
		a.mu.RUnlock()
		return token, nil
	}
	a.mu.RUnlock()

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cachedToken != "" && time.Now().Before(a.tokenExpiry.Add(-tokenRefreshBuffer)) {
		return a.cachedToken, nil
	}

	jwtToken, err := a.generateJWT()
	if err != nil {
		return "", fmt.Errorf("generating JWT: %w", err)
	}

	jwtClient := gh.NewClient(nil).WithAuthToken(jwtToken)
	setBaseURL(jwtClient, a.apiURL)
	installToken, _, err := jwtClient.Apps.CreateInstallationToken(ctx, a.installationID, nil)
	if err != nil {
		return "", fmt.Errorf("creating installation token: %w", err)
	}

	a.cachedToken = installToken.GetToken()
	a.tokenExpiry = installToken.GetExpiresAt().Time
	a.logger.Info("github app token refreshed",
		"expires_at", a.tokenExpiry.Format(time.RFC3339),
		"installation_id", a.installationID,
	)

	tmpCache := a.cachePath + ".tmp"
	if err := os.WriteFile(tmpCache, []byte(a.cachedToken), tokenCachePerms); err != nil {
		a.logger.Warn("failed to write token cache", "path", a.cachePath, "error", err)
	} else if err := os.Rename(tmpCache, a.cachePath); err != nil {
		a.logger.Warn("failed to rename token cache", "error", err)
	}

	return a.cachedToken, nil
}

// ScopedToken creates a short-lived installation token with permissions
// scoped to a contributor's trust tier. Unlike Token(), this is NOT
// cached — each call creates a fresh token.
func (a *AppAuth) ScopedToken(ctx context.Context, tier string) (string, error) {
	jwtToken, err := a.generateJWT()
	if err != nil {
		return "", fmt.Errorf("generating JWT: %w", err)
	}

	var perms *gh.InstallationPermissions
	switch tier {
	case "newcomer":
		// Newcomers can comment on issues but not access code
		perms = &gh.InstallationPermissions{
			Issues: gh.Ptr("write"),
		}
	case "contributor":
		perms = &gh.InstallationPermissions{
			Issues:       gh.Ptr("write"),
			Contents:     gh.Ptr("write"),
			PullRequests: gh.Ptr("write"),
			Metadata:     gh.Ptr("read"),
		}
	case "trusted":
		perms = &gh.InstallationPermissions{
			Issues:       gh.Ptr("write"),
			Contents:     gh.Ptr("write"),
			PullRequests: gh.Ptr("write"),
			Checks:       gh.Ptr("read"),
			Metadata:     gh.Ptr("read"),
		}
	case "advisor":
		// Advisors review agent PRs — they only need to read, not write.
		// Don't request issues permission at all to prevent creation.
		perms = &gh.InstallationPermissions{
			Metadata:     gh.Ptr("read"),
			PullRequests: gh.Ptr("read"),
		}
	default:
		perms = &gh.InstallationPermissions{
			Metadata: gh.Ptr("read"),
		}
	}

	opts := &gh.InstallationTokenOptions{Permissions: perms}
	jwtClient := gh.NewClient(nil).WithAuthToken(jwtToken)
	setBaseURL(jwtClient, a.apiURL)
	installToken, _, err := jwtClient.Apps.CreateInstallationToken(ctx, a.installationID, opts)
	if err != nil {
		return "", fmt.Errorf("creating scoped token for tier %s: %w", tier, err)
	}

	a.logger.Info("scoped token minted", "tier", tier, "expires_at", installToken.GetExpiresAt().Format(time.RFC3339))
	return installToken.GetToken(), nil
}

// AgentTokenCachePath returns the per-agent token cache file path.
func AgentTokenCachePath(agentName string) string {
	return agentTokenCacheDir + "/gh-token-" + agentName + ".cache"
}

// WriteAgentToken mints a scoped token for the given tier and writes it
// to a per-agent cache file owned by agentUID with 0600 perms. The hive
// binary (UID 1001) creates the file then chowns it to the agent UID so
// only that agent can read it.
func (a *AppAuth) WriteAgentToken(ctx context.Context, agentName, tier string, agentUID int) error {
	token, err := a.ScopedToken(ctx, tier)
	if err != nil {
		return fmt.Errorf("minting scoped token for %s: %w", agentName, err)
	}

	if err := os.MkdirAll(agentTokenCacheDir, 0o755); err != nil {
		return fmt.Errorf("creating agent token dir: %w", err)
	}

	cachePath := AgentTokenCachePath(agentName)
	tmpPath := cachePath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(token), agentTokenCachePerms); err != nil {
		return fmt.Errorf("writing agent token cache: %w", err)
	}

	if agentUID > 0 {
		if err := os.Chown(tmpPath, agentUID, -1); err != nil {
			a.logger.Warn("chown agent token cache failed — agent will use shared cache",
				"agent", agentName, "uid", agentUID, "error", err)
			os.Remove(tmpPath)
			return fmt.Errorf("chown agent token: %w", err)
		}
	}

	if err := os.Rename(tmpPath, cachePath); err != nil {
		return fmt.Errorf("rename agent token cache: %w", err)
	}

	a.logger.Info("per-agent token cached", "agent", agentName, "tier", tier, "uid", agentUID)
	return nil
}

// InstallationInfo describes the GitHub App installation this hive is
// authenticating as, resolved live from the GitHub API via the app JWT.
type InstallationInfo struct {
	InstallationID int64
	Account        string // login of the org/user the installation belongs to
	IssuesPerm     string // granted issues permission: "read", "write", or "" (none)
}

// VerifyInstallation resolves the configured installation and returns the
// account it belongs to plus the granted issues permission. A successful
// token mint alone does not prove write access: a stale installation_id can
// mint valid tokens for a DIFFERENT org (public reads still work, writes 403
// with "Resource not accessible by integration"), and an installation can be
// granted less than the app requests until the org owner approves.
func (a *AppAuth) VerifyInstallation(ctx context.Context) (*InstallationInfo, error) {
	jwtToken, err := a.generateJWT()
	if err != nil {
		return nil, fmt.Errorf("generating JWT: %w", err)
	}

	jwtClient := gh.NewClient(nil).WithAuthToken(jwtToken)
	setBaseURL(jwtClient, a.apiURL)
	inst, _, err := jwtClient.Apps.GetInstallation(ctx, a.installationID)
	if err != nil {
		return nil, fmt.Errorf("resolving installation %d: %w", a.installationID, err)
	}

	return &InstallationInfo{
		InstallationID: a.installationID,
		Account:        inst.GetAccount().GetLogin(),
		IssuesPerm:     inst.GetPermissions().GetIssues(),
	}, nil
}

type appTransport struct {
	auth *AppAuth
	base http.RoundTripper
}

func (t *appTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := t.auth.Token(req.Context())
	if err != nil {
		return nil, fmt.Errorf("getting app token: %w", err)
	}

	req2 := req.Clone(req.Context())
	req2.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(req2)
}

func NewClientFromApp(auth *AppAuth, org string, repos []string, logger *slog.Logger) *Client {
	transport := &appTransport{
		auth: auth,
		base: http.DefaultTransport,
	}
	const appClientTimeout = 30 * time.Second
	httpClient := &http.Client{Transport: transport, Timeout: appClientTimeout}
	client := gh.NewClient(httpClient)
	setBaseURL(client, auth.apiURL)

	return &Client{
		client:  client,
		org:     org,
		repos:   repos,
		logger:  logger,
		appAuth: auth,
	}
}
