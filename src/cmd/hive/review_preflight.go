package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/reviewpreflight"
)

// maxPreflightMemo bounds the per-head evidence memo.
const maxPreflightMemo = 256

// reviewPreflightSource is what the evidence builder reads from GitHub. It is
// an optional capability of the planner's GitHub client: a client without it
// simply yields no evidence.
type reviewPreflightSource interface {
	ReviewPreflightTree(ctx context.Context, repo, ref string) ([]reviewpreflight.TreeEntry, bool)
	ReviewPreflightFiles(ctx context.Context, repo string, number int) ([]reviewpreflight.ChangedFile, bool)
}

var (
	reviewPreflightClient     = reviewpreflight.NewClient()
	reviewPreflightMemoMu     sync.Mutex
	reviewPreflightMemoByHead = map[string]string{}
)

// reviewPreflightFor builds the evidence section for one PR, or "" when the
// checks are off, the PR is not about to be reviewed at this head, or the
// source is unavailable. The answer is memoized per repo, PR, head and
// effective settings, so the planner's repeated cycles cost one lookup.
func reviewPreflightFor(ctx context.Context, cfg *config.Config, ghr reviewPlannerGitHub, artifact review.Artifact, state review.DispatchState, repo string, pr github.PullRequest, logger *slog.Logger) string {
	src, ok := ghr.(reviewPreflightSource)
	if !ok || src == nil || cfg == nil || pr.HeadSHA == "" {
		return ""
	}
	set := cfg.ReviewPreflightFor(repo)
	if !set.RuleDocs && !set.Dependencies && len(set.Rules) == 0 {
		return ""
	}
	if _, judged := artifact.AggregateForBody(repo, pr.Number, pr.HeadSHA, pr.BodyDigest); judged {
		return ""
	}
	key := fmt.Sprintf("%s#%d@%s|%v", repo, pr.Number, pr.HeadSHA, set)
	reviewPreflightMemoMu.Lock()
	cached, hit := reviewPreflightMemoByHead[key]
	reviewPreflightMemoMu.Unlock()
	if hit {
		return cached
	}
	if !state.PendingSince(repo, pr.Number, pr.HeadSHA).IsZero() {
		// Already dispatched at this head: the kick is out, nothing to attach.
		return ""
	}
	in := reviewpreflight.Input{Repo: repo}
	if set.RuleDocs {
		ref := pr.BaseSHA
		if ref == "" {
			ref = pr.HeadSHA
		}
		in.Ref = ref
		in.Tree, in.TreeKnown = src.ReviewPreflightTree(ctx, repo, ref)
	}
	if set.Dependencies {
		in.Files, in.FilesKnown = src.ReviewPreflightFiles(ctx, repo, pr.Number)
	}
	out := reviewpreflight.Build(ctx, reviewPreflightClient, set, in)
	if logger != nil && out != "" {
		logger.Info("review evidence attached", "repo", repo, "pr", pr.Number, "bytes", len(out),
			"rule_docs", set.RuleDocs, "dependencies", set.Dependencies, "offline", set.Offline)
	}
	if (set.RuleDocs && !in.TreeKnown) || (set.Dependencies && !in.FilesKnown) {
		// A failed read is retried next cycle rather than remembered.
		return out
	}
	reviewPreflightMemoMu.Lock()
	if len(reviewPreflightMemoByHead) >= maxPreflightMemo {
		reviewPreflightMemoByHead = map[string]string{}
	}
	reviewPreflightMemoByHead[key] = out
	reviewPreflightMemoMu.Unlock()
	return out
}
