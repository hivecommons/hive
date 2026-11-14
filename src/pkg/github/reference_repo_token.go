package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
)

// Reference-repo write credentials (hivecommons/hive#18015).
//
// A project.reference_repos entry that opted in to writes needs a credential
// GitHub actually honours for THAT repo. The hive's own installation token
// cannot be it: an App installation is per-account, and a reference repo is by
// definition outside the hive's org. Two sources are supported, in the order
// an operator reaches for them:
//
//   - the same App installed on the target org — the hive discovers that
//     installation and mints a token on it, scoped to the one listed repo and
//     to exactly the permissions the opt-in needs;
//   - write_token_file — an operator-supplied token (a maintainer PAT, a
//     fine-grained token) read from disk on each use so a rotation is picked up
//     without a restart.
//
// Boot refuses a write-mode entry that has neither (see
// VerifyReferenceRepoWriteCredential) so the failure is a config error with
// the repo named, not a 401 a day later.

// referenceTokenRefreshMargin is how long before expiry a minted reference
// token is replaced.
const referenceTokenRefreshMargin = 5 * time.Minute

// ErrReferenceRepoNoCredential is the sentinel for a write-mode entry with no
// usable credential source.
var ErrReferenceRepoNoCredential = errors.New("reference repo write-mode has no credential")

// referenceRepoPermissions are the installation permissions an opt-in needs:
// metadata always, issues:write for issues, contents:write + pull_requests:write
// for prs (a PR needs a branch to be pushed first). Nothing wider — the proxy
// narrows further to the exact write paths.
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

// ReferenceRepoTokenSource returns a token function for the proxy: the
// write_token_file when set, else a cached installation token minted on the
// target org. auth may be replaced on key rotation, so it is taken through a
// getter rather than captured.
func ReferenceRepoTokenSource(rr config.ReferenceRepo, auth func() *AppAuth, logger *slog.Logger) func() (string, bool) {
	if rr.ReadOnly() {
		return nil
	}
	// A derived entry never reads write_token_file (hivecommons/hive#18158):
	// its only credential is the App installation on the target org.
	if rr.WriteTokenFile != "" && !rr.Derived() {
		return func() (string, bool) {
			tok, err := readTokenFile(rr.WriteTokenFile)
			if err != nil {
				logger.Warn("reference repo write token unreadable", "repo", rr.Repo, "file", rr.WriteTokenFile, "error", err)
				return "", false
			}
			return tok, true
		}
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
		a := auth()
		if a == nil {
			logger.Warn("reference repo write token: no App auth available", "repo", rr.Repo)
			return "", false
		}
		ctx, cancel := context.WithTimeout(context.Background(), mintClientTimeout)
		defer cancel()
		tok, exp, err := a.MintReferenceRepoToken(ctx, rr)
		if err != nil {
			logger.Warn("reference repo write token mint failed", "repo", rr.Repo, "error", err)
			return "", false
		}
		token, expiry = tok, exp
		return token, true
	}
}

// MintReferenceRepoToken mints an installation token on the TARGET org's
// installation of this App, scoped to rr's single repository and to the
// permissions its write opt-in needs.
func (a *AppAuth) MintReferenceRepoToken(ctx context.Context, rr config.ReferenceRepo) (string, time.Time, error) {
	if a == nil {
		return "", time.Time{}, ErrNoGitHubClient
	}
	installationID, err := a.DiscoverInstallationID(ctx, rr.Owner())
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %s: %v", ErrReferenceRepoNoCredential, rr.Repo, err)
	}
	opts := &gh.InstallationTokenOptions{
		Repositories: []string{rr.Name()},
		Permissions:  referenceRepoPermissions(rr),
	}
	return a.mintInstallationTokenWithOptions(ctx, installationID, opts)
}

func readTokenFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return tok, nil
}

// VerifyReferenceRepoWriteCredential is the boot-time check for one write-mode
// entry: the write_token_file must be readable and non-empty, or the App must
// have an installation on the target org. It returns the login the credential
// acts as — the App bot login, or the token's user — for the proxy's own-item
// check. discover and whoAmI are seams: production passes the App's installation
// discovery and a GET /user for file tokens.
func VerifyReferenceRepoWriteCredential(ctx context.Context, rr config.ReferenceRepo, botLogin string, discover func(ctx context.Context, org string) (int64, error), whoAmI func(ctx context.Context, token string) (string, error)) (string, error) {
	if rr.ReadOnly() {
		return "", nil
	}
	if rr.WriteTokenFile != "" {
		tok, err := readTokenFile(rr.WriteTokenFile)
		if err != nil {
			return "", fmt.Errorf("%w: project.reference_repos %s: write_token_file %s: %v", ErrReferenceRepoNoCredential, rr.Repo, rr.WriteTokenFile, err)
		}
		if whoAmI == nil {
			return "", fmt.Errorf("%w: project.reference_repos %s: cannot resolve the write_token_file identity", ErrReferenceRepoNoCredential, rr.Repo)
		}
		login, err := whoAmI(ctx, tok)
		if err != nil || login == "" {
			return "", fmt.Errorf("%w: project.reference_repos %s: write_token_file %s is not a usable GitHub token (%v)", ErrReferenceRepoNoCredential, rr.Repo, rr.WriteTokenFile, err)
		}
		return login, nil
	}
	if discover == nil {
		return "", fmt.Errorf("%w: project.reference_repos %s: write: %v needs the GitHub App installed on %s (or write_token_file), and this hive has no App auth", ErrReferenceRepoNoCredential, rr.Repo, rr.Write, rr.Owner())
	}
	if _, err := discover(ctx, rr.Owner()); err != nil {
		return "", fmt.Errorf("%w: project.reference_repos %s: write: %v needs the GitHub App installed on %s (or write_token_file): %v", ErrReferenceRepoNoCredential, rr.Repo, rr.Write, rr.Owner(), err)
	}
	if botLogin == "" {
		return "", fmt.Errorf("%w: project.reference_repos %s: App bot login unknown, cannot attribute the hive's own items", ErrReferenceRepoNoCredential, rr.Repo)
	}
	return botLogin, nil
}

// Derived read-only reasons (hivecommons/hive#18158). They are shown to the
// requesting project on its request issue, so they name the cure, not the
// hive's internals.
const (
	// derivedReadOnlyNoInstallationFmt is used when the App is not installed
	// on the target org; the argument is the owner.
	derivedReadOnlyNoInstallationFmt = "the hive's GitHub App is not installed on %s"
	// DerivedReadOnlyNoAppAuth is used when the hive runs without App auth at
	// all, so no installation can ever be found.
	DerivedReadOnlyNoAppAuth = "this hive has no GitHub App auth"
	// DerivedReadOnlyBotLoginUnknown is used when an installation exists but
	// the hive cannot name its own bot login, so it could not tell its own
	// items from others' and must not write.
	DerivedReadOnlyBotLoginUnknown = "the hive's App bot login is unknown"
)

// DerivedReadOnlyReason is the reason recorded on a derived entry whose
// target org has no installation of the App.
func DerivedReadOnlyReason(rr config.ReferenceRepo) string {
	return fmt.Sprintf(derivedReadOnlyNoInstallationFmt, rr.Owner())
}

// ResolveDerivedReferenceRepo resolves the credential for an entry derived
// from a request issue (hivecommons/hive#18158). Unlike a hand-written entry,
// a derived one is not the operator's mistake, so it never yields
// ErrReferenceRepoNoCredential: when the App has an installation on the
// target org the entry is returned unchanged with CredentialState write-tier
// and the bot login the credential acts as; when there is none the entry
// comes back with Write cleared, CredentialState read-only and the reason.
// write_token_file is never consulted for a derived entry. A transient
// discovery failure (network, 5xx) is returned as an error so the caller
// keeps its last good state instead of downgrading on a blip. A hand-written
// entry is returned unchanged with no login; use
// VerifyReferenceRepoWriteCredential for it.
func ResolveDerivedReferenceRepo(ctx context.Context, rr config.ReferenceRepo, botLogin string, discover func(ctx context.Context, org string) (int64, error)) (config.ReferenceRepo, string, error) {
	if !rr.Derived() {
		return rr, "", nil
	}
	rr.WriteTokenFile = ""
	readOnly := func(reason string) (config.ReferenceRepo, string, error) {
		rr.Write = nil
		rr.CredentialState = config.ReferenceCredentialReadOnly
		rr.CredentialReason = reason
		return rr, "", nil
	}
	if rr.ReadOnly() {
		return readOnly(rr.CredentialReason)
	}
	if discover == nil {
		return readOnly(DerivedReadOnlyNoAppAuth)
	}
	if _, err := discover(ctx, rr.Owner()); err != nil {
		if errors.Is(err, ErrNoInstallationForOrg) {
			return readOnly(DerivedReadOnlyReason(rr))
		}
		return rr, "", fmt.Errorf("resolving derived reference repo %s (from %s): %w", rr.Repo, rr.DerivedFrom, err)
	}
	if botLogin == "" {
		return readOnly(DerivedReadOnlyBotLoginUnknown)
	}
	rr.CredentialState = config.ReferenceCredentialWriteTier
	rr.CredentialReason = ""
	return rr, botLogin, nil
}

// TokenUserLogin resolves the login a user token acts as (GET /user).
func TokenUserLogin(ctx context.Context, token, apiURL string) (string, error) {
	user, _, err := newTokenClient(token, apiURL).Users.Get(ctx, "")
	if err != nil {
		return "", err
	}
	return user.GetLogin(), nil
}

// BotLogin returns the App bot login recorded by SetBotLogin ("" when unknown).
func (a *AppAuth) BotLogin() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.botLogin
}
