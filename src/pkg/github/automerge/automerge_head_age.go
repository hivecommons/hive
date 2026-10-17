package automerge

import (
	"context"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// headFirstSeenPruneAfter drops heads the sweep has not evaluated for this
// long so the tracker stays bounded by recently active PRs.
const headFirstSeenPruneAfter = 7 * 24 * time.Hour

// HeadFirstSeenTracker remembers, per (repo, pr, head SHA), the time the
// min_head_age gate should measure head age from (hivecommons/hive#12491,
// #12557).
//
// PR updated_at bumps on every mutation — labels, comments, reviews — so the
// hive's own stage-label writes and stage-regressed comments used to reset
// the observed head age and flip a settled PR back to "pending: head pushed
// …" every sweep. The tracker pins the first observed updated_at per head
// SHA (later bumps on the same head are ignored; a new head SHA is a new key
// and starts its own age) and caches the head commit's committer date, which
// is immutable per SHA. One tracker is shared by every engine in the process
// because the governor sweep builds a fresh engine per tick.
type HeadFirstSeenTracker struct {
	mu        sync.Mutex
	firstSeen map[string]time.Time
	commitAt  map[string]time.Time
	lastSeen  map[string]time.Time
}

var sharedHeadFirstSeen = NewHeadFirstSeenTracker()

// NewHeadFirstSeenTracker returns an empty tracker.
func NewHeadFirstSeenTracker() *HeadFirstSeenTracker {
	return &HeadFirstSeenTracker{firstSeen: make(map[string]time.Time), commitAt: make(map[string]time.Time), lastSeen: make(map[string]time.Time)}
}

// observe returns the pinned head time for key, recording candidate when the
// key is new. A zero candidate is not recorded, preserving the gate's
// "unknown push time" skip until a real timestamp is seen.
func (t *HeadFirstSeenTracker) observe(key string, candidate, now time.Time) time.Time {
	if t == nil {
		return candidate
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	first, ok := t.firstSeen[key]
	if !ok {
		if candidate.IsZero() {
			return candidate
		}
		first = candidate
		t.firstSeen[key] = first
	}
	t.touchLocked(key, now)
	return first
}

// cachedCommitTime returns the head commit time recorded for key, if any.
func (t *HeadFirstSeenTracker) cachedCommitTime(key string, now time.Time) (time.Time, bool) {
	if t == nil {
		return time.Time{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.commitAt[key]
	if ok {
		t.touchLocked(key, now)
	}
	return at, ok
}

// recordCommitTime caches the head commit time for key.
func (t *HeadFirstSeenTracker) recordCommitTime(key string, at, now time.Time) {
	if t == nil || at.IsZero() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.commitAt[key] = at
	t.touchLocked(key, now)
}

func (t *HeadFirstSeenTracker) touchLocked(key string, now time.Time) {
	t.lastSeen[key] = now
	for k, seen := range t.lastSeen {
		if now.Sub(seen) > headFirstSeenPruneAfter {
			delete(t.lastSeen, k)
			delete(t.firstSeen, k)
			delete(t.commitAt, k)
		}
	}
}

// headPushedAt is the HeadPushedAt fed to the min_head_age gate for pr at
// sha. It is never the PR's live updated_at (#12557): the hive's own label
// flips and stage-regressed comments bump updated_at, and measuring from it
// made the gate reset its own clock every sweep.
func (c *Engine) headPushedAt(ctx context.Context, owner, repo string, number int, sha string, pr *gh.PullRequest) time.Time {
	tracker := sharedHeadFirstSeen
	if c != nil && c.headFirstSeen != nil {
		tracker = c.headFirstSeen
	}
	return c.headPushedAtWith(ctx, tracker, owner, repo, number, sha, pr)
}

// headPushedAtWith starts from the updated_at pinned on this head's first
// observation, which never predates the push and cannot be restarted by later
// bumps on the same head. While that pin would still hold the min_head_age
// gate, the head commit's committer date (hgithub.HeadCommitPushedAt, cached
// per head) is consulted and wins when it is earlier — so a head first seen
// right after a hive comment/label write is still judged by when it was
// committed. If the commit lookup fails the pin is kept: it can only
// over-hold for one min_head_age window and cannot loop.
func (c *Engine) headPushedAtWith(ctx context.Context, tracker *HeadFirstSeenTracker, owner, repo string, number int, sha string, pr *gh.PullRequest) time.Time {
	var updated time.Time
	if updatedAt := pr.GetUpdatedAt(); !updatedAt.IsZero() {
		updated = updatedAt.Time
	}
	if strings.TrimSpace(sha) == "" {
		return updated
	}
	now := time.Now
	if c != nil && c.now != nil {
		now = c.now
	}
	key := contributorSoakKey(owner, repo, number, sha)
	pinned := tracker.observe(key, updated, now())
	if c == nil || c.minHeadAge <= 0 || pinned.IsZero() || now().Sub(pinned) >= c.minHeadAge {
		return pinned
	}
	committed, ok := tracker.cachedCommitTime(key, now())
	if !ok {
		at, err := hgithub.HeadCommitPushedAt(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), c.gh, owner, repo, sha, pr)
		if err != nil {
			return pinned
		}
		tracker.recordCommitTime(key, at, now())
		committed = at
	}
	if committed.Before(pinned) {
		return committed
	}
	return pinned
}
