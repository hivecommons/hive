package github

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
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
	// slowStartDeadlineMargin preserves time for the actual HTTP round trip once a
	// paced request reaches its slot.
	slowStartDeadlineMargin = 500 * time.Millisecond
	// slowStartLogInterval bounds caution-window logs during repeated extensions.
	slowStartLogInterval = time.Minute
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
	gap            time.Duration
	jitter         time.Duration
	window         time.Duration
	deadlineMargin time.Duration

	mu             sync.Mutex
	cautiousUntil  time.Time
	nextSlot       time.Time
	abandonedSlots []time.Time
	lastCautionLog time.Time
}

type slowStartTransport struct {
	inner http.RoundTripper
	state *slowStartState
}

func newSlowStartState() *slowStartState {
	return &slowStartState{
		gap:            slowStartGap,
		jitter:         slowStartJitter,
		window:         slowStartWindow,
		deadlineMargin: slowStartDeadlineMargin,
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

// ResetRateLimitPacingForTest clears the process-wide pacing ledger. A test
// that deliberately answers a real client with a rate-limit 403 engages the
// caution window for the whole test binary (every later request through the
// shared chain is spaced ~2s apart); call this in that test's Cleanup.
func ResetRateLimitPacingForTest() {
	for _, st := range []*slowStartState{sharedSlowStartState, externalSlowStartState} {
		st.mu.Lock()
		st.cautiousUntil = time.Time{}
		st.nextSlot = time.Time{}
		st.abandonedSlots = nil
		st.lastCautionLog = time.Time{}
		st.mu.Unlock()
	}
}

func (st *slowStartState) claimSlotLocked(now time.Time) (time.Time, bool) {
	for len(st.abandonedSlots) > 0 && st.abandonedSlots[0].Before(now) {
		st.abandonedSlots = st.abandonedSlots[1:]
	}
	if len(st.abandonedSlots) > 0 {
		return st.abandonedSlots[0], true
	}
	if st.nextSlot.Before(now) {
		return now, false
	}
	return st.nextSlot, false
}

func (st *slowStartState) consumeAbandonedSlotLocked(slot time.Time) {
	if len(st.abandonedSlots) > 0 && st.abandonedSlots[0].Equal(slot) {
		st.abandonedSlots = st.abandonedSlots[1:]
	}
}

func (st *slowStartState) reclaimSlot(slot time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	if slot.Before(now) || !now.Before(st.cautiousUntil) {
		return
	}
	i := 0
	for i < len(st.abandonedSlots) && st.abandonedSlots[i].Before(slot) {
		i++
	}
	if i < len(st.abandonedSlots) && st.abandonedSlots[i].Equal(slot) {
		return
	}
	st.abandonedSlots = append(st.abandonedSlots, time.Time{})
	copy(st.abandonedSlots[i+1:], st.abandonedSlots[i:])
	st.abandonedSlots[i] = slot
}

func (st *slowStartState) logCautionLocked(now time.Time, kind string, until time.Time, req *http.Request) {
	if !st.lastCautionLog.IsZero() && now.Sub(st.lastCautionLog) < slowStartLogInterval {
		return
	}
	st.lastCautionLog = now
	path := ""
	if req != nil && req.URL != nil {
		path = req.URL.Path
	}
	slog.Warn("github slow-start caution window engaged", "kind", kind, "until", until, "path", path)
}

func (t *slowStartTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Pacing: while cautious, hand each request the next free slot and sleep
	// until it. Slots are claimed under the lock; the sleep happens outside it
	// so pacing serializes REQUEST STARTS, not the lock.
	st := t.state
	var slot time.Time
	var wait time.Duration
	var claimed bool
	st.mu.Lock()
	now := time.Now()
	if now.Before(st.cautiousUntil) {
		gap := st.gap + time.Duration(rand.Int63n(int64(st.jitter)+1))
		var fromAbandoned bool
		slot, fromAbandoned = st.claimSlotLocked(now)
		deadlineMargin := st.deadlineMargin
		if deadlineMargin == 0 {
			deadlineMargin = slowStartDeadlineMargin
		}
		if deadline, ok := req.Context().Deadline(); ok && slot.Add(deadlineMargin).After(deadline) {
			st.mu.Unlock()
			return nil, fmt.Errorf("github pacing: slot would miss request deadline: %w", context.DeadlineExceeded)
		}
		if fromAbandoned {
			st.consumeAbandonedSlotLocked(slot)
		} else {
			st.nextSlot = slot.Add(gap)
		}
		wait = slot.Sub(now)
		claimed = true
	}
	st.mu.Unlock()

	if wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-req.Context().Done():
			if !timer.Stop() {
				<-timer.C
			}
			if claimed {
				st.reclaimSlot(slot)
			}
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
	//
	// The PRIMARY (hourly) limit gets the same treatment (#7430): go-github
	// refuses pre-emptively until X-RateLimit-Reset, and at that instant every
	// queued caller fires at once — the projectbluefin spoke re-exhausted a
	// 6650/hr installation quota within ONE SECOND of every reset ("rate limit
	// was reset 1s ago"). Pacing the first slowStartWindow after the reset
	// turns that stampede into a trickle the new window can absorb.
	if err == nil && resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) {
		var until time.Time
		var kind string
		if after, secondary := secondaryLimitBackoff(resp); secondary {
			until = time.Now().Add(after + st.window)
			kind = "secondary"
		} else if reset, primary := primaryLimitReset(resp, time.Now()); primary {
			until = reset.Add(st.window)
			kind = "primary"
		}
		if !until.IsZero() {
			st.mu.Lock()
			now := time.Now()
			if until.After(st.cautiousUntil) {
				st.cautiousUntil = until
				st.logCautionLocked(now, kind, until, req)
			}
			st.mu.Unlock()
		}
	}
	return resp, err
}

// primaryLimitMaxReset caps how far ahead a primary-limit reset is believed.
// GitHub's primary windows are hourly; a reset further out than that is a
// clock problem or a bad header, and pacing until it would idle the hive.
const primaryLimitMaxReset = time.Hour

// primaryLimitReset reports whether resp is a PRIMARY rate-limit refusal —
// GitHub answers 403 (or 429) with X-RateLimit-Remaining: 0 — and when the
// window resets, read from X-RateLimit-Reset (Unix seconds) and capped at
// primaryLimitMaxReset ahead. A refusal that carries no usable reset is
// treated as resetting now: the caution window still engages, it just starts
// immediately.
func primaryLimitReset(resp *http.Response, now time.Time) (time.Time, bool) {
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return time.Time{}, false
	}
	if secs, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && secs > 0 {
		if reset := time.Unix(secs, 0); reset.After(now) {
			if reset.After(now.Add(primaryLimitMaxReset)) {
				reset = now.Add(primaryLimitMaxReset)
			}
			return reset, true
		}
	}
	return now, true
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
