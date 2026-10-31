package main

import (
	"context"
	"time"

	"github.com/hivecommons/hive/crux"

	"github.com/hivecommons/hive/pkg/github"
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

// realTimer adapts a one-shot *time.Timer to loopTicker for the continuous
// re-kick wake.
type realTimer struct{ *time.Timer }

func (t realTimer) Chan() <-chan time.Time { return t.C }
func (t realTimer) Stop()                  { t.Timer.Stop() }
func (t realTimer) Reset(d time.Duration)  { t.Timer.Reset(d) }

// runLoopDeps are runLoop's effects (#7571, step 2): the two timers, the CLI
// startup wait, and the per-tick work units (eval cycle, rotation check,
// GitHub sweeps — whose last-run stamps live on *boot — state persist, crash
// recovery) — each of which does GitHub
// or disk IO or spawns agent processes. The loop shape, watchdog reconcile,
// lane gating, ticker reset, and agent-status broadcast run for real.
type runLoopDeps struct {
	waitCLIStartup func(ctx context.Context) bool
	newTicker      func(d time.Duration) loopTicker
	// newWakeTimer arms the continuous re-kick wake (one-shot). nil disables
	// the wake, which is what the loop tests want unless they drive it.
	newWakeTimer func(d time.Duration) loopTicker
	// newHealthTimer arms the webhook-health re-check (one-shot) that flips
	// the base tick back to eval_interval_s when webhooks lapse mid-interval.
	// nil disables it.
	newHealthTimer func(d time.Duration) loopTicker
	// now is the wake's clock (nil = time.Now); tests inject it to step past
	// continuousWakeMinGap and the stall hold without sleeping.
	now            func() time.Time
	runEval        func(b *boot, restarted []string, source evalCycleSource)
	runRotation    func(b *boot)
	runSweeps      func(b *boot)
	persist        func(b *boot)
	restartCrashed func(ctx context.Context, mgr crux.Runtime) []string
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
		newTicker:      func(d time.Duration) loopTicker { return realTicker{time.NewTicker(d)} },
		newWakeTimer:   func(d time.Duration) loopTicker { return realTimer{time.NewTimer(d)} },
		newHealthTimer: func(d time.Duration) loopTicker { return realTimer{time.NewTimer(d)} },
		runEval: func(b *boot, restarted []string, source evalCycleSource) {
			if b.evalLoop != nil {
				b.evalLoop.noteCycle(source)
			}
			runEvalCycle(b.ctx, b.cfg, b.ghClient, b.gov, b.sched, b.agentMgr, b.dashSrv, b.notifier, b.beadStores,
				b.tokenCollector, b.metricsCollector, b.nousState, &b.lastActionable, b.advisoryStore, b.advisoryIssues, restarted, b.approvalDesk, source, b.logger)
		},
		runRotation: func(b *boot) {
			runRotationCheck(b.ctx, b.cfg, b.rotationMgr, b.gov, b.agentMgr, b.logger)
		},
		runSweeps: func(b *boot) {
			// Merges are irreversible: bound them to the snapshot the cycle
			// planned from (refused while it is stale, re-read when old).
			sweepCtx := github.WithIrreversibleGuard(b.ctx, b.ghClient.SnapshotFreshnessGuard(b.lastActionable.Load(), time.Now))
			runAutoMergeSweepIfDue(sweepCtx, b.ghClient, b.cfg, b.dashSrv, &b.lastAutoMergeSweep, b.logger)
			decision := decideEvalBudgetWork(apiBudgetModeForClient(b.ghClient), b.cfg.Governor.OptionalSweepEveryNCycles, b.evalCycles)
			if len(decision.SkippedSteps) > 0 {
				b.ghClient.RecordAPIBudgetSkippedSteps(decision.SkippedSteps)
			}
			if !decision.SkipUnstick {
				runSupersessionSweepIfDue(b.ctx, b.ghClient, b.cfg, b.dashSrv, &b.lastSupersessionSweep, b.logger)
				runDuplicateFixSweepIfDue(b.ctx, b.ghClient, b.cfg, b.dashSrv, &b.lastDuplicateFixSweep, b.logger)
				runSharedCIRerunSweepIfDue(b.ctx, b.ghClient, b.cfg, b.dashSrv, &b.lastSharedCIRerunSweep, b.logger)
				runBlockedIssuesSweepIfDue(b.ctx, b.ghClient, b.cfg, b.dashSrv, &b.lastBlockedIssuesSweep, b.logger)
			}
			if decision.SkipOptional {
				return
			}
			runTaskListSweepIfDue(b.ctx, b.ghClient, b.dashSrv, &b.lastTaskListSweep, b.logger)
			runDuplicateSweepIfDue(b.ctx, b.cfg, b.ghClient, b.dashSrv, &b.lastDuplicateSweep, b.logger)
			runIssueUnparkSweepIfDue(b.ctx, b.ghClient, b.cfg, b.dashSrv, &b.lastIssueUnparkSweep, b.logger)
			runClosedIssueStageSweepIfDue(b.ctx, b.ghClient, b.dashSrv, &b.lastClosedIssueSweep, b.logger)
			runClarityGateResumeSweepIfDue(b.ctx, b.ghClient, b.cfg, b.dashSrv, &b.lastClarityResumeSweep, b.logger)
			runPriorArtGateResumeSweepIfDue(b.ctx, b.ghClient, b.cfg, b.dashSrv, &b.lastPriorArtResumeSweep, b.logger)
			runSentinelSweepIfDue(b.ctx, b.ghClient, b.cfg, b.dashSrv, &b.lastSentinelSweep, b.logger)
			runLegacyFleetReportSweepIfDue(b.ctx, b.ghClient, b.cfg, b.dashSrv, &b.lastLegacyFleetReportSweep, b.logger)
			runGoodFirstIssueSweepIfDue(b.ctx, b.ghClient, b.cfg, b.dashSrv, b.gfiState(), b.lastActionable.Load(), &b.lastGFISweep, b.logger)
			runFirstTimerWelcome(b.ctx, b.ghClient, b.cfg, b.dashSrv, b.firstTimerState(), b.firstTimerKnownContributor, b.lastActionable.Load(), b.logger)
			runRFCExistingCheck(b.ctx, newRFCCoverageForge(b.ghClient), b.cfg, b.dashSrv, b.rfcCoverageState(), b.lastActionable.Load(), b.logger)
			runRFCNotifySweep(b.ctx, newRFCNotifySource(b.ghClient, b.rfcNotifyState()), len(b.cfg.Project.Repos), b.cfg, b.dashSrv, b.rfcNotifyState(), b.logger)
		},
		persist: func(b *boot) {
			persistState(b.agentMgr, b.gov, b.cfg, spokeStatePath, b.logger, b.dashSrv, b.wd)
		},
		restartCrashed: func(ctx context.Context, mgr crux.Runtime) []string { return mgr.CheckAndRestartCrashedAgents(ctx) },
	}
}
