package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	gh "github.com/google/go-github/v72/github"
)

const (
	advisoryTitle     = "🐝 Hive Advisory Report"
	advisoryLabelName = "hive/advisory"
	advisoryLabelDesc = "Pinned advisory report from Hive agents"
	advisoryLabelClr  = "0e8a16"
)

// EnsureAdvisoryIssue finds or creates the pinned advisory issue for a repo.
// Returns the issue number.
func (c *Client) EnsureAdvisoryIssue(ctx context.Context, repo string) (int, error) {
	if c == nil {
		return 0, ErrNoGitHubClient
	}
	owner := c.org
	if parts := strings.SplitN(repo, "/", 2); len(parts) == 2 {
		owner = parts[0]
		repo = parts[1]
	}

	num, err := c.findAdvisoryIssue(ctx, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("searching for advisory issue: %w", err)
	}
	if num > 0 {
		c.logger.Info("found existing advisory issue", slog.String("repo", repo), slog.Int("number", num))
		return num, nil
	}

	c.logger.Info("creating advisory issue", slog.String("repo", repo))

	_, _, labelErr := c.client.Issues.CreateLabel(ctx, owner, repo, &gh.Label{
		Name:        gh.Ptr(advisoryLabelName),
		Description: gh.Ptr(advisoryLabelDesc),
		Color:       gh.Ptr(advisoryLabelClr),
	})
	labelExists := labelErr == nil || strings.Contains(labelErr.Error(), "already_exists")
	if !labelExists {
		c.logger.Warn("could not create advisory label, issue will be created without it", slog.String("error", labelErr.Error()))
	}

	body := "This issue collects advisory findings from Hive agents.\n\n" +
		"At lower ACMM levels, some agents work in advisory mode — they analyze code and post findings " +
		"here but do not create issues or PRs. At higher levels, designated agents (e.g. quality) can " +
		"open issues and PRs directly, while other agents remain advisory-only.\n\n" +
		"The governor posts periodic digest comments summarizing what advisory agents found.\n\n" +
		"**Do not close this issue.** It is a living document."

	req := &gh.IssueRequest{
		Title: gh.Ptr(advisoryTitle),
		Body:  gh.Ptr(body),
	}
	if labelExists {
		req.Labels = &[]string{advisoryLabelName}
	}
	issue, _, err := c.client.Issues.Create(ctx, owner, repo, req)
	if err != nil {
		return 0, fmt.Errorf("creating advisory issue: %w", err)
	}

	c.logger.Info("created advisory issue — pin it manually for visibility", slog.String("repo", repo), slog.Int("number", issue.GetNumber()))
	return issue.GetNumber(), nil
}

const (
	advisoryDigestPrefix    = "## 🐝 Advisory Digest"
	githubCommentCharLimit  = 65536
	truncationFooterPadding = 200
)

// PostAdvisoryDigest updates the existing digest comment on the advisory issue,
// or creates one if none exists. This prevents duplicate comments on each eval cycle.
func (c *Client) PostAdvisoryDigest(ctx context.Context, repo string, issueNum int, digest string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)

	digest = truncateDigest(digest)

	commentID, existingDigest, err := c.findDigestCommentWithBody(ctx, owner, repoName, issueNum)
	if err != nil {
		c.logger.Warn("could not search for existing digest comment, creating new", slog.String("error", err.Error()))
	}

	if commentID > 0 {
		if advisoryDigestsSemanticallyEqual(existingDigest, digest) {
			return nil
		}
		_, _, err := c.client.Issues.EditComment(ctx, owner, repoName, int64(commentID), &gh.IssueComment{
			Body: gh.Ptr(digest),
		})
		if err != nil {
			return fmt.Errorf("updating advisory digest comment on %s#%d: %w", repo, issueNum, err)
		}
		return nil
	}

	_, _, err = c.client.Issues.CreateComment(ctx, owner, repoName, issueNum, &gh.IssueComment{
		Body: gh.Ptr(digest),
	})
	if err != nil {
		return fmt.Errorf("posting advisory digest to %s#%d: %w", repo, issueNum, err)
	}
	return nil
}

func truncateDigest(digest string) string {
	if len(digest) <= githubCommentCharLimit {
		return digest
	}
	cutoff := githubCommentCharLimit - truncationFooterPadding
	lastNewline := strings.LastIndex(digest[:cutoff], "\n")
	if lastNewline > 0 {
		cutoff = lastNewline
	}
	// Ensure cutoff doesn't split a UTF-8 character
	for cutoff > 0 && !utf8.RuneStart(digest[cutoff]) {
		cutoff--
	}
	return digest[:cutoff] + fmt.Sprintf("\n\n---\n⚠️ *Digest truncated: %d → %d characters (GitHub limit: %d)*\n", len(digest), cutoff, githubCommentCharLimit)
}

func (c *Client) ensureAdvisoryLabel(ctx context.Context, owner, repo string, issueNum int) {
	_, _, labelErr := c.client.Issues.CreateLabel(ctx, owner, repo, &gh.Label{
		Name:        gh.Ptr(advisoryLabelName),
		Description: gh.Ptr(advisoryLabelDesc),
		Color:       gh.Ptr(advisoryLabelClr),
	})
	if labelErr != nil && !strings.Contains(labelErr.Error(), "already_exists") {
		return
	}
	_, _, _ = c.client.Issues.AddLabelsToIssue(ctx, owner, repo, issueNum, []string{advisoryLabelName})
}

func (c *Client) findDigestComment(ctx context.Context, owner, repo string, issueNum int) (int, error) {
	commentID, _, err := c.findDigestCommentWithBody(ctx, owner, repo, issueNum)
	return commentID, err
}

func (c *Client) findDigestCommentWithBody(ctx context.Context, owner, repo string, issueNum int) (int, string, error) {
	opts := &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: 50},
	}
	comments, _, err := c.client.Issues.ListComments(ctx, owner, repo, issueNum, opts)
	if err != nil {
		return 0, "", err
	}
	for _, comment := range comments {
		if strings.HasPrefix(comment.GetBody(), advisoryDigestPrefix) {
			return int(comment.GetID()), comment.GetBody(), nil
		}
	}
	return 0, "", nil
}

func advisoryDigestsSemanticallyEqual(existing, next string) bool {
	if existing == next {
		return true
	}
	existingBody, existingOK := advisoryDigestBodyWithoutGeneratedAt(existing)
	nextBody, nextOK := advisoryDigestBodyWithoutGeneratedAt(next)
	return existingOK && nextOK && existingBody == nextBody
}

func advisoryDigestBodyWithoutGeneratedAt(digest string) (string, bool) {
	lineEnd := strings.IndexByte(digest, '\n')
	if lineEnd <= 0 {
		return "", false
	}
	generatedAt, ok := strings.CutPrefix(digest[:lineEnd], advisoryDigestPrefix+" — ")
	if !ok {
		return "", false
	}
	if _, err := time.Parse("2006-01-02 15:04 MST", generatedAt); err != nil {
		return "", false
	}
	return digest[lineEnd+1:], true
}

func (c *Client) findAdvisoryIssue(ctx context.Context, owner, repo string) (int, error) {
	// Use search API to find the advisory issue across all open issues,
	// regardless of how many issues the repo has.
	query := fmt.Sprintf(`repo:%s/%s is:issue is:open in:title "%s"`, owner, repo, advisoryTitle)
	result, _, err := c.client.Search.Issues(ctx, query, &gh.SearchOptions{
		ListOptions: gh.ListOptions{PerPage: 5},
	})
	if err == nil {
		for _, issue := range result.Issues {
			if issue.GetTitle() == advisoryTitle {
				return issue.GetNumber(), nil
			}
		}
	} else {
		c.logger.Warn("search API failed, falling back to label filter", slog.String("error", err.Error()))
	}

	// Fallback 1: list by label (works when search API is unavailable)
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{advisoryLabelName},
		ListOptions: gh.ListOptions{PerPage: 10},
	}
	issues, _, listErr := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
	if listErr == nil {
		for _, issue := range issues {
			if issue.GetTitle() == advisoryTitle {
				return issue.GetNumber(), nil
			}
		}
	}

	// Fallback 2: scan recent open issues by title (handles the case where
	// the label was never applied — prevents duplicate advisory issues)
	const maxPagesToScan = 3
	listOpts := &gh.IssueListByRepoOptions{
		State:       "open",
		Sort:        "created",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: 50},
	}
	for page := 0; page < maxPagesToScan; page++ {
		allIssues, _, scanErr := c.client.Issues.ListByRepo(ctx, owner, repo, listOpts)
		if scanErr != nil {
			break
		}
		for _, issue := range allIssues {
			if issue.IsPullRequest() {
				continue
			}
			if issue.GetTitle() == advisoryTitle {
				c.logger.Info("found advisory issue via title scan (missing label)", slog.Int("number", issue.GetNumber()))
				c.ensureAdvisoryLabel(ctx, owner, repo, issue.GetNumber())
				return issue.GetNumber(), nil
			}
		}
		if len(allIssues) < 50 {
			break
		}
		listOpts.ListOptions.Page++
	}
	return 0, nil
}
