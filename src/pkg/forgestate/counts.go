package forgestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// CountsReconcileInterval is how often the outcome counters (merged PRs,
// closed issues) are checked against GitHub Search. Between reconciles the
// counters move only on webhook events, so the search path runs hourly
// instead of on every eval cycle (hivecommons/hive#13376).
const CountsReconcileInterval = time.Hour

// CountsReconcileBudget is how many repos one Reconcile pass asks GitHub
// about. Each repo costs two to four Search API calls and search allows 30
// a minute, so a hive with many repos (projectbluefin's 19 → 38–76 calls)
// blew the limit every hour and never produced a baseline. A pass now
// covers a slice of the repos and the rest follow after
// CountsReconcilePace; the round is complete once every repo has been
// asked.
const CountsReconcileBudget = 5

// CountsReconcilePace is the minimum gap between passes of one round while
// repos are still pending — long enough for the per-minute search window
// to reset.
const CountsReconcilePace = 2 * time.Minute

// OpenCounts is the cache's view of one repo's open issues and PRs.
type OpenCounts struct {
	Issues int `json:"issues"`
	PRs    int `json:"prs"`
	// Ready is false until the repo's first full reconcile has landed; the
	// counts are then zero and must not be shown as real.
	Ready bool `json:"ready"`
	// Stale is set while the cache serves last-good items after a failed
	// reconcile; StaleSince is when that began.
	Stale      bool      `json:"stale"`
	StaleSince time.Time `json:"stale_since,omitempty"`
	LastGoodAt time.Time `json:"last_good_at,omitempty"`
}

// OutcomeCounts are the hive-attributed merged-PR and closed-issue totals.
type OutcomeCounts struct {
	MergedPRs    int `json:"merged_prs"`
	ClosedIssues int `json:"closed_issues"`
	// Ready is false until at least one reconcile has succeeded (or a
	// seed was loaded) for every repo; webhook increments alone do not make
	// the counts trustworthy because the baseline is unknown.
	Ready bool `json:"ready"`
	// Partial is set when some but not all repos have a baseline: the totals
	// cover ReposReady of ReposTotal repos and undercount the rest. A partial
	// aggregate is still worth serving (marked) rather than discarded.
	Partial    bool `json:"partial,omitempty"`
	ReposReady int  `json:"repos_ready,omitempty"`
	ReposTotal int  `json:"repos_total,omitempty"`
	// Stale is set when the most recent reconcile failed; the counts are
	// the last good ones plus webhook increments since, and StaleSince is
	// when the failures began.
	Stale       bool      `json:"stale"`
	StaleSince  time.Time `json:"stale_since,omitempty"`
	StaleReason string    `json:"stale_reason,omitempty"`
	// LastGoodAt is when the last successful reconcile landed.
	LastGoodAt time.Time `json:"last_good_at,omitempty"`
	// Source is "webhook" when the last movement was an event increment and
	// "reconcile" right after a successful API check.
	Source Source `json:"source,omitempty"`
}

// OpenSource is what Counts reads open totals from; *Store satisfies it.
type OpenSource interface {
	Snapshot(repo string) (Snapshot, bool)
}

// OutcomeReconciler returns the authoritative merged-PR and closed-issue
// totals for one repo. It is the only place the search API is still asked.
type OutcomeReconciler func(ctx context.Context, repo string) (mergedPRs, closedIssues int, err error)

// CountsOptions configures NewCounts.
type CountsOptions struct {
	Repos []string
	// Open supplies the open issue / PR totals. Optional: without it Open
	// reports not-ready for every repo.
	Open OpenSource
	// Reconcile is the hourly authoritative check. Optional: without it the
	// counters only ever move on webhook events and never become Ready.
	Reconcile OutcomeReconciler
	// Author scopes webhook increments to items opened by this login (the
	// hive actor, usually an App bot). Empty counts every author.
	Author string
	// Interval between reconciles; CountsReconcileInterval by default.
	Interval time.Duration
	// Budget is the most repos one Reconcile pass asks about;
	// CountsReconcileBudget by default. Pace is the gap between passes
	// while repos are pending; CountsReconcilePace by default.
	Budget int
	Pace   time.Duration
	// IsRateLimit reports whether a reconcile error means GitHub is pacing
	// us, in which case the pass stops instead of burning the remaining
	// repos' calls on the same refusal. Defaults to a "rate" substring match.
	IsRateLimit func(error) bool
	Now         func() time.Time
}

type outcomeState struct {
	merged      int
	closed      int
	ready       bool
	stale       bool
	staleSince  time.Time
	staleReason string
	lastGoodAt  time.Time
	source      Source
	// round is the reconcile round this repo was last asked in.
	round int
}

// Counts derives the dashboard's overview totals from the cache and keeps
// the outcome counters that used to be re-searched every cycle. Safe for
// concurrent use.
type Counts struct {
	opts CountsOptions
	mu   sync.RWMutex
	// repos is keyed by canonicalRepo; order keeps Repos() deterministic.
	repos         map[string]*outcomeState
	order         []string
	lastReconcile time.Time
	// round numbers the reconcile rounds; roundStart is when the current
	// one began. A round is complete when every repo's round matches.
	round      int
	roundStart time.Time
}

// NewCounts returns a Counts for o. It does no I/O.
func NewCounts(o CountsOptions) *Counts {
	if o.Interval <= 0 {
		o.Interval = CountsReconcileInterval
	}
	if o.Budget <= 0 {
		o.Budget = CountsReconcileBudget
	}
	if o.Pace <= 0 {
		o.Pace = CountsReconcilePace
	}
	if o.IsRateLimit == nil {
		o.IsRateLimit = isRateLimitError
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	c := &Counts{opts: o, repos: map[string]*outcomeState{}}
	for _, r := range o.Repos {
		c.addRepoLocked(r)
	}
	return c
}

func (c *Counts) addRepoLocked(repo string) *outcomeState {
	key := canonicalRepo(repo)
	if key == "" {
		return nil
	}
	if st, ok := c.repos[key]; ok {
		return st
	}
	st := &outcomeState{}
	c.repos[key] = st
	c.order = append(c.order, key)
	return st
}

// Repos returns the configured repos in canonical form.
func (c *Counts) Repos() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]string(nil), c.order...)
}

// Open returns the cache's open issue / PR totals for repo. ok is false when
// no open source is attached or the repo is unknown to it.
func (c *Counts) Open(repo string) (OpenCounts, bool) {
	if c == nil || c.opts.Open == nil {
		return OpenCounts{}, false
	}
	snap, ok := c.opts.Open.Snapshot(repo)
	if !ok {
		return OpenCounts{}, false
	}
	return OpenCounts{
		Issues:     snap.IssueCount(),
		PRs:        snap.PRCount(),
		Ready:      snap.Ready,
		Stale:      snap.Stale,
		StaleSince: snap.StaleSince,
		LastGoodAt: snap.LastGoodAt,
	}, true
}

// OpenItems returns the cache's open items for repo — the same snapshot
// Open counts — so a reader that must list what it counts (the dashboard's
// canonical open-items set, hivecommons/hive#13373) reads items and totals
// from one place. ok is false when no open source is attached or the repo
// is unknown to it.
func (c *Counts) OpenItems(repo string) (Snapshot, bool) {
	if c == nil || c.opts.Open == nil {
		return Snapshot{}, false
	}
	return c.opts.Open.Snapshot(repo)
}

// Outcomes aggregates the outcome counters over repos (every configured repo
// when none is given). The aggregate is Ready only when every repo is,
// Partial when some are, Stale when any is, and StaleSince is the earliest
// stale mark.
func (c *Counts) Outcomes(repos ...string) OutcomeCounts {
	if c == nil {
		return OutcomeCounts{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := c.order
	if len(repos) > 0 {
		keys = make([]string, 0, len(repos))
		for _, r := range repos {
			keys = append(keys, canonicalRepo(r))
		}
	}
	out := OutcomeCounts{Ready: len(keys) > 0, ReposTotal: len(keys)}
	for _, key := range keys {
		st, ok := c.repos[key]
		if !ok {
			out.Ready = false
			continue
		}
		out.MergedPRs += st.merged
		out.ClosedIssues += st.closed
		if st.ready {
			out.ReposReady++
		} else {
			out.Ready = false
		}
		if st.stale {
			out.Stale = true
			if out.StaleSince.IsZero() || st.staleSince.Before(out.StaleSince) {
				out.StaleSince = st.staleSince
				out.StaleReason = st.staleReason
			}
		}
		if st.lastGoodAt.After(out.LastGoodAt) {
			out.LastGoodAt = st.lastGoodAt
		}
		if st.source == SourceWebhook || out.Source == "" {
			out.Source = st.source
		}
	}
	out.Partial = !out.Ready && out.ReposReady > 0
	return out
}

// Seed installs previously persisted totals (for example the counts a
// restart loaded from disk) as the baseline so webhook increments have
// something to add to before the first reconcile. It does nothing for a
// repo that already has a reconciled baseline.
func (c *Counts) Seed(repo string, mergedPRs, closedIssues int, asOf time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.addRepoLocked(repo)
	if st == nil || st.ready {
		return
	}
	st.merged, st.closed = mergedPRs, closedIssues
	st.ready = true
	st.lastGoodAt = asOf
	st.source = SourceReconcile
}

// NoteClosed increments the counters for one terminal event: a PR that was
// merged, or an issue that was closed. A PR closed without merging and a
// reopen are not outcomes and are ignored. author is the item's opener;
// when Counts has an Author it must match (case-insensitively, with or
// without the "[bot]" suffix). Unknown repos are added so a webhook for a
// repo configured after construction still counts.
func (c *Counts) NoteClosed(repo string, kind Kind, merged bool, author string) bool {
	if c == nil {
		return false
	}
	if !authorMatches(c.opts.Author, author) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.addRepoLocked(repo)
	if st == nil {
		return false
	}
	switch {
	case kind == KindPR && merged:
		st.merged++
	case kind == KindIssue:
		st.closed++
	default:
		return false
	}
	st.source = SourceWebhook
	return true
}

// authorMatches reports whether login is the configured hive author. GitHub
// renders App actors as "<slug>[bot]" in some payloads and "<slug>" in
// others, so the suffix is ignored on both sides.
func authorMatches(want, login string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return true
	}
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.TrimSuffix(s, "[bot]")
		return strings.TrimPrefix(s, "app/")
	}
	return norm(want) == norm(login)
}

// ReconcileDue reports whether a reconcile pass should run now: immediately
// at first, after Pace while the current round still has repos pending, and
// after Interval once a round is complete.
func (c *Counts) ReconcileDue() bool {
	if c == nil || c.opts.Reconcile == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.lastReconcile.IsZero() {
		return true
	}
	now := c.opts.Now()
	if len(c.pendingLocked()) > 0 {
		return !now.Before(c.lastReconcile.Add(c.opts.Pace))
	}
	return !now.Before(c.roundStart.Add(c.opts.Interval))
}

// pendingLocked lists the repos the current round has not asked about yet,
// repos without a baseline first so a fresh hive becomes useful soonest.
func (c *Counts) pendingLocked() []string {
	var unready, ready []string
	for _, key := range c.order {
		st := c.repos[key]
		if st == nil || st.round == c.round {
			continue
		}
		if st.ready {
			ready = append(ready, key)
		} else {
			unready = append(unready, key)
		}
	}
	return append(unready, ready...)
}

// ReconcilePending reports how many repos the current round has yet to ask
// GitHub about.
func (c *Counts) ReconcilePending() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.pendingLocked())
}

// Reconcile runs one pass of the authoritative check: up to Budget pending
// repos are asked for their totals and overwritten with the answer,
// including any webhook increments that landed since the last check (they
// are part of the API's total). When the previous round is complete a new
// one starts. On a failure the repo keeps its last-good counts plus
// increments and is marked Stale with StaleSince; a later success clears
// the mark. A rate-limit refusal ends the pass early — the repos not yet
// asked stay pending for the next pass rather than each burning a call on
// the same refusal. The returned error joins the per-repo failures.
func (c *Counts) Reconcile(ctx context.Context) error {
	if c == nil || c.opts.Reconcile == nil {
		return nil
	}
	c.mu.Lock()
	now := c.opts.Now()
	c.lastReconcile = now
	if len(c.pendingLocked()) == 0 {
		c.round++
		c.roundStart = now
	}
	repos := c.pendingLocked()
	if len(repos) > c.opts.Budget {
		repos = repos[:c.opts.Budget]
	}
	c.mu.Unlock()

	var errs []error
	for _, repo := range repos {
		merged, closed, err := c.opts.Reconcile(ctx, repo)
		now := c.opts.Now()
		c.mu.Lock()
		st := c.repos[repo]
		if st == nil {
			c.mu.Unlock()
			continue
		}
		st.round = c.round
		if err != nil {
			if !st.stale {
				st.stale = true
				st.staleSince = now
			}
			st.staleReason = err.Error()
			c.mu.Unlock()
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
			if c.opts.IsRateLimit(err) {
				break
			}
			continue
		}
		st.merged, st.closed = merged, closed
		st.ready = true
		st.stale = false
		st.staleSince = time.Time{}
		st.staleReason = ""
		st.lastGoodAt = now
		st.source = SourceReconcile
		c.mu.Unlock()
	}
	return errors.Join(errs...)
}

// isRateLimitError is the default CountsOptions.IsRateLimit: GitHub's
// primary and secondary rate-limit refusals both say "rate limit".
func isRateLimitError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "rate")
}

// outcomePayload is the slice of an issues / pull_request delivery the
// counters need.
type outcomePayload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue *struct {
		Number      int `json:"number"`
		PullRequest *struct {
		} `json:"pull_request"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"issue"`
	PullRequest *struct {
		Number int  `json:"number"`
		Merged bool `json:"merged"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"pull_request"`
}

// NoteWebhook applies one raw GitHub delivery to the counters. Only
// `pull_request closed` with merged=true and `issues closed` count; every
// other event, action or kind is ignored. It reports whether a counter moved.
// Other than the body being undecodable it never errors: the counters are a
// hint reconciled hourly, not a ledger.
func (c *Counts) NoteWebhook(event string, payload []byte) (bool, error) {
	if c == nil {
		return false, nil
	}
	switch event {
	case "issues", "pull_request":
	default:
		return false, nil
	}
	var p outcomePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return false, fmt.Errorf("forgestate: decode %s webhook: %w", event, err)
	}
	if !strings.EqualFold(p.Action, "closed") || p.Repository.FullName == "" {
		return false, nil
	}
	switch {
	case event == "pull_request" && p.PullRequest != nil:
		return c.NoteClosed(p.Repository.FullName, KindPR, p.PullRequest.Merged, p.PullRequest.User.Login), nil
	case event == "issues" && p.Issue != nil && p.Issue.PullRequest == nil:
		return c.NoteClosed(p.Repository.FullName, KindIssue, false, p.Issue.User.Login), nil
	}
	return false, nil
}
