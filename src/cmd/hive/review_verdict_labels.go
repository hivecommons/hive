package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// reviewVerdictLabelMaxChanges bounds verdict-label writes per cycle, so a
// hive turning the feature on for a deep queue converges over a few cycles
// instead of spending one burst of API calls.
const reviewVerdictLabelMaxChanges = 25

// reviewVerdictLabelSweepInterval spaces the closed-PR sweep: it costs one
// Search call per verdict label and owner, and the search API allows 30 a
// minute.
const reviewVerdictLabelSweepInterval = 10 * time.Minute

// reviewVerdictLabeler is the slice of *github.Client the verdict label
// mirror writes through. Removals are journaled against the PR state they
// were planned from (RemoveLabelAsOf): the plain RemoveLabel journals
// "remove X on #N" as applied forever, so once a PR's in-progress label had
// come off it could never come off again, and the next approval left
// in-progress and approved side by side (hivecommons/hive#14361).
type reviewVerdictLabeler interface {
	ApplyHumanDecisionLabel(ctx context.Context, repo string, number int, label string) error
	RemoveLabelAsOf(ctx context.Context, repo string, number int, label string, updatedAt time.Time) error
}

// closedPRVerdictLabeler adds the search the closed-PR sweep needs.
type closedPRVerdictLabeler interface {
	reviewVerdictLabeler
	ClosedPRsWithLabel(ctx context.Context, owner, label string, limit int) ([]github.LabelledPR, error)
}

// applyReviewVerdictLabels mirrors each open PR's review state onto one
// review.verdict_labels label (hivecommons/hive#13833): reviewing while the
// current head is under review, then its settled verdict. It runs after the
// cycle's dispatch plan, so a head kicked this cycle already reads as
// reviewing. Only PRs whose labels are out of step are touched. Labels are
// never created (the same contract as human_decision_label and
// priority_labels): a missing label is logged and skipped. A PR's stale
// labels come off before its new one goes on, and the cap counts a PR's
// whole change, so two states never show at once.
func applyReviewVerdictLabels(ctx context.Context, cfg *config.Config, labeler reviewVerdictLabeler, actionable *github.ActionableResult, plan review.DispatchPlan, logger *slog.Logger) {
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
	updatedAt := make(map[string]time.Time, len(prs))
	for _, pr := range prs {
		updatedAt[github.ReviewQueueKey(pr.Repo, pr.Number)] = pr.UpdatedAt
	}
	for _, ch := range review.PlanVerdictLabels(set, prs, artifact, plan.State, reviewVerdictLabelMaxChanges) {
		observed := updatedAt[github.ReviewQueueKey(ch.Repo, ch.Number)]
		for _, stale := range ch.Remove {
			if err := labeler.RemoveLabelAsOf(ctx, ch.Repo, ch.Number, stale, observed); err != nil && logger != nil {
				logger.Warn("stale review verdict label not removed", "repo", ch.Repo, "pr", ch.Number, "label", stale, "error", err)
			}
		}
		if ch.Add != "" {
			if err := labeler.ApplyHumanDecisionLabel(ctx, ch.Repo, ch.Number, ch.Add); err != nil {
				if logger != nil {
					logger.Warn("review verdict label not applied", "repo", ch.Repo, "pr", ch.Number, "label", ch.Add, "error", err)
				}
			} else if logger != nil {
				logger.Info("review verdict label applied", "repo", ch.Repo, "pr", ch.Number, "label", ch.Add)
			}
		}
	}
}

var verdictLabelSweep struct {
	mu   sync.Mutex
	last time.Time
}

// sweepClosedPRVerdictLabels takes the review verdict labels off PRs that
// merged or closed while carrying one. The open-PR mirror never sees those
// PRs again, so a PR merged mid-review kept hive-review/in-progress forever
// (hivecommons/hive#14361). It runs at most once per
// reviewVerdictLabelSweepInterval, searches each configured owner for closed
// PRs carrying each verdict label, and removes at most
// reviewVerdictLabelMaxChanges labels per run. A PR it cleaned no longer
// matches, so a settled hive pays one empty search per label and owner.
func sweepClosedPRVerdictLabels(ctx context.Context, cfg *config.Config, labeler closedPRVerdictLabeler, now time.Time, logger *slog.Logger) {
	if cfg == nil || labeler == nil || !cfg.Review.VerdictLabels.Enabled() {
		return
	}
	verdictLabelSweep.mu.Lock()
	if !verdictLabelSweep.last.IsZero() && now.Sub(verdictLabelSweep.last) < reviewVerdictLabelSweepInterval {
		verdictLabelSweep.mu.Unlock()
		return
	}
	verdictLabelSweep.last = now
	verdictLabelSweep.mu.Unlock()

	governed, owners := verdictLabelSweepScope(cfg)
	removed := 0
	for _, owner := range owners {
		for _, label := range verdictLabelNames(cfg.Review.VerdictLabels) {
			if removed >= reviewVerdictLabelMaxChanges {
				return
			}
			found, err := labeler.ClosedPRsWithLabel(ctx, owner, label, reviewVerdictLabelMaxChanges-removed)
			if err != nil {
				if logger != nil {
					logger.Warn("review verdict labels: closed-PR sweep search failed", "owner", owner, "label", label, "error", err)
				}
				continue
			}
			for _, pr := range found {
				if removed >= reviewVerdictLabelMaxChanges {
					return
				}
				if len(governed) > 0 && !governed[strings.ToLower(pr.Repo)] {
					continue
				}
				removed++
				if err := labeler.RemoveLabelAsOf(ctx, pr.Repo, pr.Number, label, pr.UpdatedAt); err != nil {
					if logger != nil {
						logger.Warn("review verdict label not removed from closed PR", "repo", pr.Repo, "pr", pr.Number, "label", label, "error", err)
					}
				} else if logger != nil {
					logger.Info("review verdict label removed from closed PR", "repo", pr.Repo, "pr", pr.Number, "label", label)
				}
			}
		}
	}
}

// verdictLabelSweepScope returns the governed repos (lower-case owner/name)
// and the owners to search. With no repos configured every PR under the
// project org is in scope.
func verdictLabelSweepScope(cfg *config.Config) (map[string]bool, []string) {
	governed := map[string]bool{}
	seen := map[string]bool{}
	var owners []string
	addOwner := func(o string) {
		o = strings.ToLower(strings.TrimSpace(o))
		if o != "" && !seen[o] {
			seen[o] = true
			owners = append(owners, o)
		}
	}
	for _, r := range cfg.Project.Repos {
		full := strings.ToLower(fullRepoName(strings.TrimSpace(r), cfg.Project.Org))
		if owner, _, ok := strings.Cut(full, "/"); ok {
			governed[full] = true
			addOwner(owner)
		}
	}
	if len(owners) == 0 {
		addOwner(cfg.Project.Org)
	}
	return governed, owners
}

func reviewVerdictLabelSet(v config.ReviewVerdictLabelsConfig) review.VerdictLabelSet {
	return review.VerdictLabelSet{
		Reviewing:        v.Reviewing,
		Approve:          v.Approve,
		ChangesRequested: v.ChangesRequested,
		RequiresHuman:    v.RequiresHuman,
	}
}

// verdictLabelNames is the distinct configured verdict labels, in state order.
func verdictLabelNames(v config.ReviewVerdictLabelsConfig) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range []string{v.Reviewing, v.Approve, v.ChangesRequested, v.RequiresHuman} {
		l = strings.TrimSpace(l)
		if l != "" && !seen[strings.ToLower(l)] {
			seen[strings.ToLower(l)] = true
			out = append(out, l)
		}
	}
	return out
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
				Repo:      pr.Repo,
				Number:    pr.Number,
				HeadSHA:   pr.HeadSHA,
				Labels:    append([]string(nil), pr.Labels...),
				UpdatedAt: pr.UpdatedAt,
			})
		}
	}
	add(actionable.PRs.Items)
	add(actionable.PRs.Held)
	return out
}
