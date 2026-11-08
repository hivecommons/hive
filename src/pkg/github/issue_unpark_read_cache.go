package github

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// The un-park sweep's per-item read cache.
//
// Every pass (every five minutes, sooner after a webhook) the sweep read
// the comment list of every parked issue, of every PR waiting at 085 or
// 070, and of every PR updated in the last week, plus the label timeline of
// every issue carrying an approval label on a label-approval repo. Almost
// none of those items change between two passes, but the reads were charged
// anyway whenever the ETag cache could not answer them: after every
// installation-token refresh (the ETag cache is keyed by identity) and
// whenever the hour-rounded since moved. On the knuckle hive (2026-11-08,
// 00:55–01:45Z) that was 1222 comment reads (878 charged) and 480 timeline
// reads (363 charged) in eight passes, the largest single consumer.
//
// Each item the sweep starts from already carries its updated_at in the
// issue list response, and GitHub moves it on every change these reads can
// see: a new, edited or deleted comment, and a label added or removed. So a
// result read while the item had the same updated_at is still that item's
// data, and the sweep reuses it instead of asking again.
//
// Only raw data is cached, never a decision. Every check that depends on
// the clock or on live state (the seven-day command window, the
// commenter's repo permission, an approval label applier's trust) runs on
// every pass against the cached comments and events, exactly as it would
// on a fresh read.
//
// A cached result is reused only when all of these hold; otherwise the item
// is read again:
//   - the listed updated_at is exactly the one the result was read under;
//   - the result was read at least unparkReadSettle after that updated_at,
//     so a list and a comment read that disagree for a moment after a change
//     cannot pin a stale result;
//   - the result is younger than unparkReadTTL, a backstop for any change
//     GitHub does not stamp;
//   - the sweep wrote nothing to the item since (forgetUnparkRead).
//
// The cache lives in memory only: after a restart it is empty and the
// first pass reads everything, which is the old behaviour. Entries for items
// that leave a repo's open list are dropped after that repo's pass, and the
// whole cache is bounded by unparkReadMaxItems.

const (
	// unparkReadTTL bounds how long one read is reused for an item whose
	// updated_at has not moved.
	unparkReadTTL = 6 * time.Hour
	// unparkReadSettle is how long after an item's updated_at a read must
	// have happened before the sweep trusts it to contain that update.
	unparkReadSettle = 2 * time.Minute
	// unparkReadMaxItems and unparkReadMaxComments bound the cache; past
	// either the oldest read is dropped. The knuckle hive's sweep reads about
	// 220 items holding about 1,200 comments (a few MB).
	unparkReadMaxItems    = 4096
	unparkReadMaxComments = 50000
)

// unparkReadEntry is what the sweep last read for one item.
type unparkReadEntry struct {
	// updatedAt is the item's updated_at the reads below were taken under.
	updatedAt time.Time

	// comments is the item's comment list; commentsAt is when it was read
	// (zero: not read). commentsSince is the since that read sent, already
	// rounded by cacheableSince; zero means the full list.
	comments      []*gh.IssueComment
	commentsAt    time.Time
	commentsSince time.Time

	// labelEvents is the item's label timeline; labelEventsAt is when it
	// was read (zero: not read).
	labelEvents   []issueLabelEvent
	labelEventsAt time.Time
}

// unparkReadCache maps "owner/repo#n" to its entry. The zero value is ready
// to use.
type unparkReadCache struct {
	mu      sync.Mutex
	entries map[string]*unparkReadEntry
}

func unparkReadKey(owner, repo string, number int) string {
	return strings.ToLower(owner+"/"+repo) + "#" + strconv.Itoa(number)
}

// usable reports whether a read taken at readAt under the entry's
// updated_at may stand in for a fresh one for an item listed at updatedAt.
func (e *unparkReadEntry) usable(readAt, updatedAt, now time.Time) bool {
	if e == nil || readAt.IsZero() || updatedAt.IsZero() || !e.updatedAt.Equal(updatedAt) {
		return false
	}
	if readAt.Sub(updatedAt) < unparkReadSettle {
		return false
	}
	return now.Sub(readAt) < unparkReadTTL
}

// entryFor returns the item's entry for updatedAt, resetting it when the
// item moved. The caller holds mu, fills the entry, then calls evictLocked.
func (rc *unparkReadCache) entryFor(key string, updatedAt time.Time) *unparkReadEntry {
	if rc.entries == nil {
		rc.entries = map[string]*unparkReadEntry{}
	}
	e := rc.entries[key]
	if e == nil || !e.updatedAt.Equal(updatedAt) {
		e = &unparkReadEntry{updatedAt: updatedAt}
		rc.entries[key] = e
	}
	return e
}

// evictLocked drops the entries read longest ago until the cache is within
// unparkReadMaxItems and unparkReadMaxComments. The caller holds mu.
func (rc *unparkReadCache) evictLocked() {
	comments := 0
	for _, e := range rc.entries {
		comments += len(e.comments)
	}
	for len(rc.entries) > unparkReadMaxItems || comments > unparkReadMaxComments {
		oldestKey, oldest := "", time.Time{}
		for k, e := range rc.entries {
			at := e.commentsAt
			if e.labelEventsAt.After(at) {
				at = e.labelEventsAt
			}
			if oldestKey == "" || at.Before(oldest) {
				oldestKey, oldest = k, at
			}
		}
		comments -= len(rc.entries[oldestKey].comments)
		delete(rc.entries, oldestKey)
	}
}

func (rc *unparkReadCache) comments(key string, updatedAt, since, now time.Time) ([]*gh.IssueComment, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	e := rc.entries[key]
	if e == nil || !e.usable(e.commentsAt, updatedAt, now) {
		return nil, false
	}
	// A full list answers any since; a since list answers a later one.
	// Callers filter on their own cutoff (listIssueCommentsSince).
	if !e.commentsSince.IsZero() && (since.IsZero() || since.Before(e.commentsSince)) {
		return nil, false
	}
	return append([]*gh.IssueComment(nil), e.comments...), true
}

func (rc *unparkReadCache) storeComments(key string, updatedAt, since, readAt time.Time, comments []*gh.IssueComment) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	e := rc.entryFor(key, updatedAt)
	// Keep a full list over a narrower since list read later in the pass.
	if !e.commentsAt.IsZero() && e.commentsSince.IsZero() && !since.IsZero() {
		return
	}
	e.comments = append([]*gh.IssueComment(nil), comments...)
	e.commentsAt = readAt
	e.commentsSince = since
	rc.evictLocked()
}

func (rc *unparkReadCache) labelEvents(key string, updatedAt, now time.Time) ([]issueLabelEvent, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	e := rc.entries[key]
	if e == nil || !e.usable(e.labelEventsAt, updatedAt, now) {
		return nil, false
	}
	return append([]issueLabelEvent(nil), e.labelEvents...), true
}

func (rc *unparkReadCache) storeLabelEvents(key string, updatedAt, readAt time.Time, events []issueLabelEvent) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	e := rc.entryFor(key, updatedAt)
	e.labelEvents = append([]issueLabelEvent(nil), events...)
	e.labelEventsAt = readAt
	rc.evictLocked()
}

func (rc *unparkReadCache) forget(key string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	delete(rc.entries, key)
}

// prune drops every entry of owner/repo whose number is not in listed: the
// item closed or left the list.
func (rc *unparkReadCache) prune(owner, repo string, listed map[int]bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	prefix := strings.ToLower(owner+"/"+repo) + "#"
	for k := range rc.entries {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(k, prefix))
		if err != nil || !listed[n] {
			delete(rc.entries, k)
		}
	}
}

func (rc *unparkReadCache) len() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.entries)
}

// unparkReadScope carries the cache and the pass's clock through one sweep.
// Reads made outside a sweep (no scope on ctx) always go to GitHub.
type unparkReadScope struct {
	cache *unparkReadCache
	now   time.Time
}

type unparkReadScopeKey struct{}

func withUnparkReadScope(ctx context.Context, cache *unparkReadCache, now time.Time) context.Context {
	return context.WithValue(ctx, unparkReadScopeKey{}, unparkReadScope{cache: cache, now: now})
}

func unparkReadScopeFrom(ctx context.Context) (unparkReadScope, bool) {
	s, ok := ctx.Value(unparkReadScopeKey{}).(unparkReadScope)
	return s, ok && s.cache != nil
}

// unparkComments is listIssueCommentsSince for an item the sweep listed:
// it reuses the last read while the item's updated_at has not moved.
func (c *Client) unparkComments(ctx context.Context, owner, repo string, issue *gh.Issue, since time.Time) ([]*gh.IssueComment, error) {
	number := issue.GetNumber()
	scope, ok := unparkReadScopeFrom(ctx)
	updatedAt := issue.GetUpdatedAt().Time
	if !ok || updatedAt.IsZero() {
		return c.listIssueCommentsSince(ctx, owner, repo, number, since)
	}
	key := unparkReadKey(owner, repo, number)
	rounded := cacheableSince(since)
	if comments, hit := scope.cache.comments(key, updatedAt, rounded, scope.now); hit {
		return comments, nil
	}
	comments, err := c.listIssueCommentsSince(ctx, owner, repo, number, since)
	if err != nil {
		return nil, err
	}
	scope.cache.storeComments(key, updatedAt, rounded, scope.now, comments)
	return comments, nil
}

// unparkLabelEvents is listIssueLabelEvents for an item the sweep listed,
// reused the same way as unparkComments.
func (c *Client) unparkLabelEvents(ctx context.Context, owner, repo string, issue *gh.Issue) ([]issueLabelEvent, error) {
	number := issue.GetNumber()
	scope, ok := unparkReadScopeFrom(ctx)
	updatedAt := issue.GetUpdatedAt().Time
	if !ok || updatedAt.IsZero() {
		return c.listIssueLabelEvents(ctx, owner, repo, number)
	}
	key := unparkReadKey(owner, repo, number)
	if events, hit := scope.cache.labelEvents(key, updatedAt, scope.now); hit {
		return events, nil
	}
	events, err := c.listIssueLabelEvents(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	scope.cache.storeLabelEvents(key, updatedAt, scope.now, events)
	return events, nil
}

// forgetUnparkRead drops the item's cached reads after the sweep wrote to
// it, or failed part-way, so the next pass reads it fresh even if the list
// it starts from has not caught up with the write yet.
func (c *Client) forgetUnparkRead(ctx context.Context, owner, repo string, number int) {
	if scope, ok := unparkReadScopeFrom(ctx); ok {
		scope.cache.forget(unparkReadKey(owner, repo, number))
	}
}
