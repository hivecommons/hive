package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// IssueThread is the read-only view of one issue that the question
// auto-closer (hivecommons/hive#9584) decides on: is it still open, who filed
// it, what is it labelled, and what has been said on it since the answer.
type IssueThread struct {
	// State is GitHub's issue state: "open" or "closed".
	State string
	// Author is the login that filed the issue.
	Author string
	// Labels are the issue's current label names.
	Labels []string
	// IsPullRequest is true when the number is a pull request, which the
	// auto-closer never touches.
	IsPullRequest bool
	// BugFamily is true when CloseIssue's reporter-confirmation gate would
	// refuse this close (a human-filed bug). The auto-closer treats it as out
	// of scope rather than tripping the gate's comment.
	BugFamily bool
	// Comments are every comment on the issue, oldest first.
	Comments []ThreadComment
}

// ThreadComment is one issue comment in an IssueThread.
type ThreadComment struct {
	ID        int64
	Author    string
	Body      string
	CreatedAt time.Time
}

// IssueThread reads one issue and all of its comments.
func (c *Client) IssueThread(ctx context.Context, repo string, number int) (IssueThread, error) {
	if c == nil || c.client == nil {
		return IssueThread{}, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	if err := validateRepoRef(owner, repoName); err != nil {
		return IssueThread{}, fmt.Errorf("IssueThread: %w", err)
	}
	issue, _, err := c.client.Issues.Get(ctx, owner, repoName, number)
	if err != nil {
		return IssueThread{}, fmt.Errorf("reading issue %s/%s#%d: %w", owner, repoName, number, err)
	}
	thread := IssueThread{
		State:         issue.GetState(),
		Author:        issue.GetUser().GetLogin(),
		IsPullRequest: issue.IsPullRequest(),
		BugFamily:     ReporterConfirmationCloseGateReason(issue) != "",
	}
	for _, l := range issue.Labels {
		if name := l.GetName(); name != "" {
			thread.Labels = append(thread.Labels, name)
		}
	}
	comments, err := c.listIssueComments(ctx, owner, repoName, number)
	if err != nil {
		return IssueThread{}, err
	}
	for _, cm := range comments {
		thread.Comments = append(thread.Comments, ThreadComment{
			ID:        cm.GetID(),
			Author:    cm.GetUser().GetLogin(),
			Body:      cm.GetBody(),
			CreatedAt: cm.GetCreatedAt().Time,
		})
	}
	return thread, nil
}

// CommentReactors returns the logins that reacted to an issue comment with
// the given reaction content ("+1", "-1", ...).
func (c *Client) CommentReactors(ctx context.Context, repo string, commentID int64, content string) ([]string, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	if err := validateRepoRef(owner, repoName); err != nil {
		return nil, fmt.Errorf("CommentReactors: %w", err)
	}
	opts := &gh.ListReactionOptions{Content: content, ListOptions: gh.ListOptions{PerPage: 100}}
	var logins []string
	for {
		reactions, resp, err := c.client.Reactions.ListIssueCommentReactions(ctx, owner, repoName, commentID, opts)
		if err != nil {
			return nil, fmt.Errorf("listing reactions on %s/%s comment %d: %w", owner, repoName, commentID, err)
		}
		for _, r := range reactions {
			if content != "" && r.GetContent() != content {
				continue
			}
			if login := r.GetUser().GetLogin(); login != "" {
				logins = append(logins, login)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return logins, nil
		}
		opts.Page = resp.NextPage
	}
}

// IsHiveAuthorLogin reports whether login is one of this hive's own posting
// identities: the GitHub App bot login (SetAppBotLogin) or an account in the
// configured HiveIdentity (project.ai_author, the App slug's "[bot]" login).
// Any other account, including other bots, is not the hive. With no identity
// configured nothing matches, so callers that need hive authorship fail closed.
func (c *Client) IsHiveAuthorLogin(login string) bool {
	login = strings.TrimSpace(login)
	if c == nil || login == "" {
		return false
	}
	if c.appBotLogin != "" && strings.EqualFold(login, c.appBotLogin) {
		return true
	}
	return c.getHiveIdentity().Matches(login)
}
