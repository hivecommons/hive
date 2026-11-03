package main

import (
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// fakeAfterFunc records armed callbacks so the test fires them by hand,
// stepping a fake clock instead of sleeping.
type fakeAfterFunc struct {
	now     time.Time
	delays  []time.Duration
	pending []func()
}

func (f *fakeAfterFunc) clock() time.Time { return f.now }

func (f *fakeAfterFunc) after(d time.Duration, fn func()) webhookTimerStop {
	f.delays = append(f.delays, d)
	f.pending = append(f.pending, fn)
	idx := len(f.pending) - 1
	return func() bool {
		if f.pending[idx] == nil {
			return false
		}
		f.pending[idx] = nil
		return true
	}
}

// fire runs the oldest armed callback after advancing the clock by its delay.
func (f *fakeAfterFunc) fire(t *testing.T) {
	t.Helper()
	for i, fn := range f.pending {
		if fn != nil {
			f.now = f.now.Add(f.delays[i])
			f.pending[i] = nil
			fn()
			return
		}
	}
	t.Fatal("no armed webhook wake timer")
}

func newTestWebhookWaker(debounce, minGap time.Duration) (*webhookWaker, *fakeAfterFunc) {
	f := &fakeAfterFunc{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	w := newWebhookWaker(func() time.Duration { return debounce }, func() time.Duration { return minGap })
	w.now = f.clock
	w.afterFunc = f.after
	return w, f
}

func TestWebhookWakerDebouncesBurstIntoOneWake(t *testing.T) {
	w, f := newTestWebhookWaker(10*time.Second, 30*time.Second)

	w.Request("push org/repo #1")
	f.now = f.now.Add(2 * time.Second)
	w.Request("check_run org/repo #1")
	w.Request("check_suite org/repo #1")
	if len(f.delays) != 1 || f.delays[0] != 10*time.Second {
		t.Fatalf("burst should arm one debounce timer of 10s, got %v", f.delays)
	}
	select {
	case <-w.out:
		t.Fatal("wake must not fire before the debounce elapses")
	default:
	}
	f.fire(t)
	select {
	case b := <-w.out:
		if b.Count != 3 || b.Reason != "push org/repo #1" || b.LastRe != "check_suite org/repo #1" {
			t.Fatalf("batch = %+v", b)
		}
		// 2s elapsed inside the burst plus the 10s debounce after the last arm.
		if b.Coalesced != 12*time.Second {
			t.Fatalf("coalesced = %v, want 12s since the first delivery", b.Coalesced)
		}
	default:
		t.Fatal("debounce elapsed without a wake")
	}
}

func TestWebhookWakerHonorsMinGapBetweenWakes(t *testing.T) {
	w, f := newTestWebhookWaker(10*time.Second, 30*time.Second)

	w.Request("pull_request org/repo #1")
	f.fire(t)
	<-w.out

	// 5s after the first wake fired a new delivery arrives: the next wake
	// must wait for the 30s gap (25s more), not just the 10s debounce.
	f.now = f.now.Add(5 * time.Second)
	w.Request("issue_comment org/repo #2")
	if got := f.delays[len(f.delays)-1]; got != 25*time.Second {
		t.Fatalf("second wake delay = %v, want 25s (min gap)", got)
	}
	f.fire(t)
	select {
	case b := <-w.out:
		if b.Count != 1 || b.Reason != "issue_comment org/repo #2" {
			t.Fatalf("batch = %+v", b)
		}
	default:
		t.Fatal("min gap elapsed without a wake")
	}

	// Well past the gap the plain debounce applies again.
	f.now = f.now.Add(time.Hour)
	w.Request("issues org/repo issue #3")
	if got := f.delays[len(f.delays)-1]; got != 10*time.Second {
		t.Fatalf("delay after a long quiet period = %v, want 10s", got)
	}
}

func TestWebhookWakerStopCancelsPendingWake(t *testing.T) {
	w, f := newTestWebhookWaker(10*time.Second, 30*time.Second)
	w.Request("pull_request org/repo #1")
	w.Stop()
	if f.pending[0] != nil {
		t.Fatal("Stop should cancel the armed timer")
	}
	w.Request("pull_request org/repo #1")
	if len(f.delays) != 2 {
		t.Fatalf("a request after Stop should re-arm, delays = %v", f.delays)
	}
}

func TestRunLoopWithRunsAnEarlyCycleOnWebhookWake(t *testing.T) {
	t.Cleanup(github.ResetWebhookTrackerForTest())
	b, log, cancel := newLoopBoot(t, 60, 0)
	b.installWebhookWake()
	gov := newFakeTicker()
	h := &loopHarness{startupOK: true, pending: []*fakeTicker{gov}, persisted: make(chan struct{}, 8)}
	done := runLoopAsync(b, h.deps())
	<-h.persisted // first cycle done

	b.webhookWaker.out <- webhookWakeBatch{Reason: "pull_request_review org/repo #7", LastRe: "pull_request_review org/repo #7", Count: 1}
	<-h.persisted // early cycle done
	gov.fire()
	<-h.persisted // cadence cycle still runs
	cancel()
	waitDone(t, done)

	if len(h.evals) != 3 {
		t.Fatalf("evals = %d, want first + webhook wake + cadence", len(h.evals))
	}
	// The wake plans from the cached snapshot; only the cadence tick lists.
	if want := []evalCycleSource{evalSourceEnumerate, evalSourceCached, evalSourceEnumerate}; !reflect.DeepEqual(h.sources, want) {
		t.Fatalf("eval sources = %v, want %v", h.sources, want)
	}
	if !strings.Contains(log.String(), "github webhook wake: running an early eval cycle") || !strings.Contains(log.String(), "source=cached") {
		t.Fatalf("log = %s", log.String())
	}
	snap := b.evalLoop.snapshot()
	if snap.LastWakeReason != "pull_request_review org/repo #7" || snap.WebhookWakes != 1 || snap.LastWakeAt == nil {
		t.Fatalf("eval loop status = %+v, want the wake recorded", snap)
	}
	if snap.Mode != evalModePoll {
		t.Fatalf("eval mode = %q, want poll without webhook health", snap.Mode)
	}
}

// TestEvalIntervalStretchesWhileWebhooksHealthy is the API-call reduction
// half of the webhook wake: with a verified delivery inside the health
// window the base tick becomes eval_interval_webhook_s and the loop reports
// eval mode webhook; once the window lapses it drops back to eval_interval_s
// and mode poll, logging one line per transition.
func TestEvalIntervalStretchesWhileWebhooksHealthy(t *testing.T) {
	clock := &lockedClock{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(clock.Now))
	cfg := &config.Config{}
	cfg.Governor.EvalIntervalS = 120
	cfg.Governor.EvalIntervalMaxS = 1800
	cfg.Governor.EvalIntervalWebhookS = 900
	var sb strings.Builder
	logger := slog.New(slog.NewTextHandler(&sb, nil))

	if got, mode := evalIntervalAndModeForConfig(cfg, nil, logger); got != 120*time.Second || mode != evalModePoll {
		t.Fatalf("no webhooks: interval = %v mode = %q, want 120s poll", got, mode)
	}
	github.RecordWebhookEventForTest("org/repo")
	got, mode := evalIntervalAndModeForConfig(cfg, nil, logger)
	if got != 900*time.Second || mode != evalModeWebhook {
		t.Fatalf("healthy webhooks: interval = %v mode = %q, want 900s webhook", got, mode)
	}
	if n := strings.Count(sb.String(), "webhooks healthy; webhook cadence engaged"); n != 1 {
		t.Fatalf("want one polling → webhook INFO line, got %d in %s", n, sb.String())
	}
	evalIntervalAndModeForConfig(cfg, nil, logger)
	if n := strings.Count(sb.String(), "webhooks healthy; webhook cadence engaged"); n != 1 {
		t.Fatalf("healthy must be logged once per transition, got %d", n)
	}

	// A quiet repo (window lapsed, no GitHub activity seen) keeps the
	// webhook cadence (#13381): silence alone is not degradation.
	clock.Advance(2*120*time.Second + time.Second) // past the 2 × eval_interval_s window
	got, mode = evalIntervalAndModeForConfig(cfg, nil, logger)
	if got != 900*time.Second || mode != evalModeWebhook {
		t.Fatalf("lapsed but quiet: interval = %v mode = %q, want 900s webhook", got, mode)
	}
	// A poll that lists an item updated after the last delivery proves the
	// silence was an outage: the cadence drops back to eval_interval_s.
	github.RecordGitHubActivity(clock.Now())
	got, mode = evalIntervalAndModeForConfig(cfg, nil, logger)
	if got != 120*time.Second || mode != evalModePoll {
		t.Fatalf("degraded webhooks: interval = %v mode = %q, want 120s poll", got, mode)
	}
	if n := strings.Count(sb.String(), "webhooks degraded; polling cadence restored"); n != 1 {
		t.Fatalf("want one webhook → polling INFO line, got %d in %s", n, sb.String())
	}
	if snap := github.WebhookHealthSnapshot(); snap.Mode != github.WebhookModePolling || snap.Since == nil {
		t.Fatalf("status webhooks block = %+v, want mode polling with since", snap.WebhookModeStatus)
	}
	// Recovery needs WebhookRecoveryHealthyWindows consecutive healthy
	// windows, not one delivery.
	github.RecordWebhookEventForTest("org/repo")
	if got, mode = evalIntervalAndModeForConfig(cfg, nil, logger); mode != evalModePoll {
		t.Fatalf("one delivery after a degradation: mode = %q interval = %v, want poll (hysteresis)", mode, got)
	}
	clock.Advance(100 * time.Second)
	github.RecordWebhookEventForTest("org/repo")
	clock.Advance(140 * time.Second) // ≥ one further window of uninterrupted health
	if got, mode = evalIntervalAndModeForConfig(cfg, nil, logger); got != 900*time.Second || mode != evalModeWebhook {
		t.Fatalf("recovered: interval = %v mode = %q, want 900s webhook", got, mode)
	}
	if n := strings.Count(sb.String(), "webhooks healthy; webhook cadence engaged"); n != 2 {
		t.Fatalf("want a second polling → webhook INFO line on recovery, got %d in %s", n, sb.String())
	}
}

func TestWebhookHealthRecheckDelay(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	last := now.Add(-100 * time.Second)
	if d := webhookHealthRecheckDelay(github.WebhookHealth{}, 240*time.Second, now); d != 0 {
		t.Fatalf("no events: delay = %v, want 0", d)
	}
	if d := webhookHealthRecheckDelay(github.WebhookHealth{Healthy: false, LastEventAt: &last}, 240*time.Second, now); d != 0 {
		t.Fatalf("already stale: delay = %v, want 0", d)
	}
	if d := webhookHealthRecheckDelay(github.WebhookHealth{Healthy: true, LastEventAt: &last}, 240*time.Second, now); d != 141*time.Second {
		t.Fatalf("healthy: delay = %v, want window remainder + 1s = 141s", d)
	}
	old := now.Add(-time.Hour)
	if d := webhookHealthRecheckDelay(github.WebhookHealth{Healthy: true, LastEventAt: &old}, 240*time.Second, now); d != time.Second {
		t.Fatalf("overdue: delay = %v, want 1s floor", d)
	}
}

// lockedClock is a test clock safe to step from the test goroutine while the
// loop reads it.
type lockedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *lockedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *lockedClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// TestRunLoopHealthRecheckResetsTickerWhenWebhooksLapse: a loop in webhook
// mode re-checks health when the window closes and, finding it lapsed,
// resets the ticker to eval_interval_s without waiting for the stretched tick.
func TestRunLoopHealthRecheckResetsTickerWhenWebhooksLapse(t *testing.T) {
	clock := &lockedClock{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(clock.Now))
	github.RecordWebhookEventForTest("org/repo")

	b, log, cancel := newLoopBoot(t, 60, 0)
	b.cfg.Governor.EvalIntervalWebhookS = 900
	b.cfg.Governor.EvalIntervalMaxS = 1800
	b.installWebhookWake()
	gov := newFakeTicker()
	health := newFakeTicker()
	h := &loopHarness{startupOK: true, pending: []*fakeTicker{gov}, persisted: make(chan struct{}, 8)}
	deps := h.deps()
	deps.now = clock.Now
	deps.newHealthTimer = func(d time.Duration) loopTicker { health.period = d; return health }
	done := runLoopAsync(b, deps)
	<-h.persisted // first cycle done

	// Still inside the window: the re-check only re-arms. The unbuffered fire
	// rendezvous with the select, which the loop reaches only after arming.
	health.fire()
	if gov.period != 900*time.Second {
		t.Fatalf("healthy webhooks: governor ticker period = %v, want 900s", gov.period)
	}
	if health.period != 121*time.Second {
		t.Fatalf("health re-check armed for %v, want window (2×60s) + 1s", health.period)
	}
	if mode := b.evalLoop.snapshot().Mode; mode != evalModeWebhook {
		t.Fatalf("eval mode = %q, want webhook", mode)
	}
	if len(gov.resets) != 0 {
		t.Fatalf("inside the window the ticker must not reset: %v", gov.resets)
	}

	clock.Advance(121 * time.Second)
	github.RecordGitHubActivity(clock.Now()) // a poll saw a change webhooks missed
	health.fire()
	<-h.persisted // the loop top's overdue reconcile pass (asserted below)
	cancel()
	waitDone(t, done)

	if len(gov.resets) != 1 || gov.resets[0] != 60*time.Second {
		t.Fatalf("ticker resets = %v, want one reset to 60s when health lapsed", gov.resets)
	}
	if mode := b.evalLoop.snapshot().Mode; mode != evalModePoll {
		t.Fatalf("eval mode after lapse = %q, want poll", mode)
	}
	if !strings.Contains(log.String(), "webhooks degraded; polling cadence restored") {
		t.Fatalf("log = %s", log.String())
	}
	// The re-check itself runs no eval cycle; the one extra cycle is the
	// loop top's owed reconcile pass, because 121s have passed since the
	// last one against the restored 60s cadence (hivecommons/hive#15779).
	if len(h.sources) != 2 || h.sources[1] != evalSourceEnumerate {
		t.Fatalf("sources = %v, want first cycle + one overdue reconcile pass", h.sources)
	}
	if !strings.Contains(log.String(), "eval interval elapsed without a reconcile pass; enumerating") {
		t.Fatalf("missing overdue reconcile log: %s", log.String())
	}
}
