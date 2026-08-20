package main

import (
	"log/slog"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/dashboard"
	"github.com/kubestellar/hive/pkg/github"
)

// observeConvergenceKickAdmission is the #4247 eval-cycle seam: it runs the
// shared convergence dependency admission (the SAME observer + pure evaluator
// the contributor queue and selectTask consume, via
// dashboard.ConvergenceKickProjection) over the issue population that is about
// to be cached for manual kicks and rendered into scheduled kicks.
//
// Rollout contract (maintainer requirement, ahead of #4263):
//
//   - convergence.mode "off" (DEFAULT): return before touching anything — no
//     sweep is built, no decision is computed, the kick path is byte-for-byte
//     the pre-#4247 path. Zero behaviour change for existing v4 hives.
//   - convergence.mode "shadow": compute the projection ONCE on a fresh sweep
//     and LOG which candidates the shared evaluator would have withheld from
//     internal kicks, with the decision's stable reason, blockers, and observed
//     record/generation. NOTHING is enforced: the caller always passes the raw
//     actionable result to Scheduler.SetLastActionable and BuildKickMessages,
//     so scheduled kicks, cached manual/dashboard kicks, governor counts,
//     prompts, ordering, and PR/review dispatch are all unchanged.
//
// The function never mutates actionable. It is nil-safe on every input so a
// hive booted without a dashboard (or a test harness) cannot panic here.
func observeConvergenceKickAdmission(cfg *config.Config, dashSrv *dashboard.Server, actionable *github.ActionableResult, logger *slog.Logger) {
	if cfg.ConvergenceMode() != config.ConvergenceModeShadow {
		return // off (default): entirely inert
	}
	if dashSrv == nil || actionable == nil || logger == nil {
		return
	}
	admitted, withheld := dashSrv.ConvergenceKickProjection(actionable.Issues.Items)
	if len(withheld) == 0 {
		logger.Debug("convergence shadow: every enumerated issue is admitted for internal kicks",
			"issues", len(actionable.Issues.Items))
		return
	}
	for _, f := range withheld {
		logger.Info("convergence shadow: candidate would be WITHHELD from internal agent kicks (not enforced)",
			"repo", f.Issue.Repo,
			"number", f.Issue.Number,
			"reason", f.Decision.Reason,
			"blockers", f.Decision.Blockers,
			"observed_record", f.Decision.ObservedRecord,
			"observed_generation", f.Decision.ObservedGeneration,
		)
	}
	logger.Info("convergence shadow kick projection complete",
		"raw_issues", len(actionable.Issues.Items),
		"admitted", len(admitted),
		"withheld", len(withheld),
		"enforced", false,
	)
}
