package github

import (
	"sync"
	"time"
)

// Shared retry policy for the request-file watchers (PR-open and PR-review).
//
// A request whose API call fails must not be retried every 10s tick for the
// life of the pod: ~10 permanently failing PR-open requests at 2 API calls
// each per tick is ~7,000 content-generating requests/hour of pure junk —
// enough on its own to trip GitHub's secondary rate limit and take the whole
// App installation (every spoke in the org) dark for an hour at a time
// (observed live on kubestellar/console, 2026-08-25: four consecutive hourly
// trips). Failures back off exponentially per request file, and a request
// that still has not succeeded after the give-up horizon is quarantined
// (.failed) so the queue stays finite. The issue-request watcher carries an
// earlier per-watcher copy of this same policy; the merge watcher caps
// attempts via .exhausted.
const (
	requestRetryBase   = 30 * time.Second
	requestRetryMax    = 15 * time.Minute
	requestRetryMaxAge = 24 * time.Hour
)

// retryState is one request file's failure history.
type retryState struct {
	attempts int
	nextTry  time.Time
	firstTry time.Time
}

// retryTracker holds per-request-file backoff state. The zero value is ready
// to use. State is in-memory only: a pod restart retries everything once,
// then backs off again — acceptable, same as the issue watcher.
type retryTracker struct {
	mu sync.Mutex
	m  map[string]*retryState
}

// allows reports whether the request at path is eligible for an attempt now
// (its backoff window has elapsed, or it has never failed).
func (t *retryTracker) allows(path string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.m[path]
	if st == nil {
		return true
	}
	return !now.Before(st.nextTry)
}

// noteFailure records a failed attempt, schedules the next eligible try with
// exponential backoff, and returns true when the request has exceeded the
// give-up horizon and should be quarantined.
func (t *retryTracker) noteFailure(path string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*retryState{}
	}
	st := t.m[path]
	if st == nil {
		st = &retryState{firstTry: now}
		t.m[path] = st
	}
	st.attempts++
	backoff := requestRetryBase << uint(min(st.attempts-1, 30))
	if backoff > requestRetryMax || backoff <= 0 {
		backoff = requestRetryMax
	}
	st.nextTry = now.Add(backoff)
	return now.Sub(st.firstTry) > requestRetryMaxAge
}

// clear drops the tracked state for path (request consumed or quarantined).
func (t *retryTracker) clear(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, path)
}
