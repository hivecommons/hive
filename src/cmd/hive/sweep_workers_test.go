package main

import (
	"context"
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
		runSweepWorker(ctx, job, func() *config.Config { return cfg }, func(time.Duration) loopTicker { return slow }, now, func(*config.Config, uint64) {
			calls.Add(1)
			slowStarted <- struct{}{}
			<-release
		})
	}()
	go func() {
		defer close(fastDone)
		runSweepWorker(ctx, job, func() *config.Config { return cfg }, func(time.Duration) loopTicker { return fast }, now, func(*config.Config, uint64) { fastRan <- struct{}{} })
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
		runSweepWorker(ctx, sweepJob{interval: func(c *config.Config) time.Duration { return time.Duration(c.Governor.EvalIntervalS) * time.Second }}, live.Load,
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
