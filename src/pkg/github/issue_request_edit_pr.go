package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// The `edit_pr` relay operation (hivecommons/hive#19399).
//
// A PR's title and body go stale when the PR changes after review, and the
// lane that answers the review is often not the lane that opened the PR. The
// relays had no operation for a title or body, so a lane listed under
// write_surface.enforce, or one whose direct PATCH /pulls/{n} is refused, could
// not correct a description even on a PR the hive itself opened. The
// issue-request watcher's `edit_pr` kind closes that gap with the same
// file-UID authorizer, lane allowlist, repo pause, repo scope, outbound prose
// filter and redacted audit entry as every other relay operation.
//
// Its own content gate is authorship: the relay edits only pull requests whose
// author is one of this hive's accounts (the App bot login or
// project.ai_author). Hive never edits a human-authored PR body, and "the
// author could not be read" never resolves into editing one. A merged PR is
// refused too: its description is the record of what landed.

// editPRShapeError returns why an edit_pr request can never succeed, or ""
// when it is well formed. The body checks are the comment relay's: a body
// that still carries a template placeholder or literal "\n" sequences is a
// broken render, not a description.
func editPRShapeError(req IssueRequest) string {
	if strings.TrimSpace(req.Repo) == "" || req.Number <= 0 || strings.TrimSpace(req.Agent) == "" {
		return "edit_pr request requires repo, number, and agent"
	}
	if strings.TrimSpace(req.Title) == "" && strings.TrimSpace(req.Body) == "" {
		return "edit_pr request requires a title or a body (an empty body cannot be written through the relay)"
	}
	for _, field := range []struct{ name, text string }{{"title", req.Title}, {"body", req.Body}} {
		if placeholder, ok := issueUnsubstitutedTemplatePlaceholder(field.text); ok {
			return "edit_pr request " + field.name + " contains unsubstituted template placeholder " + strconv.Quote(placeholder)
		}
	}
	if strings.Contains(req.Title, "\n") {
		return "edit_pr request title must be a single line"
	}
	if issueBodyHasMisEscapedNewlines(req.Body) {
		return "edit_pr request body contains literal newline escape sequences; use real newlines or --body-file"
	}
	return ""
}

// isHiveAccount reports whether user is positively one of this hive's own
// accounts: the App bot login or a project.ai_author identity. Unlike
// !isHumanAuthor it does not count every bot (dependabot, another App) as
// ours, and an empty login is never ours.
func (c *Client) isHiveAccount(user *gh.User) bool {
	if c == nil {
		return false
	}
	login := strings.TrimSpace(user.GetLogin())
	if login == "" {
		return false
	}
	if bot := strings.TrimSpace(c.appBotLogin); bot != "" && strings.EqualFold(login, bot) {
		return true
	}
	return c.getHiveIdentity().Matches(login)
}

// editPRRefusal reads the target PR and reports whether the edit_pr relay must
// refuse it, and why. A lookup failure is returned as err for the watcher's
// retry/quarantine classification (a 404, i.e. not a PR, is a definite
// refusal and is quarantined), never as permission to edit.
func (c *Client) editPRRefusal(ctx context.Context, repo string, number int) (string, bool, error) {
	if c == nil || c.client == nil {
		return "", false, ErrNoGitHubClient
	}
	if !c.commentOnlyGuardActive() {
		return "edit_pr refused: this hive has no configured identity (project.ai_author or App bot login), so it cannot tell its own pull requests from anyone else's", true, nil
	}
	owner, repoName := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:edit_pr_relay"), owner, repoName, number)
	if err != nil {
		return "", false, err
	}
	if pr == nil {
		return fmt.Sprintf("edit_pr refused: %s#%d could not be read as a pull request", repo, number), true, nil
	}
	if !c.isHiveAccount(pr.GetUser()) {
		login := strings.TrimSpace(SafeGetLogin(pr.GetUser()))
		if login == "" {
			login = "an unidentifiable account"
		} else {
			login = "@" + login
		}
		return fmt.Sprintf("edit_pr refused: %s#%d was opened by %s, not by one of this hive's accounts; the hive never edits a human-authored pull request's title or body. Leave a comment instead", repo, number, login), true, nil
	}
	if pr.GetMerged() {
		return fmt.Sprintf("edit_pr refused: %s#%d is merged; its description is the record of what landed. Leave a comment instead", repo, number), true, nil
	}
	return "", false, nil
}

// EditPullRequest replaces a pull request's title and/or body. A nil field is
// left as it is; at least one must be set.
func (c *Client) EditPullRequest(ctx context.Context, repo string, number int, title, body *string) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	if title == nil && body == nil {
		return nil
	}
	owner, repoName := c.splitRepo(repo)
	_, _, err := c.client.PullRequests.Edit(ctx, owner, repoName, number, &gh.PullRequest{Title: title, Body: body}) // writeback:solicited edit_pr relay, hive-authored PR only (editPRRefusal)
	return err
}
