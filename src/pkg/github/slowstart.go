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
	"strings"
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
// (a Retry-After header or the "secondary rate limit" body phrase —
// permission 403s carry neither), it enters a caution window extending past
// the limit's reset. While cautious, requests are globally
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
	// slowStartSecondaryBackoff is the back-off assumed for a secondary-limit
	// 403 that names no retry time (no usable Retry-After, and
	// X-RateLimit-Remaining is not 0). GitHub's documented rule for that case
	// is "wait for at least one minute before retrying", with exponentially
	// longer waits if the limit keeps tripping. A flat hour here (the old
	// default, which hivecommons/hive#4674 applied to every header-less 403)
	// kept one search 403 after a restart pacing — and shedding low-class
	// work, including the merge sweep's status reads — for 70 minutes while
	// the primary quota sat at 87% (projectbluefin hive, 2026-10-28).
	slowStartSecondaryBackoff = time.Minute
	// slowStartSecondaryBackoffMax caps the escalation on repeated trips.
	slowStartSecondaryBackoffMax = 15 * time.Minute
	// slowStartSecondaryRepeatWithin is how soon after the previous back-off
	// ended a new header-less secondary 403 counts as a repeat and doubles
	// the back-off; a trip after a quiet spell this long starts over at
	// slowStartSecondaryBackoff.
	slowStartSecondaryRepeatWithin = 30 * time.Minute
	// slowStartDeadlineMargin preserves time for the actual HTTP round trip once a
	// paced request reaches its slot.
	slowStartDeadlineMargin = 500 * time.Millisecond
	// slowStartLogInterval bounds caution-window logs during repeated extensions.
	slowStartLogInterval = time.Minute

	// pacingLimitHeader is set on a 403/429 response the pacer recognised as
	// a rate-limit refusal; its value is "primary" or "secondary". The OTel
	// transport (otel_transport.go) reads it to count secondary-limit hits.
	pacingLimitHeader = "X-Hive-Pacing-Limit"
	// pacingErrPrefix starts every error the pacer returns in place of a
	// response (deadline-miss refusals today, priority shedding when it
	// lands); the OTel transport reports those as github.shed.
	pacingErrPrefix = "github pacing:"
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
	// secondaryStreak counts back-to-back header-less secondary trips (0 on
	// the first); secondaryBackoffEnd is when the current one's back-off
	// ends. See secondaryBackoffLocked.
	secondaryStreak     int
	secondaryBackoffEnd time.Time
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

// cautious reports whether the caution window is engaged at now. Optional
// housekeeping callers use it to stand down instead of queueing for slots.
func (st *slowStartState) cautious(now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return now.Before(st.cautiousUntil)
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
		st.secondaryStreak = 0
		st.secondaryBackoffEnd = time.Time{}
		st.mu.Unlock()
	}
	resetPacingShedLogForTest()
}

// EngageRateLimitCautionForTest puts the process-wide pacer into a caution
// window whose queue is already queueDepth ahead of now, with later slots
// gap apart and no jitter, so a test in another package can check how its
// callers behave while cautious without minute-long sleeps. The returned
// func restores the pacer; call it in Cleanup.
func EngageRateLimitCautionForTest(queueDepth, gap time.Duration) (restore func()) {
	st := sharedSlowStartState
	now := time.Now()
	st.mu.Lock()
	prevGap, prevJitter := st.gap, st.jitter
	st.gap, st.jitter = gap, 0
	st.cautiousUntil = now.Add(time.Hour)
	st.nextSlot = now.Add(queueDepth)
	st.abandonedSlots = nil
	st.mu.Unlock()
	return func() {
		st.mu.Lock()
		st.gap, st.jitter = prevGap, prevJitter
		st.mu.Unlock()
		ResetRateLimitPacingForTest()
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

// secondaryBackoffLocked returns the back-off for a secondary-limit 403 that
// named no retry time, following GitHub's guidance: one minute, doubling on
// each repeat up to slowStartSecondaryBackoffMax. A 403 that arrives while
// the current back-off is still running is the rest of the same burst (the
// requests already in flight when the limit tripped all come back refused),
// so it neither escalates nor extends the back-off. One that arrives within
// slowStartSecondaryRepeatWithin of the back-off ending is a re-trip and
// escalates; anything later starts over.
func (st *slowStartState) secondaryBackoffLocked(now time.Time) time.Duration {
	switch {
	case now.Before(st.secondaryBackoffEnd):
		return st.secondaryBackoffEnd.Sub(now)
	case !st.secondaryBackoffEnd.IsZero() && now.Before(st.secondaryBackoffEnd.Add(slowStartSecondaryRepeatWithin)):
		st.secondaryStreak++
	default:
		st.secondaryStreak = 0
	}
	after := slowStartSecondaryBackoff
	for i := 0; i < st.secondaryStreak && after < slowStartSecondaryBackoffMax; i++ {
		after *= 2
	}
	if after > slowStartSecondaryBackoffMax {
		after = slowStartSecondaryBackoffMax
	}
	st.secondaryBackoffEnd = now.Add(after)
	return after
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
		// Priority ladder (pacing_priority.go): a request whose class may
		// not queue this deep is shed without taking a slot, so periodic
		// housekeeping cannot push the governor's enumeration past its
		// deadline.
		class := PacingClassOf(req.Context())
		if limit, bounded := class.maxQueue(); bounded {
			if eta := slot.Sub(now); eta > limit {
				st.mu.Unlock()
				logPacingShedOnce("transport:"+class.String(), now, "path", req.URL.Path, "queue_eta", eta.Round(time.Millisecond), "limit", limit)
				return nil, pacingShedError(class, eta, limit)
			}
		}
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
		after, named, secondary := secondaryLimitBackoff(resp, time.Now())
		if !secondary {
			if reset, primary := primaryLimitReset(resp, time.Now()); primary {
				until = reset.Add(st.window)
				kind = "primary"
			}
		}
		if secondary || !until.IsZero() {
			// Tell the observability layer outside this transport which
			// limit tripped without it having to re-sniff the body.
			st.mu.Lock()
			now := time.Now()
			if secondary {
				kind = "secondary"
				if !named {
					after = st.secondaryBackoffLocked(now)
				}
				until = now.Add(after + st.window)
			}
			resp.Header.Set(pacingLimitHeader, kind)
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

// secondaryLimitBackoff reports whether resp is a secondary-rate-limit 403 and,
// when GitHub said, how long to back off. Detection is a Retry-After header
// or, since GitHub does not reliably attach one, the "secondary rate limit"
// phrase in the 403 body (peeked and RESTORED onto resp.Body). named is true
// only when the response names the wait: a parseable Retry-After (seconds or
// an HTTP date), or X-RateLimit-Remaining: 0 with a future X-RateLimit-Reset
// (capped at primaryLimitMaxReset). Otherwise the caller picks the back-off
// (secondaryBackoffLocked). X-RateLimit-Reset is deliberately NOT honoured
// while Remaining is non-zero: it is the primary bucket's reset, which on a
// core endpoint can be up to an hour away and has nothing to do with the
// secondary block — exactly how the old default turned one 403 into an hour.
// A plain permission 403 returns secondary=false.
func secondaryLimitBackoff(resp *http.Response, now time.Time) (after time.Duration, named, secondary bool) {
	ra := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if ra == "" {
		if resp.Body == nil {
			return 0, false, false
		}
		peek := make([]byte, secondaryLimitSniffBytes)
		n, _ := io.ReadFull(resp.Body, peek)
		rest := resp.Body
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(peek[:n]), rest), rest}
		if !bytes.Contains(bytes.ToLower(peek[:n]), []byte("secondary rate limit")) {
			return 0, false, false
		}
	} else {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second, true, true
		}
		if at, err := http.ParseTime(ra); err == nil {
			if d := at.Sub(now); d > 0 {
				return d, true, true
			}
			return 0, true, true
		}
	}
	// primaryLimitReset answers only for X-RateLimit-Remaining: 0.
	if reset, primary := primaryLimitReset(resp, now); primary && reset.After(now) {
		return reset.Sub(now), true, true
	}
	return 0, false, true
}
