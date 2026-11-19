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
	// writeIdentity reads project.remote_write_identity as of now; nil reads
	// as the default (bot).
	writeIdentity func() string
	// app is the hive's GitHub App seam for the bot identity, read per call
	// because the AppAuth is replaced on key rotation; nil, or a zero value,
	// means no App path (bot falls back to the owner, bot_only is read-only).
	app func() github.ReferenceRepoApp
}

// appSeam is deps.app with a nil guard.
func (d referenceRepoDeps) appSeam() github.ReferenceRepoApp {
	if d.app == nil {
		return github.ReferenceRepoApp{}
	}
	return d.app()
}

// writeIdentitySetting is deps.writeIdentity with a nil guard.
func (d referenceRepoDeps) writeIdentitySetting() string {
	if d.writeIdentity == nil {
		return ""
	}
	return d.writeIdentity()
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
// entry (hivecommons/hive#18158) is resolved instead, never an error. Which
// identity a write-tier entry writes as follows project.remote_write_identity
// (github.ResolveReferenceRepoCredential): the App bot where the App covers
// the repo, the owner's connected account, or read-only. Only entries in refs
// ever get a token source, so an App installation shared by other hives is
// never a grant by itself.
func buildReferenceRepoPolicies(ctx context.Context, refs []config.ReferenceRepo, deps referenceRepoDeps, logger *slog.Logger) (referenceRepoResolution, error) {
	if deps.refreshOwner != nil {
		deps.refreshOwner(ctx)
	}
	owner := deps.ownerCredential()
	setting := deps.writeIdentitySetting()
	app := deps.appSeam()
	res := referenceRepoResolution{
		policies: make([]proxy.ReferenceRepoPolicy, 0, len(refs)),
		repos:    make([]config.ReferenceRepo, 0, len(refs)),
	}
	for _, rr := range refs {
		resolved, cred, err := github.ResolveReferenceRepoCredential(ctx, rr, setting, owner, app, deps.whoAmI)
		if err != nil {
			return referenceRepoResolution{}, err
		}
		if cred.LookupErr != nil {
			logger.Warn("reference repo: App installation lookup failed, serving read-only this pass", "repo", rr.Repo, "error", cred.LookupErr.Error())
		}
		rr = resolved
		pol := proxy.ReferenceRepoPolicy{Repo: rr.Repo, Issues: rr.WritesIssues(), PRs: rr.WritesPRs()}
		if rr.CredentialState == config.ReferenceCredentialUnresolved {
			// No credential this pass (a hand-written entry keeps its Write
			// opt-in in config): the proxy refuses its writes until the next
			// pass resolves it.
			pol = proxy.ReferenceRepoPolicy{Repo: rr.Repo}
		} else if !rr.ReadOnly() {
			pol.Login = cred.Login
			if cred.Identity == config.ReferenceIdentityBot {
				pol.Token = github.BotReferenceRepoTokenSource(rr, deps.appSeam, logger)
			} else {
				pol.Token = github.ReferenceRepoTokenSource(rr, deps.ownerToken, logger)
			}
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
	rt.deps.writeIdentity = func() string {
		if b.cfg == nil {
			return ""
		}
		return b.cfg.Project.RemoteWriteIdentity
	}
	rt.deps.app = func() github.ReferenceRepoApp {
		if b.ghClient == nil {
			return github.ReferenceRepoApp{}
		}
		return b.ghClient.AppAuth().ReferenceRepoApp()
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

// checkRemoteWriteIdentity is the dashboard's dry run for a
// project.remote_write_identity change (hivecommons/hive#18516): the
// hand-written reference repos are resolved under setting with the live
// seams, and the error a hand-written write entry would stop the next boot
// with is returned so the change is refused instead. Nothing is installed.
func (b *boot) checkRemoteWriteIdentity(ctx context.Context, setting string) error {
	if b.githubProxy == nil || b.cfg == nil {
		return nil
	}
	if err := config.ValidateRemoteWriteIdentity(setting); err != nil {
		return err
	}
	rt := b.referenceRepos
	if rt == nil {
		rt = b.newReferenceRepoRuntime()
	}
	deps := rt.deps
	deps.writeIdentity = func() string { return setting }
	_, err := buildReferenceRepoPolicies(ctx, config.HandWrittenReferenceRepos(b.cfg.Project.ReferenceRepos), deps, b.logger)
	return err
}

// remoteWriteIdentityChanged is the dashboard's hook after the setting was
// saved: one full pass (derive, resolve, install) so the next write already
// carries the new identity and the Settings page reloads the resolved state.
// It runs on the boot context, not the request's, because the pass starts
// background work (the read-only notice, the own-item follow-up).
func (b *boot) remoteWriteIdentityChanged() {
	if b.referenceRepos == nil || b.ctx == nil {
		return
	}
	b.rederiveReferenceRepos(b.ctx)
}
