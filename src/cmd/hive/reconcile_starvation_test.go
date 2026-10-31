package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// loopClock is a fake clock the test advances from its own goroutine while
// the loop reads it through deps.now. It counts reads so a test can wait
// for the loop to reach a known point (the loop-top reconcileOverdue
// check reads the clock once after every persist, then the loop parks in
// select) before advancing time.
type loopClock struct {
	mu    sync.Mutex
	t     time.Time
	reads int
	read  chan struct{}
}

func newLoopClock() *loopClock {
	return &loopClock{t: time.Now(), read: make(chan struct{}, 64)}
}

func (c *loopClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	select {
	case c.read <- struct{}{}:
	default:
	}
	return c.t
}

func (c *loopClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// waitReads blocks until the loop has read the clock at least n times in
// total.
func (c *loopClock) waitReads(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		c.mu.Lock()
		got := c.reads
		c.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-c.read:
		case <-deadline:
			t.Fatalf("clock reads = %d, want >= %d", got, n)
		}
	}
}

// Wake-driven cached cycles arriving more often than the eval interval
// must not starve the reconcile pass: once the cached snapshot is older
// than the interval, the next wake enumerates even if the ticker never
// fires (hivecommons/hive#15779).
func TestRunLoopWakeEnumeratesWhenSnapshotOlderThanInterval(t *testing.T) {
	b, log, cancel := newLoopBoot(t, 900, 0)
	defer cancel()
	b.reviewWake = make(chan struct{}, 1)
	clock := newLoopClock()
	gov := newFakeTicker()
	h := &loopHarness{startupOK: true, pending: []*fakeTicker{gov}, persisted: make(chan struct{}, 16)}
	deps := h.deps()
	deps.now = clock.now
	done := runLoopAsync(b, deps)
	<-h.persisted // initial reconcile pass
	// The initial pass stamps lastEnumerateAt, then the loop top checks
	// reconcileOverdue once before parking in select.
	reads := 2
	clock.waitReads(t, reads)

	// Four wakes 5 minutes apart against a 900s interval: the first two
	// plan from the cache, the third lands 15m after the last reconcile
	// and must enumerate; the fourth is cached again.
	//
	// Advance the clock only once the loop is parked in select: if the
	// loop-top reconcileOverdue check ran against the already-advanced
	// clock it would enumerate there instead, and the pending wake would
	// then run as an extra cached cycle.
	for i := 0; i < 4; i++ {
		clock.advance(5 * time.Minute)
		b.reviewWake <- struct{}{}
		<-h.persisted
		// tick reads the clock for its overdue check (plus once more to
		// stamp lastEnumerateAt when it upgrades), then the loop top reads
		// it again before the next select.
		reads += 2
		if i == 2 {
			reads++
		}
		clock.waitReads(t, reads)
	}
	cancel()
	waitDone(t, done)

	want := []evalCycleSource{evalSourceEnumerate, evalSourceCached, evalSourceCached, evalSourceEnumerate, evalSourceCached}
	if len(h.sources) != len(want) {
		t.Fatalf("sources = %v, want %v", h.sources, want)
	}
	for i := range want {
		if h.sources[i] != want[i] {
			t.Fatalf("sources = %v, want %v", h.sources, want)
		}
	}
	if !strings.Contains(log.String(), "cached snapshot older than eval interval; enumerating") {
		t.Fatalf("missing upgrade log line:\n%s", log.String())
	}
}

// A ticker tick that is lost (never delivered) must not stop the reconcile
// pass: any loop iteration that finds a full interval elapsed since the
// last reconcile runs one, here on a not-yet-due continuous wake timer.
func TestRunLoopTopRunsOverdueReconcileWithoutTickerTick(t *testing.T) {
	b, log, cancel := newLoopBoot(t, 900, 0)
	defer cancel()
	b.cfg.Governor.Modes = map[string]config.ModeConfig{"idle": {Cadences: map[string]config.Cadence{"alpha": "continuous"}}}
	b.gov = governor.New(b.cfg.Governor, b.cfg.EnabledAgents(), b.logger)
	b.gov.Evaluate(0, 0, 0, 0)
	clock := newLoopClock()
	b.gov.SeedContinuousNextKickForTest("alpha", clock.now().Add(24*time.Hour))
	gov := newFakeTicker()
	wake := newFakeTicker()
	h := &loopHarness{startupOK: true, pending: []*fakeTicker{gov}, persisted: make(chan struct{}, 16)}
	deps := h.deps()
	deps.now = clock.now
	deps.newWakeTimer = func(time.Duration) loopTicker { return wake }
	done := runLoopAsync(b, deps)
	<-h.persisted

	// Poll wake inside the interval: nothing runs, only a re-arm.
	clock.advance(10 * time.Minute)
	wake.fire()
	// Poll wake after the interval elapsed with no ticker tick delivered.
	clock.advance(6 * time.Minute)
	wake.fire()
	<-h.persisted
	cancel()
	waitDone(t, done)

	if len(h.sources) != 2 || h.sources[1] != evalSourceEnumerate {
		t.Fatalf("sources = %v, want initial + loop-top reconcile pass", h.sources)
	}
	if h.sweeps != 2 {
		t.Fatalf("sweeps = %d, want the owed reconcile to run as a full tick", h.sweeps)
	}
	if !strings.Contains(log.String(), "eval interval elapsed without a reconcile pass; enumerating") {
		t.Fatalf("missing loop-top log line:\n%s", log.String())
	}
}
