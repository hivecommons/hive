package main

import (
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/retro"
	"github.com/hivecommons/hive/pkg/trajectory"
	"github.com/hivecommons/hive/pkg/watchdog"
)

func (w *spokeWire) wireSpokeLanesAndLoop() {
	// Trajectory-review lane (opt-in): a second-model check that reads each
	// running agent's recent transcript and pauses on goal drift. Built once;
	// runs off the governor tick, gated by its own cadence. If the reviewer
	// cannot be constructed (no LiteLLM endpoint/model), the lane is disabled
	// with a single warning rather than erroring every tick.
	if w.cfg.Governor.Trajectory.IsEnabled() {
		reviewEndpoint, reviewKey, reviewModel := w.cfg.Governor.ResolveReviewer()
		reviewer, terr := trajectory.NewReviewer(trajectory.Config{
			Endpoint:        reviewEndpoint,
			APIKey:          reviewKey,
			Model:           reviewModel,
			TranscriptLines: w.cfg.Governor.Trajectory.TranscriptLines,
		})
		if terr != nil {
			// Enabled but not runnable — surface it as a dashboard alert, not
			// just a log line, so a safety control is never silently inert.
			// (Reconciled below so it also clears when the lane is disabled.)
			w.logger.Warn("trajectory-review lane enabled but not running", "reason", terr.Error())
		} else {
			w.trajLane = trajectory.NewLane(reviewer, w.agentMgr,
				dashboard.NewTrajectorySink(w.dashSrv, w.notifier),
				trajectory.LaneConfig{
					IntervalS:    w.cfg.Governor.Trajectory.IntervalS,
					OnDivergence: w.cfg.Governor.Trajectory.OnDivergence,
					ExemptAgents: w.cfg.Governor.Trajectory.ExemptAgents,
				}, w.logger)
			w.logger.Info("trajectory-review lane enabled",
				"model", reviewModel,
				"interval_s", w.cfg.Governor.Trajectory.IntervalS,
				"on_divergence", w.cfg.Governor.Trajectory.OnDivergence)
		}
	}
	// Clear any legacy "not configured" banner alert persisted by an older
	// build. The half-configured state is shown inline in Settings →
	// General, not in the top banner.
	w.dashSrv.ReconcileTrajectoryAlert(&w.cfg.Governor)

	// Stall-replan lane (Phase 3 planning intelligence): periodically detects
	// approved plans whose sub-tasks have stopped progressing and re-kicks the
	// architect to revise them, bounded by a per-plan replan cap. It runs off the
	// governor tick, gated by its own Due() cadence (no goroutine of its own), and
	// drives the architect only through SendKick (agentKicker) from this tick —
	// never from the agent-launch path — so it cannot touch the manager lock
	// unsafely. On by default; a no-op when there are no approved plans.
	if w.cfg.Governor.Replan.IsEnabled() {
		rc := w.cfg.Governor.Replan
		w.replanLane = planning.NewReplanLane(
			w.beadStores,
			agentKicker{mgr: w.agentMgr},
			w.gov,
			dashboard.NewReplanSink(w.dashSrv, w.notifier),
			planning.ReplanLaneConfig{
				IntervalS: rc.IntervalS,
				Stall: planning.StallConfig{
					StallThreshold: time.Duration(rc.StallThresholdS) * time.Second,
					MaxReplans:     rc.MaxReplans,
				},
			}, w.logger)
		w.logger.Info("stall-replan lane enabled",
			"interval_s", rc.IntervalS,
			"stall_threshold_s", rc.StallThresholdS,
			"max_replans", rc.MaxReplans)
	}
	if w.cfg.Retro.Enabled {
		retroStore := w.beadStores[retro.Actor]
		escalationStoreOnce.Do(func() {
			escalationStore = escalation.Load(escalationLedgerPath)
		})
		w.retroLane = retro.NewLane(w.beadStores, retroStore, w.dashSrv.LifecycleTimeline(), escalationStore, retro.Config{
			Enabled:             w.cfg.Retro.Enabled,
			ScanIntervalS:       w.cfg.Retro.ScanIntervalS,
			MaxFixAttempts:      w.cfg.Retro.MaxFixAttempts,
			MaxKicks:            w.cfg.Retro.MaxKicks,
			LongStallDays:       w.cfg.Retro.LongStallDays,
			RecentClosedWindowS: w.cfg.Retro.RecentClosedWindowS,
			AnalysisModel:       w.cfg.Retro.AnalysisModel,
			AnalysisEndpoint:    w.cfg.Governor.LiteLLM.ResolveEndpoint(),
			AnalysisAPIKey:      w.cfg.Governor.LiteLLM.ResolveAPIKey(),
		}, w.logger)
		if w.knowledgeAPI != nil {
			w.retroLane.SetKnowledgeSink(w.knowledgeAPI)
		}
		w.logger.Info("retro lane enabled",
			"scan_interval_s", w.cfg.Retro.ScanIntervalS,
			"max_fix_attempts", w.cfg.Retro.MaxFixAttempts,
			"max_kicks", w.cfg.Retro.MaxKicks,
			"long_stall_days", w.cfg.Retro.LongStallDays,
			"analysis_enabled", w.cfg.Retro.AnalysisModel != "")
	}

	w.logger.Info("entering governor loop", "interval_seconds", w.cfg.Governor.EvalIntervalS)
	lastEvalInterval := w.cfg.Governor.EvalIntervalS
	ticker := time.NewTicker(time.Duration(w.cfg.Governor.EvalIntervalS) * time.Second)
	defer ticker.Stop()

	var agentTicker *time.Ticker
	if w.cfg.Dashboard.AgentPollIntervalS > 0 {
		agentTicker = time.NewTicker(time.Duration(w.cfg.Dashboard.AgentPollIntervalS) * time.Second)
		defer agentTicker.Stop()
		w.logger.Info("fast agent status enabled", "interval_seconds", w.cfg.Dashboard.AgentPollIntervalS)
	}

	// NOTE: w.dashSrv.MarkReady() was previously HERE, after the staggered agent
	// launch and the heartbeat/trajectory/ticker setup. It has been moved to
	// immediately after the HTTP listener starts (before the agent-launch loop),
	// so the pod becomes Ready in seconds instead of minutes. See the MarkReady
	// call and comment above the agent-launch goroutine.

	const cliStartupDelay = 10 * time.Second
	w.logger.Info("waiting for CLI startup before first eval", "delay", cliStartupDelay)
	select {
	case <-time.After(cliStartupDelay):
	case <-w.ctx.Done():
		return
	}

	// #2573: startup must NOT clear persisted last-kick timestamps. It used to
	// (w.gov.ClearLastKicks) so that every eligible agent was kicked on the first
	// eval — "one kick per agent per pod boot, by design". But on hosted hives
	// the hub rolls the Deployment for every auto-upgrade, and each roll is a
	// brand-new pod with container restart count 0, so agents on 4h/6h cadences
	// were re-kicked at roll frequency — burning backend tokens ("Bob coins")
	// far beyond any configured cadence, while "next run" (recomputed from the
	// wiped timestamps) showed sooner than the cadence implied. LastKick state
	// is persisted to /data (PVC) after every eval and restored above via
	// SeedLastKicks, so this first eval kicks exactly the agents whose cadence
	// has actually elapsed — including everything after downtime longer than an
	// interval. A hive with no persisted state (fresh install) has no LastKick
	// entries, and every cadenced agent is still kicked here, unchanged.
	w.logger.Info("startup honors persisted cadence state — first eval kicks only agents whose cadence has elapsed")
	runEvalCycle(w.ctx, w.cfg, w.ghClient, w.gov, w.sched, w.agentMgr, w.dashSrv, w.notifier, w.beadStores, w.tokenCollector, w.metricsCollector, w.nousState, &w.lastActionable, w.advisoryStore, w.advisoryIssues, nil, w.approvalDesk, w.logger)
	runRotationCheck(w.ctx, w.cfg, w.rotationMgr, w.gov, w.agentMgr, w.logger)
	if w.wd != nil {
		w.wd.Tick(w.ctx)
	}
	runAutoMergeSweepIfDue(w.ctx, w.ghClient, w.cfg, w.dashSrv, &w.lastAutoMergeSweep, w.logger)
	persistState(w.agentMgr, w.gov, w.cfg, spokeStatePath, w.logger, w.dashSrv, w.wd)

	agentTickCh := func() <-chan time.Time {
		if agentTicker != nil {
			return agentTicker.C
		}
		return nil
	}()

	for {
		select {
		case <-w.ctx.Done():
			w.logger.Info("shutting down, persisting state")
			persistState(w.agentMgr, w.gov, w.cfg, spokeStatePath, w.logger, w.dashSrv, w.wd)
			return
		case <-ticker.C:
			restarted := w.agentMgr.CheckAndRestartCrashedAgents(w.ctx)
			for _, name := range restarted {
				w.dashSrv.AuditLog("system", "restart", "trigger=crash-recovery", name)
			}
			// If brainstorm crashed during inception, re-kick via SendKick.
			// SendKick waits for the CLI to be ready and sends the message
			// If brainstorm crashed during inception, re-kick with bootstrap.
			// The table parser in the watcher will catch questions from the
			// agent's output even if bd create doesn't execute.
			for _, name := range restarted {
				if name == "brainstorm" && w.inceptionEngine != nil {
					if state := w.inceptionEngine.GetState(); state != nil && state.Phase == knowledge.PhaseCapture {
						msg := w.sched.BuildAgentMessage("brainstorm", nil, w.sched.GetLastActionable())
						if err := w.agentMgr.RestartWithBootstrap(w.ctx, "brainstorm", msg); err != nil {
							w.logger.Warn("inception re-kick after crash failed", "error", err)
						} else {
							w.logger.Info("brainstorm re-kicked after crash", "phase", state.Phase)
							w.dashSrv.AuditLog("system", "kick", "trigger=inception-crash-recovery", "brainstorm")
						}
						w.gov.RecordKick("brainstorm")
					}
				}
			}
			// Watchdog sweep (RFC #4665): synchronous but bounded — every
			// probe carries a deadline and restarts run detached under a hard
			// timeout, so a wedged agent can never stall this tick. Tick
			// self-gates to watchdog.probe_interval_s.
			//
			// It runs BEFORE runEvalCycle so agents it revived join this
			// cycle's resume-kick list rather than waiting a full eval
			// interval. Restarts are detached, so a given sweep's completions
			// are usually collected on the next pass — TakeRestarted drains
			// whatever has finished, and the governor gate gets the final say
			// either way.
			if w.wd != nil {
				// Re-resolve the mode each sweep so a change w.saved from the
				// dashboard (or the fleet-wide kill switch being engaged)
				// takes effect without a restart — and so dead-session
				// ownership moves with it. Without this, leaving heal via the
				// settings page would stop the watchdog restarting while the
				// manager's crash loop was still standing down: a window in
				// which NEITHER recovers a dead agent.
				if s, errs := watchdog.SettingsFrom(w.cfg.Governor.Watchdog); s.Mode != w.wd.Mode() {
					for _, e := range errs {
						w.logger.Warn("watchdog config problem", "error", e)
					}
					w.logger.Info("watchdog mode changed", "from", string(w.wd.Mode()), "to", string(s.Mode))
					w.dashSrv.AuditLog("system", "watchdog-mode", "from="+string(w.wd.Mode())+", to="+string(s.Mode), "")
					w.wd.SetSettings(s)
					w.agentMgr.SetDeadSessionRecoveryOwner(s.MayAct())
				}
				w.wd.Tick(w.ctx)
				for _, name := range w.wd.TakeRestarted() {
					w.dashSrv.AuditLog("system", "restart", "trigger=watchdog", name)
					restarted = append(restarted, name)
				}
			}
			runEvalCycle(w.ctx, w.cfg, w.ghClient, w.gov, w.sched, w.agentMgr, w.dashSrv, w.notifier, w.beadStores, w.tokenCollector, w.metricsCollector, w.nousState, &w.lastActionable, w.advisoryStore, w.advisoryIssues, restarted, w.approvalDesk, w.logger)
			runRotationCheck(w.ctx, w.cfg, w.rotationMgr, w.gov, w.agentMgr, w.logger)
			runAutoMergeSweepIfDue(w.ctx, w.ghClient, w.cfg, w.dashSrv, &w.lastAutoMergeSweep, w.logger)
			// Trajectory review runs after the eval cycle (so kicks/intents are
			// current) on its own cadence, gated by Due().
			if w.trajLane != nil && w.trajLane.Due(time.Now()) {
				w.trajLane.Run(w.ctx)
			}
			// Stall-replan runs on the same tick, gated by its own Due() cadence.
			// It is synchronous and adds no goroutine; kicks go through the same
			// out-of-band SendKick path as the eval cycle above.
			if w.replanLane != nil && w.replanLane.Due(time.Now()) {
				if n := w.replanLane.Run(w.ctx); n > 0 {
					w.logger.Info("stall-replan lane re-kicked stalled plans", "replans", n)
				}
			}
			if w.retroLane != nil && w.retroLane.Due(time.Now()) {
				if n := w.retroLane.Run(w.ctx); n > 0 {
					w.logger.Info("retro lane filed advisory beads", "findings", n)
				}
			}
			persistState(w.agentMgr, w.gov, w.cfg, spokeStatePath, w.logger, w.dashSrv, w.wd)
			if w.cfg.Governor.EvalIntervalS != lastEvalInterval && w.cfg.Governor.EvalIntervalS > 0 {
				w.logger.Info("eval interval changed, resetting ticker",
					"from", lastEvalInterval, "to", w.cfg.Governor.EvalIntervalS)
				ticker.Reset(time.Duration(w.cfg.Governor.EvalIntervalS) * time.Second)
				lastEvalInterval = w.cfg.Governor.EvalIntervalS
			}
		case <-agentTickCh:
			govState := w.gov.GetState()
			agentStatuses := w.agentMgr.AllStatuses()
			payload := dashboard.BuildAgentOnlyStatus(govState, agentStatuses, w.cfg)
			w.dashSrv.BroadcastAgentStatus(payload)
		}
	}

}
