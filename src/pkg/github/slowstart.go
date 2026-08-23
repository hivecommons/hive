package github

import (
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Slow-start pacing after a GitHub SECONDARY rate limit.
//
// GitHub's secondary limit throttles BURST/concurrency, not hourly volume.
// When the hive trips it, go-github records the reset time and synthesizes
// 403s client-side until then ("not making remote request") — but the moment
// the window expires, every queued caller (pr-request watcher retries, the
// enumeration pass, the automerge sweep, stats) fires SIMULTANEOUSLY, GitHub
// sees the burst, and the limit re-trips for another hour. Observed live on
// kubestellar/console (2026-08-23): three consecutive hourly re-trips —
// 07:49 → 08:49 → 09:49 — with the spoke's entire GitHub output dark the
// whole time. The client backed off correctly; it just un-backed-off as a
// stampede.
//
// slowStartTransport breaks the loop at the one place all callers converge:
// the shared HTTP transport. When a REAL secondary-limit 403 passes through
// (identified by the Retry-After header GitHub attaches to abuse/secondary
// blocks — permission 403s do not carry it), it enters a caution window
// extending past the limit's reset. While cautious, requests are globally
// serialized with a ~2s jittered gap — the post-reset wave becomes a trickle
// the secondary limiter tolerates, and normal concurrency resumes when the
// window ends.
const (
	// slowStartWindow is how long past the limit's reset the pacing lasts.
	// Long enough for the queued backlog to drain gently; short enough that
	// full throughput returns within one governor cycle.
	slowStartWindow = 10 * time.Minute
	// slowStartGap is the minimum spacing between requests while cautious.
	slowStartGap = 2 * time.Second
	// slowStartJitter is added (0..jitter) per request so even multiple
	// spokes behind one App key do not phase-lock.
	slowStartJitter = time.Second
	// slowStartDefaultRetryAfter is assumed when a secondary 403 carries an
	// unparseable Retry-After: cover a full secondary window.
	slowStartDefaultRetryAfter = time.Hour
)

type slowStartTransport struct {
	inner http.RoundTripper
	// gap/jitter/window default from the package consts; fields so tests can
	// use millisecond values without minute-long sleeps.
	gap    time.Duration
	jitter time.Duration
	window time.Duration

	mu            sync.Mutex
	cautiousUntil time.Time
	nextSlot      time.Time
}

func newSlowStartTransport(inner http.RoundTripper) *slowStartTransport {
	return &slowStartTransport{
		inner:  inner,
		gap:    slowStartGap,
		jitter: slowStartJitter,
		window: slowStartWindow,
	}
}

// sharedSlowStart wraps the process-wide proxy-trusting transport exactly once
// so pacing is GLOBAL: the herd is a cross-caller phenomenon, and per-client
// pacing would still stampede in aggregate.
var (
	sharedSlowStartOnce sync.Once
	sharedSlowStartRT   *slowStartTransport
)

func slowStartWrap(inner http.RoundTripper) http.RoundTripper {
	sharedSlowStartOnce.Do(func() {
		sharedSlowStartRT = newSlowStartTransport(inner)
	})
	return sharedSlowStartRT
}

func (t *slowStartTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Pacing: while cautious, hand each request the next free slot and sleep
	// until it. Slots are claimed under the lock; the sleep happens outside it
	// so pacing serializes REQUEST STARTS, not the lock.
	t.mu.Lock()
	var wait time.Duration
	now := time.Now()
	if now.Before(t.cautiousUntil) {
		gap := t.gap + time.Duration(rand.Int63n(int64(t.jitter)+1))
		slot := t.nextSlot
		if slot.Before(now) {
			slot = now
		}
		t.nextSlot = slot.Add(gap)
		wait = slot.Sub(now)
	}
	t.mu.Unlock()

	if wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
	}

	resp, err := t.inner.RoundTrip(req)

	// A real secondary-limit 403 carries Retry-After. Enter (or extend) the
	// caution window to its reset plus the slow-start tail.
	if err == nil && resp != nil && resp.StatusCode == http.StatusForbidden {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			after := slowStartDefaultRetryAfter
			if secs, perr := strconv.Atoi(ra); perr == nil && secs >= 0 {
				after = time.Duration(secs) * time.Second
			}
			until := time.Now().Add(after + t.window)
			t.mu.Lock()
			if until.After(t.cautiousUntil) {
				t.cautiousUntil = until
			}
			t.mu.Unlock()
		}
	}
	return resp, err
}
