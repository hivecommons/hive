package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"

	"github.com/hivecommons/hive/crux/proxy"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

// referenceRepoCredentialExitCode marks an exit caused by a
// project.reference_repos entry that opted in to writes without a credential
// GitHub would honour for that repo (hivecommons/hive#18015). Its own code so
// the termination state says "connect the owner's GitHub account or supply
// write_token_file", not "bug".
const referenceRepoCredentialExitCode = 22

// derivedReadOnlyPoster posts the one-shot read-only notice on a request
// issue (hivecommons/hive#18158). Production is
// github.Client.CommentDerivedReadOnly; tests stub it.
type derivedReadOnlyPoster func(ctx context.Context, issueRepo string, number int, readOnly []config.ReferenceRepo) (bool, error)

// referenceRepoDeps are the seams buildReferenceRepoPolicies needs
// (hivecommons/hive#18197): the hive owner's connected GitHub identity — its
// login ("" when not connected; github.ErrOwnerIdentityInvalid when GitHub
// rejected the token) and its token, both read live so a connect or
// disconnect on the dashboard is seen without a restart — and
// token-identity resolution for write_token_file. A nil seam reads as "not
// connected", in which case a write-mode entry can only be served by
// write_token_file.
type referenceRepoDeps struct {
	ownerLogin func() (string, error)
	ownerToken func() (string, bool)
	whoAmI     func(ctx context.Context, token string) (string, error)
}

// owner returns the connected owner login, or the reason there is none.
func (d referenceRepoDeps) owner() (string, error) {
	if d.ownerLogin == nil {
		return "", nil
	}
	return d.ownerLogin()
}

// referenceRepoResolution is what buildReferenceRepoPolicies produces: the
// proxy policies and the entries as resolved, with each derived entry's
// CredentialState filled in (and its Write cleared when it was downgraded),
// so the kick and the status payload describe what the proxy enforces.
type referenceRepoResolution struct {
	policies []proxy.ReferenceRepoPolicy
	repos    []config.ReferenceRepo
}

// buildReferenceRepoPolicies turns the config list into proxy policies. A
// read-only entry needs nothing; a hand-written write-mode entry is verified
// now so the failure is a boot-time config error naming the repo. A derived
// entry (hivecommons/hive#18158) is resolved instead: write tier when the
// hive owner's GitHub account is connected, read-only otherwise, never an
// error. Every write entry comes back with its CredentialState, and the
// login it writes as in CredentialLogin (hivecommons/hive#18197).
func buildReferenceRepoPolicies(ctx context.Context, refs []config.ReferenceRepo, deps referenceRepoDeps, logger *slog.Logger) (referenceRepoResolution, error) {
	res := referenceRepoResolution{
		policies: make([]proxy.ReferenceRepoPolicy, 0, len(refs)),
		repos:    make([]config.ReferenceRepo, 0, len(refs)),
	}
	ownerLogin, ownerErr := deps.owner()
	for _, rr := range refs {
		login := ""
		if !rr.Derived() {
			// Resolved below; an editor echo must not carry a stale state.
			rr.CredentialState, rr.CredentialReason, rr.CredentialLogin = "", "", ""
		}
		writes := !rr.ReadOnly()
		switch {
		case rr.Derived():
			rr, login = github.ResolveDerivedReferenceRepo(rr, ownerLogin, ownerErr)
			writes = !rr.ReadOnly()
		case !rr.ReadOnly():
			if rr.WriteTokenFile == "" && errors.Is(ownerErr, github.ErrOwnerIdentityInvalid) {
				// The owner connected once and GitHub now rejects the token:
				// not an operator config error, so the entry is served
				// read-only until the owner reconnects. Write stays as
				// configured so nothing is lost when the editor saves.
				logger.Warn("reference repo: the hive owner's GitHub token was rejected, serving read-only until reconnected", "repo", rr.Repo)
				rr.CredentialState = config.ReferenceCredentialReadOnly
				rr.CredentialReason = github.ReferenceRepoOwnerReconnect
				writes = false
				break
			}
			l, err := github.VerifyReferenceRepoWriteCredential(ctx, rr, ownerLogin, deps.whoAmI)
			if err != nil {
				return referenceRepoResolution{}, err
			}
			login = l
			rr.CredentialState = config.ReferenceCredentialWriteTier
			rr.CredentialLogin = login
		}
		pol := proxy.ReferenceRepoPolicy{Repo: rr.Repo}
		if writes {
			pol.Issues, pol.PRs = rr.WritesIssues(), rr.WritesPRs()
			pol.Login = login
			pol.Token = github.ReferenceRepoTokenSource(rr, deps.ownerToken, logger)
		}
		res.policies = append(res.policies, pol)
		res.repos = append(res.repos, rr)
	}
	return res, nil
}

// derivedReadOnlyByRequest groups the downgraded derived entries by the
// request issue that granted them, in a stable order of issue refs.
func derivedReadOnlyByRequest(repos []config.ReferenceRepo) (refs []string, byRef map[string][]config.ReferenceRepo) {
	byRef = map[string][]config.ReferenceRepo{}
	for _, rr := range repos {
		if !rr.DerivedReadOnly() {
			continue
		}
		if _, ok := byRef[rr.DerivedFrom]; !ok {
			refs = append(refs, rr.DerivedFrom)
		}
		byRef[rr.DerivedFrom] = append(byRef[rr.DerivedFrom], rr)
	}
	sort.Strings(refs)
	return refs, byRef
}

// notifyDerivedReadOnly posts the read-only notice for every request issue
// whose derived entries were downgraded. The poster is idempotent per
// (issue, repo set), so calling this on every reload posts at most once.
func notifyDerivedReadOnly(ctx context.Context, repos []config.ReferenceRepo, post derivedReadOnlyPoster, logger *slog.Logger) {
	if post == nil {
		return
	}
	refs, byRef := derivedReadOnlyByRequest(repos)
	for _, ref := range refs {
		readOnly := byRef[ref]
		issueRepo, number, ok := readOnly[0].DerivedFromIssue()
		if !ok {
			logger.Warn("derived reference repo: malformed request issue ref, notice skipped", "from", ref)
			continue
		}
		posted, err := post(ctx, issueRepo, number, readOnly)
		if err != nil {
			logger.Warn("derived reference repo: read-only notice not posted", "from", ref, "error", err.Error())
			continue
		}
		if posted {
			logger.Info("derived reference repos served read-only, request issue notified", "from", ref, "count", len(readOnly))
		}
	}
}

// wireReferenceRepos installs project.reference_repos on the proxy, refusing
// to boot when a hand-written write-mode entry has no usable credential. A
// derived entry never refuses boot: it is downgraded to read-only and the
// request issue is told once (hivecommons/hive#18158). With a GitHub client
// the derived entries are listed from accepted request issues first
// (hivecommons/hive#18159) and kept current by every config reload and a
// periodic pass.
func (b *boot) wireReferenceRepos() {
	if b.githubProxy == nil {
		return
	}
	rt := b.newReferenceRepoRuntime()
	if len(b.cfg.Project.ReferenceRepos) == 0 && rt.deriver == nil {
		return
	}
	b.wireReferenceReposWith(rt)
}

// newReferenceRepoRuntime builds the seams: the hive owner's identity store
// (hivecommons/hive#18197) for the write credential and the live GitHub
// client for request-issue discovery and notices.
func (b *boot) newReferenceRepoRuntime() *referenceRepoRuntime {
	owner := b.githubOwnerIdentity()
	rt := &referenceRepoRuntime{
		deps: referenceRepoDeps{
			ownerLogin: owner.Login,
			ownerToken: owner.Token,
			whoAmI:     b.tokenUserLogin,
		},
		owner:    owner,
		interval: referenceRepoDeriveInterval(),
	}
	if b.ghClient == nil {
		return rt
	}
	rt.post = func(ctx context.Context, issueRepo string, number int, readOnly []config.ReferenceRepo) (bool, error) {
		return b.ghClient.CommentDerivedReadOnly(ctx, issueRepo, number, readOnly)
	}
	rt.deriver = newReferenceRepoDeriver(
		func(ctx context.Context, repo, label string, limit int) ([]github.RequestIssueCandidate, error) {
			return b.ghClient.ListRequestIssueCandidates(ctx, repo, label, limit)
		},
		func(ctx context.Context, repo string, number int, label string) (github.RequestIssueAccepter, error) {
			return b.ghClient.RequestIssueAccepterFor(ctx, repo, number, label)
		},
		workflowIntakeResolver(b.cfg, wfstore.Open(wfstore.Root(b.cfg.Data.WorkflowsDir))),
		b.cfg.GitHub.HostLabel(),
	)
	return rt
}

// wireReferenceReposWith is wireReferenceRepos with the runtime injected
// (tests supply fake listers). It runs the boot pass, exits on a hand-written
// credential error and starts the periodic re-derive.
func (b *boot) wireReferenceReposWith(rt *referenceRepoRuntime) {
	b.referenceRepos = rt
	if err := b.applyReferenceRepos(b.ctx, rt); err != nil {
		b.logger.Error("refusing to start: project.reference_repos write-mode entry has no usable credential (#18015)", "error", err.Error())
		if errors.Is(err, github.ErrReferenceRepoNoCredential) {
			fmt.Fprintln(os.Stderr, err.Error())
		}
		os.Exit(referenceRepoCredentialExitCode)
	}
	b.startReferenceRepoDerivePoller(b.ctx, rt)
}

// applyEditedReferenceRepos is the dashboard editor's hook
// (hivecommons/hive#18136): it verifies the write credentials of the edited
// hand-written refs and, only when every entry passes, installs the rebuilt
// policies (derived entries from the last pass stay merged in). The error is
// returned to the owner verbatim and the previous list is kept.
func (b *boot) applyEditedReferenceRepos(ctx context.Context, refs []config.ReferenceRepo) error {
	rt := b.referenceRepos
	if rt == nil {
		rt = b.newReferenceRepoRuntime()
	}
	if b.githubProxy == nil || b.cfg == nil {
		return nil
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	merged := mergedReferenceRepos(b.cfg, config.HandWrittenReferenceRepos(refs), rt.lastDerived, b.logger)
	res, err := buildReferenceRepoPolicies(ctx, merged, rt.deps, b.logger)
	if err != nil {
		return err
	}
	b.githubProxy.SetReferenceRepos(res.policies)
	return nil
}

// githubOwnerIdentity returns the hive owner's GitHub identity store
// (hivecommons/hive#18197), shared by the reference-repo pass and the
// dashboard's connect/disconnect handlers so both see one validity state.
func (b *boot) githubOwnerIdentity() *github.OwnerIdentityStore {
	b.ownerIdentityOnce.Do(func() {
		b.ownerIdentity = github.NewOwnerIdentityStore(github.OwnerIdentityFile(), b.tokenUserLogin)
	})
	return b.ownerIdentity
}

// tokenUserLogin resolves a user token's login (GET /user) against the
// hive's GitHub API. The API URL is read per call: the App auth is replaced
// on key rotation.
func (b *boot) tokenUserLogin(ctx context.Context, token string) (string, error) {
	apiURL := ""
	if b.ghClient != nil {
		if auth := b.ghClient.AppAuth(); auth != nil {
			apiURL = auth.APIURL()
		}
	}
	return github.TokenUserLogin(ctx, token, apiURL)
}

// onGitHubOwnerIdentityChanged is the dashboard's hook after the owner
// connects or disconnects their GitHub account: the reference repos are
// re-resolved in the background so proxy policies flip without a restart.
func (b *boot) onGitHubOwnerIdentityChanged() {
	if b.referenceRepos == nil || b.ctx == nil {
		return
	}
	go b.rederiveReferenceRepos(b.ctx)
}
