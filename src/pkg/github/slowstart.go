package github

import (
	"bytes"
	"io"
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

// slowStartState is the shared pacing ledger. It is deliberately SEPARATE
// from the wrapper that carries the inner transport: pacing must be global
// (the herd is a cross-caller phenomenon — per-client pacing would still
// stampede in aggregate), but the inner transport must be whatever is CURRENT
// at wrap time. An earlier design cached the first inner in a sync.Once,
// which pinned the process to a stale transport after the proxy-trust layer
// rebuilt it (new CA) — every later client silently used dead TLS roots.
type slowStartState struct {
	// gap/jitter/window default from the package consts; fields so tests can
	// use millisecond values without minute-long sleeps.
	gap    time.Duration
	jitter time.Duration
	window time.Duration

	mu            sync.Mutex
	cautiousUntil time.Time
	nextSlot      time.Time
}

type slowStartTransport struct {
	inner http.RoundTripper
	state *slowStartState
}

func newSlowStartState() *slowStartState {
	return &slowStartState{
		gap:    slowStartGap,
		jitter: slowStartJitter,
		window: slowStartWindow,
	}
}

func newSlowStartTransport(inner http.RoundTripper) *slowStartTransport {
	return &slowStartTransport{inner: inner, state: newSlowStartState()}
}

// sharedSlowStartState is the process-wide pacing ledger every wrapped client
// shares. The wrapper itself is cheap and constructed per client around the
// CURRENT inner transport.
var sharedSlowStartState = newSlowStartState()

func slowStartWrap(inner http.RoundTripper) http.RoundTripper {
	return &slowStartTransport{inner: inner, state: sharedSlowStartState}
}

func (t *slowStartTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Pacing: while cautious, hand each request the next free slot and sleep
	// until it. Slots are claimed under the lock; the sleep happens outside it
	// so pacing serializes REQUEST STARTS, not the lock.
	st := t.state
	st.mu.Lock()
	var wait time.Duration
	now := time.Now()
	if now.Before(st.cautiousUntil) {
		gap := st.gap + time.Duration(rand.Int63n(int64(st.jitter)+1))
		slot := st.nextSlot
		if slot.Before(now) {
			slot = now
		}
		st.nextSlot = slot.Add(gap)
		wait = slot.Sub(now)
	}
	st.mu.Unlock()

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

	// Detect a secondary-limit 403 and enter (or extend) the caution window to
	// its reset plus the slow-start tail. GitHub does NOT reliably attach
	// Retry-After to secondary blocks (observed live: the 2026-08-23 re-trip
	// happened with header-only detection armed — caution never engaged and the
	// reset-boundary stampede fired anyway), so match the way go-github itself
	// does: the documented "secondary rate limit" phrase in the 403 body. The
	// body is peeked and restored so downstream error decoding still sees it.
	if err == nil && resp != nil && resp.StatusCode == http.StatusForbidden {
		after, secondary := secondaryLimitBackoff(resp)
		if secondary {
			until := time.Now().Add(after + st.window)
			st.mu.Lock()
			if until.After(st.cautiousUntil) {
				st.cautiousUntil = until
			}
			st.mu.Unlock()
		}
	}
	return resp, err
}

// secondaryLimitSniffBytes bounds how much of a 403 body is peeked for the
// secondary-limit phrase. GitHub's abuse/secondary payloads are a couple
// hundred bytes; 2 KiB is generous without buffering real content responses.
const secondaryLimitSniffBytes = 2048

// secondaryLimitBackoff reports whether resp is a secondary-rate-limit 403 and
// how long GitHub asked us to back off. Retry-After is honored when present;
// otherwise the 403 body is peeked (and RESTORED onto resp.Body) for the
// "secondary rate limit" phrase, with the default backoff covering a full
// window. A plain permission 403 returns false.
func secondaryLimitBackoff(resp *http.Response) (time.Duration, bool) {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		after := slowStartDefaultRetryAfter
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			after = time.Duration(secs) * time.Second
		}
		return after, true
	}
	if resp.Body == nil {
		return 0, false
	}
	peek := make([]byte, secondaryLimitSniffBytes)
	n, _ := io.ReadFull(resp.Body, peek)
	rest := resp.Body
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek[:n]), rest), rest}
	if bytes.Contains(bytes.ToLower(peek[:n]), []byte("secondary rate limit")) {
		return slowStartDefaultRetryAfter, true
	}
	return 0, false
}
