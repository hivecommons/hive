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

// referenceRepoCredentialExitCode marks an exit caused by a hand-written
// project.reference_repos entry that opted in to writes without a credential
// GitHub would honour for that repo (hivecommons/hive#18015). Its own code so
// the termination state says "connect the hive owner's GitHub account or
// supply write_token_file", not "bug".
const referenceRepoCredentialExitCode = 22

// derivedReadOnlyPoster posts the one-shot read-only notice on a request
// issue (hivecommons/hive#18158). Production is
// github.Client.CommentDerivedReadOnly; tests stub it.
type derivedReadOnlyPoster func(ctx context.Context, issueRepo string, number int, readOnly []config.ReferenceRepo) (bool, error)

// referenceRepoDeps are the seams buildReferenceRepoPolicies needs: the hive
// owner's connected GitHub identity (hivecommons/hive#18197) and
// token-identity resolution for write_token_file entries. owner may be nil
// when no identity store is wired, in which case a write-mode entry can only
// be served by write_token_file.
type referenceRepoDeps struct {
	// owner reports the owner credential as of now (login, or why none).
	owner func() github.OwnerCredential
	// ownerToken is the proxy's token source for the owner identity.
	ownerToken github.OwnerTokenSource
	// refreshOwner re-validates the stored owner token (rate-limited inside
	// the store) before a pass resolves against it; nil skips.
	refreshOwner func(ctx context.Context)
	whoAmI       func(ctx context.Context, token string) (string, error)
}

// ownerCredential is deps.owner with a nil guard.
func (d referenceRepoDeps) ownerCredential() github.OwnerCredential {
	if d.owner == nil {
		return github.OwnerCredential{Unavailable: github.OwnerIdentityNotConnected}
	}
	return d.owner()
}

// referenceRepoResolution is what buildReferenceRepoPolicies produces: the
// proxy policies and the entries as resolved, with each write entry's
// CredentialState and CredentialLogin filled in (and a derived entry's Write
// cleared when it was downgraded), so the kick and the status payload
// describe what the proxy enforces.
type referenceRepoResolution struct {
	policies []proxy.ReferenceRepoPolicy
	repos    []config.ReferenceRepo
}

// buildReferenceRepoPolicies turns the config list into proxy policies. A
// read-only entry needs nothing; a hand-written write-mode entry is verified
// now so the failure is a boot-time config error naming the repo. A derived
// entry (hivecommons/hive#18158) is resolved instead: write tier when the
// owner identity is usable, read-only otherwise, never an error.
func buildReferenceRepoPolicies(ctx context.Context, refs []config.ReferenceRepo, deps referenceRepoDeps, logger *slog.Logger) (referenceRepoResolution, error) {
	if deps.refreshOwner != nil {
		deps.refreshOwner(ctx)
	}
	owner := deps.ownerCredential()
	res := referenceRepoResolution{
		policies: make([]proxy.ReferenceRepoPolicy, 0, len(refs)),
		repos:    make([]config.ReferenceRepo, 0, len(refs)),
	}
	for _, rr := range refs {
		login := ""
		if rr.Derived() {
			rr, login = github.ResolveDerivedReferenceRepo(rr, owner)
		} else if !rr.ReadOnly() {
			l, err := github.VerifyReferenceRepoWriteCredential(ctx, rr, owner, deps.whoAmI)
			if err != nil {
				return referenceRepoResolution{}, err
			}
			login = l
			rr.CredentialState = config.ReferenceCredentialWriteTier
			rr.CredentialReason = ""
			rr.CredentialLogin = login
		}
		pol := proxy.ReferenceRepoPolicy{Repo: rr.Repo, Issues: rr.WritesIssues(), PRs: rr.WritesPRs()}
		if !rr.ReadOnly() {
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
// (hivecommons/hive#18159) and kept current by every config reload, a
// periodic pass, and every change to the owner identity (#18197).
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

// newReferenceRepoRuntime builds the seams from the live GitHub client and
// the owner identity store. The closures read b.ghClient on every call
// because the client is replaced on App key rotation.
func (b *boot) newReferenceRepoRuntime() *referenceRepoRuntime {
	rt := &referenceRepoRuntime{
		deps:     referenceRepoDeps{},
		interval: referenceRepoDeriveInterval(),
	}
	apiURL := ""
	if b.ghClient != nil {
		if auth := b.ghClient.AppAuth(); auth != nil {
			apiURL = auth.APIURL()
		}
	}
	rt.deps.whoAmI = func(ctx context.Context, token string) (string, error) {
		return github.TokenUserLogin(ctx, token, apiURL)
	}
	if store := b.ownerIdentityStore(); store != nil {
		rt.deps.owner = store.Credential
		rt.deps.ownerToken = store.Token
		rt.deps.refreshOwner = func(ctx context.Context) {
			if err := store.Verify(ctx, rt.deps.whoAmI, false); err != nil && !errors.Is(err, github.ErrOwnerIdentityNotConnected) {
				b.logger.Warn("owner identity: verification skipped this pass", "error", err.Error())
			}
		}
	}
	rt.remoteItems = b.newRemoteItemsPoller(rt)
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

// ownerIdentityChanged is the dashboard's hook after the owner connects,
// reconnects or disconnects their GitHub account (hivecommons/hive#18197):
// the reference-repo policies are re-resolved so a derived entry flips
// between read-only and write-tier without a restart. Off the caller's path;
// the dashboard response does not wait for GitHub.
func (b *boot) ownerIdentityChanged() {
	if b.referenceRepos == nil || b.ctx == nil {
		return
	}
	go b.rederiveReferenceRepos(b.ctx)
}

// ownerIdentityStore returns the boot's owner identity store, creating it on
// first use over github.OwnerIdentityPath() so the dashboard's consent routes
// and the reference-repo wiring share one instance whatever order they are
// built in.
func (b *boot) ownerIdentityStore() *github.OwnerIdentityStore {
	if b.ownerIdentity == nil {
		b.ownerIdentity = github.NewOwnerIdentityStore("")
	}
	return b.ownerIdentity
}
