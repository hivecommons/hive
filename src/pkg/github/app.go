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
	// tokenCachePerms guards the SHARED, FULL-privilege installation-token cache
	// (TokenCachePath / DocsTokenCachePath). It MUST stay owner-only (0600): the
	// hive process runs as the "dev" user, and every agent UID is a member of the
	// shared "node" group, so anything group-readable here would hand every agent
	// the full-privilege installation token and defeat the per-agent scoped-token
	// scheme entirely (audit H3, CWE-522/732). Per-agent scoped caches are a
	// SEPARATE path and use agentTokenCachePerms — do not conflate the two.
	tokenCachePerms = 0o600

	// tokenMintTimeout hard-bounds every live App-JWT → installation-token
	// exchange (CreateInstallationToken / GetInstallation). These calls are made
	// with a bare gh.NewClient (no per-client Timeout) and are sometimes reached
	// with an undeadlined process context (startup advisory-issue ensure, docs
	// token refresh, appTransport.RoundTrip). On a firewalled / GHE-hosted spoke
	// whose App is not yet installed (github.ibm.com unreachable, or the org has
	// no installation) the underlying TCP/TLS connect can hang for minutes.
	//
	// That hang is the #2439 root cause: it froze the FIRST heartbeat attempt
	// and blocked readiness (advisory-issue ensure runs before MarkReady), so
	// livez/health deadline-exceeded, the pod crash-looped, and the hub stayed
	// stuck "Upgrading" (it never received a fresh heartbeat with the new
	// version). Bounding the mint turns "hang forever" into a fast, soft error
	// the caller already treats as "App not usable → show install banner".
	tokenMintTimeout = 8 * time.Second

	// Per-agent scoped token caches. entrypoint.sh pre-creates each file as
	// dev:hive-<agent> mode 0640 so the hive process (dev) can rewrite it in
	// place and ONLY the owning agent's private group can read it — no chown
	// at runtime, which an unprivileged process cannot perform.
	defaultAgentTokenCacheDir = "/var/run/hive-metrics/agent-tokens"
	// Applied only when the file was NOT pre-created (degraded path): keep it
	// owner-only rather than leaking a token to the shared "node" group.
	agentTokenCachePerms = 0o600
	// Directory must be traversable by every agent UID so each can open its
	// own 0640 file; the files themselves carry the per-agent restriction.
	agentTokenCacheDirPerms = 0o755

	// botLoginFileName is the trusted bot-identity file gh-wrapper.sh's author
	// gate reads (#4044). App installation tokens have no /user identity —
	// `gh api user` 403s for every staff agent — so the wrapper cannot resolve
	// "who am I" from the token itself. This process DOES know: it mints every
	// tier token as "<app-slug>[bot]". Publishing that login into the
	// agent-token cache dir (dev-owned, mode 0755 — no agent UID can write it)
	// gives the wrapper an identity oracle the agent cannot spoof, preserving
	// the #3982 fail-closed/anti-spoof posture while making author-gated
	// self-listing work again.
	botLoginFileName = "gh-bot-login"
	// botLoginFilePerms: world-readable on purpose — the bot login is public
	// metadata (it appears on every PR the App authors), and every agent UID
	// must be able to read it. The security property is UNWRITABILITY (dir
	// owned by dev), not secrecy.
	botLoginFilePerms = 0o644
)

// agentTokenCacheDir is the directory holding per-agent token caches. It is a
// variable rather than a constant solely so tests can redirect it to a temp
// dir; production code never reassigns it.
var agentTokenCacheDir = defaultAgentTokenCacheDir

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
	// botLogin is the App bot identity ("<app-slug>[bot]") this installation's
	// tokens act as. Set via SetBotLogin at client wiring time; when non-empty,
	// WriteAgentToken publishes it to the trusted bot-identity file (#4044).
	botLogin string
}

func NewAppAuth(appID, installationID int64, keyFile string, logger *slog.Logger, apiURL string) (*AppAuth, error) {
	return NewAppAuthWithCache(appID, installationID, keyFile, TokenCachePath, logger, apiURL)
}

func NewAppAuthFromPEM(appID, installationID int64, keyData []byte, logger *slog.Logger, apiURL string) (*AppAuth, error) {
	block, _ := pem.Decode(keyData)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
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
		apiURL:         apiURL,
	}, nil
}

func NewAppAuthWithCache(appID, installationID int64, keyFile, cachePath string, logger *slog.Logger, apiURL string) (*AppAuth, error) {
	keyData, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("reading app key %s: %w", keyFile, err)
	}

	auth, err := NewAppAuthFromPEM(appID, installationID, keyData, logger, apiURL)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyFile, err)
	}
	auth.cachePath = cachePath
	return auth, nil
}

// mintContext derives a child context bounded by tokenMintTimeout from the
// caller's context. It guarantees a deadline even when the caller passed a
// background/process context with none, so a token mint is a bounded operation
// end to end. The returned cancel MUST be called by the caller.
func mintContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, tokenMintTimeout)
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

	// We are inside the tokenRefreshBuffer window (or have no token yet): try to
	// PROACTIVELY mint a fresh token. A GitHub installation token is minted with
	// a live network call to CreateInstallationToken, and on GHE-adjacent /
	// firewalled clusters that call intermittently fails (transient 5xx, TLS
	// blip, network hiccup). Crucially, a proactive-refresh failure is NOT an
	// auth failure while the currently-cached token is still valid: the buffer
	// only exists to refresh EARLY, so a blip here must fall back to the good
	// cached token rather than surface an error. Erroring here is exactly what
	// made the dashboard's github_auth health check flap red↔green on a hive
	// whose App is genuinely working. We only hard-fail once no usable token
	// remains — i.e. the cache is empty or the cached token has truly expired.
	newToken, expiry, err := a.mintInstallationToken(ctx)
	if err != nil {
		if a.cachedToken != "" && time.Now().Before(a.tokenExpiry) {
			a.logger.Warn("github app token proactive refresh failed; serving still-valid cached token",
				"expires_at", a.tokenExpiry.Format(time.RFC3339),
				"installation_id", a.installationID,
				"error", err,
			)
			return a.cachedToken, nil
		}
		return "", err
	}

	a.cachedToken = newToken
	a.tokenExpiry = expiry
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

// mintInstallationToken performs the live GitHub call that exchanges an
// App JWT for an installation token. It is the network step shared by the
// cache-refresh path in Token(); it does NOT touch the cache or take the
// mutex (callers already hold a.mu). It is the only failure-prone step in
// Token(), so isolating it lets Token() distinguish "mint blipped" (fall back
// to a still-valid cached token) from a genuine expiry.
func (a *AppAuth) mintInstallationToken(ctx context.Context) (token string, expiry time.Time, err error) {
	return a.mintInstallationTokenWithOptions(ctx, nil)
}

// MintInstallationToken returns a short-lived installation token constrained by
// opts. It intentionally bypasses the shared full-permission token cache used by
// Token(), so hub-hosted lite scans can request read-only or issues-only tokens
// without ever widening an agent-visible credential.
func (a *AppAuth) MintInstallationToken(ctx context.Context, opts *gh.InstallationTokenOptions) (token string, expiry time.Time, err error) {
	if a == nil {
		return "", time.Time{}, ErrNoGitHubClient
	}
	return a.mintInstallationTokenWithOptions(ctx, opts)
}

func (a *AppAuth) mintInstallationTokenWithOptions(ctx context.Context, opts *gh.InstallationTokenOptions) (token string, expiry time.Time, err error) {
	jwtToken, err := a.generateJWT()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generating JWT: %w", err)
	}

	// Proxy-trusting client: on a fresh-PVC boot with forced egress this call is
	// MITM'd by the in-process proxy, whose CA must be trusted for the mint to
	// succeed. See proxytrust.go for the chicken-and-egg this breaks.
	mintCtx, cancel := mintContext(ctx)
	defer cancel()
	jwtClient := newJWTClient(jwtToken, a.apiURL)
	installToken, _, err := jwtClient.Apps.CreateInstallationToken(mintCtx, a.installationID, opts)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("creating installation token: %w", err)
	}
	return installToken.GetToken(), installToken.GetExpiresAt().Time, nil
}

// ScopedToken creates a short-lived installation token with permissions
// scoped to a contributor's trust tier. Unlike Token(), this is NOT
// cached — each call creates a fresh token.
func (a *AppAuth) ScopedToken(ctx context.Context, tier string) (string, error) {
	return a.ScopedTokenForRepos(ctx, tier, nil)
}

// ScopedTokenForRepos creates a short-lived installation token with permissions
// scoped to a contributor's trust tier AND, when repos is non-empty, restricted to
// those repositories within the installation (kubestellar/hive C4). Restricting the
// repository scope means a contributor relay's credential can only touch the single
// repo its assigned issue lives in, not every repo the installation covers — the
// difference between a least-privilege, per-task credential and an
// installation-wide one. repos are bare repository NAMES (not "owner/repo"); an
// empty/nil slice yields the tier-scoped, installation-wide token (unchanged
// behavior for callers that do not scope by repo). Like ScopedToken, the token is
// NOT cached — each call mints fresh.
func (a *AppAuth) ScopedTokenForRepos(ctx context.Context, tier string, repos []string) (string, error) {
	jwtToken, err := a.generateJWT()
	if err != nil {
		return "", fmt.Errorf("generating JWT: %w", err)
	}

	var perms *gh.InstallationPermissions
	switch tier {
	case "newcomer":
		// Newcomers (ISSUES_ONLY agents) can comment on and file issues, and
		// READ code — filing a meaningful issue requires reading the repo it
		// is about (#4289). Write remains impossible: contents:write is
		// absent, so any push authenticates but is rejected server-side by
		// GitHub with 403.
		perms = &gh.InstallationPermissions{
			Issues:   gh.Ptr("write"),
			Contents: gh.Ptr("read"),
			Metadata: gh.Ptr("read"),
		}
	case "contributor":
		perms = &gh.InstallationPermissions{
			Issues:       gh.Ptr("write"),
			Contents:     gh.Ptr("write"),
			PullRequests: gh.Ptr("write"),
			Metadata:     gh.Ptr("read"),
		}
	case "trusted", "merger":
		perms = &gh.InstallationPermissions{
			Issues:       gh.Ptr("write"),
			Contents:     gh.Ptr("write"),
			PullRequests: gh.Ptr("write"),
			Checks:       gh.Ptr("read"),
			Metadata:     gh.Ptr("read"),
			// Workflows:write lets trusted-tier agents push branches that
			// touch .github/workflows (#4677) — without it GitHub rejects
			// such pushes server-side ("refusing to allow a GitHub App to
			// create or update workflow ... without workflows permission").
			// A requested permission must be a subset of what the App
			// installation grants, so if the installation has not accepted
			// Workflows read & write yet, the mint below falls back to a
			// token without it and logs the missing grant instead of
			// breaking every trusted-tier mint.
			Workflows: gh.Ptr("write"),
		}
	case "advisor":
		// Advisors review agent PRs and audit repo contents — their core
		// function is READING the repo, so Contents:read is required (#4289:
		// without it every contents/tarball API call and git fetch 403s no
		// matter what the installation grants). They must not write:
		// contents:write is absent, so pushes are rejected server-side by
		// GitHub. Don't request issues permission at all to prevent creation.
		perms = &gh.InstallationPermissions{
			Contents:     gh.Ptr("read"),
			Metadata:     gh.Ptr("read"),
			PullRequests: gh.Ptr("read"),
		}
	default:
		perms = &gh.InstallationPermissions{
			Metadata: gh.Ptr("read"),
		}
	}

	opts := &gh.InstallationTokenOptions{Permissions: perms}
	// C4: when the caller named repositories, restrict the token to them so it
	// cannot reach any other repo the installation covers. Repositories is keyed on
	// the bare repo name within the installation's org.
	if len(repos) > 0 {
		opts.Repositories = repos
	}
	mintCtx, cancel := mintContext(ctx)
	defer cancel()
	jwtClient := newJWTClient(jwtToken, a.apiURL)
	installToken, _, err := jwtClient.Apps.CreateInstallationToken(mintCtx, a.installationID, opts)
	if err != nil && perms.Workflows != nil {
		// The installation has likely not accepted the Workflows permission
		// on the App yet — GitHub refuses to mint a token requesting a
		// permission the installation does not grant. Degrade honestly
		// rather than failing every trusted-tier mint: retry without
		// workflows and surface exactly what the operator must flip (#4677).
		a.logger.Warn("scoped token mint with workflows:write failed; retrying without it — "+
			"pushes touching .github/workflows will be rejected until the App grants Workflows read & write "+
			"(App settings → Permissions → Workflows → Read and write, then re-accept on the installation; see docs/github-app-setup.md)",
			"tier", tier, "error", err)
		perms.Workflows = nil
		retryCtx, retryCancel := mintContext(ctx)
		defer retryCancel()
		installToken, _, err = jwtClient.Apps.CreateInstallationToken(retryCtx, a.installationID, opts)
	}
	if err != nil {
		return "", fmt.Errorf("creating scoped token for tier %s: %w", tier, err)
	}

	a.logger.Info("scoped token minted", "tier", tier, "repos", len(repos), "expires_at", installToken.GetExpiresAt().Format(time.RFC3339))
	return installToken.GetToken(), nil
}

// AgentTokenCachePath returns the per-agent token cache file path.
func AgentTokenCachePath(agentName string) string {
	return agentTokenCacheDir + "/gh-token-" + agentName + ".cache"
}

// BotLoginFilePath returns the trusted bot-identity file path read by
// gh-wrapper.sh's author gate (#4044).
func BotLoginFilePath() string {
	return agentTokenCacheDir + "/" + botLoginFileName
}

// SetBotLogin records the App bot login ("<app-slug>[bot]") so token delivery
// can publish it to the trusted bot-identity file. Empty (no usable App)
// leaves the file unwritten and the wrapper's author gate fail-closed.
func (a *AppAuth) SetBotLogin(login string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.botLogin = login
}

// publishBotLogin writes the trusted bot-identity file the gh wrapper's author
// gate reads (#4044). Failure is deliberately SOFT (warn, never error): the
// file only widens what an agent can list; without it the gate stays exactly
// as fail-closed as before this file existed, whereas failing token delivery
// over it would take down every GitHub operation to protect a listing.
func (a *AppAuth) publishBotLogin() {
	a.mu.RLock()
	login := a.botLogin
	a.mu.RUnlock()
	if login == "" {
		return
	}

	path := BotLoginFilePath()
	if cur, err := os.ReadFile(path); err == nil && string(cur) == login {
		return // already current — skip the write (called on every token refresh)
	}

	// Write-temp-then-rename, the OPPOSITE of the token caches' in-place rule:
	// this file is dev-owned world-readable (no pre-created ownership to
	// preserve), it is shared by every agent, and concurrent launch paths call
	// this without coordination — rename keeps every reader seeing a complete
	// login, never a torn write.
	tmp, err := os.CreateTemp(agentTokenCacheDir, botLoginFileName+".tmp-")
	if err != nil {
		a.logger.Warn("cannot publish bot identity file — author-gated gh listing will stay fail-closed for staff agents (#4044)", "path", path, "error", err)
		return
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	_, werr := tmp.WriteString(login)
	if werr == nil {
		werr = tmp.Chmod(botLoginFilePerms)
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		a.logger.Warn("cannot publish bot identity file — author-gated gh listing will stay fail-closed for staff agents (#4044)", "path", path, "error", werr)
	}
}

// WriteAgentToken mints a tier-scoped token for the given agent and writes it
// into that agent's pre-created cache file.
//
// Delivery model (no chown at runtime):
//
// The hive binary drops privileges to dev (UID 1001) and therefore CANNOT
// chown — handing a file to a different UID requires CAP_CHOWN, which is not
// granted in the hardened/OpenShift environments hive targets (the same
// container already fails NET_ADMIN for iptables). The previous
// implementation wrote a temp file and chowned it to the agent UID; that
// chown ALWAYS failed with EPERM, the temp file was deleted, and no agent on
// any hive ever received a token.
//
// Instead, entrypoint.sh pre-creates the cache file as root, before the
// privilege drop, as dev:hive-<agent> mode 0640 (see the per-agent user loop
// there). That gives us:
//
//   - owner dev  -> this process can rewrite the contents in place
//   - group hive-<agent>, whose only member is that agent -> only the owning
//     agent can read it (NOT group "node", which every agent shares)
//
// The write must therefore be IN PLACE. A write-temp-then-rename would
// replace the pre-created inode with a fresh dev:node one and silently
// destroy both the ownership and the isolation guarantee.
func (a *AppAuth) WriteAgentToken(ctx context.Context, agentName, tier string, agentUID int) error {
	token, err := a.ScopedToken(ctx, tier)
	if err != nil {
		return fmt.Errorf("minting scoped token for %s: %w", agentName, err)
	}

	if err := os.MkdirAll(agentTokenCacheDir, agentTokenCacheDirPerms); err != nil {
		return fmt.Errorf("creating agent token dir: %w", err)
	}

	// Publish the trusted bot-identity file alongside the token (#4044). Every
	// mint/refresh path re-asserts it, so a pod that lost /var/run self-heals
	// within one token-refresh interval. Soft-fails internally — see doc.
	a.publishBotLogin()

	cachePath := AgentTokenCachePath(agentName)
	preCreated := true
	if _, statErr := os.Stat(cachePath); statErr != nil {
		preCreated = false
	}

	// O_TRUNC (not O_APPEND) so a shorter token fully replaces a longer one.
	// The perm argument only applies if the file does not already exist; when
	// entrypoint pre-created it, the existing 0640 dev:hive-<agent> mode wins.
	f, err := os.OpenFile(cachePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, agentTokenCachePerms)
	if err != nil {
		return fmt.Errorf("opening agent token cache %s: %w", cachePath, err)
	}
	if _, err := f.WriteString(token); err != nil {
		_ = f.Close() // best-effort cleanup; the write error is what's returned
		return fmt.Errorf("writing agent token cache %s: %w", cachePath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing agent token cache %s: %w", cachePath, err)
	}

	if !preCreated && agentUID > 0 {
		// The file did not exist, so we just created it as dev:node 0600 —
		// the agent UID cannot read it. This is a real, silent-auth-failure
		// condition, not a benign fallback: state it plainly and actionably.
		// Do NOT attempt a chown here; it cannot succeed without CAP_CHOWN
		// and the failure path is what caused this bug in the first place.
		a.logger.Warn("agent token cache was not pre-created by entrypoint — the agent CANNOT read it and will fall back to the shared full-privilege token (least-privilege scoping lost); ensure entrypoint.sh created this file as dev:hive-<agent> 0640",
			"agent", agentName, "uid", agentUID, "path", cachePath)
	}

	a.logger.Info("per-agent token cached", "agent", agentName, "tier", tier, "uid", agentUID, "pre_created", preCreated)
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

	mintCtx, cancel := mintContext(ctx)
	defer cancel()
	jwtClient := newJWTClient(jwtToken, a.apiURL)
	inst, _, err := jwtClient.Apps.GetInstallation(mintCtx, a.installationID)
	if err != nil {
		return nil, fmt.Errorf("resolving installation %d: %w", a.installationID, err)
	}

	return &InstallationInfo{
		InstallationID: a.installationID,
		Account:        inst.GetAccount().GetLogin(),
		IssuesPerm:     inst.GetPermissions().GetIssues(),
	}, nil
}

// VerifyRepoRead proves the ADVISORY-TIER repository read path works for
// owner/repo: it mints an advisor scoped token restricted to that single repo
// (exactly the credential an L2 guide agent receives) and performs a real
// Contents read with it. This is the verification gate for closing #2575's
// "no clone mechanism / no repository access mechanism" findings — a digest
// post only proves issues:write, while those findings are about Contents
// read, which the advisor tier lacked entirely before #4291. A nil return
// means the read demonstrably succeeded; any error means the finding's
// condition may still hold and the caller must not close it.
func (a *AppAuth) VerifyRepoRead(ctx context.Context, owner, repo string) error {
	if owner == "" || repo == "" {
		return fmt.Errorf("verifying repo read: owner and repo are required")
	}
	token, err := a.ScopedTokenForRepos(ctx, "advisor", []string{repo})
	if err != nil {
		return fmt.Errorf("minting advisor token for %s/%s: %w", owner, repo, err)
	}
	readCtx, cancel := mintContext(ctx)
	defer cancel()
	client := newTokenClient(token, a.apiURL)
	// Root directory listing: works on any non-empty repo with Contents:read,
	// unlike a README fetch, which 404s on repos without one.
	if _, _, _, err := client.Repositories.GetContents(readCtx, owner, repo, "", nil); err != nil {
		return fmt.Errorf("reading contents of %s/%s with advisor token: %w", owner, repo, err)
	}
	return nil
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
	base := t.base
	if base == nil {
		base = sharedProxyTrust.sharedTransport()
	}
	return base.RoundTrip(req2)
}

func NewClientFromApp(auth *AppAuth, org string, repos []string, logger *slog.Logger) *Client {
	return NewClientFromAppWithBotLogin(auth, org, repos, logger, "")
}

func NewClientFromAppWithBotLogin(auth *AppAuth, org string, repos []string, logger *slog.Logger, appBotLogin string) *Client {
	// This factory is the one place where the AppAuth that mints agent tokens
	// meets the resolved bot login (cfg.GitHub.BotLogin()), including every
	// config-refresh/reinit path in main.go. Recording it here is what lets
	// WriteAgentToken publish the trusted bot-identity file for the gh
	// wrapper's author gate (#4044) without threading the login through each
	// call site separately.
	auth.SetBotLogin(appBotLogin)
	transport := &appTransport{
		auth: auth,
	}
	const appClientTimeout = 30 * time.Second
	httpClient := &http.Client{Transport: transport, Timeout: appClientTimeout}
	client := gh.NewClient(httpClient)
	setBaseURL(client, auth.apiURL)

	return &Client{
		client:      client,
		org:         org,
		repos:       repos,
		logger:      logger,
		appAuth:     auth,
		appBotLogin: appBotLogin,
	}
}
