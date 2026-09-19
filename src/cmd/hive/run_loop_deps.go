package main

import (
	"context"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

// cliStartupDelay is how long runLoop waits after boot for the agent CLIs to
// come up before the first eval cycle.
const cliStartupDelay = 10 * time.Second

// loopTicker is the subset of *time.Ticker runLoop uses, so a test can drive
// governor and agent-status ticks by hand.
type loopTicker interface {
	Chan() <-chan time.Time
	Stop()
	Reset(d time.Duration)
}

type realTicker struct{ *time.Ticker }

func (t realTicker) Chan() <-chan time.Time { return t.C }

// sweepClock holds the last-run stamps of the three periodic GitHub sweeps
// that runLoop threads through every cycle.
type sweepClock struct {
	autoMerge, taskList, duplicate time.Time
}

// runLoopDeps are runLoop's effects (#7571, step 2): the two timers, the CLI
// startup wait, and the per-tick work units (eval cycle, rotation check,
// GitHub sweeps, state persist, crash recovery) — each of which does GitHub
// or disk IO or spawns agent processes. The loop shape, watchdog reconcile,
// lane gating, ticker reset, and agent-status broadcast run for real.
type runLoopDeps struct {
	waitCLIStartup func(ctx context.Context) bool
	newTicker      func(d time.Duration) loopTicker
	runEval        func(b *boot, restarted []string)
	runRotation    func(b *boot)
	runSweeps      func(b *boot, clock *sweepClock)
	persist        func(b *boot)
	restartCrashed func(ctx context.Context, mgr *agent.Manager) []string
}

func defaultRunLoopDeps() runLoopDeps {
	return runLoopDeps{
		waitCLIStartup: func(ctx context.Context) bool {
			select {
			case <-time.After(cliStartupDelay):
				return true
			case <-ctx.Done():
				return false
			}
		},
		newTicker: func(d time.Duration) loopTicker { return realTicker{time.NewTicker(d)} },
		runEval: func(b *boot, restarted []string) {
			runEvalCycle(b.ctx, b.cfg, b.ghClient, b.gov, b.sched, b.agentMgr, b.dashSrv, b.notifier, b.beadStores,
				b.tokenCollector, b.metricsCollector, b.nousState, &b.lastActionable, b.advisoryStore, b.advisoryIssues, restarted, b.logger)
		},
		runRotation: func(b *boot) {
			runRotationCheck(b.ctx, b.cfg, b.rotationMgr, b.gov, b.agentMgr, b.logger)
		},
		runSweeps: func(b *boot, clock *sweepClock) {
			runAutoMergeSweepIfDue(b.ctx, b.ghClient, b.dashSrv, &clock.autoMerge, b.logger)
			runTaskListSweepIfDue(b.ctx, b.ghClient, b.dashSrv, &clock.taskList, b.logger)
			runDuplicateSweepIfDue(b.ctx, b.cfg, b.ghClient, b.dashSrv, &clock.duplicate, b.logger)
		},
		persist: func(b *boot) {
			persistState(b.agentMgr, b.gov, b.cfg, hiveStatePath, b.logger, b.dashSrv, b.wd)
		},
		restartCrashed: func(ctx context.Context, mgr *agent.Manager) []string { return mgr.CheckAndRestartCrashedAgents(ctx) },
	}
}
