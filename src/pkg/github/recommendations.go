package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/logscrub"
)

// RecommendationsResult reports what the post did, so a caller can log the
// difference between a real update and a no-op.
type RecommendationsResult struct {
	Number  int
	URL     string
	Created bool
	Updated bool
	// Unchanged is true when the body already on the issue was identical and
	// no write was performed.
	Unchanged bool
}

// PostRecommendations maintains the hive's recommendations issue for one
// repository: one issue, found by title, rewritten in place.
//
// The anti-spam design is the whole point, so it is worth being explicit about
// each part:
//
//   - One issue per repository, located by its exact title. Not a new comment,
//     not a new issue -- a maintainer subscribed to it gets one notification
//     ever, because GitHub does not notify on an edit.
//   - The skip decision compares against THE BODY CURRENTLY ON THE ISSUE,
//     fetched each time, never against something remembered in this process.
//     A hive that restarts nine times a day (as a bluefin spoke did on
//     2026-09-18) would otherwise forget what it had already written and
//     rewrite it after every restart. Deriving the decision from the target
//     itself makes restarts irrelevant by construction.
//   - No @mentions survive rendering, so a rewrite can never re-notify a
//     person who happens to be named in it.
//
// It will not open an issue just to say there is nothing to say: when no issue
// exists yet and the digest has nothing actionable, it does nothing at all.
func (c *Client) PostRecommendations(ctx context.Context, repo, title, body string, labels []string, worthOpening bool) (RecommendationsResult, error) {
	if c == nil {
		return RecommendationsResult{}, ErrNoGitHubClient
	}
	if strings.TrimSpace(title) == "" {
		return RecommendationsResult{}, fmt.Errorf("PostRecommendations: empty title")
	}
	owner, repoName := c.splitRepo(repo)

	// Belt-and-suspenders, matching PostAdvisoryDigest: the body is rewritten
	// on a schedule, so one raw "@username" anywhere in it would re-notify
	// that person on every refresh. NeutralizeMentions is idempotent.
	body = advisory.NeutralizeMentions(body)

	// Canary-gated like CreateIssue/CreatePR/PostAdvisoryDigest
	// (kubestellar/hive#4960): this body aggregates repository-sourced text
	// (PR titles, check names) and posts it to the forge, the same
	// exfiltration shape as the other write paths, so it honors the same
	// fail-closed contract.
	if leak, ok := c.scanCanaryText(title+"\n"+body, "hive-recommendations:"+repo); ok {
		if c.canaryFailClosed {
			return RecommendationsResult{}, fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
		}
	}
	body = logscrub.ScrubString(body)

	existing, err := c.findOpenIssueByTitle(ctx, owner, repoName, title)
	if err != nil {
		// Failing to look up is not a reason to open a duplicate issue. A
		// missed cycle costs nothing; a second recommendations issue costs
		// the maintainer trust.
		return RecommendationsResult{}, fmt.Errorf("PostRecommendations: lookup failed: %w", err)
	}

	if existing == nil {
		if !worthOpening {
			c.logger.Debug("recommendations: nothing actionable and no issue exists — not opening one",
				slog.String("repo", repo))
			return RecommendationsResult{}, nil
		}
		created, _, err := c.client.Issues.Create(ctx, owner, repoName, &gh.IssueRequest{
			Title:  gh.Ptr(title),
			Body:   gh.Ptr(body),
			Labels: gh.Ptr(labels),
		})
		if err != nil {
			return RecommendationsResult{}, fmt.Errorf("PostRecommendations: create failed: %w", err)
		}
		c.logger.Info("recommendations issue opened",
			slog.String("repo", repo), slog.Int("number", created.GetNumber()))
		return RecommendationsResult{Number: created.GetNumber(), URL: created.GetHTMLURL(), Created: true}, nil
	}

	// The comparison baseline is the live body, so this survives a restart.
	if strings.TrimSpace(existing.GetBody()) == strings.TrimSpace(body) {
		c.logger.Debug("recommendations unchanged — skipping forge write",
			slog.String("repo", repo), slog.Int("number", existing.GetNumber()))
		return RecommendationsResult{Number: existing.GetNumber(), URL: existing.GetHTMLURL(), Unchanged: true}, nil
	}

	updated, _, err := c.client.Issues.Edit(ctx, owner, repoName, existing.GetNumber(), &gh.IssueRequest{
		Body: gh.Ptr(body),
	})
	if err != nil {
		return RecommendationsResult{}, fmt.Errorf("PostRecommendations: edit failed: %w", err)
	}
	c.logger.Info("recommendations issue updated",
		slog.String("repo", repo), slog.Int("number", updated.GetNumber()))
	return RecommendationsResult{Number: updated.GetNumber(), URL: updated.GetHTMLURL(), Updated: true}, nil
}
