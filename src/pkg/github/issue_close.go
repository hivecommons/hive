package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

var ErrReporterConfirmationRequired = errors.New("reporter confirmation required before closing human-filed bug issue")

type IssueCloseOptions struct {
	OverrideReason          string
	SuppressOverrideComment bool
	// StateReason, when set, is sent as GitHub's state_reason ("completed" or
	// "not_planned"). Empty leaves GitHub's default.
	StateReason string
}

// IssueStateReasonCompleted is GitHub's state_reason for an issue closed
// because it was resolved.
const IssueStateReasonCompleted = "completed"

func ReporterConfirmationCloseGateReason(issue *gh.Issue) string {
	return humanFiledBugReason(issue)
}

func (c *Client) CloseIssue(ctx context.Context, repo string, number int, opts IssueCloseOptions) error {
	_, err := c.closeIssue(ctx, repo, number, opts)
	return err
}

// closeIssue is CloseIssue that also reports whether the closed number was a
// pull request (GitHub's issues API closes PRs too), so the issue-request
// watcher can audit a PR close as pr_closed rather than agent_issue_closed.
func (c *Client) closeIssue(ctx context.Context, repo string, number int, opts IssueCloseOptions) (bool, error) {
	if c == nil || c.client == nil {
		return false, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	if err := validateRepoRef(owner, repoName); err != nil {
		return false, fmt.Errorf("CloseIssue: %w", err)
	}
	if number <= 0 {
		return false, fmt.Errorf("CloseIssue: issue number is required")
	}

	issue, _, err := c.client.Issues.Get(ctx, owner, repoName, number)
	if err != nil {
		return false, fmt.Errorf("reading issue %s/%s#%d before close: %w", owner, repoName, number, err)
	}
	isPR := issue.IsPullRequest()
	reason := ReporterConfirmationCloseGateReason(issue)
	overrideReason := strings.TrimSpace(opts.OverrideReason)
	if reason != "" && overrideReason == "" {
		// Ask ONCE. A blocked close is not necessarily a one-shot event: an
		// agent that retries, or several agents converging on the same issue,
		// would otherwise re-notify the reporter on every attempt. Spamming the
		// reporter of a human-filed bug is the same discourtesy this gate
		// exists to prevent, just in the opposite direction.
		if c.hasReporterConfirmationRequest(ctx, owner, repoName, number) {
			if labelErr := c.applyReporterConfirmationLabels(ctx, owner, repoName, number, issue); labelErr != nil {
				return false, fmt.Errorf("%w: %s; additionally failed to label confirmation request: %v", ErrReporterConfirmationRequired, reason, labelErr)
			}
			c.warn("issue close still blocked pending reporter confirmation; request already posted",
				"repo", owner+"/"+repoName,
				"issue", number,
				"reason", reason)
			return false, fmt.Errorf("%w: %s", ErrReporterConfirmationRequired, reason)
		}
		body := reporterConfirmationRequestComment(issue)
		if _, _, commentErr := c.client.Issues.CreateComment(ctx, owner, repoName, number, &gh.IssueComment{Body: gh.Ptr(body)}); commentErr != nil {
			return false, fmt.Errorf("%w: %s; additionally failed to post confirmation request: %v", ErrReporterConfirmationRequired, reason, commentErr)
		}
		if labelErr := c.applyReporterConfirmationLabels(ctx, owner, repoName, number, issue); labelErr != nil {
			return false, fmt.Errorf("%w: %s; additionally failed to label confirmation request: %v", ErrReporterConfirmationRequired, reason, labelErr)
		}
		c.warn("issue close blocked pending reporter confirmation",
			"repo", owner+"/"+repoName,
			"issue", number,
			"reason", reason)
		return false, fmt.Errorf("%w: %s", ErrReporterConfirmationRequired, reason)
	}
	if reason != "" {
		if !opts.SuppressOverrideComment {
			body := fmt.Sprintf("Reporter-confirmation close override used for this human-filed bug-family issue.\n\nReason: %s", overrideReason)
			if _, _, err := c.client.Issues.CreateComment(ctx, owner, repoName, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
				return false, fmt.Errorf("posting reporter-confirmation override on %s/%s#%d: %w", owner, repoName, number, err)
			}
		}
		c.warn("issue close override used",
			"repo", owner+"/"+repoName,
			"issue", number,
			"reason", reason,
			"override_reason", overrideReason)
	}

	req := &gh.IssueRequest{State: gh.Ptr("closed")}
	if sr := strings.TrimSpace(opts.StateReason); sr != "" {
		req.StateReason = gh.Ptr(sr)
	}
	if _, _, err := c.client.Issues.Edit(ctx, owner, repoName, number, req); err != nil {
		return false, fmt.Errorf("closing issue %s/%s#%d: %w", owner, repoName, number, err)
	}
	if _, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repoName, number, issueNeedsReporterConfirmationLabel); err != nil && !githubStatusError(err, http.StatusNotFound) {
		// Best-effort: the issue is already closed, so a stale label must not fail the close.
		c.logger.Warn("removing reporter-confirmation label after close failed",
			"repo", owner+"/"+repoName, "issue", number, "error", err)
	}
	return isPR, nil
}

// reporterConfirmationRequestMarker identifies a confirmation request the hive
// has already posted. It is the literal opening of the request body, so the
// two cannot drift apart without this file changing.
const reporterConfirmationRequestMarker = "Reporter-confirmation gate:"

func (c *Client) applyReporterConfirmationLabels(ctx context.Context, owner, repo string, number int, issue *gh.Issue) error {
	if err := c.EnsureIssueLabel(ctx, owner+"/"+repo, issueNeedsReporterConfirmationLabel, "", ""); err != nil {
		return fmt.Errorf("ensuring %s label: %w", issueNeedsReporterConfirmationLabel, err)
	}
	labels := issueLabelNames(issue.Labels)
	toAdd := make([]string, 0, 2)
	if !labelPresent(labels, issueNeedsReporterConfirmationLabel) {
		toAdd = append(toAdd, issueNeedsReporterConfirmationLabel)
	}
	if reporterHasWriteAccess(ctx, c, owner, repo, safeGetLogin(issue.GetUser())) && !labelPresent(labels, issueNeedsHumanLabel) {
		toAdd = append(toAdd, issueNeedsHumanLabel)
	}
	if len(toAdd) == 0 {
		return nil
	}
	if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, toAdd); err != nil {
		return fmt.Errorf("adding labels %v: %w", toAdd, err)
	}
	return nil
}

func reporterHasWriteAccess(ctx context.Context, c *Client, owner, repo, login string) bool {
	login = strings.TrimSpace(login)
	if c == nil || c.client == nil || login == "" {
		return false
	}
	perm, _, err := c.client.Repositories.GetPermissionLevel(ctx, owner, repo, login)
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(perm.GetPermission())) {
	case "admin", "maintain", "write":
		return true
	default:
		return false
	}
}

// hasReporterConfirmationRequest reports whether the hive has already asked the
// reporter to confirm. It deliberately fails OPEN (false) when the comments
// cannot be read: a duplicate request is a far smaller harm than silently
// closing with no explanation, and the caller still returns the gate error
// either way. Mirrors checkCommentsForSHA's handling in client.go.
func (c *Client) hasReporterConfirmationRequest(ctx context.Context, owner, repo string, number int) bool {
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := c.client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			c.warn("failed to list comments for reporter-confirmation check",
				"repo", owner+"/"+repo, "issue", number, "error", err)
			return false
		}
		for _, comment := range comments {
			if strings.Contains(comment.GetBody(), reporterConfirmationRequestMarker) {
				return true
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return false
		}
		opts.Page = resp.NextPage
	}
}

func reporterConfirmationRequestComment(issue *gh.Issue) string {
	reporter := strings.TrimSpace(issue.GetUser().GetLogin())
	if reporter != "" {
		reporter = " @" + reporter
	}
	return reporterConfirmationRequestMarker + " this looks like a human-filed bug-family issue, so the hive is leaving it open until the reporter confirms the symptom is gone." +
		reporter + ", please confirm when you have verified the fix. A maintainer can still close deliberately by recording an explicit override reason."
}
