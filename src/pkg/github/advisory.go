package github

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
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

	commentID, err := c.findDigestComment(ctx, owner, repoName, issueNum)
	if err != nil {
		c.logger.Warn("could not search for existing digest comment, creating new", slog.String("error", err.Error()))
	}

	var author string
	if commentID > 0 {
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
	opts := &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: 50},
	}
	comments, _, err := c.client.Issues.ListComments(ctx, owner, repo, issueNum, opts)
	if err != nil {
		return 0, err
	}
	var botAuthored int64
	for _, comment := range comments {
		if !strings.HasPrefix(comment.GetBody(), advisoryDigestPrefix) {
			continue
		}
		if c.appAuth == nil {
			// Token (PAT) client: historical prefix-only match. The credential
			// may legitimately be the human who authored the comment, and
			// authorship cannot be verified without an extra /user round trip.
			return int(comment.GetID()), nil
		}
		login := comment.GetUser().GetLogin()
		if c.appBotLogin != "" && login == c.appBotLogin {
			// Exactly our own bot comment — the one credential-safe choice.
			return int(comment.GetID()), nil
		}
		if strings.HasSuffix(login, "[bot]") || comment.GetUser().GetType() == "Bot" {
			// Bot-authored but not provably ours (bot login unknown, or a slug
			// mismatch between config and the real App). Remember the first as
			// a fallback rather than skipping it: refusing our own comment on
			// a misconfigured slug would create a duplicate every cycle.
			if botAuthored == 0 {
				botAuthored = comment.GetID()
			}
			continue
		}
		// A digest comment this App can never edit — e.g. one left behind by
		// the removed user-token fallback (#1927), authored by a human. GitHub
		// hard-forbids an App from editing a foreign-authored comment (403
		// "Resource not accessible by integration") no matter what the
		// installation grants, so adopting it wedges the digest forever
		// (kalantar-msb/soft-reflective#1). Skip it; if no bot-authored digest
		// comment exists a fresh App-authored one is created and every later
		// cycle edits THAT one, so nothing is duplicated per cycle.
		c.logger.Warn("skipping advisory digest comment not authored by this App — an installation token can never edit it",
			slog.String("repo", owner+"/"+repo),
			slog.Int("issue", issueNum),
			slog.Int64("comment_id", comment.GetID()),
			slog.String("author", login))
	}
	return int(botAuthored), nil
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
		ListOptions: gh.ListOptions{PerPage: 50, Page: 1},
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

	// Fallback 3: a CLOSED advisory issue is REUSED, not replaced (#4167).
	// The issue says "do not close this issue", but people close it anyway —
	// and creating a fresh one then splits the digest: the hive writes to the
	// new issue while everyone who subscribed to the old one watches a comment
	// that never updates again, which reads exactly like a wedged digest. So
	// reopen the most recent closed one and keep posting to the URL people
	// already follow. Only after this finds nothing does the caller create.
	if num, ok := c.findClosedAdvisoryIssue(ctx, owner, repo); ok {
		if _, _, err := c.client.Issues.Edit(ctx, owner, repo, num, &gh.IssueRequest{State: gh.Ptr("open")}); err != nil {
			// Cannot reopen (permissions, locked). Report NOT-FOUND rather than
			// the closed number: posting a digest to a closed issue would be
			// invisible, and the caller's create path at least yields a live one.
			c.logger.Warn("found closed advisory issue but could not reopen it",
				slog.Int("number", num), slog.String("error", err.Error()))
			return 0, nil
		}
		c.logger.Info("reopened closed advisory issue instead of creating a duplicate",
			slog.String("repo", repo), slog.Int("number", num))
		c.ensureAdvisoryLabel(ctx, owner, repo, num)
		return num, nil
	}
	return 0, nil
}

// findClosedAdvisoryIssue returns the most recently updated CLOSED advisory
// issue for a repo, if one exists. Split out from findAdvisoryIssue so the
// "reuse the issue people already subscribe to" rule is testable on its own.
func (c *Client) findClosedAdvisoryIssue(ctx context.Context, owner, repo string) (int, bool) {
	opts := &gh.IssueListByRepoOptions{
		State:       "closed",
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: 50, Page: 1},
	}
	issues, _, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
	if err != nil {
		return 0, false
	}
	for _, issue := range issues {
		if issue.IsPullRequest() {
			continue
		}
		if issue.GetTitle() == advisoryTitle {
			return issue.GetNumber(), true
		}
	}
	return 0, false
}
