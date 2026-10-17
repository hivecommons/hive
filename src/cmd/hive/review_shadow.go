package main

import (
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
)

// reviewShadowGate evaluates the review-approval gate the way
// require_approval would, without acting on it (review.shadow). It returns
// nil when shadow is off. It must never influence the merge bucket.
func reviewShadowGate(g mergeGates, fullRepo string, pr github.PullRequest, now time.Time) *evidence.ShadowGate {
	if !g.shadowReview {
		return nil
	}
	held, reason := false, ""
	switch {
	case !g.reviewLoaded:
		held, reason = true, "review approval required, but review-verdicts.json is unavailable"
	case !g.reviewArtifact.HasAggregateApproval(fullRepo, pr.Number, pr.HeadSHA):
		held, reason = true, "awaiting review approval"
	}
	return &evidence.ShadowGate{WouldHaveHeld: &held, Reason: reason, Confidence: 1, EvaluatedAt: now}
}

// recordReviewShadow logs the review.shadow observation (audit log) and folds
// it into the PR head's evidence bundle. Recording only: failures are logged
// and never block a merge.
func recordReviewShadow(cfg *config.Config, logger *slog.Logger, pr github.PullRequest, fullRepo string, g mergeGates, now time.Time) {
	gate := reviewShadowGate(g, fullRepo, pr, now)
	if gate == nil || cfg == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("review shadow gate", "repo", fullRepo, "number", pr.Number, "head", pr.HeadSHA,
		"would_have_held", *gate.WouldHaveHeld, "reason", gate.Reason, "confidence", gate.Confidence)
	writeShadowEvidence(cfg, logger, "review", fullRepo, pr.Number, pr.HeadSHA, evidence.Shadow{ReviewGate: gate})
}
