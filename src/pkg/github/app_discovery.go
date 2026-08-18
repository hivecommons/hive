package github

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const (
	// InstallationDiscoveryTTL bounds how often a hive may re-query GitHub for
	// the installation covering its org. Discovery is only ever triggered from
	// slow paths (startup, and the self-heal tick that already runs at the
	// heartbeat cadence while the App banner is showing), but the self-heal
	// tick would otherwise re-discover on EVERY beat for a hive whose App is
	// genuinely not installed on the org — burning App-JWT rate limit forever.
	// Five minutes matches the spoke's slow auto-discovery poll, so delayed org
	// admin approval is picked up promptly without hammering the App API.
	InstallationDiscoveryTTL = 5 * time.Minute

	// discoveryListPerPage is the page size used when falling back to
	// GET /app/installations. GitHub caps per_page at 100.
	discoveryListPerPage = 100

	// discoveryMaxPages bounds the fallback listing walk. An App installed on
	// more than 100*discoveryMaxPages accounts is not a hive App; stopping
	// keeps a pathological listing from stalling startup or the heartbeat.
	discoveryMaxPages = 10

	// discoveryTimeout bounds a whole discovery attempt (direct lookup plus
	// any fallback listing) so a hung GHE instance can never wedge startup or
	// a heartbeat tick.
	discoveryTimeout = 30 * time.Second
)

// discoveryCache memoises the outcome of DiscoverInstallationID per
// (apiURL, appID, org) so repeated calls from the self-heal loop do not hit
// the API. Negative results are cached too — "the App is not installed on
// this org" is the expensive-to-recheck case that motivates the TTL.
var (
	discoveryCacheMu sync.Mutex
	discoveryCache   = map[string]discoveryCacheEntry{}
)

type discoveryCacheEntry struct {
	id       int64
	err      error
	cachedAt time.Time
}

func discoveryCacheKey(apiURL string, appID int64, org string) string {
	return fmt.Sprintf("%s|%d|%s", apiURL, appID, strings.ToLower(org))
}

// ResetInstallationDiscoveryCache clears the memoised discovery results.
// Exported for tests; never called in production.
func ResetInstallationDiscoveryCache() {
	discoveryCacheMu.Lock()
	defer discoveryCacheMu.Unlock()
	discoveryCache = map[string]discoveryCacheEntry{}
}

// ForgetInstallationDiscovery removes this App/org discovery result from the
// cache. Use it for explicit operator-driven checks so a recent negative poll
// cannot mask an installation an org admin just approved.
func (a *AppAuth) ForgetInstallationDiscovery(org string) {
	if a == nil {
		return
	}
	key := discoveryCacheKey(a.apiURL, a.appID, org)
	discoveryCacheMu.Lock()
	defer discoveryCacheMu.Unlock()
	delete(discoveryCache, key)
}

// ErrNoInstallationForOrg means the App JWT authenticated fine but no
// installation of this App covers the target org. This is NOT a transient
// failure and NOT something the caller can fix by retrying: the App has to be
// installed on the org. Callers must leave the configured installation_id
// untouched when they see it.
var ErrNoInstallationForOrg = fmt.Errorf("no installation of this app covers the target org")

// DiscoverInstallationID resolves the installation ID of this App on org.
//
// It prefers GET /orgs/{org}/installation — a single App-JWT-authenticated
// call that answers the exact question and is correct by construction (GitHub
// resolves the org, so there is no login-matching for us to get wrong). When
// that endpoint is unavailable (older GHE, or the target is a user account
// rather than an org) it falls back to walking GET /app/installations and
// matching the installation account login case-insensitively.
//
// Both endpoints are authenticated with the App JWT, not an installation
// token, so this works even when the CONFIGURED installation_id is wrong —
// which is the whole point.
//
// It never returns an installation belonging to a different account: a
// plausible-but-wrong ID produces exactly the confusing 403s this is meant to
// cure. Ambiguity or absence yields ErrNoInstallationForOrg and the caller
// must leave the config alone.
func (a *AppAuth) DiscoverInstallationID(ctx context.Context, org string) (int64, error) {
	if a == nil {
		return 0, fmt.Errorf("no app auth configured")
	}
	if a.key == nil {
		// A hive with no App private key simply is not App-authenticated.
		// This is not a discovery failure — callers skip silently.
		return 0, fmt.Errorf("app private key not loaded")
	}
	org = strings.TrimSpace(org)
	if org == "" {
		return 0, fmt.Errorf("target org is empty")
	}

	key := discoveryCacheKey(a.apiURL, a.appID, org)
	discoveryCacheMu.Lock()
	if entry, ok := discoveryCache[key]; ok && time.Since(entry.cachedAt) < InstallationDiscoveryTTL {
		discoveryCacheMu.Unlock()
		return entry.id, entry.err
	}
	discoveryCacheMu.Unlock()

	id, err := a.discoverInstallationUncached(ctx, org)

	discoveryCacheMu.Lock()
	discoveryCache[key] = discoveryCacheEntry{id: id, err: err, cachedAt: time.Now()}
	discoveryCacheMu.Unlock()

	return id, err
}

func (a *AppAuth) discoverInstallationUncached(ctx context.Context, org string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	jwtToken, err := a.generateJWT()
	if err != nil {
		return 0, fmt.Errorf("generating JWT: %w", err)
	}
	jwtClient := newJWTClient(jwtToken, a.apiURL)

	// Preferred: direct per-org lookup.
	inst, resp, err := jwtClient.Apps.FindOrganizationInstallation(ctx, org)
	if err == nil && inst != nil && inst.GetID() != 0 {
		// Defensive: only adopt when the returned account really is the org.
		// The endpoint is authoritative, but adopting an ID whose account we
		// have not confirmed is exactly the failure mode being fixed.
		if acct := inst.GetAccount().GetLogin(); acct != "" && !strings.EqualFold(acct, org) {
			return 0, fmt.Errorf("%w: direct lookup for %q returned an installation owned by %q", ErrNoInstallationForOrg, org, acct)
		}
		return inst.GetID(), nil
	}
	// A 404 means "this App is not installed on that org" — authoritative and
	// final. Anything else (405 on old GHE, 5xx, network) falls through to the
	// listing walk, which also covers user-account targets.
	directErr := err
	if resp != nil && resp.StatusCode == 404 {
		// Still try the listing: on some GHE versions the org endpoint 404s for
		// user-account targets that DO have a listable installation.
		directErr = fmt.Errorf("direct org lookup: not found")
	}

	var matchedID int64
	matches := 0
	opts := &gh.ListOptions{PerPage: discoveryListPerPage}
	for page := 0; page < discoveryMaxPages; page++ {
		installs, listResp, listErr := jwtClient.Apps.ListInstallations(ctx, opts)
		if listErr != nil {
			return 0, fmt.Errorf("listing app installations (direct lookup: %v): %w", directErr, listErr)
		}
		for _, in := range installs {
			if in == nil {
				continue
			}
			if strings.EqualFold(in.GetAccount().GetLogin(), org) {
				if id := in.GetID(); id != 0 {
					matchedID = id
					matches++
				}
			}
		}
		if listResp == nil || listResp.NextPage == 0 {
			break
		}
		opts.Page = listResp.NextPage
	}
	if matches == 1 {
		return matchedID, nil
	}
	if matches > 1 {
		return 0, fmt.Errorf("%w: app %d has %d installations for %q", ErrNoInstallationForOrg, a.appID, matches, org)
	}

	return 0, fmt.Errorf("%w: app %d has no installation for %q (direct lookup: %v)", ErrNoInstallationForOrg, a.appID, org, directErr)
}

// VerifyInstallationForOrg verifies that installationID is an installation of
// this GitHub App whose account login matches org. It uses an App JWT, not an
// installation token, so it is safe to call before the hive has accepted or
// configured the installation ID.
func (a *AppAuth) VerifyInstallationForOrg(ctx context.Context, installationID int64, org string) error {
	if a == nil {
		return fmt.Errorf("no app auth configured")
	}
	if a.key == nil {
		return fmt.Errorf("app private key not loaded")
	}
	org = strings.TrimSpace(org)
	if org == "" {
		return fmt.Errorf("target org is empty")
	}
	if installationID <= 0 {
		return fmt.Errorf("installation ID must be positive")
	}

	ctx, cancel := context.WithTimeout(ctx, tokenMintTimeout)
	defer cancel()

	jwtToken, err := a.generateJWT()
	if err != nil {
		return fmt.Errorf("generating JWT: %w", err)
	}
	inst, _, err := newJWTClient(jwtToken, a.apiURL).Apps.GetInstallation(ctx, installationID)
	if err != nil {
		return fmt.Errorf("getting app installation %d: %w", installationID, err)
	}
	if inst == nil || inst.GetID() != installationID {
		return fmt.Errorf("%w: installation %d was not returned by GitHub", ErrNoInstallationForOrg, installationID)
	}
	if acct := inst.GetAccount().GetLogin(); acct == "" || !strings.EqualFold(acct, org) {
		return fmt.Errorf("%w: installation %d belongs to %q, not %q", ErrNoInstallationForOrg, installationID, inst.GetAccount().GetLogin(), org)
	}
	return nil
}

// InstallationAccountLogin returns the account login that owns installationID.
// It authenticates as the App with a JWT and does not mint or persist an
// installation token.
func (a *AppAuth) InstallationAccountLogin(ctx context.Context, installationID int64) (string, error) {
	if a == nil {
		return "", fmt.Errorf("no app auth configured")
	}
	if a.key == nil {
		return "", fmt.Errorf("app private key not loaded")
	}
	if installationID <= 0 {
		return "", fmt.Errorf("installation ID must be positive")
	}

	ctx, cancel := context.WithTimeout(ctx, tokenMintTimeout)
	defer cancel()

	jwtToken, err := a.generateJWT()
	if err != nil {
		return "", fmt.Errorf("generating JWT: %w", err)
	}
	inst, _, err := newJWTClient(jwtToken, a.apiURL).Apps.GetInstallation(ctx, installationID)
	if err != nil {
		return "", fmt.Errorf("getting app installation %d: %w", installationID, err)
	}
	if inst == nil || inst.GetID() != installationID {
		return "", fmt.Errorf("%w: installation %d was not returned by GitHub", ErrNoInstallationForOrg, installationID)
	}
	login := strings.TrimSpace(inst.GetAccount().GetLogin())
	if login == "" {
		return "", fmt.Errorf("%w: installation %d has no account login", ErrNoInstallationForOrg, installationID)
	}
	return login, nil
}

// AppID returns the configured GitHub App ID.
func (a *AppAuth) AppID() int64 { return a.appID }

// InstallationID returns the installation ID this auth currently uses.
func (a *AppAuth) InstallationID() int64 { return a.installationID }

// APIURL returns the GitHub API base URL this auth targets ("" = public).
func (a *AppAuth) APIURL() string { return a.apiURL }

// HasKey reports whether an App private key was loaded. A hive without one is
// not App-authenticated and must be skipped rather than treated as failing.
func (a *AppAuth) HasKey() bool { return a != nil && a.key != nil }

// DropCachedToken discards the installation token this auth is holding, in
// memory and on disk.
//
// Call it when the SIGNING KEY changes. A token is minted by presenting a JWT
// signed with the old key; once the key is replaced that token is a dead
// credential, and it is not enough to rebuild the AppAuth in memory — the
// on-disk cache at cachePath is read directly by agent processes, so a stale
// token there keeps being handed out until it expires. Dropping it forces the
// next caller to mint a fresh token under the new key.
//
// A missing cache file is the normal case, not an error.
func (a *AppAuth) DropCachedToken() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cachedToken = ""
	a.tokenExpiry = time.Time{}
	if a.cachePath == "" {
		return
	}
	if err := os.Remove(a.cachePath); err != nil && !os.IsNotExist(err) && a.logger != nil {
		a.logger.Warn("failed to drop stale app token cache after key change",
			"path", a.cachePath, "error", err)
	}
}

// AdoptInstallationID switches this auth to a different installation and
// drops the cached installation token (which was minted for the OLD, wrong
// installation and would keep 403ing on writes).
func (a *AppAuth) AdoptInstallationID(id int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.installationID = id
	a.cachedToken = ""
	a.tokenExpiry = time.Time{}
	// The on-disk cache holds the same stale token; agents read it directly.
	if a.cachePath != "" {
		if err := os.Remove(a.cachePath); err != nil && !os.IsNotExist(err) {
			a.logger.Warn("failed to drop stale app token cache after adopting new installation",
				"path", a.cachePath, "error", err)
		}
	}
}

// RediscoverAndAdopt is the self-heal entry point. It verifies the configured
// installation and, ONLY when that installation belongs to an account other
// than org, discovers the correct installation for org and adopts it in place.
//
// It returns the newly adopted installation ID, or 0 when nothing changed.
// Every failure mode is soft: a nil auth, a missing key, an API error, or an
// ambiguous/absent discovery result all return (0, err-or-nil) with the
// configured ID left exactly as it was, so the existing "check your
// installation_id" diagnosis still stands and startup/heartbeat proceed.
func (a *AppAuth) RediscoverAndAdopt(ctx context.Context, org string, logger *slog.Logger) (int64, error) {
	if a == nil || !a.HasKey() {
		return 0, nil // not App-authenticated — nothing to heal, not an error
	}
	if strings.TrimSpace(org) == "" {
		return 0, nil
	}
	if logger == nil {
		logger = a.logger
	}

	info, err := a.VerifyInstallation(ctx)
	if err != nil {
		// Cannot prove the current installation is wrong — never guess.
		return 0, fmt.Errorf("verifying installation before rediscovery: %w", err)
	}
	if strings.EqualFold(info.Account, org) {
		return 0, nil // installation already belongs to the right account
	}

	logger.Warn("github app installation belongs to the wrong account — attempting rediscovery",
		"installation_id", info.InstallationID, "account", info.Account, "expected_org", org)

	newID, err := a.DiscoverInstallationID(ctx, org)
	if err != nil {
		logger.Warn("github app installation rediscovery failed — leaving installation_id untouched",
			"expected_org", org, "error", err)
		return 0, err
	}
	if newID == 0 || newID == info.InstallationID {
		return 0, nil
	}

	a.AdoptInstallationID(newID)
	logger.Info("adopted rediscovered github app installation",
		"was", info.InstallationID, "now", newID, "org", org)
	return newID, nil
}
