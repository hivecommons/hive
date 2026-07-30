package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// CreatePRResult is what CreatePR returns: the opened (or pre-existing) PR.
type CreatePRResult struct {
	Number int
	URL    string
	// AlreadyExisted is true when an open PR for this head branch was already
	// present, so we returned it instead of opening a duplicate.
	AlreadyExisted bool
}

// CreatePR opens a pull request on behalf of THIS hive's GitHub App — so the PR
// is authored by the App bot ("<slug>[bot]"), deterministically, regardless of
// which agent/CLI produced the branch. This is the hive-opens-the-PR path: an
// agent pushes its branch (the credential helper already uses the App token for
// the push), then the hive opens the PR here rather than the agent running
// `gh pr create` (which would attribute the PR to the Copilot login user).
//
// repo may be "owner/repo" or a bare repo name (owner defaults to the hive org).
// head is the branch the agent pushed; base defaults to "main" when empty.
//
// It is idempotent: if an OPEN PR for head already exists, it returns that PR
// with AlreadyExisted=true instead of erroring or opening a duplicate — this
// makes the file-watcher safe to retry a request that partially succeeded.
func (c *Client) CreatePR(ctx context.Context, repo, head, base, title, body string) (CreatePRResult, error) {
	if c == nil || c.client == nil {
		return CreatePRResult{}, ErrNoGitHubClient
	}
	owner := c.org
	if parts := strings.SplitN(repo, "/", 2); len(parts) == 2 {
		owner, repo = parts[0], parts[1]
	}
	head = strings.TrimSpace(head)
	if head == "" {
		return CreatePRResult{}, fmt.Errorf("CreatePR: head branch is required")
	}
	if base = strings.TrimSpace(base); base == "" {
		base = "main"
	}
	if strings.TrimSpace(title) == "" {
		return CreatePRResult{}, fmt.Errorf("CreatePR: title is required")
	}

	// Idempotency: don't open a second PR for a branch that already has an open
	// one. GitHub itself 422s on a duplicate, but checking first lets us return
	// the existing PR cleanly (the watcher may re-process a request after a
	// crash between "PR opened" and "request file deleted").
	if existing, err := c.findOpenPRForHead(ctx, owner, repo, head); err != nil {
		c.logger.Warn("CreatePR: dedupe lookup failed, proceeding to create",
			slog.String("repo", repo), slog.String("head", head), slog.String("error", err.Error()))
	} else if existing != nil {
		c.logger.Info("CreatePR: open PR already exists for head, reusing",
			slog.String("repo", repo), slog.String("head", head), slog.Int("number", existing.GetNumber()))
		return CreatePRResult{Number: existing.GetNumber(), URL: existing.GetHTMLURL(), AlreadyExisted: true}, nil
	}

	pr, _, err := c.client.PullRequests.Create(ctx, owner, repo, &gh.NewPullRequest{
		Title: gh.Ptr(title),
		Head:  gh.Ptr(head),
		Base:  gh.Ptr(base),
		Body:  gh.Ptr(body),
	})
	if err != nil {
		// A concurrent creator may have raced us; treat an existing-PR 422 as a
		// reuse rather than a hard failure.
		if strings.Contains(err.Error(), "A pull request already exists") {
			if existing, lookErr := c.findOpenPRForHead(ctx, owner, repo, head); lookErr == nil && existing != nil {
				return CreatePRResult{Number: existing.GetNumber(), URL: existing.GetHTMLURL(), AlreadyExisted: true}, nil
			}
		}
		return CreatePRResult{}, fmt.Errorf("creating PR %s/%s %s->%s: %w", owner, repo, head, base, err)
	}
	c.logger.Info("CreatePR: opened PR as the App bot",
		slog.String("repo", repo), slog.String("head", head), slog.Int("number", pr.GetNumber()))
	return CreatePRResult{Number: pr.GetNumber(), URL: pr.GetHTMLURL()}, nil
}

// findOpenPRForHead returns the open PR whose head branch matches, or nil. The
// head filter is "owner:branch" per the GitHub API.
func (c *Client) findOpenPRForHead(ctx context.Context, owner, repo, head string) (*gh.PullRequest, error) {
	prs, _, err := c.client.PullRequests.List(ctx, owner, repo, &gh.PullRequestListOptions{
		State:       "open",
		Head:        owner + ":" + head,
		ListOptions: gh.ListOptions{PerPage: 5},
	})
	if err != nil {
		return nil, err
	}
	if len(prs) > 0 {
		return prs[0], nil
	}
	return nil, nil
}
