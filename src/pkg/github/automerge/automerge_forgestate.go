package automerge

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/forgestate"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// OpenPRSource is the forgestate surface the self-authored, trusted-author
// and contributor lanes read (hivecommons/hive#13377). With one wired in,
// no lane lists /pulls itself: every lane reads the one cached open-PR set
// per repo, and the merge step reads the PR through with a freshness bound.
// *forgestate.Store satisfies it.
type OpenPRSource interface {
	Snapshot(repo string) (forgestate.Snapshot, bool)
	ReadThrough(ctx context.Context, repo string, kind forgestate.Kind, n int, bound time.Duration) (forgestate.Item, bool, error)
}

// readyWaiter is the optional startup gate an OpenPRSource may offer
// (*forgestate.Store does): the event-driven loop holds its first full pass
// until every repo has completed one reconcile.
type readyWaiter interface {
	WaitReady(ctx context.Context) error
}

const (
	// automergeSweepFloorInterval is the periodic fallback pass the
	// event-driven lanes run when no webhook has named a PR. It exists so
	// a lost delivery or a webhook outage still lands eligible PRs; the
	// 10s ticker it replaces was the lanes' only trigger.
	automergeSweepFloorInterval = 5 * time.Minute
	// automergeMergeFreshnessBound is how old the cached PR may be at the
	// merge step. An item older than this is re-fetched once (a targeted
	// refresh through the cache) before the merge is allowed.
	automergeMergeFreshnessBound = 60 * time.Second

	autoMergeReasonForgeStateNotReady    = "forgestate-not-ready"
	autoMergeReasonForgeStateStale       = "forgestate-stale"
	autoMergeReasonForgeStateUnknownRepo = "forgestate-unknown-repo"
	autoMergeReasonForgeStateRefresh     = "forgestate-refresh"
	autoMergeReasonHeadChangedSinceEval  = "head-changed-since-eval"
	autoMergeReasonGone                  = "gone"
)

// SweepTargets names the PRs one event-driven pass evaluates, keyed by
// lower-cased owner/name. A nil SweepTargets means every PR of every repo.
type SweepTargets map[string]map[int]bool

func canonicalSweepRepo(repo string) string { return strings.ToLower(strings.TrimSpace(repo)) }

func (t SweepTargets) add(repo string, n int) bool {
	if n <= 0 {
		return false
	}
	key := canonicalSweepRepo(repo)
	if key == "" {
		return false
	}
	set := t[key]
	if set == nil {
		set = map[int]bool{}
		t[key] = set
	}
	if set[n] {
		return false
	}
	set[n] = true
	return true
}

// selectsRepo reports whether the pass must visit repo at all.
func (t SweepTargets) selectsRepo(repo string) bool {
	if t == nil {
		return true
	}
	return len(t[canonicalSweepRepo(repo)]) > 0
}

// selects reports whether the pass must evaluate PR n of repo.
func (t SweepTargets) selects(repo string, n int) bool {
	if t == nil {
		return true
	}
	return t[canonicalSweepRepo(repo)][n]
}

// Numbers returns the targeted PR numbers of repo, ascending (tests, logs).
func (t SweepTargets) Numbers(repo string) []int {
	set := t[canonicalSweepRepo(repo)]
	out := make([]int, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// SweepTrigger turns webhook hints into the next lane pass. A delivery for a
// PR (check_suite, check_run, status, pull_request_review, pull_request)
// names it here; the event-driven loop drains the accumulated set and
// evaluates only those PRs. Several deliveries between two passes coalesce
// into one pass. It is safe for concurrent use.
type SweepTrigger struct {
	mu      sync.Mutex
	source  OpenPRSource
	pending SweepTargets
	full    bool
	wake    chan struct{}
	// notified counts PRs accepted since start (status, tests).
	notified int
}

// NewSweepTrigger returns a trigger; source (may be nil) resolves a head
// SHA to its PR for `status` deliveries, which carry no PR number.
func NewSweepTrigger(source OpenPRSource) *SweepTrigger {
	return &SweepTrigger{source: source, pending: SweepTargets{}, wake: make(chan struct{}, 1)}
}

// NotifyPullRequests schedules an event-driven pass for the named PRs of
// repo and reports how many were newly scheduled.
func (t *SweepTrigger) NotifyPullRequests(repo string, numbers ...int) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	added := 0
	for _, n := range numbers {
		if t.pending.add(repo, n) {
			added++
		}
	}
	t.notified += added
	t.mu.Unlock()
	if added > 0 {
		t.signal()
	}
	return added
}

// NotifyHead schedules a pass for the open PR of repo whose head is sha,
// looked up in the cached snapshot. It reports how many PRs matched.
func (t *SweepTrigger) NotifyHead(repo, sha string) int {
	if t == nil || t.source == nil {
		return 0
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return 0
	}
	snap, ok := t.source.Snapshot(repo)
	if !ok {
		return 0
	}
	var numbers []int
	for n, it := range snap.PRs {
		if it.HeadSHA == sha {
			numbers = append(numbers, n)
		}
	}
	return t.NotifyPullRequests(repo, numbers...)
}

// RequestFull schedules a pass over every repo and PR.
func (t *SweepTrigger) RequestFull() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.full = true
	t.mu.Unlock()
	t.signal()
}

// Pending reports how many PRs are waiting for the next pass.
func (t *SweepTrigger) Pending() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, set := range t.pending {
		n += len(set)
	}
	return n
}

func (t *SweepTrigger) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// C is the channel the event-driven loop selects on.
func (t *SweepTrigger) C() <-chan struct{} {
	if t == nil {
		return nil
	}
	return t.wake
}

// take drains the pending set. A full request yields nil targets.
func (t *SweepTrigger) take() SweepTargets {
	t.mu.Lock()
	defer t.mu.Unlock()
	targets, full := t.pending, t.full
	t.pending, t.full = SweepTargets{}, false
	if full {
		return nil
	}
	return targets
}

// pullRequestFromItem rebuilds the list-shaped PullRequest the lanes'
// prefilters and stage observations read from a cached item.
func pullRequestFromItem(it forgestate.Item) *gh.PullRequest {
	pr := &gh.PullRequest{
		Number:  gh.Ptr(it.Number),
		State:   gh.Ptr(it.State),
		Draft:   gh.Ptr(it.Draft),
		Merged:  gh.Ptr(it.Merged),
		Title:   gh.Ptr(it.Title),
		Body:    gh.Ptr(it.Body),
		HTMLURL: gh.Ptr(it.HTMLURL),
		Head:    &gh.PullRequestBranch{SHA: gh.Ptr(it.HeadSHA), Ref: gh.Ptr(it.HeadRef)},
		Base:    &gh.PullRequestBranch{Ref: gh.Ptr(it.BaseRef)},
	}
	if it.MergeableState != "" {
		pr.MergeableState = gh.Ptr(it.MergeableState)
	}
	if it.Author != "" {
		pr.User = &gh.User{Login: gh.Ptr(it.Author)}
	}
	if it.HeadRepo != "" {
		pr.Head.Repo = &gh.Repository{FullName: gh.Ptr(it.HeadRepo)}
	}
	if !it.CreatedAt.IsZero() {
		pr.CreatedAt = &gh.Timestamp{Time: it.CreatedAt}
	}
	if !it.UpdatedAt.IsZero() {
		pr.UpdatedAt = &gh.Timestamp{Time: it.UpdatedAt}
	}
	for _, l := range it.Labels {
		pr.Labels = append(pr.Labels, &gh.Label{Name: gh.Ptr(l)})
	}
	return pr
}

// openPullRequests returns the open PRs of owner/repoName a lane evaluates
// this pass, narrowed to targets when the pass is event-driven. With an
// OpenPRSource wired it reads the cached set and never lists GitHub; a
// repo whose snapshot is not ready yet or is stale is skipped with the
// returned reason (nothing from a stale snapshot may drive a merge).
// Without a source it lists /pulls as before. The cache and the targets
// are keyed by owner/name, never by the configured (possibly bare) name.
func (c *Engine) openPullRequests(ctx context.Context, owner, repoName string, targets SweepTargets) ([]*gh.PullRequest, string, error) {
	fullRepo := owner + "/" + repoName
	if c.openPRs == nil {
		prs, err := c.listOpenPullRequests(ctx, owner, repoName)
		if err == nil {
			c.rememberClaimants(fullRepo, prs)
		}
		if err != nil || targets == nil {
			return prs, "", err
		}
		out := prs[:0]
		for _, pr := range prs {
			if pr != nil && targets.selects(fullRepo, pr.GetNumber()) {
				out = append(out, pr)
			}
		}
		return out, "", nil
	}
	snap, ok := c.openPRs.Snapshot(fullRepo)
	switch {
	case !ok:
		return nil, autoMergeReasonForgeStateUnknownRepo, nil
	case !snap.Ready:
		return nil, autoMergeReasonForgeStateNotReady, nil
	case snap.Stale:
		return nil, autoMergeReasonForgeStateStale, nil
	}
	numbers := make([]int, 0, len(snap.PRs))
	for n := range snap.PRs {
		if targets.selects(fullRepo, n) {
			numbers = append(numbers, n)
		}
	}
	sort.Ints(numbers)
	out := make([]*gh.PullRequest, 0, len(numbers))
	for _, n := range numbers {
		out = append(out, pullRequestFromItem(snap.PRs[n]))
	}
	return out, "", nil
}

// forgestateMergeGate is the read-through in front of every lane's merge
// call. With no OpenPRSource it allows. Otherwise the PR must come back
// from the cache fetched within automergeMergeFreshnessBound — older items
// are refreshed once — from a snapshot that is ready and not stale, still
// open, and still at the head the lane evaluated. Any other answer refuses
// with a skip reason; a refresh failure is returned as the error.
func (c *Engine) forgestateMergeGate(ctx context.Context, owner, repo string, number int, evaluatedHeadSHA string) (string, error) {
	if c == nil || c.openPRs == nil {
		return "", nil
	}
	it, ok, err := c.openPRs.ReadThrough(ctx, owner+"/"+repo, forgestate.KindPR, number, automergeMergeFreshnessBound)
	switch {
	case errors.Is(err, forgestate.ErrStale):
		return autoMergeReasonForgeStateStale, nil
	case errors.Is(err, forgestate.ErrNotReady):
		return autoMergeReasonForgeStateNotReady, nil
	case errors.Is(err, forgestate.ErrUnknownRepo):
		return autoMergeReasonForgeStateUnknownRepo, nil
	case err != nil:
		return autoMergeReasonForgeStateRefresh, err
	case !ok:
		return autoMergeReasonGone, nil
	case it.HeadSHA == "" || it.HeadSHA != evaluatedHeadSHA:
		return autoMergeReasonHeadChangedSinceEval, nil
	}
	return "", nil
}

// Trigger returns the engine's sweep trigger; nil when no OpenPRSource is
// wired (the lanes then run on their own ticker and nothing can nudge them).
func (c *Engine) Trigger() *SweepTrigger {
	if c == nil {
		return nil
	}
	return c.sweepTrigger
}

func (c *Engine) floorInterval() time.Duration {
	if c.sweepFloor > 0 {
		return c.sweepFloor
	}
	return automergeSweepFloorInterval
}

// runEventDrivenSweep is the lanes' loop with an OpenPRSource wired: one
// full pass once the source is ready, then a targeted pass per trigger and
// a full pass every automergeSweepFloorInterval as the fallback.
func (c *Engine) runEventDrivenSweep(ctx context.Context, maxMerges int, acmmAllowed bool) {
	if w, ok := c.openPRs.(readyWaiter); ok {
		if err := w.WaitReady(ctx); err != nil {
			return
		}
	}
	floor := time.NewTicker(c.floorInterval())
	defer floor.Stop()
	c.sweepClock.armFloorEvery(c.now(), c.floorInterval())
	c.sweepPass(ctx, maxMerges, acmmAllowed, nil, "startup")
	for {
		select {
		case <-ctx.Done():
			return
		case <-floor.C:
			c.sweepClock.armFloorEvery(c.now(), c.floorInterval())
			// The floor pass is low-priority work like the legacy tick: under
			// rate-limit caution it yields to the queue (#13382); event passes
			// are targeted and still run.
			if hgithub.ShedLowPriorityTick("automerge:floor-sweep", selfAuthoredSweepTickBudget) {
				continue
			}
			c.sweepPass(ctx, maxMerges, acmmAllowed, nil, "floor")
		case <-c.sweepTrigger.C():
			targets := c.sweepTrigger.take()
			c.sweepPass(ctx, maxMerges, acmmAllowed, targets, "event")
		}
	}
}
