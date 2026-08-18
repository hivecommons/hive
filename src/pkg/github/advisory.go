package github

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/pkg/advisory"
	"github.com/kubestellar/hive/pkg/logscrub"
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

	// Invocation-attribution trail (attribution.go): this issue is created by
	// the hive itself, so the "agent" is the governor flow — backend/model are
	// genuinely unknown here and are omitted rather than guessed. Trailer is
	// config-gated; the audit entry below is unconditional.
	meta := InvocationMeta{Agent: AttributionAgentGovernor}
	if c.attributionTrailerOn() {
		body = AppendTrailer(body, meta)
	}

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
	c.recordCreationAudit(AuditActionHiveIssueCreated, meta,
		"repo", owner+"/"+repo,
		"number", strconv.Itoa(issue.GetNumber()),
		"author", issue.GetUser().GetLogin(),
		"url", issue.GetHTMLURL(),
		"flow", "advisory")

	c.logger.Info("created advisory issue — pin it manually for visibility", slog.String("repo", repo), slog.Int("number", issue.GetNumber()))
	return issue.GetNumber(), nil
}

const (
	advisoryDigestPrefix    = "## 🐝 Advisory Digest"
	githubCommentCharLimit  = 65536
	truncationFooterPadding = 200
	// advisoryDigestAuditInterval is how often (in digest posts) an
	// advisory_commented audit entry is emitted, in addition to the very first
	// post. The digest is refreshed ~once a minute; a value of 50 yields roughly
	// one audit pulse per ~50 minutes, enough to show the loop is alive without
	// flooding the dashboard audit log.
	advisoryDigestAuditInterval = 50
)

// PostAdvisoryDigest updates the existing digest comment on the advisory issue,
// or creates one if none exists. This prevents duplicate comments on each eval cycle.
func (c *Client) PostAdvisoryDigest(ctx context.Context, repo string, issueNum int, digest string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)

	// Belt-and-suspenders enforcement: no advisory body may ever carry a raw
	// @mention. The digest comment is rewritten every update cycle, so a
	// single "@username" in any of its findings would re-notify that human on
	// every refresh. FormatDigestMarkdown already neutralizes mentions at
	// render time; NeutralizeMentions is idempotent, so applying it again here
	// guarantees the invariant for every caller of this post path. The secret
	// scrub (logscrub) runs after, so nothing it redacts can re-introduce a
	// mention and neither pass weakens the other.
	digest = advisory.NeutralizeMentions(digest)
	digest = truncateDigest(logscrub.ScrubString(digest))

	commentID, existingDigest, err := c.findDigestCommentWithBody(ctx, owner, repoName, issueNum)
	if err != nil {
		c.logger.Warn("could not search for existing digest comment, creating new", slog.String("error", err.Error()))
	}

	var author string
	if commentID > 0 {
		if advisoryDigestsSemanticallyEqual(existingDigest, digest) {
			return nil
		}
		edited, _, err := c.client.Issues.EditComment(ctx, owner, repoName, int64(commentID), &gh.IssueComment{
			Body: gh.Ptr(digest),
		})
		if err != nil {
			return fmt.Errorf("updating advisory digest comment on %s#%d: %w", repo, issueNum, err)
		}
		author = edited.GetUser().GetLogin()
	} else {
		comment, _, err := c.client.Issues.CreateComment(ctx, owner, repoName, issueNum, &gh.IssueComment{
			Body: gh.Ptr(digest),
		})
		if err != nil {
			return fmt.Errorf("posting advisory digest to %s#%d: %w", repo, issueNum, err)
		}
		author = comment.GetUser().GetLogin()
	}

	// Audit the FIRST post and every advisoryDigestAuditInterval-th one, not the
	// ~once-a-minute refreshes in between: auditing every update would flood the
	// audit log (1000s/day) and evict the discrete create→merge events. A
	// periodic pulse still proves the advisory loop is alive on the dashboard.
	c.advisoryMu.Lock()
	if c.advisoryDigestPosts == nil {
		c.advisoryDigestPosts = make(map[string]int)
	}
	key := fmt.Sprintf("%s/%s#%d", owner, repoName, issueNum)
	c.advisoryDigestPosts[key]++
	count := c.advisoryDigestPosts[key]
	c.advisoryMu.Unlock()

	if count == 1 || count%advisoryDigestAuditInterval == 0 {
		c.recordCreationAudit(AuditActionAdvisoryCommented, InvocationMeta{Agent: AttributionAgentGovernor},
			"repo", owner+"/"+repoName,
			"number", strconv.Itoa(issueNum),
			"author", author,
			"post_count", strconv.Itoa(count),
			"flow", "advisory-digest")
	}
	return nil
}

// ProbeIssueWrite (#2353) verifies that the current credential can actually
// WRITE to repo by performing a benign, self-reverting edit of the advisory
// issue: it reads the issue's current body and writes that same body straight
// back. GitHub records no visible change, but the request still exercises the
// exact issues:write path (and repo scope) a real digest post needs.
//
// This closes the recheck false-positive: a READ (finding the advisory issue)
// succeeds even when the App cannot write, and an installation-permission check
// cannot see that the repo is absent from the App installation's selected
// repos. Only a real write proves write capability, so callers that must not
// clear the write-forbidden banner (#2353) probe with this before declaring the
// App healthy.
//
// Returns nil on a successful write; the underlying error (e.g. the 403
// "Resource not accessible by integration") otherwise, so the caller can
// classify it exactly as it would a failed digest post.
func (c *Client) ProbeIssueWrite(ctx context.Context, repo string, issueNum int) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, repoName, issueNum)
	if err != nil {
		return fmt.Errorf("reading advisory issue %s#%d for write probe: %w", repo, issueNum, err)
	}
	// Write the current body back unchanged — a no-op edit that still requires
	// (and thus proves) issues:write on THIS repo.
	_, _, err = c.client.Issues.Edit(ctx, owner, repoName, issueNum, &gh.IssueRequest{
		Body: gh.Ptr(issue.GetBody()),
	})
	if err != nil {
		return fmt.Errorf("write probe on advisory issue %s#%d: %w", repo, issueNum, err)
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
