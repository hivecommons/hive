package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Reference-repo write credentials (hivecommons/hive#18015, #18197).
//
// A project.reference_repos entry that opted in to writes needs a credential
// GitHub actually honours for THAT repo. The hive's own installation token
// cannot be it: an App installation is per-account, and a reference repo is by
// definition outside the hive's org. Two sources are supported:
//
//   - the hive owner's own GitHub account, connected once on the spoke
//     dashboard (Settings → Reference repos → Connect GitHub account; see
//     owner_identity.go) — the remote lane writes as the owner, not the App;
//   - write_token_file — an operator-supplied token (a maintainer PAT, a
//     fine-grained token) on a hand-written entry, read from disk on each use
//     so a rotation is picked up without a restart. It wins when set.
//
// Boot refuses a hand-written write-mode entry that has neither (see
// VerifyReferenceRepoWriteCredential) so the failure is a config error with
// the repo named, not a 401 a day later.

// ErrReferenceRepoNoCredential is the sentinel for a write-mode entry with no
// usable credential source.
var ErrReferenceRepoNoCredential = errors.New("reference repo write-mode has no credential")

// ReferenceRepoConnectHint names the cure for a missing write credential.
const ReferenceRepoConnectHint = "connect the hive owner's GitHub account in Settings → Reference repos → Connect GitHub account (or set write_token_file)"

// ReferenceRepoTokenSource returns a token function for the proxy: the
// write_token_file of a hand-written entry when set, else the hive owner's
// connected token. ownerToken is read on every call so a connect or
// disconnect on the dashboard takes effect without a restart.
func ReferenceRepoTokenSource(rr config.ReferenceRepo, ownerToken func() (string, bool), logger *slog.Logger) func() (string, bool) {
	if rr.ReadOnly() {
		return nil
	}
	// A derived entry never reads write_token_file (hivecommons/hive#18158).
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
	return func() (string, bool) {
		if ownerToken == nil {
			logger.Warn("reference repo write token: the hive owner has not connected a GitHub account", "repo", rr.Repo)
			return "", false
		}
		tok, ok := ownerToken()
		if !ok || tok == "" {
			logger.Warn("reference repo write token: the hive owner's GitHub account is not connected or was rejected", "repo", rr.Repo)
			return "", false
		}
		return tok, true
	}
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

// VerifyReferenceRepoWriteCredential is the boot-time check for one
// hand-written write-mode entry: a write_token_file must be readable,
// non-empty and resolve to a GitHub user, or the hive owner's GitHub account
// must be connected (ownerLogin non-empty). It returns the login the
// credential acts as, for the proxy's own-item check. whoAmI is a seam:
// production passes a GET /user for file tokens.
func VerifyReferenceRepoWriteCredential(ctx context.Context, rr config.ReferenceRepo, ownerLogin string, whoAmI func(ctx context.Context, token string) (string, error)) (string, error) {
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
	if ownerLogin == "" {
		return "", fmt.Errorf("%w: project.reference_repos %s: write: %v needs a GitHub identity that can write there; %s", ErrReferenceRepoNoCredential, rr.Repo, rr.Write, ReferenceRepoConnectHint)
	}
	return ownerLogin, nil
}

// Read-only reasons (hivecommons/hive#18158, #18197). A derived entry's
// reason is shown to the requesting project on its request issue, so it
// names the cure, not the hive's internals.
const (
	// DerivedReadOnlyOwnerNotConnected is used when the hive owner has not
	// connected a GitHub account on the dashboard.
	DerivedReadOnlyOwnerNotConnected = "the hive owner has not connected a GitHub account"
	// ReferenceRepoOwnerReconnect is used when the owner's stored token was
	// rejected by GitHub (401): the owner must reconnect.
	ReferenceRepoOwnerReconnect = "the hive owner's GitHub account must be reconnected"
)

// DerivedReadOnlyReason is the default reason recorded on a derived entry
// served read-only.
func DerivedReadOnlyReason(config.ReferenceRepo) string {
	return DerivedReadOnlyOwnerNotConnected
}

// ResolveDerivedReferenceRepo resolves the credential for an entry derived
// from a request issue (hivecommons/hive#18158). It needs no network: the
// credential is the hive owner's connected GitHub account. When it is
// connected (ownerLogin non-empty) the entry comes back with CredentialState
// write-tier and CredentialLogin the owner's login; otherwise with Write
// cleared, CredentialState read-only and a reason (ownerErr
// ErrOwnerIdentityInvalid says the owner must reconnect). write_token_file
// is never consulted for a derived entry. A hand-written entry is returned
// unchanged with no login; use VerifyReferenceRepoWriteCredential for it.
func ResolveDerivedReferenceRepo(rr config.ReferenceRepo, ownerLogin string, ownerErr error) (config.ReferenceRepo, string) {
	if !rr.Derived() {
		return rr, ""
	}
	rr.WriteTokenFile = ""
	readOnly := func(reason string) (config.ReferenceRepo, string) {
		rr.Write = nil
		rr.CredentialState = config.ReferenceCredentialReadOnly
		rr.CredentialReason = reason
		rr.CredentialLogin = ""
		return rr, ""
	}
	if rr.ReadOnly() {
		return readOnly(rr.CredentialReason)
	}
	if errors.Is(ownerErr, ErrOwnerIdentityInvalid) {
		return readOnly(ReferenceRepoOwnerReconnect)
	}
	if ownerLogin == "" {
		return readOnly(DerivedReadOnlyOwnerNotConnected)
	}
	rr.CredentialState = config.ReferenceCredentialWriteTier
	rr.CredentialReason = ""
	rr.CredentialLogin = ownerLogin
	return rr, ownerLogin
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
