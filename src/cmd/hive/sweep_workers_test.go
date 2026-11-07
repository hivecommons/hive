package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestSweepWorkersIndependentAndSingleFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	slowStarted, fastRan := make(chan struct{}, 4), make(chan struct{}, 4)
	slow, fast := newFakeTicker(), newFakeTicker()
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	cfg := &config.Config{}
	job := sweepJob{interval: func(*config.Config) time.Duration { return time.Minute }}
	var calls atomic.Int32
	slowDone, fastDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(slowDone)
		runSweepWorker(ctx, job, 0, func() *config.Config { return cfg }, func(time.Duration) loopTicker { return slow }, now, func(*config.Config, uint64) {
			calls.Add(1)
			slowStarted <- struct{}{}
			<-release
		})
	}()
	go func() {
		defer close(fastDone)
		runSweepWorker(ctx, job, 0, func() *config.Config { return cfg }, func(time.Duration) loopTicker { return fast }, now, func(*config.Config, uint64) { fastRan <- struct{}{} })
	}()
	defer func() { cancel(); close(release); waitDone(t, slowDone); waitDone(t, fastDone) }()
	waitSignal := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("sweep did not run independently")
		}
	}
	waitSignal(slowStarted)
	waitSignal(fastRan)
	// A whole interval passes while the slow sweep is blocked. The unrelated
	// sweep must fire again, without waiting for the slow sweep's IO or eval.
	clock.Add(int64(time.Minute))
	select {
	case fast.ch <- now():
	case <-time.After(5 * time.Second):
		t.Fatal("fast timer stalled")
	}
	waitSignal(fastRan)
	if calls.Load() != 1 {
		t.Fatal("slow sweep overlapped itself")
	}
}

func TestSweepWorkerObservesReloadedInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticker := newFakeTicker()
	var live atomic.Pointer[config.Config]
	cfg := &config.Config{}
	cfg.Governor.EvalIntervalS = 3600
	live.Store(cfg)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	ran := make(chan struct{}, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSweepWorker(ctx, sweepJob{interval: func(c *config.Config) time.Duration { return time.Duration(c.Governor.EvalIntervalS) * time.Second }}, 0, live.Load,
			func(time.Duration) loopTicker { return ticker }, now, func(*config.Config, uint64) { ran <- struct{}{} })
	}()
	defer func() { cancel(); waitDone(t, done) }()
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("no initial pass")
	}
	next := &config.Config{}
	next.Governor.EvalIntervalS = 30
	live.Store(next)
	clock.Add(int64(time.Minute))
	select {
	case ticker.ch <- now():
	case <-time.After(5 * time.Second):
		t.Fatal("timer stalled")
	}
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("shorter interval not observed")
	}
}

func TestSweepBudgetClasses(t *testing.T) {
	for _, mode := range []github.APIBudgetMode{github.APIBudgetNormal, github.APIBudgetConserve, github.APIBudgetCritical} {
		decision := decideEvalBudgetWork(mode, 1, 1)
		if sweepBudgetSkipped(sweepEssential, decision) {
			t.Fatal("essential sweep shed")
		}
		if got := sweepBudgetSkipped(sweepUnstick, decision); got != (mode == github.APIBudgetCritical) {
			t.Fatalf("unstick mode=%s skipped=%v", mode, got)
		}
		if got := sweepBudgetSkipped(sweepOptional, decision); got != (mode != github.APIBudgetNormal) {
			t.Fatalf("optional mode=%s skipped=%v", mode, got)
		}
	}
	if !sweepBudgetSkipped(sweepOptional, decideEvalBudgetWork(github.APIBudgetNormal, 2, 1)) {
		t.Fatal("optional cadence gate lost")
	}
}

func TestRunLoopStartsSweepsBeforeFirstEvalAndStopsThem(t *testing.T) {
	b, _, cancel := newLoopBoot(t, 60, 0)
	h := &loopHarness{startupOK: true, pending: []*fakeTicker{newFakeTicker()}}
	deps := h.deps()
	started, stopped := false, false
	deps.startSweeps = func(*boot) func() {
		started = true
		return func() { stopped = true }
	}
	deps.runEval = func(*boot, []string, evalCycleSource) {
		if !started {
			t.Error("slow first eval would delay sweep startup")
		}
		cancel()
	}
	b.runLoopWith(deps)
	if !stopped {
		t.Fatal("sweep workers not stopped on loop shutdown")
	}
}

func TestRunLoopStartupCadenceMeasuredFromEvalStart(t *testing.T) {
	b, _, cancel := newLoopBoot(t, 60, 0)
	defer cancel()
	h := &loopHarness{startupOK: true, pending: []*fakeTicker{newFakeTicker()}}
	deps := h.deps()
	clock := time.Now()
	deps.now = func() time.Time { return clock }
	cycles := 0
	deps.runEval = func(*boot, []string, evalCycleSource) {
		cycles++
		if cycles == 1 {
			clock = clock.Add(61 * time.Second)
		} else {
			cancel()
		}
	}
	done := runLoopAsync(b, deps)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		waitDone(t, done)
		t.Fatal("startup evaluation completion reset the reconcile period")
	}
	if cycles != 2 {
		t.Fatalf("cycles=%d, want immediate overdue reconcile", cycles)
	}
}

// runStaggeredSweepWorker starts one worker with firstDelay and a fake clock
// and returns a step func that advances the clock by d and returns how many
// passes have run so far. Each step sends two ticks: the second rendezvous
// only succeeds once the worker has finished evaluating the first.
func runStaggeredSweepWorker(t *testing.T, interval, firstDelay time.Duration) func(d time.Duration) int32 {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ticker := newFakeTicker()
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	var fired atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSweepWorker(ctx, sweepJob{interval: func(*config.Config) time.Duration { return interval }}, firstDelay,
			func() *config.Config { return &config.Config{} }, func(time.Duration) loopTicker { return ticker }, now,
			func(*config.Config, uint64) { fired.Add(1) })
	}()
	t.Cleanup(func() { cancel(); waitDone(t, done) })
	send := func() {
		t.Helper()
		select {
		case ticker.ch <- now():
		case <-time.After(5 * time.Second):
			t.Fatal("sweep worker stalled")
		}
	}
	return func(d time.Duration) int32 {
		t.Helper()
		send() // the previous evaluation has finished once this lands
		clock.Add(int64(d))
		send()
		send()
		return fired.Load()
	}
}

// A restart must not run every sweep in its first second (2026-11-06: the
// burst spent the App's hourly quota): the first pass waits its offset, and
// later passes keep the sweep's own interval.
func TestSweepWorkerFirstPassWaitsForStartupOffset(t *testing.T) {
	step := runStaggeredSweepWorker(t, 15*time.Minute, 2*time.Minute)
	if n := step(time.Minute); n != 0 {
		t.Fatalf("passes before the startup offset = %d, want 0", n)
	}
	if n := step(time.Minute); n != 1 {
		t.Fatalf("passes once the startup offset passed = %d, want 1", n)
	}
	if n := step(14 * time.Minute); n != 1 {
		t.Fatalf("passes before the sweep's interval = %d, want 1", n)
	}
	if n := step(time.Minute); n != 2 {
		t.Fatalf("passes after the sweep's interval = %d, want 2", n)
	}
}

// A short-interval sweep never waits longer than one interval for its first
// pass, however late its offset in the spread.
func TestSweepWorkerStartupOffsetCappedAtInterval(t *testing.T) {
	step := runStaggeredSweepWorker(t, 30*time.Second, 4*time.Minute)
	if n := step(20 * time.Second); n != 0 {
		t.Fatalf("passes before one interval = %d, want 0", n)
	}
	if n := step(10 * time.Second); n != 1 {
		t.Fatalf("passes after one interval = %d, want 1 (offset must not outlast the interval)", n)
	}
}

func TestSweepFirstDelaysSpreadNonEssential(t *testing.T) {
	jobs := []sweepJob{{class: sweepEssential}, {class: sweepUnstick}, {class: sweepOptional}, {class: sweepOptional}}
	delays := sweepFirstDelays(jobs)
	if delays[0] != 0 {
		t.Fatalf("essential sweep delayed: %s", delays[0])
	}
	prev := time.Duration(0)
	for i, d := range delays[1:] {
		if d <= prev || d >= sweepStartupSpread {
			t.Fatalf("job %d delay %s not inside (%s, %s)", i+1, d, prev, sweepStartupSpread)
		}
		prev = d
	}
}

// A sweep worker's GitHub calls are attributed to "hive:<job name>" in REST
// accounting (/api/gh-rate-limits), not the undifferentiated "hive" that hid
// ~4,400 calls an hour on the knuckle hive. The timeline read stands in for
// any untagged call a sweep makes.
func TestSweepJobRunTaggedAttributesRESTCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/testorg/widget/issues/1/timeline" {
			t.Errorf("unexpected operation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))
	job := sweepJob{name: "p34_caller_probe", run: func(ctx context.Context, gh *github.Client, _ *config.Config) {
		if _, err := gh.IssueTimelineSince(ctx, "widget", 1, time.Time{}); err != nil {
			t.Errorf("timeline read: %v", err)
		}
	}}
	job.runTagged(context.Background(), ghClient, &config.Config{})

	for _, c := range github.RESTTopConsumers(1000) {
		if c.Caller == "hive:p34_caller_probe" && c.Endpoint == "/repos/{owner}/{repo}/issues/{number}/timeline" && c.Requests >= 1 {
			return
		}
	}
	t.Fatalf("REST accounting has no hive:p34_caller_probe timeline row: %+v", github.RESTTopConsumers(1000))
}

// The code scanning sweep (code_scanning) is an optional sweep: it gets a
// staggered first pass, runs on its own configurable interval (hourly by
// default), and is shed whenever the GitHub API budget is not normal.
func TestCodeScanningSweepJobIsOptionalStaggeredAndHourly(t *testing.T) {
	b := &boot{}
	jobs := b.sweepJobs()
	delays := sweepFirstDelays(jobs)
	for i, job := range jobs {
		if job.name != "code_scanning" {
			continue
		}
		if job.class != sweepOptional {
			t.Fatalf("code_scanning class %v, want optional", job.class)
		}
		if delays[i] <= 0 || delays[i] >= sweepStartupSpread {
			t.Fatalf("code_scanning first pass at %s, want inside the startup spread", delays[i])
		}
		cfg := &config.Config{}
		if got := job.interval(cfg); got != time.Hour {
			t.Fatalf("default interval %s, want 1h", got)
		}
		cfg.CodeScanning.Interval = 3 * time.Hour
		if got := job.interval(cfg); got != 3*time.Hour {
			t.Fatalf("configured interval %s, want 3h", got)
		}
		if !sweepBudgetSkipped(job.class, decideEvalBudgetWork(github.APIBudgetConserve, 1, 1)) {
			t.Fatal("code_scanning runs in conserve mode")
		}
		return
	}
	t.Fatal("no code_scanning sweep job")
}
