package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/refderive"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
)

// Derived reference repos (hivecommons/hive#18159, RFC #18139 step 3).
//
// An assessment hive's authorized repo holds request issues. Once a
// maintainer applies refderive.GrantLabel to one, the repos the request names
// become project.reference_repos entries at the issues+prs tier, in memory
// only: this file lists those issues at boot, on every config reload and on
// a timer, runs the pure refderive.Derive on each, merges the result under
// the operator's hand-written entries and installs the outcome on the proxy.
// Nothing here writes a config file; a closed issue or a removed label simply
// stops yielding entries and the next pass drops them.

// referenceRepoDeriveIntervalEnv overrides how often the hive re-lists the
// request issues between config reloads.
const referenceRepoDeriveIntervalEnv = "HIVE_REFERENCE_REPOS_DERIVE_INTERVAL"

// referenceRepoDeriveIntervalDefault is the re-derive cadence when the env
// var is unset or unusable: long enough to cost one list call per authorized
// repo per ten minutes, short enough that an ended grant is gone before an
// agent's next session.
const referenceRepoDeriveIntervalDefault = 10 * time.Minute

// maxRequestIssuesPerRepoPerPass bounds how many labelled issues one pass
// reads per authorized repo: an assessment hive has a handful of open
// requests, and a repo with hundreds of triage/accepted issues is not one
// whose model should be deriving anything.
const maxRequestIssuesPerRepoPerPass = 50

// referenceRepoDeriveInterval reads the re-derive cadence from the
// environment, falling back to the default for an unset, unparseable or
// non-positive value.
func referenceRepoDeriveInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv(referenceRepoDeriveIntervalEnv))
	if raw == "" {
		return referenceRepoDeriveIntervalDefault
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return referenceRepoDeriveIntervalDefault
	}
	return d
}

// requestIssueLister lists the open issues in an authorized repo carrying
// label. Production is github.Client.ListRequestIssueCandidates.
type requestIssueLister func(ctx context.Context, repo, label string, limit int) ([]github.RequestIssueCandidate, error)

// requestIssueAccepterFunc reads who applied label to repo#number and their
// permission. Production is github.Client.RequestIssueAccepterFor.
type requestIssueAccepterFunc func(ctx context.Context, repo string, number int, label string) (github.RequestIssueAccepter, error)

// intakeResolver returns the intake declaration of the workflow model bound
// to an authorized repo ("owner/name"), and false when the repo is unbound,
// the model is unusable or it declares no intake.referenceRepoFields.
type intakeResolver func(repo string) (workflowmodel.Intake, bool)

// workflowIntakeResolver resolves bindings the same way the stage engine does
// (stageEngineWorkflowFor): cfg is read on every call so a reload or a
// dashboard binding edit takes effect on the next pass, and the model is read
// from the store rather than compiled because the intake declaration lives on
// the model's spec, not on the compiled profile.
func workflowIntakeResolver(cfg *config.Config, st *wfstore.Store) intakeResolver {
	return func(repo string) (workflowmodel.Intake, bool) {
		if cfg == nil || st == nil || !cfg.Workflow.Enabled {
			return workflowmodel.Intake{}, false
		}
		b, ok := cfg.WorkflowBindingFor(repo)
		if !ok {
			return workflowmodel.Intake{}, false
		}
		m, _, err := st.Get(b.Name, b.Version)
		if err != nil || m == nil {
			return workflowmodel.Intake{}, false
		}
		return m.Spec.Intake, m.Spec.Intake.DerivesReferenceRepos()
	}
}

// accepterCacheEntry is the accepter read for one request issue, valid while
// the issue's updated_at has not moved.
type accepterCacheEntry struct {
	updatedAt time.Time
	accepter  github.RequestIssueAccepter
}

// referenceRepoDeriver runs derivation passes. It remembers the accepter per
// issue so an unchanged issue costs no timeline or permission read, and the
// last good entries per authorized repo so a listing error keeps the previous
// grant instead of dropping it.
type referenceRepoDeriver struct {
	list      requestIssueLister
	accepter  requestIssueAccepterFunc
	intakeFor intakeResolver
	// host is the forge host repository URLs must be on (refderive.Options).
	host string

	mu sync.Mutex
	// cache is keyed by the issue ref ("owner/repo#N").
	cache map[string]accepterCacheEntry
	// lastByRepo holds the entries the previous pass derived from each
	// authorized repo's issues, unresolved (as refderive produced them).
	lastByRepo map[string][]config.ReferenceRepo
}

func newReferenceRepoDeriver(list requestIssueLister, accepter requestIssueAccepterFunc, intakeFor intakeResolver, host string) *referenceRepoDeriver {
	return &referenceRepoDeriver{
		list:       list,
		accepter:   accepter,
		intakeFor:  intakeFor,
		host:       host,
		cache:      map[string]accepterCacheEntry{},
		lastByRepo: map[string][]config.ReferenceRepo{},
	}
}

// authorizedRepoSlugs is project.repos as qualified "owner/name" slugs.
func authorizedRepoSlugs(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Project.Repos))
	for _, repo := range cfg.Project.Repos {
		out = append(out, config.QualifyRepo(cfg.Project.Org, repo))
	}
	return out
}

// bodyNamesIntakeField is the cheap pre-filter: an issue whose body carries
// none of the model's reference-repo headings was not filed from the request
// form, so its timeline and the accepter's permission are never fetched.
func bodyNamesIntakeField(body string, intake workflowmodel.Intake) bool {
	lower := strings.ToLower(body)
	for _, f := range intake.ReferenceRepoFields {
		f = strings.ToLower(strings.TrimSpace(f))
		if f != "" && strings.Contains(lower, f) {
			return true
		}
	}
	return false
}

// entriesFrom returns the entries in list derived from the issue ref.
func entriesFrom(list []config.ReferenceRepo, ref string) []config.ReferenceRepo {
	var out []config.ReferenceRepo
	for _, rr := range list {
		if rr.DerivedFrom == ref {
			out = append(out, rr)
		}
	}
	return out
}

// derive runs one pass over every authorized repo whose bound model derives
// reference repos and returns the derived entries, unresolved, in authorized
// repo order then issue order. A nil deriver derives nothing. Errors never
// propagate: a repo whose listing failed keeps its previous entries, an
// issue whose accepter could not be read keeps its previous entries, and a
// refused grant is logged at debug and yields nothing.
func (d *referenceRepoDeriver) derive(ctx context.Context, cfg *config.Config, logger *slog.Logger) []config.ReferenceRepo {
	if d == nil || cfg == nil || d.list == nil || d.accepter == nil || d.intakeFor == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	authorized := authorizedRepoSlugs(cfg)
	opts := refderive.Options{Host: d.host, Authorized: authorized}
	seenIssues := map[string]bool{}
	nextByRepo := make(map[string][]config.ReferenceRepo, len(d.lastByRepo))
	var out []config.ReferenceRepo
	for _, full := range authorized {
		intake, ok := d.intakeFor(full)
		if !ok {
			continue
		}
		previous := d.lastByRepo[full]
		cands, err := d.list(ctx, full, refderive.GrantLabel, maxRequestIssuesPerRepoPerPass)
		if err != nil {
			logger.Warn("derived reference repos: request issues not listed, keeping previous entries", "repo", full, "kept", len(previous), "error", err.Error())
			for _, rr := range previous {
				seenIssues[rr.DerivedFrom] = true
			}
			nextByRepo[full] = previous
			out = append(out, previous...)
			continue
		}
		var entries []config.ReferenceRepo
		for _, cand := range cands {
			if cand.Closed || !bodyNamesIntakeField(cand.Body, intake) {
				continue
			}
			issue := refderive.RequestIssue{
				Repo:   cand.Repo,
				Number: cand.Number,
				Title:  cand.Title,
				Body:   cand.Body,
				Closed: cand.Closed,
				Labels: cand.Labels,
			}
			ref := issue.Ref()
			seenIssues[ref] = true
			cached, hit := d.cache[ref]
			if !hit || !cached.updatedAt.Equal(cand.UpdatedAt) {
				acc, err := d.accepter(ctx, cand.Repo, cand.Number, refderive.GrantLabel)
				if err != nil {
					kept := entriesFrom(previous, ref)
					logger.Warn("derived reference repos: accepter not read, keeping previous entries for the issue", "issue", ref, "kept", len(kept), "error", err.Error())
					entries = append(entries, kept...)
					continue
				}
				cached = accepterCacheEntry{updatedAt: cand.UpdatedAt, accepter: acc}
				d.cache[ref] = cached
			}
			issue.AcceptedBy = cached.accepter.Login
			issue.AcceptedByIsBot = cached.accepter.IsBot
			issue.AcceptedByPermission = cached.accepter.Permission
			res, err := refderive.Derive(issue, intake, opts)
			if err != nil {
				logger.Debug("derived reference repos: request issue grants nothing", "issue", ref, "reason", err.Error())
				continue
			}
			for _, sk := range res.Skipped {
				logger.Debug("derived reference repos: repository skipped", "issue", ref, "repo", sk.Repo, "reason", sk.Reason)
			}
			entries = append(entries, res.Repos...)
		}
		nextByRepo[full] = entries
		out = append(out, entries...)
	}
	for ref := range d.cache {
		if !seenIssues[ref] {
			delete(d.cache, ref)
		}
	}
	d.lastByRepo = nextByRepo
	return out
}

// clampDerivedWrite returns write restricted to the surfaces a derived entry
// may ever carry (refderive.DerivedWrite): the parser only emits that tier,
// the merge asserts it.
func clampDerivedWrite(write []string) []string {
	var out []string
	for _, w := range write {
		for _, allowed := range refderive.DerivedWrite {
			if strings.EqualFold(strings.TrimSpace(w), allowed) {
				out = append(out, allowed)
				break
			}
		}
	}
	return out
}

// mergeReferenceRepos is the pure merge of derived entries under the
// operator's hand-written list. Hand-written entries come first and
// unchanged; a derived entry is dropped when a hand-written entry names the
// same repo (the operator's tier wins), when the repo is an authorized repo
// (never both), when it carries no provenance, or when an earlier derived
// entry already named the repo (first wins; the other is logged). Every kept
// derived entry's Write is clamped to issues+prs. Other derived fields
// (CredentialState, CredentialReason) pass through so a resolved entry can be
// re-merged after a reload without a new credential lookup.
func mergeReferenceRepos(handWritten, derived []config.ReferenceRepo, authorized []string, logger *slog.Logger) []config.ReferenceRepo {
	if logger == nil {
		logger = slog.Default()
	}
	key := func(repo string) string { return strings.ToLower(strings.TrimSpace(repo)) }
	taken := make(map[string]string, len(handWritten)+len(derived))
	out := make([]config.ReferenceRepo, 0, len(handWritten)+len(derived))
	for _, rr := range handWritten {
		taken[key(rr.Repo)] = "project.reference_repos"
		out = append(out, rr)
	}
	authorizedKeys := make(map[string]bool, len(authorized))
	for _, a := range authorized {
		authorizedKeys[key(a)] = true
	}
	for _, rr := range derived {
		k := key(rr.Repo)
		switch {
		case k == "" || !rr.Derived():
			logger.Warn("derived reference repos: entry without provenance dropped", "repo", rr.Repo)
			continue
		case authorizedKeys[k]:
			logger.Info("derived reference repos: authorized repo is never a reference repo", "repo", rr.Repo, "from", rr.DerivedFrom)
			continue
		}
		if by, dup := taken[k]; dup {
			logger.Info("derived reference repos: repo already listed, first wins", "repo", rr.Repo, "from", rr.DerivedFrom, "kept", by)
			continue
		}
		taken[k] = rr.DerivedFrom
		rr.Write = clampDerivedWrite(rr.Write)
		rr.WriteTokenFile = ""
		out = append(out, rr)
	}
	return out
}

// referenceRepoRuntime is the state the lifecycle keeps between passes: the
// seams, the deriver and the derived entries as last resolved, so a config
// reload can put them back in memory before the next pass re-lists.
type referenceRepoRuntime struct {
	deriver *referenceRepoDeriver
	deps    referenceRepoDeps
	post    derivedReadOnlyPoster
	// interval is the periodic re-derive cadence; zero disables the poller.
	interval time.Duration

	mu sync.Mutex
	// lastDerived are the derived entries as buildReferenceRepoPolicies
	// resolved them on the last successful pass.
	lastDerived []config.ReferenceRepo
}

// derivedEntries returns the derived entries of a resolved list.
func derivedEntries(refs []config.ReferenceRepo) []config.ReferenceRepo {
	var out []config.ReferenceRepo
	for _, rr := range refs {
		if rr.Derived() {
			out = append(out, rr)
		}
	}
	return out
}

// mergedReferenceRepos validates the merge against the live project; a merge
// that would not load from a file falls back to the hand-written list.
func mergedReferenceRepos(cfg *config.Config, handWritten, derived []config.ReferenceRepo, logger *slog.Logger) []config.ReferenceRepo {
	merged := mergeReferenceRepos(handWritten, derived, authorizedRepoSlugs(cfg), logger)
	check := &config.Config{Project: config.ProjectConfig{Org: cfg.Project.Org, Repos: cfg.Project.Repos, ReferenceRepos: merged}}
	if err := check.ValidateReferenceRepos(); err != nil {
		logger.Warn("derived reference repos: merged list invalid, serving hand-written entries only", "error", err.Error())
		return handWritten
	}
	return merged
}

// applyReferenceRepos runs one full pass: derive, merge, resolve, install.
// On success the resolved list replaces cfg.Project.ReferenceRepos in memory,
// the proxy gets the rebuilt policies (so an entry that vanished loses its
// policy at once) and the read-only notices go out. A credential error on a
// hand-written write-mode entry is returned for the caller to judge: boot
// exits, a later pass keeps the previous installation.
func (b *boot) applyReferenceRepos(ctx context.Context, rt *referenceRepoRuntime) error {
	if rt == nil || b.cfg == nil || b.githubProxy == nil {
		return nil
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	handWritten := config.HandWrittenReferenceRepos(b.cfg.Project.ReferenceRepos)
	derived := rt.deriver.derive(ctx, b.cfg, b.logger)
	merged := mergedReferenceRepos(b.cfg, handWritten, derived, b.logger)
	res, err := buildReferenceRepoPolicies(ctx, merged, rt.deps, b.logger)
	if err != nil {
		return err
	}
	// The resolved entries replace the list in memory only: a derived entry
	// is never written back to any config file (yaml:"-" on its provenance
	// and the save test in reference_repos_derive_test.go guard that), and
	// its credential state is what the kick and /api/status read.
	b.cfg.Project.ReferenceRepos = res.repos
	b.githubProxy.SetReferenceRepos(res.policies)
	rt.lastDerived = derivedEntries(res.repos)
	b.logger.Info("reference repos wired", "count", len(res.policies), "derived", len(rt.lastDerived), "repos", b.cfg.ReferenceRepoSlugs())
	// Off the caller's path: the notice is a GitHub write and must not delay
	// the proxy coming up or a reload completing.
	go notifyDerivedReadOnly(ctx, res.repos, rt.post, b.logger)
	return nil
}

// rederiveReferenceRepos is the pass a reload or the poller runs: never
// fatal, the previous installation stays when it fails.
func (b *boot) rederiveReferenceRepos(ctx context.Context) {
	rt := b.referenceRepos
	if rt == nil {
		return
	}
	if err := b.applyReferenceRepos(ctx, rt); err != nil {
		b.logger.Warn("reference repos: re-derive failed, previous policies kept", "error", err.Error())
	}
}

// reconcileReferenceReposAfterReload runs inside the config reload hook,
// after the new config was swapped in: the reload rebuilt
// cfg.Project.ReferenceRepos from the file, so the derived entries the last
// pass resolved are merged back at once (no network, so the kick never loses
// them while the pass runs), then a full pass re-lists the request issues in
// the background so a grant that ended drops without an operator action.
func (b *boot) reconcileReferenceReposAfterReload() {
	if b.referenceRepos == nil || b.cfg == nil {
		return
	}
	b.remergeLastDerivedReferenceRepos()
	go b.rederiveReferenceRepos(b.ctx)
}

// remergeLastDerivedReferenceRepos puts the derived entries of the last
// successful pass back under the hand-written list now in cfg.
func (b *boot) remergeLastDerivedReferenceRepos() {
	rt := b.referenceRepos
	if rt == nil || b.cfg == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	handWritten := config.HandWrittenReferenceRepos(b.cfg.Project.ReferenceRepos)
	b.cfg.Project.ReferenceRepos = mergedReferenceRepos(b.cfg, handWritten, rt.lastDerived, b.logger)
}

// startReferenceRepoDerivePoller re-derives on rt.interval until ctx ends.
func (b *boot) startReferenceRepoDerivePoller(ctx context.Context, rt *referenceRepoRuntime) {
	if rt == nil || rt.deriver == nil || rt.interval <= 0 || ctx == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(rt.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.rederiveReferenceRepos(ctx)
			}
		}
	}()
}
