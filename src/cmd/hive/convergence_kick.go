package main

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/notify"
)

// applyConvergenceKickAdmission is the #4247 eval-cycle seam, formalised by
// #4263 into the SINGLE mode applicator every enrolled guard consumes (no
// per-feature booleans). It runs the shared convergence dependency admission
// (the SAME observer + pure evaluator the contributor queue and selectTask
// consume, via dashboard.ConvergenceKickProjectionDetailed) over the issue
// population that is about to be cached for manual kicks and rendered into
// scheduled kicks, and returns the ActionableResult the scheduler must use.
//
// Rollout contract (maintainer requirement, kubestellar/hive#4263):
//
//   - "off" (DEFAULT): return the input untouched before computing anything —
//     no sweep, no decision, no telemetry. The kick path is byte-for-byte the
//     pre-#4247 path; existing v4 hives see ZERO behaviour change.
//   - "shadow": compute the projection ONCE on a fresh sweep, LOG what would
//     have been withheld, record one soak telemetry row, and return the RAW
//     input — observed, never enforced.
//   - "enforce": the SAME projection from the SAME sweep gates ONLY this
//     enrolled internal-dispatch path: the returned result carries the
//     admitted issue projection while PRs, holds, clusters, and per-repo
//     counts stay untouched. The raw actionable population remains
//     authoritative for governor policy, cadence, classification, dashboard
//     status, PR/review dispatch, escalation, and every path not explicitly
//     enrolled — the caller passes the raw result everywhere else.
//
// The (mode, generation) pair is captured ONCE at the start of the pass
// (dashboard.CaptureConvergenceMode); every candidate in this pass is judged
// and attributed under that captured pair even if an owner flips the setting
// concurrently — the next pass picks up the change, so a switch is live
// without a rebuild, restart, queue surgery, or stored-state migration.
//
// Mode TRANSITIONS are logged and notified exactly once per crossing (the
// #4305 probe-notification discipline): never at boot, never once per cycle.
//
// The function never mutates its input. It is nil-safe on every dependency so
// a hive booted without a dashboard (or a test harness) cannot panic here.
func applyConvergenceKickAdmission(cfg *config.Config, dashSrv *dashboard.Server, actionable *github.ActionableResult, notifier *notify.Notifier, logger *slog.Logger) *github.ActionableResult {
	// Capture the pass's (mode, generation) pair — one pair for every
	// candidate in this pass. Without a dashboard there is no tracker (and no
	// projection below would work either), so resolve the mode directly.
	var (
		mode       string
		generation uint64
		changed    bool
		previous   string
	)
	mode = cfg.ConvergenceMode()
	if dashSrv != nil {
		mode, generation, changed, previous = dashSrv.CaptureConvergenceMode(mode)
	}

	if changed {
		// Transition-only: log + notify ONCE on the crossing, whatever the
		// source (runtime settings PUT, external YAML reload, env override).
		if logger != nil {
			logger.Info("convergence rollout mode changed",
				"mode", mode, "previous", previous, "generation", generation)
		}
		if notifier != nil {
			notifier.Send("Convergence mode changed",
				fmt.Sprintf("convergence rollout switched from %q to %q (generation %d) — %s",
					previous, mode, generation, convergenceModeEffect(mode)),
				notify.PriorityDefault)
		}
	}

	if mode == config.ConvergenceModeOff {
		return actionable // off (default): entirely inert
	}
	if dashSrv == nil || actionable == nil || logger == nil {
		return actionable
	}

	enforced := mode == config.ConvergenceModeEnforce
	started := time.Now()
	admitted, withheld, coverage := dashSrv.ConvergenceKickProjectionDetailed(actionable.Issues.Items)
	latency := time.Since(started)

	blocked, unknown := 0, 0
	for _, f := range withheld {
		if f.Decision.Reason == convergence.ReasonWaitingForDependency {
			blocked++
		} else {
			unknown++
		}
		verb := "would be WITHHELD from internal agent kicks (not enforced)"
		if enforced {
			verb = "WITHHELD from internal agent kicks"
		}
		logger.Info("convergence "+mode+": candidate "+verb,
			"repo", f.Issue.Repo,
			"number", f.Issue.Number,
			"reason", f.Decision.Reason,
			"blockers", f.Decision.Blockers,
			"observed_record", f.Decision.ObservedRecord,
			"observed_generation", f.Decision.ObservedGeneration,
		)
	}
	logger.Info("convergence kick projection complete",
		"mode", mode,
		"generation", generation,
		"raw_issues", len(actionable.Issues.Items),
		"admitted", len(admitted),
		"withheld", len(withheld),
		"enforced", enforced,
	)

	// One bounded soak row per enrolled pass (#4263): the fixed-commit
	// comparison facts. Recorded in shadow AND enforce so the same workload
	// can be compared across treatments; commit is stamped by the recorder.
	dashSrv.RecordConvergenceSoak(dashboard.ConvergenceSoakEntry{
		Timestamp:         time.Now().UnixMilli(),
		Mode:              mode,
		Generation:        generation,
		RawIssues:         len(actionable.Issues.Items),
		Admitted:          len(admitted),
		Blocked:           blocked,
		Unknown:           unknown,
		PartialLedger:     coverage.Partial,
		WouldDiffer:       len(withheld) > 0,
		Enforced:          enforced,
		DecisionLatencyMs: latency.Milliseconds(),
	})

	if !enforced {
		return actionable // shadow: exact baseline disposition
	}

	// enforce: gate ONLY the enrolled internal-dispatch path. A shallow copy
	// carries the admitted issue projection; PRs, holds, clusters, per-repo
	// totals, and the raw input itself are untouched.
	gated := *actionable
	gated.Issues.Items = admitted
	gated.Issues.Count = len(admitted)
	return &gated
}

// convergenceModeEffect is the one-line operator explanation attached to the
// transition notification.
func convergenceModeEffect(mode string) string {
	switch mode {
	case config.ConvergenceModeShadow:
		return "decisions are computed and reported only; nothing is enforced"
	case config.ConvergenceModeEnforce:
		return "convergence decisions now gate internal scheduled/cached agent kicks"
	default:
		return "convergence code paths are inert; behavior is the pre-enrollment baseline"
	}
}
