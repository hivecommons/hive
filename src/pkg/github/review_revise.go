package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// Revising a review, rather than posting another one, is the difference between
// correcting the record and shouting over it.
//
// The review swarm skips any PR that already carries a verdict for its head
// SHA, which is right while the reviewer is sound and wrong the moment a
// reviewer-side defect is found: on the bluefin spoke 93 of 95 PRs were frozen
// at "no findings" — including a 30-file change removing 1655 lines — because
// the kick had told the reviewer to judge the diff without reading the
// surrounding tree. Re-reviewing those PRs the ordinary way would have left a
// second review on each one and notified every subscriber, which is exactly the
// spam the hive is supposed to avoid. Editing the existing review updates what
// a maintainer reads without notifying anyone at all.
//
// The capability is allowlisted per repo (Review.ReviseRepos) because silently
// rewriting text someone has already read is not a default any hive should hold
// over a repo it does not own.

// reviseSuppressedIdentical is the audit reason recorded when a revision would
// not change anything. It mirrors the advisory digest's skip-if-unchanged
// guard: a no-op PATCH still spends API budget and still races other writers,
// and a revision loop that rewrites the same bytes every sweep is how a quiet
// feature becomes a loud one.
const reviseSuppressedIdentical = "identical"

// reviseBodyDigest fingerprints a review body so an unchanged revision can be
// skipped without storing the body itself.
func reviseBodyDigest(body string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(body)))
	return hex.EncodeToString(sum[:8])
}

// findOwnLatestReview returns the most recent review on the PR written by this
// App, or nil when there is none.
//
// It deliberately reads GitHub rather than review-links.json: the links file
// records a URL but no review ID, and the PRs this exists to correct were
// reviewed before any of this shipped. Asking the API is the only way to find
// the review that is actually there.
func (c *Client) findOwnLatestReview(ctx context.Context, owner, repoName string, number int) (*gh.PullRequestReview, error) {
	login := strings.TrimSpace(c.appBotLogin)
	if login == "" {
		// Without an identity we cannot tell our own review from a human's,
		// and editing a person's review would be a serious breach. Fail
		// closed.
		return nil, fmt.Errorf("no App bot login configured; cannot identify the hive's own review")
	}

	opts := &gh.ListOptions{PerPage: 100}
	var latest *gh.PullRequestReview
	for {
		reviews, resp, err := c.client.PullRequests.ListReviews(ctx, owner, repoName, number, opts)
		if err != nil {
			return nil, fmt.Errorf("list reviews: %w", err)
		}
		for _, r := range reviews {
			if !strings.EqualFold(safeGetLogin(r.GetUser()), login) {
				continue
			}
			// Only advisory comments are revisable. An APPROVED or
			// CHANGES_REQUESTED review is a formal state on the PR, and
			// rewriting its body would change what that state appears to
			// say after the fact.
			if !strings.EqualFold(r.GetState(), "COMMENTED") {
				continue
			}
			if latest == nil || r.GetSubmittedAt().Time.After(latest.GetSubmittedAt().Time) {
				latest = r
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latest, nil
}

// reviseReviewAllowed reports whether this repo has been granted the in-place
// revision path.
func reviseReviewAllowed(repo string, allowlist []string) bool {
	want := strings.ToLower(strings.TrimSpace(repo))
	if want == "" {
		return false
	}
	for _, entry := range allowlist {
		if strings.EqualFold(strings.TrimSpace(entry), want) {
			return true
		}
	}
	return false
}

// reviseReview updates the hive's existing review on a PR in place. It returns
// the revised review, or nil when there was nothing to revise or the new body
// is identical to what is already published.
func (c *Client) reviseReview(ctx context.Context, req ReviewRequest, body string) (*gh.PullRequestReview, bool, error) {
	if !reviseReviewAllowed(req.Repo, c.reviseRepos) {
		return nil, false, fmt.Errorf("repo %s is not allowlisted for in-place review revision", req.Repo)
	}

	owner, repoName := c.splitRepo(req.Repo)
	existing, err := c.findOwnLatestReview(ctx, owner, repoName, req.Number)
	if err != nil {
		return nil, false, err
	}
	if existing == nil {
		// Nothing to revise. The caller falls back to posting normally --
		// a first review on a PR is not a revision.
		return nil, false, nil
	}

	if reviseBodyDigest(existing.GetBody()) == reviseBodyDigest(body) {
		c.logger.Info("review-request watcher: revision suppressed, body unchanged",
			slog.String("repo", req.Repo), slog.Int("number", req.Number),
			slog.String("reason", reviseSuppressedIdentical))
		return existing, false, nil
	}

	updated, _, err := c.client.PullRequests.UpdateReview(ctx, owner, repoName, req.Number, existing.GetID(), body)
	if err != nil {
		return nil, false, fmt.Errorf("update review %d: %w", existing.GetID(), err)
	}

	c.logger.Info("review-request watcher: revised its own review in place",
		slog.String("repo", req.Repo), slog.Int("number", req.Number),
		slog.Int64("review_id", existing.GetID()),
		slog.String("was", reviseBodyDigest(existing.GetBody())),
		slog.String("now", reviseBodyDigest(body)))
	return updated, true, nil
}
