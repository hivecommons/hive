package main

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/recommend"
)

// postRecommendationsForCycle maintains the "what should I merge next?" issue
// for each watched repository.
//
// It runs off the PR set the cycle has ALREADY enumerated and CI-enriched, so
// it costs no additional GitHub reads beyond the one issue lookup per repo.
// That matters: a hive watching sixteen repositories on a 15-minute cadence
// cannot afford a second enumeration just to render a digest.
//
// Errors are logged and never returned. A digest is a convenience; failing to
// render one must not fail the eval cycle that also runs the merge sweep.
func postRecommendationsForCycle(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	actionable *github.ActionableResult,
	logger *slog.Logger,
) {
	if cfg == nil || ghClient == nil || actionable == nil {
		return
	}
	rc := cfg.Review.Recommendations
	if !rc.Enabled {
		return
	}

	byRepo := groupPRsByRepo(actionable)
	if len(byRepo) == 0 {
		return
	}

	allowed := map[string]bool{}
	for _, r := range rc.Repos {
		allowed[r] = true
	}

	minReady := rc.MinReadyToOpen
	if minReady == 0 {
		minReady = 1
	}

	// Deterministic order so logs from consecutive cycles line up.
	repos := make([]string, 0, len(byRepo))
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	for _, repo := range repos {
		if len(allowed) > 0 && !allowed[repo] {
			continue
		}
		report := recommend.Build(repo, byRepo[repo], recommend.Options{
			Now:                time.Now(),
			HumanDecisionLabel: cfg.Review.HumanDecisionLabel,
		})
		body := report.Markdown()

		// worthOpening gates only the FIRST post. Once the issue exists,
		// PostRecommendations keeps it current even when the answer is
		// "nothing is ready" — a maintainer reading it wants to know that.
		worthOpening := len(report.Buckets[recommend.BucketReady]) >= minReady

		res, err := ghClient.PostRecommendations(ctx, repo, recommend.Title, body, rc.Labels, worthOpening)
		if err != nil {
			logger.Warn("recommendations post failed",
				slog.String("repo", repo), slog.String("error", err.Error()))
			continue
		}
		switch {
		case res.Created:
			logger.Info("recommendations issue opened",
				slog.String("repo", repo), slog.Int("number", res.Number),
				slog.Int("ready", len(report.Buckets[recommend.BucketReady])),
				slog.Int("total_open", report.TotalOpen))
		case res.Updated:
			logger.Info("recommendations issue updated",
				slog.String("repo", repo), slog.Int("number", res.Number),
				slog.Int("ready", len(report.Buckets[recommend.BucketReady])),
				slog.Int("total_open", report.TotalOpen))
		}
	}
}

// groupPRsByRepo folds the cycle's actionable PRs and stale drafts into one
// list per repository.
//
// Held PRs are deliberately excluded. A hold is the maintainer's explicit
// "not now", and a digest that recommends merging something a human just held
// would destroy the digest's credibility on its first outing.
func groupPRsByRepo(actionable *github.ActionableResult) map[string][]github.PullRequest {
	byRepo := map[string][]github.PullRequest{}
	add := func(prs []github.PullRequest) {
		for _, pr := range prs {
			if pr.Repo == "" {
				continue
			}
			byRepo[pr.Repo] = append(byRepo[pr.Repo], pr)
		}
	}
	add(actionable.PRs.Items)
	add(actionable.PRs.StaleDrafts)
	return byRepo
}
