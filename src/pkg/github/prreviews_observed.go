package github

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// prTerminalReviewCacheTTL bounds how long a terminal PR's reviews stay
// cached. The settle scan only revisits a PR while it is inside
// mergedClaimScanWindow, so twice that is enough for every rescan of the
// window to hit the cache, and nothing older is ever asked for again.
const prTerminalReviewCacheTTL = 2 * mergedClaimScanWindow

// PRReviewObservation reports a review someone other than the hive submitted
// on a PR the settle scan saw merge or close (hivecommons/hive#16116). Like
// PRTerminalObservation it is an observation, not a forge write: the
// dashboard audit sink records it so the throughput card's Reviewed row
// counts people's and other bots' reviews, not only the hive's.
type PRReviewObservation struct {
	Repo     string
	Number   int
	ReviewID int64
	Reviewer string
	// Attribution is prTerminalMergeAttribution's class for Reviewer:
	// "human", "other_automation", or "unknown" for a deleted (ghost)
	// account with no login. The hive's own reviews are never
	// observed — agent_pr_reviewed already counts them.
	Attribution string
	// State is GitHub's review state (APPROVED, CHANGES_REQUESTED,
	// COMMENTED, DISMISSED). Every submitted review counts, whatever it is.
	State       string
	SubmittedAt time.Time
}

type PRReviewObservedHook func(PRReviewObservation)

// SetPRReviewObservedHook installs the sink for observed reviews. Without
// one the settle scan does not read reviews at all.
func (c *Client) SetPRReviewObservedHook(fn PRReviewObservedHook) {
	if c == nil {
		return
	}
	c.prReviewObservedHook.Store(&fn)
}

func (c *Client) prReviewObserved() PRReviewObservedHook {
	if c == nil {
		return nil
	}
	hook := c.prReviewObservedHook.Load()
	if hook == nil {
		return nil
	}
	return *hook
}

// emitTerminalPRReviews reports the reviews of one PR the settle scan just
// saw merged or closed. The reviews are read from GitHub once per PR and
// served from cache on every later scan of the window while the PR's
// updated_at is unchanged; when it moved, the list is read again and only
// the reviews not seen before are emitted. GitHub bumps a PR's updated_at
// when a review is submitted (verified against the API: a PR's updated_at
// equals its latest review's submitted_at when the review is the last
// event), so a review that lands after the first read is caught by the
// next scan. The dashboard sink de-duplicates
// repeats by review ID. A failed read emits nothing, is
// not cached, and does not fail the scan: the next scan retries it.
func (c *Client) emitTerminalPRReviews(ctx context.Context, owner, repoName, repo string, number int, updatedAt time.Time, identity HiveIdentity) {
	hook := c.prReviewObserved()
	if hook == nil || number <= 0 {
		return
	}
	key := strings.ToLower(owner+"/"+repoName) + "#" + strconv.Itoa(number)
	now := time.Now()
	reviews, ok := c.prTerminalReviews.get(key, updatedAt, now)
	if ok {
		for _, r := range reviews {
			hook(r)
		}
		return
	}
	fresh, err := c.listTerminalPRReviews(ctx, owner, repoName, repo, number, identity)
	if err != nil {
		if c.logger != nil {
			c.logger.Debug("terminal PR reviews lookup failed", "repo", owner+"/"+repoName, "number", number, "error", err)
		}
		return
	}
	seen := c.prTerminalReviews.seen(key, now)
	c.prTerminalReviews.put(key, updatedAt, fresh, now)
	for _, r := range fresh {
		if !seen[r.ReviewID] {
			hook(r)
		}
	}
}

// listTerminalPRReviews pages through a PR's reviews and keeps the submitted
// ones the hive did not write.
func (c *Client) listTerminalPRReviews(ctx context.Context, owner, repoName, repo string, number int, identity HiveIdentity) ([]PRReviewObservation, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	ctx = WithRESTCaller(ctx, "hive:pr_terminal_reviews")
	opts := &gh.ListOptions{PerPage: claimSearchPerPage}
	var out []PRReviewObservation
	for page := 0; page < claimSearchMaxPages; page++ {
		reviews, resp, err := c.client.PullRequests.ListReviews(ctx, owner, repoName, number, opts)
		if err != nil {
			return nil, err
		}
		for _, r := range reviews {
			if obs, ok := c.observedReview(r, repo, number, identity); ok {
				out = append(out, obs)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// observedReview classifies one review. A pending review (no submitted_at)
// is not a review yet; the hive's own review is not counted here, and one with
// no reviewer login (a deleted account) is counted as "unknown".
func (c *Client) observedReview(r *gh.PullRequestReview, repo string, number int, identity HiveIdentity) (PRReviewObservation, bool) {
	if r == nil || r.GetID() == 0 || r.GetSubmittedAt().Time.IsZero() {
		return PRReviewObservation{}, false
	}
	login := safeGetLogin(r.GetUser())
	class := prTerminalMergeAttribution(login, identity, c.appBotLogin)
	if class == PRMergeAttributionHive {
		return PRReviewObservation{}, false
	}
	return PRReviewObservation{
		Repo:        repo,
		Number:      number,
		ReviewID:    r.GetID(),
		Reviewer:    login,
		Attribution: class,
		State:       r.GetState(),
		SubmittedAt: r.GetSubmittedAt().Time,
	}, true
}

// prTerminalReviewCache holds each terminal PR's observed reviews, keyed
// lower(owner/repo)#number. The zero value is ready to use.
type prTerminalReviewCache struct {
	mu      sync.Mutex
	entries map[string]prTerminalReviewEntry
}

type prTerminalReviewEntry struct {
	reviews   []PRReviewObservation
	updatedAt time.Time
	storedAt  time.Time
}

// get serves the cached reviews only while the PR is unchanged since they
// were read; a moved updated_at means a review may have landed since.
func (p *prTerminalReviewCache) get(key string, updatedAt, now time.Time) ([]PRReviewObservation, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[key]
	if !ok || now.Sub(e.storedAt) > prTerminalReviewCacheTTL || !e.updatedAt.Equal(updatedAt) {
		return nil, false
	}
	return e.reviews, true
}

// seen returns the IDs of the reviews already cached for key, none once the entry is past the TTL.
func (p *prTerminalReviewCache) seen(key string, now time.Time) map[int64]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[int64]bool{}
	e, ok := p.entries[key]
	if !ok || now.Sub(e.storedAt) > prTerminalReviewCacheTTL {
		return out
	}
	for _, r := range e.reviews {
		out[r.ReviewID] = true
	}
	return out
}

func (p *prTerminalReviewCache) put(key string, updatedAt time.Time, reviews []PRReviewObservation, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries == nil {
		p.entries = map[string]prTerminalReviewEntry{}
	}
	for k, e := range p.entries {
		if now.Sub(e.storedAt) > prTerminalReviewCacheTTL {
			delete(p.entries, k)
		}
	}
	p.entries[key] = prTerminalReviewEntry{reviews: reviews, updatedAt: updatedAt, storedAt: now}
}
