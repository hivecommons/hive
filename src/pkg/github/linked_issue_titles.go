package github

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/forgestate"
)

// The activity monitor's PR rows name the issue each PR closes. When that
// issue is still open it has its own row and the dashboard copies the title
// across; when it is already closed — fixed by an earlier PR while this
// superseded PR's body still says `Fixes #N` — no open-items listing knows
// it, and only the number survives from the closing reference. The GraphQL
// passes carry title and state, but a number-only reference still reaches
// the tracker from an older persisted snapshot or a path that could not ask
// for them, and the row then shows a bare "#N". EnrichLinkedIssueTitles
// fills those in: from the forgestate cache when it still holds the issue,
// else one targeted read remembered for linkedIssueTitleTTL — never once
// per cycle, and at most linkedIssueTitleBudget reads per call.

const (
	// linkedIssueTitleTTL is how long a resolved (or 404) summary is kept;
	// a closed issue's title does not change in any way the slot follows.
	linkedIssueTitleTTL = 24 * time.Hour
	// linkedIssueTitleRetry is how long a failed read (transport, rate
	// limit) is remembered before the issue is asked about again.
	linkedIssueTitleRetry = time.Hour
	// linkedIssueTitleBudget caps targeted reads per call so a cold start
	// with many superseded PRs cannot stretch the cycle.
	linkedIssueTitleBudget = 8
)

// linkedIssueTitleEntry is one remembered lookup: found is false for a 404
// or a failed read (failed tells the two apart), at is when it was recorded.
type linkedIssueTitleEntry struct {
	summary IssueSummary
	found   bool
	failed  bool
	at      time.Time
}

type linkedIssueTitleCache struct {
	mu      sync.Mutex
	entries map[string]linkedIssueTitleEntry
}

func (c *linkedIssueTitleCache) get(key string, now time.Time) (linkedIssueTitleEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return e, false
	}
	ttl := linkedIssueTitleTTL
	if e.failed {
		ttl = linkedIssueTitleRetry
	}
	if now.Sub(e.at) >= ttl {
		delete(c.entries, key)
		return linkedIssueTitleEntry{}, false
	}
	return e, true
}

func (c *linkedIssueTitleCache) put(key string, e linkedIssueTitleEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]linkedIssueTitleEntry{}
	}
	c.entries[key] = e
}

// EnrichLinkedIssueTitles completes the title, state and URL of every
// closing reference on prs and on the sidelined PRs in details that still
// lacks a title or a state. References that already carry both (the GraphQL
// passes fill them) cost nothing. Either argument may be nil; the slices
// are updated in place. It runs on the eval tick and on manual rescan,
// after EnrichReviewSignalsAndWorkPRs.
func (c *Client) EnrichLinkedIssueTitles(ctx context.Context, details map[string]RepoWorkDetails, prs ...[]PullRequest) {
	c.enrichLinkedIssueTitlesAt(ctx, time.Now(), details, prs...)
}

// enrichLinkedIssueTitlesAt is EnrichLinkedIssueTitles at a given instant,
// which is what the TTL arithmetic reads; tests drive the clock through it.
func (c *Client) enrichLinkedIssueTitlesAt(ctx context.Context, now time.Time, details map[string]RepoWorkDetails, prs ...[]PullRequest) {
	if c == nil {
		return
	}
	budget := linkedIssueTitleBudget
	for _, list := range prs {
		for i := range list {
			c.fillLinkedIssueTitles(ctx, list[i].Repo, list[i].LinkedIssues, &budget, now)
		}
	}
	for repo, d := range details {
		for i := range d.PRs {
			c.fillLinkedIssueTitles(ctx, nonEmptyRepo(d.PRs[i].Repo, repo), d.PRs[i].LinkedIssues, &budget, now)
		}
	}
}

func nonEmptyRepo(repo, fallback string) string {
	if repo != "" {
		return repo
	}
	return fallback
}

func (c *Client) fillLinkedIssueTitles(ctx context.Context, repo string, lis []PRLinkedIssue, budget *int, now time.Time) {
	for j := range lis {
		li := &lis[j]
		if li.Number <= 0 || (li.Title != "" && li.State != "") {
			continue
		}
		issueRepo := nonEmptyRepo(li.Repo, repo)
		if issueRepo == "" {
			continue
		}
		sum, ok := c.linkedIssueSummary(ctx, issueRepo, li.Number, budget, now)
		if !ok {
			continue
		}
		if li.Title == "" {
			li.Title = sum.Title
		}
		if li.State == "" {
			li.State = sum.State
		}
		if li.URL == "" {
			li.URL = sum.URL
		}
	}
}

// linkedIssueSummary answers for repo#number from the forgestate cache, the
// memo, or (budget permitting) one targeted read whose outcome — found, 404
// or failed — is remembered.
func (c *Client) linkedIssueSummary(ctx context.Context, repo string, number int, budget *int, now time.Time) (IssueSummary, bool) {
	if store := c.ForgeState(); store != nil {
		if snap, ok := store.Snapshot(repo); ok {
			if sum, ok := linkedIssueFromSnapshot(snap, number); ok {
				return sum, true
			}
		}
	}
	key := repo + "#" + strconv.Itoa(number)
	e, cached := c.linkedIssueTitles.get(key, now)
	if !cached {
		if *budget <= 0 {
			return IssueSummary{}, false
		}
		*budget--
		sum, found, err := c.IssueSummary(ctx, repo, number)
		e = linkedIssueTitleEntry{summary: sum, found: found && err == nil, failed: err != nil, at: now}
		if err != nil {
			c.logger.Debug("linked issue title lookup failed", "repo", repo, "number", number, "error", err)
		}
		c.linkedIssueTitles.put(key, e)
	}
	return e.summary, e.found
}

// linkedIssueFromSnapshot reads repo#number from a forgestate snapshot. The
// cache drops closed items on their webhook, so this mostly answers for an
// issue that is open but outside the tracker's set (reopened moments ago,
// or held); it costs no request and needs no TTL.
func linkedIssueFromSnapshot(snap forgestate.Snapshot, number int) (IssueSummary, bool) {
	it, ok := snap.Issues[number]
	if !ok || it.Title == "" {
		return IssueSummary{}, false
	}
	state := "open"
	if it.Closed {
		state = "closed"
	}
	return IssueSummary{Number: number, Title: it.Title, State: state, URL: it.HTMLURL}, true
}
