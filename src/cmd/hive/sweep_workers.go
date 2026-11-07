package main

import (
	"context"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/rfcnotify"
)

// publishSweepConfig is called with cfgReloadMu held (or before the loop
// starts). Workers never read the mutable live config or wait behind eval IO.
func (b *boot) publishSweepConfig() {
	if b.cfg == nil {
		b.sweepConfig.Store(nil)
		return
	}
	cfg, err := b.cfg.Snapshot()
	if err != nil {
		b.logger.Error("cannot snapshot sweep configuration", "error", err)
		// Fail closed instead of continuing with an old write policy.
		b.sweepConfig.Store(nil)
		return
	}
	b.sweepConfig.Store(cfg)
}

type sweepClass int

const (
	sweepEssential sweepClass = iota
	sweepUnstick
	sweepOptional
)

type sweepJob struct {
	name     string
	interval func(*config.Config) time.Duration
	class    sweepClass
	run      func(context.Context, *github.Client, *config.Config)
}

// runTagged runs the job with its GitHub REST calls attributed to
// "hive:<name>" in /api/gh-rate-limits. Calls a sweep already tags more
// narrowly keep their own name; before this, every untagged call from every
// sweep was reported as plain "hive".
func (j sweepJob) runTagged(ctx context.Context, gh *github.Client, cfg *config.Config) {
	j.run(github.WithRESTCaller(ctx, "hive:"+j.name), gh, cfg)
}

// sweepStartupSpread is the window the non-essential sweeps' first passes are
// spread across after a start. All workers start in the same second; on a
// cold ETag cache their first passes each listed every repo at once, all
// judged against one API budget reading, and a restart spent much of the
// hourly quota in minutes (2026-11-06). Spread out, later passes revalidate
// lists earlier ones fetched (a 304 is free) and see the budget they left.
const sweepStartupSpread = 5 * time.Minute

// sweepFirstDelays is each job's first-pass offset after start: zero for
// essential sweeps, evenly spaced inside sweepStartupSpread for the rest.
// Only the first pass moves; later passes keep each job's interval.
func sweepFirstDelays(jobs []sweepJob) []time.Duration {
	delays := make([]time.Duration, len(jobs))
	spread := 0
	for _, job := range jobs {
		if job.class != sweepEssential {
			spread++
		}
	}
	k := 0
	for i, job := range jobs {
		if job.class == sweepEssential {
			continue
		}
		k++
		delays[i] = sweepStartupSpread * time.Duration(k) / time.Duration(spread+1)
	}
	return delays
}

// runSweepWorker owns one job's state and timer. Its synchronous callback is
// the single-flight: there can never be two passes of this sweep in flight.
// Reset the heartbeat after IO so slow passes do not queue timer events.
// The short heartbeat also observes shortened intervals after config reload.
// The first pass waits firstDelay after start, never longer than one interval.
func runSweepWorker(ctx context.Context, job sweepJob, firstDelay time.Duration, snapshot func() *config.Config, newTicker func(time.Duration) loopTicker, now func() time.Time, fire func(*config.Config, uint64)) {
	ticker := newTicker(time.Second)
	defer ticker.Stop()
	started := now()
	var last time.Time
	var attempts uint64
	for {
		if ctx.Err() != nil {
			return
		}
		if cfg := snapshot(); cfg != nil {
			at := now()
			interval := job.interval(cfg)
			if last.IsZero() && at.Sub(started) >= min(firstDelay, interval) || !last.IsZero() && at.Sub(last) >= interval {
				last = at
				attempts++
				fire(cfg, attempts)
				ticker.Reset(time.Second)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
		}
	}
}

func (b *boot) sweepJobs() []sweepJob {
	fixed := func(d time.Duration) func(*config.Config) time.Duration {
		return func(*config.Config) time.Duration { return d }
	}
	baseInterval := func(cfg *config.Config) time.Duration {
		if cfg.Governor.EvalIntervalS <= 0 {
			return 5 * time.Minute
		}
		return time.Duration(cfg.Governor.EvalIntervalS) * time.Second
	}
	// Initialize shared hook state before any worker starts.
	gfi, first, coverage, notify := b.gfiState(), b.firstTimerState(), b.rfcCoverageState(), b.rfcNotifyState()
	var unparkLast time.Time
	return []sweepJob{
		{"automerge", fixed(autoMergeSweepInterval), sweepEssential, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			// Workers start before the first eval finishes. A nil snapshot
			// means the freshness guard would be absent, not fail closed.
			last := b.lastActionable.Load()
			if last == nil {
				return
			}
			ctx = github.WithIrreversibleGuard(ctx, gh.SnapshotFreshnessGuard(last, time.Now))
			runAutoMergeSweepIfDue(ctx, gh, cfg, b.dashSrv, nil, b.logger)
		}},
		{"supersession", fixed(supersessionSweepInterval), sweepUnstick, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runSupersessionSweepIfDue(ctx, gh, cfg, b.dashSrv, nil, b.logger)
		}},
		{"duplicate_fix", fixed(duplicateFixSweepInterval), sweepUnstick, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runDuplicateFixSweepIfDue(ctx, gh, cfg, b.dashSrv, nil, b.logger)
		}},
		{"shared_ci_rerun", fixed(sharedCIRerunInterval), sweepUnstick, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runSharedCIRerunSweepIfDue(ctx, gh, cfg, b.dashSrv, nil, b.logger)
		}},
		{"red_main", fixed(redMainSweepInterval), sweepUnstick, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runRedMainSweepIfDue(ctx, gh, nil, b.logger)
		}},
		{"blocked_issues", fixed(blockedIssuesSweepInterval), sweepUnstick, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runBlockedIssuesSweepIfDue(ctx, gh, cfg, b.dashSrv, nil, b.logger)
		}},
		{"task_list", fixed(taskListSweepInterval), sweepOptional, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runTaskListSweepIfDue(ctx, gh, b.dashSrv, nil, b.logger)
		}},
		{"duplicate", fixed(duplicateSweepInterval), sweepOptional, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runDuplicateSweepIfDue(ctx, cfg, gh, b.dashSrv, nil, b.logger)
		}},
		// Keep the webhook-aware minimum/reconcile gates inside the unpark pass.
		{"issue_unpark", fixed(issueUnparkSweepWebhookInterval), sweepOptional, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runIssueUnparkSweepIfDue(ctx, gh, cfg, b.dashSrv, &unparkLast, b.logger)
		}},
		{"closed_issue_stage", fixed(closedIssueStageSweepInterval), sweepOptional, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runClosedIssueStageSweepIfDue(ctx, gh, b.dashSrv, nil, b.logger)
		}},
		{"clarity_resume", fixed(clarityGateResumeSweepInterval), sweepOptional, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runClarityGateResumeSweepIfDue(ctx, gh, cfg, b.dashSrv, nil, b.logger)
		}},
		{"prior_art_resume", fixed(priorArtGateResumeSweepInterval), sweepOptional, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runPriorArtGateResumeSweepIfDue(ctx, gh, cfg, b.dashSrv, nil, b.logger)
		}},
		{"sentinel", fixed(sentinelSweepInterval), sweepOptional, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runSentinelSweepIfDue(ctx, gh, cfg, b.dashSrv, nil, b.logger)
		}},
		{"legacy_fleet_report", fixed(legacyFleetReportSweepInterval), sweepOptional, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runLegacyFleetReportSweepIfDue(ctx, gh, cfg, b.dashSrv, nil, b.logger)
		}},
		{"good_first_issue", func(cfg *config.Config) time.Duration { return cfg.GoodFirstIssue.IntervalOrDefault() }, sweepOptional, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runGoodFirstIssueSweepIfDue(ctx, gh, cfg, b.dashSrv, gfi, b.lastActionable.Load(), nil, b.logger)
		}},
		{"first_timer", baseInterval, sweepOptional, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runFirstTimerWelcome(ctx, gh, cfg, b.dashSrv, first, b.firstTimerKnownContributor, b.lastActionable.Load(), b.logger)
		}},
		{"rfc_existing", baseInterval, sweepOptional, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runRFCExistingCheck(ctx, newRFCCoverageForge(gh), cfg, b.dashSrv, coverage, b.lastActionable.Load(), b.logger)
		}},
		{"rfc_notify", fixed(rfcnotify.DefaultInterval + time.Second), sweepOptional, func(ctx context.Context, gh *github.Client, cfg *config.Config) {
			runRFCNotifySweep(ctx, newRFCNotifySource(gh, notify), len(cfg.Project.Repos), cfg, b.dashSrv, notify, b.logger)
		}},
	}
}

func sweepBudgetSkipped(class sweepClass, decision evalBudgetDecision) bool {
	return class == sweepUnstick && decision.SkipUnstick || class == sweepOptional && decision.SkipOptional
}

func (b *boot) startSweepWorkers() func() {
	ctx, cancel := context.WithCancel(b.ctx)
	var wg sync.WaitGroup
	jobs := b.sweepJobs()
	firstDelays := sweepFirstDelays(jobs)
	for i, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSweepWorker(ctx, job, firstDelays[i], b.sweepConfig.Load, func(d time.Duration) loopTicker { return realTicker{time.NewTicker(d)} }, time.Now, func(cfg *config.Config, attempt uint64) {
				gh := b.currentGitHubClient()
				if gh == nil {
					return
				}
				decision := decideEvalBudgetWork(apiBudgetModeForClient(gh), cfg.Governor.OptionalSweepEveryNCycles, attempt)
				skipped := sweepBudgetSkipped(job.class, decision)
				if skipped {
					gh.RecordAPIBudgetSkippedSteps([]string{job.name})
				}
				started := time.Now()
				if b.dashSrv != nil {
					b.dashSrv.RecordSweepCadence(job.name, started, job.interval(cfg), !skipped, !skipped)
				}
				if skipped {
					return
				}
				job.runTagged(ctx, gh, cfg)
				if b.dashSrv != nil {
					b.dashSrv.RecordSweepCadence(job.name, started, job.interval(cfg), true, false)
				}
				b.logger.Info("governor sweep complete", "sweep", job.name, "wall_s", time.Since(started).Seconds())
			})
		}()
	}
	return func() { cancel(); wg.Wait() }
}
