package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// reviewVerdictLabelMaxChanges bounds verdict-label writes per cycle, so a
// hive turning the feature on for a deep queue converges over a few cycles
// instead of spending one burst of API calls.
const reviewVerdictLabelMaxChanges = 25

// applyReviewVerdictLabels mirrors each open PR's review state onto one
// review.verdict_labels label (hivecommons/hive#13833): reviewing while the
// current head is under review, then its settled verdict. It runs after the
// cycle's dispatch plan, so a head kicked this cycle already reads as
// reviewing. Only PRs whose labels are out of step are touched. Labels are
// never created (the same contract as human_decision_label and
// priority_labels): a missing label is logged and skipped, and a stale label
// is removed even when the new one could not be applied.
func applyReviewVerdictLabels(ctx context.Context, cfg *config.Config, labeler reviewPriorityLabeler, actionable *github.ActionableResult, plan review.DispatchPlan, logger *slog.Logger) {
	if cfg == nil || labeler == nil || actionable == nil || !cfg.Review.VerdictLabels.Enabled() {
		return
	}
	set := reviewVerdictLabelSet(cfg.Review.VerdictLabels)
	artifact, err := review.LoadArtifact("")
	if err != nil && !os.IsNotExist(err) {
		if logger != nil {
			logger.Warn("review verdict labels: verdict artifact unavailable; labels left as they are", "error", err)
		}
		return
	}
	prs := verdictLabelPRs(cfg, actionable)
	for _, ch := range review.PlanVerdictLabels(set, prs, artifact, plan.State, reviewVerdictLabelMaxChanges) {
		if ch.Add != "" {
			if err := labeler.ApplyHumanDecisionLabel(ctx, ch.Repo, ch.Number, ch.Add); err != nil {
				if logger != nil {
					logger.Warn("review verdict label not applied", "repo", ch.Repo, "pr", ch.Number, "label", ch.Add, "error", err)
				}
			} else if logger != nil {
				logger.Info("review verdict label applied", "repo", ch.Repo, "pr", ch.Number, "label", ch.Add)
			}
		}
		for _, stale := range ch.Remove {
			if err := labeler.RemoveLabel(ctx, ch.Repo, ch.Number, stale); err != nil && logger != nil {
				logger.Warn("stale review verdict label not removed", "repo", ch.Repo, "pr", ch.Number, "label", stale, "error", err)
			}
		}
	}
}

func reviewVerdictLabelSet(v config.ReviewVerdictLabelsConfig) review.VerdictLabelSet {
	return review.VerdictLabelSet{
		Reviewing:        v.Reviewing,
		Approve:          v.Approve,
		ChangesRequested: v.ChangesRequested,
		RequiresHuman:    v.RequiresHuman,
	}
}

// verdictLabelPRs is the cycle's open PRs (actionable and held) with the
// labels the enumeration already read, in the planner's PullRequest shape.
func verdictLabelPRs(cfg *config.Config, actionable *github.ActionableResult) []review.PullRequest {
	seen := map[string]bool{}
	var out []review.PullRequest
	add := func(items []github.PullRequest) {
		for _, pr := range items {
			key := github.ReviewQueueKey(fullRepoName(pr.Repo, cfg.Project.Org), pr.Number)
			if pr.Number <= 0 || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, review.PullRequest{
				Repo:    pr.Repo,
				Number:  pr.Number,
				HeadSHA: pr.HeadSHA,
				Labels:  append([]string(nil), pr.Labels...),
			})
		}
	}
	add(actionable.PRs.Items)
	add(actionable.PRs.Held)
	return out
}
