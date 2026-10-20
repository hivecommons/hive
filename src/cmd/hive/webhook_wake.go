package main

import (
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

// webhookWakeBatch is one coalesced request for an early eval cycle: the
// first and last delivery reasons it covers, how many deliveries it absorbed
// and how long the first one waited.
type webhookWakeBatch struct {
	Reason    string
	LastRe    string
	Count     int
	FirstAt   time.Time
	Coalesced time.Duration
}

// webhookTimerStop stops a pending AfterFunc; it reports whether the callback
// had not run yet, like (*time.Timer).Stop.
type webhookTimerStop func() bool

// webhookWaker turns verified GitHub webhook deliveries into at most one
// governor wake per quiet period. Request records the delivery and arms a
// one-shot timer for governor.webhook_wake_debounce_s; further requests
// during that window only extend the batch. When the timer fires, the batch
// goes to out (buffered 1, non-blocking — a pending wake covers it). Wakes
// are also at least governor.webhook_wake_min_gap_s apart, so a webhook storm
// stretches the batch instead of spinning cycles.
type webhookWaker struct {
	mu        sync.Mutex
	now       func() time.Time
	afterFunc func(d time.Duration, f func()) webhookTimerStop
	debounce  func() time.Duration
	minGap    func() time.Duration
	out       chan webhookWakeBatch

	armed    bool
	stop     webhookTimerStop
	pending  webhookWakeBatch
	lastFire time.Time
	fires    int
}

func newWebhookWaker(debounce, minGap func() time.Duration) *webhookWaker {
	return &webhookWaker{
		now: time.Now,
		afterFunc: func(d time.Duration, f func()) webhookTimerStop {
			t := time.AfterFunc(d, f)
			return t.Stop
		},
		debounce: debounce,
		minGap:   minGap,
		out:      make(chan webhookWakeBatch, 1),
	}
}

// Request records one invalidating delivery and (re)arms the wake if needed.
func (w *webhookWaker) Request(reason string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if w.pending.Count == 0 {
		w.pending = webhookWakeBatch{Reason: reason, FirstAt: now}
	}
	w.pending.Count++
	w.pending.LastRe = reason
	if w.armed {
		return
	}
	delay := w.debounce()
	if !w.lastFire.IsZero() {
		if earliest := w.lastFire.Add(w.minGap()); earliest.After(now.Add(delay)) {
			delay = earliest.Sub(now)
		}
	}
	w.armed = true
	w.stop = w.afterFunc(delay, w.fire)
}

func (w *webhookWaker) fire() {
	w.mu.Lock()
	batch := w.pending
	w.pending = webhookWakeBatch{}
	w.armed = false
	w.stop = nil
	now := w.now()
	w.lastFire = now
	w.fires++
	batch.Coalesced = now.Sub(batch.FirstAt)
	w.mu.Unlock()
	select {
	case w.out <- batch:
	default:
	}
}

// Stop cancels a pending wake; a request after Stop re-arms.
func (w *webhookWaker) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != nil {
		w.stop()
	}
	w.armed = false
	w.stop = nil
}

// evalLoopState is what /api/status reports about the governor loop's pacing
// (dashboard.EvalLoopStatus), written by runLoop and read on every GET.
type evalLoopState struct {
	mu             sync.Mutex
	mode           string
	intervalS      int
	lastWakeReason string
	lastWakeAt     time.Time
	webhookWakes   int
}

const (
	evalModePoll    = "poll"
	evalModeWebhook = "webhook"
)

func (s *evalLoopState) setInterval(d time.Duration, mode string) {
	s.mu.Lock()
	s.intervalS = int(d.Seconds())
	s.mode = mode
	s.mu.Unlock()
}

func (s *evalLoopState) noteWake(b webhookWakeBatch, at time.Time) {
	s.mu.Lock()
	s.lastWakeReason = b.Reason
	s.lastWakeAt = at
	s.webhookWakes++
	s.mu.Unlock()
}

func (s *evalLoopState) snapshot() dashboard.EvalLoopStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := dashboard.EvalLoopStatus{Mode: s.mode, IntervalS: s.intervalS, LastWakeReason: s.lastWakeReason, WebhookWakes: s.webhookWakes}
	if out.Mode == "" {
		out.Mode = evalModePoll
	}
	if !s.lastWakeAt.IsZero() {
		at := s.lastWakeAt.UTC()
		out.LastWakeAt = &at
	}
	return out
}

// evalModeForInterval names the loop's pacing: "webhook" when healthy
// webhooks are what set the current base tick, else "poll".
func evalModeForInterval(webhooksHealthy bool, webhookSeconds int) string {
	if webhooksHealthy && webhookSeconds > 0 {
		return evalModeWebhook
	}
	return evalModePoll
}

// installWebhookWake builds the waker the dashboard webhook receiver calls
// and the loop state /api/status reads. Idempotent, like installReviewEvents.
func (b *boot) installWebhookWake() {
	if b.webhookWaker == nil {
		b.webhookWaker = newWebhookWaker(
			func() time.Duration { return b.cfg.WebhookWakeDebounce() },
			func() time.Duration { return b.cfg.WebhookWakeMinGap() },
		)
	}
	if b.evalLoop == nil {
		b.evalLoop = &evalLoopState{}
	}
}

// webhookWakeFunc is the dashboard.Dependencies.WebhookWake hook.
func (b *boot) webhookWakeFunc() func(string) {
	if b.webhookWaker == nil {
		return nil
	}
	return b.webhookWaker.Request
}

// evalLoopStatusFunc is the dashboard.Dependencies.EvalLoop hook.
func (b *boot) evalLoopStatusFunc() func() dashboard.EvalLoopStatus {
	if b.evalLoop == nil {
		return nil
	}
	return b.evalLoop.snapshot
}

// webhookWakeChan is the loop's receive side; nil when the waker is not
// installed so the select case never fires.
func (b *boot) webhookWakeChan() <-chan webhookWakeBatch {
	if b.webhookWaker == nil {
		return nil
	}
	return b.webhookWaker.out
}

// webhookHealthRecheckDelay says when the loop should re-check webhook health
// so a lapse flips the base tick back to governor.eval_interval_s before the
// stretched webhook tick would next fire. Zero means no re-check is needed
// (not in webhook mode, or no event recorded).
func webhookHealthRecheckDelay(snap github.WebhookHealth, window time.Duration, now time.Time) time.Duration {
	if !snap.Healthy || snap.LastEventAt == nil {
		return 0
	}
	delay := snap.LastEventAt.Add(window).Sub(now) + time.Second
	if delay < time.Second {
		delay = time.Second
	}
	return delay
}
