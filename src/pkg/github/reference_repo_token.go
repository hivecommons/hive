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
// definition outside the hive's org. The operator does not want the App
// installed on any remote org either (hivecommons/hive#18196), so the
// credential is a person's: the hive OWNER's GitHub user token, consented once
// on the spoke dashboard (OwnerIdentityStore). Two sources, in the order an
// operator reaches for them:
//
//   - the owner identity — every write-tier entry, derived or hand-written,
//     writes as the owner's login unless the next source overrides it;
//   - write_token_file — an operator-supplied token (a maintainer PAT, a
//     fine-grained token) for a HAND-WRITTEN entry, read from disk on each use
//     so a rotation is picked up without a restart. A derived entry never
//     reads it.
//
// project.remote_write_identity (hivecommons/hive#18516) adds a third
// source in front of the owner: the hive's own GitHub App, only on a granted
// repo whose owner installed it themselves. See reference_repo_identity.go.
//
// Boot refuses a hand-written write-mode entry that has neither (see
// VerifyReferenceRepoWriteCredential) so the failure is a config error with
// the repo named, not a 401 a day later. A derived entry is downgraded to
// read-only instead and the request issue is told once.

// ErrReferenceRepoNoCredential is the sentinel for a write-mode entry with no
// usable credential source.
var ErrReferenceRepoNoCredential = errors.New("reference repo write-mode has no credential")

// OwnerTokenSource yields the hive owner's token for the proxy, or
// ("", false) when no usable identity is connected. OwnerIdentityStore.Token
// is the production source.
type OwnerTokenSource func() (string, bool)

// ownerIdentityCure is the sentence every missing-credential message ends
// with: where the owner connects their account.
const ownerIdentityCure = "connect the hive owner's GitHub account in Settings → Reference repos"

// ReferenceRepoTokenSource returns a token function for the proxy: the
// write_token_file when a hand-written entry sets one, else the owner token.
func ReferenceRepoTokenSource(rr config.ReferenceRepo, owner OwnerTokenSource, logger *slog.Logger) func() (string, bool) {
	if rr.ReadOnly() {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
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
		if owner == nil {
			logger.Warn("reference repo write: no owner identity store wired", "repo", rr.Repo)
			return "", false
		}
		tok, ok := owner()
		if !ok {
			logger.Warn("reference repo write: owner identity not connected or no longer valid", "repo", rr.Repo, "cure", ownerIdentityCure)
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
// hand-written write-mode entry: the write_token_file must be readable and
// non-empty, or the owner identity must be connected and usable. It returns
// the login the credential acts as — the token file's user, or the owner's
// login — for the proxy's own-item check. whoAmI is a seam: production passes
// GET /user for file tokens.
func VerifyReferenceRepoWriteCredential(ctx context.Context, rr config.ReferenceRepo, owner OwnerCredential, whoAmI func(ctx context.Context, token string) (string, error)) (string, error) {
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
	if !owner.Usable() {
		reason := owner.Unavailable
		if reason == "" {
			reason = OwnerIdentityNotConnected
		}
		return "", fmt.Errorf("%w: project.reference_repos %s: write: %v needs a credential for %s: %s — %s, or set write_token_file", ErrReferenceRepoNoCredential, rr.Repo, rr.Write, rr.Owner(), reason, ownerIdentityCure)
	}
	return owner.Login, nil
}

// DerivedReadOnlyReason is the reason recorded on a derived entry when the
// owner identity is not usable; owner.Unavailable when it names one, else the
// not-connected default. rr is accepted for symmetry with the callers that
// render per-repo lines.
func DerivedReadOnlyReason(rr config.ReferenceRepo, owner OwnerCredential) string {
	_ = rr
	if owner.Unavailable != "" {
		return owner.Unavailable
	}
	return OwnerIdentityNotConnected
}

// ResolveDerivedReferenceRepo resolves the credential for an entry derived
// from a request issue (hivecommons/hive#18158, #18197). Unlike a hand-written
// entry, a derived one is not the operator's mistake, so it never yields
// ErrReferenceRepoNoCredential: with a usable owner identity the entry is
// returned with CredentialState write-tier and the owner's login; without one
// it comes back with Write cleared, CredentialState read-only and the reason.
// write_token_file is never consulted for a derived entry. No network is
// involved. A hand-written entry is returned unchanged with no login; use
// VerifyReferenceRepoWriteCredential for it.
func ResolveDerivedReferenceRepo(rr config.ReferenceRepo, owner OwnerCredential) (config.ReferenceRepo, string) {
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
	if !owner.Usable() {
		return readOnly(DerivedReadOnlyReason(rr, owner))
	}
	rr.CredentialState = config.ReferenceCredentialWriteTier
	rr.CredentialReason = ""
	rr.CredentialLogin = owner.Login
	return rr, owner.Login
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
