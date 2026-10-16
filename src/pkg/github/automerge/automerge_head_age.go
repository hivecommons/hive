package automerge

import (
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// headFirstSeenPruneAfter drops heads the sweep has not evaluated for this
// long so the tracker stays bounded by recently active PRs.
const headFirstSeenPruneAfter = 7 * 24 * time.Hour

// HeadFirstSeenTracker remembers, per (repo, pr, head SHA), the time the
// min_head_age gate should measure head age from (hivecommons/hive#12491).
//
// PR updated_at bumps on every mutation — labels, comments, reviews — so the
// hive's own stage-label writes used to reset the observed head age and flip
// a settled PR back to "pending: head pushed …". The tracker pins the first
// observed value per head SHA: later updated_at bumps on the same head are
// ignored, while a new head SHA is a new key and starts its own age.
// updated_at is only consulted on a head's first observation, where it is a
// conservative (never earlier than the push) stand-in for the push time.
// One tracker is shared by every engine in the process because the governor
// sweep builds a fresh engine per tick.
type HeadFirstSeenTracker struct {
	mu        sync.Mutex
	firstSeen map[string]time.Time
	lastSeen  map[string]time.Time
}

var sharedHeadFirstSeen = NewHeadFirstSeenTracker()

// NewHeadFirstSeenTracker returns an empty tracker.
func NewHeadFirstSeenTracker() *HeadFirstSeenTracker {
	return &HeadFirstSeenTracker{firstSeen: make(map[string]time.Time), lastSeen: make(map[string]time.Time)}
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
	t.lastSeen[key] = now
	for k, seen := range t.lastSeen {
		if now.Sub(seen) > headFirstSeenPruneAfter {
			delete(t.lastSeen, k)
			delete(t.firstSeen, k)
		}
	}
	return first
}

// headPushedAt is the HeadPushedAt fed to the min_head_age gate for pr at
// sha: the time first recorded for this head, not the PR's live updated_at.
func (c *Engine) headPushedAt(owner, repo string, number int, sha string, pr *gh.PullRequest) time.Time {
	tracker := sharedHeadFirstSeen
	if c != nil && c.headFirstSeen != nil {
		tracker = c.headFirstSeen
	}
	return c.headPushedAtWith(tracker, owner, repo, number, sha, pr)
}

func (c *Engine) headPushedAtWith(tracker *HeadFirstSeenTracker, owner, repo string, number int, sha string, pr *gh.PullRequest) time.Time {
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
	return tracker.observe(contributorSoakKey(owner, repo, number, sha), updated, now())
}
