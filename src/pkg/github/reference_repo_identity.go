package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
)

// Which identity a write-tier reference repo writes as
// (project.remote_write_identity; follow-up to hivecommons/hive#18196).
//
// The grant is unchanged: only an entry already at the write tier (a
// hand-written one, or one derived from an accepted request issue, #18139)
// ever gets a credential, and the proxy's reference-repo gate still allows
// only opening issues and PRs, pushing topic branches and commenting on the
// hive's own items. This file only decides whose NAME those writes carry:
//
//   - the App bot, when the setting allows it AND the hive's GitHub App has an
//     installation that covers that exact repository (the repo's owner
//     installed it on their own; nothing here asks anyone to). The token is
//     minted on that installation, scoped to that one repository and to the
//     permissions the entry's write tier needs, and is never handed to
//     anything but the proxy's reference-repo injection for that repository;
//   - the hive owner's connected account, when the setting is "owner", or
//     "bot" and the App does not cover the repo;
//   - nothing (read-only), otherwise.
//
// An App installation shared by many hives (the hivecommons org installs the
// hivecommons-hive App for every repository) is therefore never a grant by
// itself: a hive without an entry for a repo has no policy for it, so no
// token is ever minted for it.

// referenceTokenRefreshMargin is how long before expiry a minted bot token is
// replaced.
const referenceTokenRefreshMargin = 5 * time.Minute

// ReferenceRepoApp is the App seam the identity resolution and the bot token
// source use. Production wires it from the spoke's AppAuth
// (AppAuth.ReferenceRepoApp); tests fake it.
type ReferenceRepoApp struct {
	// Login is the App bot login ("<slug>[bot]") the minted tokens act as.
	Login string
	// Installation returns the installation of the App covering owner/name,
	// or an error wrapping ErrNoInstallationForOrg when there is none.
	Installation func(ctx context.Context, owner, name string) (int64, error)
	// Mint mints a token on installationID for rr alone.
	Mint func(ctx context.Context, installationID int64, rr config.ReferenceRepo) (string, time.Time, error)
}

// available reports whether the App path can be tried at all.
func (a ReferenceRepoApp) available() bool {
	return a.Login != "" && a.Installation != nil && a.Mint != nil
}

// ReferenceRepoApp returns the production seam over this AppAuth; the zero
// value (App path unavailable) when the hive has no App key or bot login.
func (a *AppAuth) ReferenceRepoApp() ReferenceRepoApp {
	if a == nil || !a.HasKey() || a.BotLogin() == "" {
		return ReferenceRepoApp{}
	}
	return ReferenceRepoApp{Login: a.BotLogin(), Installation: a.RepositoryInstallationID, Mint: a.MintReferenceRepoToken}
}

// referenceRepoPermissions are the installation permissions a write tier
// needs: metadata always, issues:write for issues, contents:write and
// pull_requests:write for prs (a PR needs its topic branch pushed first).
// Nothing wider; the proxy narrows further to the exact write paths.
func referenceRepoPermissions(rr config.ReferenceRepo) *gh.InstallationPermissions {
	perms := &gh.InstallationPermissions{Metadata: gh.Ptr("read"), Contents: gh.Ptr("read")}
	if rr.WritesIssues() {
		perms.Issues = gh.Ptr("write")
	}
	if rr.WritesPRs() {
		perms.Contents = gh.Ptr("write")
		perms.PullRequests = gh.Ptr("write")
	}
	return perms
}

// RepositoryInstallationID returns the installation of this App that covers
// owner/name (GET /repos/{owner}/{name}/installation, App JWT), so an
// installation limited to selected repositories is honoured. A 404 wraps
// ErrNoInstallationForOrg. Results, negative ones included, are cached for
// InstallationDiscoveryTTL like DiscoverInstallationID's.
func (a *AppAuth) RepositoryInstallationID(ctx context.Context, owner, name string) (int64, error) {
	if a == nil || !a.HasKey() {
		return 0, fmt.Errorf("%w: this hive has no GitHub App key", ErrNoInstallationForOrg)
	}
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	if owner == "" || name == "" {
		return 0, fmt.Errorf("repository installation lookup: owner and name are required")
	}
	key := "repo|" + discoveryCacheKey(a.apiURL, a.appID, owner+"/"+name)
	discoveryCacheMu.Lock()
	if entry, ok := discoveryCache[key]; ok && time.Since(entry.cachedAt) < InstallationDiscoveryTTL {
		discoveryCacheMu.Unlock()
		return entry.id, entry.err
	}
	discoveryCacheMu.Unlock()

	id, err := a.repositoryInstallationUncached(ctx, owner, name)
	if err == nil || errors.Is(err, ErrNoInstallationForOrg) {
		// Transient failures are not cached: the next pass retries them.
		discoveryCacheMu.Lock()
		discoveryCache[key] = discoveryCacheEntry{id: id, err: err, cachedAt: time.Now()}
		discoveryCacheMu.Unlock()
	}
	return id, err
}

func (a *AppAuth) repositoryInstallationUncached(ctx context.Context, owner, name string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	jwtToken, err := a.generateJWT()
	if err != nil {
		return 0, fmt.Errorf("generating JWT: %w", err)
	}
	inst, resp, err := newJWTClient(jwtToken, a.apiURL).Apps.FindRepositoryInstallation(ctx, owner, name)
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			return 0, fmt.Errorf("%w: app %d is not installed on %s/%s", ErrNoInstallationForOrg, a.appID, owner, name)
		}
		return 0, fmt.Errorf("repository installation lookup for %s/%s: %w", owner, name, err)
	}
	if inst == nil || inst.GetID() == 0 {
		return 0, fmt.Errorf("%w: app %d returned no installation for %s/%s", ErrNoInstallationForOrg, a.appID, owner, name)
	}
	if acct := inst.GetAccount().GetLogin(); acct != "" && !strings.EqualFold(acct, owner) {
		return 0, fmt.Errorf("%w: installation for %s/%s belongs to %q", ErrNoInstallationForOrg, owner, name, acct)
	}
	return inst.GetID(), nil
}

// MintReferenceRepoToken mints a token on installationID scoped to rr's one
// repository and to the permissions its write tier needs. A read-only or
// malformed entry is refused before any request leaves.
func (a *AppAuth) MintReferenceRepoToken(ctx context.Context, installationID int64, rr config.ReferenceRepo) (string, time.Time, error) {
	if a == nil {
		return "", time.Time{}, ErrNoGitHubClient
	}
	if rr.ReadOnly() {
		return "", time.Time{}, fmt.Errorf("%w: %s is read-only", ErrReferenceRepoNoCredential, rr.Repo)
	}
	if rr.Owner() == "" || rr.Name() == "" || installationID <= 0 {
		return "", time.Time{}, fmt.Errorf("%w: %s: no installation to mint on", ErrReferenceRepoNoCredential, rr.Repo)
	}
	opts := &gh.InstallationTokenOptions{
		Repositories: []string{rr.Name()},
		Permissions:  referenceRepoPermissions(rr),
	}
	return a.mintInstallationTokenWithOptions(ctx, installationID, opts)
}

// BotReferenceRepoTokenSource is the proxy's token function for a bot-identity
// entry: a token minted on the installation covering rr, cached until shortly
// before it expires. The installation is looked up again on every mint, so an
// App the repo's owner removed stops yielding tokens. app is taken through a
// getter because the AppAuth is replaced on key rotation.
func BotReferenceRepoTokenSource(rr config.ReferenceRepo, app func() ReferenceRepoApp, logger *slog.Logger) func() (string, bool) {
	if rr.ReadOnly() {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	var (
		mu     sync.Mutex
		token  string
		expiry time.Time
	)
	return func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if token != "" && time.Now().Add(referenceTokenRefreshMargin).Before(expiry) {
			return token, true
		}
		token, expiry = "", time.Time{}
		var a ReferenceRepoApp
		if app != nil {
			a = app()
		}
		if !a.available() {
			logger.Warn("reference repo bot write: no GitHub App auth available", "repo", rr.Repo)
			return "", false
		}
		ctx, cancel := context.WithTimeout(context.Background(), mintClientTimeout)
		defer cancel()
		id, err := a.Installation(ctx, rr.Owner(), rr.Name())
		if err != nil {
			logger.Warn("reference repo bot write: App installation lookup failed", "repo", rr.Repo, "error", err.Error())
			return "", false
		}
		tok, exp, err := a.Mint(ctx, id, rr)
		if err != nil || tok == "" {
			logger.Warn("reference repo bot write: token mint failed", "repo", rr.Repo, "error", fmt.Sprint(err))
			return "", false
		}
		token, expiry = tok, exp
		return token, true
	}
}

// Notes and reasons the resolution records (CredentialNote,
// CredentialReason). They are shown on the Settings page and, for a derived
// read-only entry, on the request issue, so they name no internals and never
// ask anyone to install the App.
const (
	referenceNoteAppInstalledFmt    = "the hive's GitHub App is installed on %s"
	referenceNoteAppNotInstalledFmt = "the hive's GitHub App is not installed on %s"
	referenceNoteNoApp              = "this hive has no GitHub App credentials"
	referenceNoteOwnerSetting       = "this hive posts on other projects as its owner"
	referenceNoteTokenFile          = "write_token_file"
	// ReferenceReadOnlyBotOnlyPrefix starts the read-only reason of an entry
	// left read-only by bot_only; the request-issue notice keys on it to
	// leave out the connect-your-account cure, which would not help.
	ReferenceReadOnlyBotOnlyPrefix = "this hive posts on other projects only as its GitHub App"
	referenceReadOnlyBotOnlyFmt    = ReferenceReadOnlyBotOnlyPrefix + ", which is not installed on %s"
	referenceUnresolvedFmt         = "could not check whether the hive's GitHub App is installed on %s; retrying on the next pass"
)

// ReferenceRepoCredential is how one write-tier entry resolved.
type ReferenceRepoCredential struct {
	// Identity is config.ReferenceIdentityBot, ...Owner or ...TokenFile; empty
	// when the entry is served read-only.
	Identity string
	// Login is the GitHub login the writes appear under.
	Login string
	// LookupErr is the transient App lookup failure behind an unresolved
	// entry, for the caller to log; nil otherwise.
	LookupErr error
}

// appLookup is the outcome of the App check for one repo.
type appLookup int

const (
	appUnavailable appLookup = iota
	appInstalled
	appNotInstalled
	appTransient
)

func lookupReferenceApp(ctx context.Context, rr config.ReferenceRepo, app ReferenceRepoApp) (appLookup, string, error) {
	if !app.available() {
		return appUnavailable, referenceNoteNoApp, nil
	}
	_, err := app.Installation(ctx, rr.Owner(), rr.Name())
	switch {
	case err == nil:
		return appInstalled, fmt.Sprintf(referenceNoteAppInstalledFmt, rr.Owner()), nil
	case errors.Is(err, ErrNoInstallationForOrg):
		return appNotInstalled, fmt.Sprintf(referenceNoteAppNotInstalledFmt, rr.Owner()), nil
	}
	return appTransient, fmt.Sprintf(referenceUnresolvedFmt, rr.Owner()), err
}

// ResolveReferenceRepoCredential resolves one entry against
// project.remote_write_identity (setting, any spelling; empty is the
// default), the owner identity and the App. It returns the entry as resolved
// (CredentialState, CredentialLogin, CredentialIdentity, CredentialNote, and
// Write cleared when it was served read-only) and the credential.
//
// A read-only entry passes through unchanged (a derived one is marked as
// ResolveDerivedReferenceRepo always did). A hand-written entry's
// write_token_file wins over the setting. A hand-written write entry left
// with no credential is ErrReferenceRepoNoCredential, as before; a derived
// one is downgraded to read-only instead. A transient App lookup failure
// serves the entry read-only for this pass (CredentialState unresolved, and
// an empty credential the caller must build a read-only policy from) rather
// than posting under another identity, and is never an error.
func ResolveReferenceRepoCredential(ctx context.Context, rr config.ReferenceRepo, setting string, owner OwnerCredential, app ReferenceRepoApp, whoAmI func(ctx context.Context, token string) (string, error)) (config.ReferenceRepo, ReferenceRepoCredential, error) {
	if rr.ReadOnly() {
		if rr.Derived() {
			rr, _ = ResolveDerivedReferenceRepo(rr, owner)
		}
		return rr, ReferenceRepoCredential{}, nil
	}
	if rr.Derived() {
		rr.WriteTokenFile = ""
	} else if rr.WriteTokenFile != "" {
		login, err := VerifyReferenceRepoWriteCredential(ctx, rr, owner, whoAmI)
		if err != nil {
			return rr, ReferenceRepoCredential{}, err
		}
		return writeTier(rr, config.ReferenceIdentityTokenFile, login, referenceNoteTokenFile)
	}

	mode := (config.ProjectConfig{RemoteWriteIdentity: setting}).EffectiveRemoteWriteIdentity()
	ownerNote := referenceNoteOwnerSetting
	if mode != config.RemoteWriteIdentityOwner {
		state, note, err := lookupReferenceApp(ctx, rr, app)
		switch state {
		case appInstalled:
			return writeTier(rr, config.ReferenceIdentityBot, app.Login, note)
		case appTransient:
			// A derived entry is rebuilt from its request issue every pass, so
			// its Write is cleared like a read-only downgrade. A hand-written
			// one keeps Write: the resolved list replaces the config in
			// memory, and the editor saves from it, so clearing it would turn
			// a blip into a permanent edit. The caller serves it read-only.
			if rr.Derived() {
				rr.Write = nil
			}
			rr.CredentialState = config.ReferenceCredentialUnresolved
			rr.CredentialReason = note
			rr.CredentialLogin, rr.CredentialIdentity, rr.CredentialNote = "", "", note
			return rr, ReferenceRepoCredential{LookupErr: err}, nil
		}
		if mode == config.RemoteWriteIdentityBotOnly {
			reason := fmt.Sprintf(referenceReadOnlyBotOnlyFmt, rr.Owner())
			if state == appUnavailable {
				reason = ReferenceReadOnlyBotOnlyPrefix + ", and " + referenceNoteNoApp
			}
			if !rr.Derived() {
				return rr, ReferenceRepoCredential{}, fmt.Errorf("%w: project.reference_repos %s: write: %v has no credential: %s; set write_token_file, or change project.remote_write_identity", ErrReferenceRepoNoCredential, rr.Repo, rr.Write, reason)
			}
			return readOnlyDerived(rr, reason, note)
		}
		ownerNote = note
	}

	if !owner.Usable() {
		if !rr.Derived() {
			_, err := VerifyReferenceRepoWriteCredential(ctx, rr, owner, whoAmI)
			return rr, ReferenceRepoCredential{}, err
		}
		return readOnlyDerived(rr, DerivedReadOnlyReason(rr, owner), ownerNote)
	}
	return writeTier(rr, config.ReferenceIdentityOwner, owner.Login, ownerNote)
}

func writeTier(rr config.ReferenceRepo, identity, login, note string) (config.ReferenceRepo, ReferenceRepoCredential, error) {
	rr.CredentialState = config.ReferenceCredentialWriteTier
	rr.CredentialReason = ""
	rr.CredentialLogin = login
	rr.CredentialIdentity = identity
	rr.CredentialNote = note
	return rr, ReferenceRepoCredential{Identity: identity, Login: login}, nil
}

func readOnlyDerived(rr config.ReferenceRepo, reason, note string) (config.ReferenceRepo, ReferenceRepoCredential, error) {
	rr.Write = nil
	rr.CredentialState = config.ReferenceCredentialReadOnly
	rr.CredentialReason = reason
	rr.CredentialLogin = ""
	rr.CredentialIdentity = ""
	rr.CredentialNote = note
	return rr, ReferenceRepoCredential{}, nil
}
