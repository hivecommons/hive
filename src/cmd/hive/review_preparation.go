package main

import (
	"context"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

type preparationObserver interface {
	ActiveBasePreparation(context.Context, string, string, string) (automerge.BasePreparationObservation, bool, error)
}

func reviewPreparationOwesVerdict(artifact review.Artifact, repo string, pr github.PullRequest) bool {
	agg, judged := artifact.AggregateForBody(repo, pr.Number, pr.HeadSHA, pr.BodyDigest)
	return !judged || agg.Verdict == review.VerdictIncomplete
}

func reviewPreparationObserver(cfg *config.Config, ghr reviewPlannerGitHub, logger *slog.Logger) preparationObserver {
	if observer, ok := ghr.(preparationObserver); ok {
		return observer
	}
	client, ok := ghr.(*github.Client)
	if !ok || client == nil {
		return nil
	}
	return automerge.New(client, automerge.Options{
		Logger: logger, RequireCurrentBase: cfg.AutoMerge.RequireCurrentBaseFor,
		StageLabelPolicy: stageLabelPolicyFunc(cfg), HumanMergeRule: func() prstage.HumanMergeRule { return humanMergeRuleFor(cfg) },
	})
}

// reviewPreparationHeads reads at most once per destination, outside the review
// planner and preparation mutation locks. No observation grants eligibility.
func reviewPreparationHeads(ctx context.Context, cfg *config.Config, candidates []github.PullRequest, observer preparationObserver, artifact review.Artifact, logger *slog.Logger) map[string]string {
	heads := map[string]string{}
	if observer == nil {
		return heads
	}
	seen := map[string]bool{}
	for _, pr := range candidates {
		repo := fullRepoName(pr.Repo, cfg.Project.Org)
		if !cfg.AutoMerge.RequireCurrentBaseFor(repo) || pr.BaseRef == "" || !reviewMergeReady(pr) || reviewCIPending(pr) {
			continue
		}
		if !reviewPreparationOwesVerdict(artifact, repo, pr) {
			continue
		}
		destination := strings.ToLower(repo) + ":" + pr.BaseRef
		if seen[destination] {
			continue
		}
		seen[destination] = true
		owner, name, ok := strings.Cut(repo, "/")
		if !ok {
			continue
		}
		observation, active, err := observer.ActiveBasePreparation(ctx, owner, name, pr.BaseRef)
		if err != nil {
			if logger != nil {
				logger.Warn("review preparation observation unavailable", "repo", repo, "base", pr.BaseRef, "error", err)
			}
			continue
		}
		if active {
			heads[github.ReviewQueueKey(repo, observation.Number)] = observation.HeadSHA
		}
	}
	return heads
}
