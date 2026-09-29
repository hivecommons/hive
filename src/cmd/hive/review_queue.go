package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// stampReviewQueue ranks this cycle's enumerated PRs into the PR review
// queue (hivecommons/hive#9590) and stamps each PR's position, priority and
// reasons onto the snapshot. A missing verdict artifact is normal (no review
// has been recorded yet) and ranks every PR as unreviewed; an unreadable one
// is logged and does the same, since a queue without confidence is still a
// better order than none.
func stampReviewQueue(cfg *config.Config, actionable *github.ActionableResult, logger *slog.Logger) []github.ReviewQueueEntry {
	if cfg == nil || actionable == nil {
		return nil
	}
	artifact, err := review.LoadArtifact("")
	if err != nil {
		if !os.IsNotExist(err) && logger != nil {
			logger.Warn("review verdict artifact unavailable for review queue ranking; ranking every PR as unreviewed", "error", err)
		}
		artifact = review.Artifact{}
	}
	return github.StampReviewQueue(actionable, github.ReviewQueueOptions{
		Org:          cfg.Project.Org,
		AIAuthor:     cfg.EffectiveAIAuthor(),
		Verdicts:     artifact,
		ChangedPaths: github.CachedPRChangedPaths,
		Now:          time.Now(),
	})
}

// reviewPriorityLabeler is the slice of *github.Client the label mirror
// needs, so the mirror can be tested without GitHub.
type reviewPriorityLabeler interface {
	// ApplyHumanDecisionLabel applies an EXISTING repo label and refuses to
	// create one; despite its name it is label-agnostic, and the priority
	// labels follow the same never-create contract as the human decision
	// label.
	ApplyHumanDecisionLabel(ctx context.Context, repo string, number int, label string) error
	RemoveLabel(ctx context.Context, repo string, number int, label string) error
}

// applyReviewPriorityLabels mirrors the review queue's rank onto one
// review-priority/* label per PR when review.priority_labels is on (default
// off). The hive applies it from the computed rank; the authoring agent never
// does. Only PRs whose labels are out of step are touched, at most
// github.DefaultReviewPriorityLabelMaxChanges per cycle, so the steady state
// costs no API calls. A failure on one PR is logged and skipped: a missing
// label must degrade to "unlabelled", never block the cycle.
func applyReviewPriorityLabels(ctx context.Context, cfg *config.Config, labeler reviewPriorityLabeler, queue []github.ReviewQueueEntry, logger *slog.Logger) {
	if cfg == nil || labeler == nil || !cfg.Review.PriorityLabels || len(queue) == 0 {
		return
	}
	for _, ch := range github.PlanReviewPriorityLabels(queue, github.DefaultReviewPriorityLabelMaxChanges) {
		if ch.Add != "" {
			if err := labeler.ApplyHumanDecisionLabel(ctx, ch.Repo, ch.Number, ch.Add); err != nil {
				if logger != nil {
					logger.Warn("review priority label not applied", "repo", ch.Repo, "pr", ch.Number, "label", ch.Add, "error", err)
				}
			} else if logger != nil {
				logger.Info("review priority label applied", "repo", ch.Repo, "pr", ch.Number, "label", ch.Add)
			}
		}
		// Stale priority labels are removed even when the new one could not
		// be applied: no label is less misleading than a wrong one.
		for _, stale := range ch.Remove {
			if err := labeler.RemoveLabel(ctx, ch.Repo, ch.Number, stale); err != nil && logger != nil {
				logger.Warn("stale review priority label not removed", "repo", ch.Repo, "pr", ch.Number, "label", stale, "error", err)
			}
		}
	}
}
