package github

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/mention"
)

// ListMentionComments lists recently-created mention-bearing surfaces across
// one repo: issue/PR comments, PR review comments, and opened issues.
func (c *Client) ListMentionComments(ctx context.Context, repo string, since time.Time) ([]mention.Event, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	var out []mention.Event
	issueComments, err := c.listMentionIssueComments(ctx, owner, repoName, since)
	if err != nil {
		return nil, err
	}
	out = append(out, issueComments...)
	reviewComments, err := c.listMentionReviewComments(ctx, owner, repoName, since)
	if err != nil {
		return nil, err
	}
	out = append(out, reviewComments...)
	issues, err := c.listMentionOpenedIssues(ctx, owner, repoName, since)
	if err != nil {
		return nil, err
	}
	out = append(out, issues...)
	return out, nil
}

func (c *Client) listMentionIssueComments(ctx context.Context, owner, repoName string, since time.Time) ([]mention.Event, error) {
	path := fmt.Sprintf("repos/%s/%s/issues/comments?per_page=100", url.PathEscape(owner), url.PathEscape(repoName))
	if !since.IsZero() {
		path += "&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
	}
	var all []*gh.IssueComment
	for path != "" {
		req, err := c.client.NewRequest("GET", path, nil)
		if err != nil {
			return nil, err
		}
		var page []*gh.IssueComment
		resp, err := c.client.Do(ctx, req, &page)
		if err != nil {
			return nil, fmt.Errorf("listing mention comments for %s/%s: %w", owner, repoName, err)
		}
		all = append(all, page...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		path = fmt.Sprintf("repos/%s/%s/issues/comments?per_page=100&page=%d", url.PathEscape(owner), url.PathEscape(repoName), resp.NextPage)
		if !since.IsZero() {
			path += "&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
		}
	}
	out := make([]mention.Event, 0, len(all))
	for _, ic := range all {
		number := issueNumberFromURL(ic.GetIssueURL())
		kind := "issue"
		if strings.Contains(ic.GetHTMLURL(), "/pull/") {
			kind = "pr"
		}
		body := ic.GetBody()
		_, marker := mention.ExtractActionMarker(body)
		out = append(out, mention.Event{Repo: owner + "/" + repoName, Kind: kind, Number: number, NodeID: ic.GetNodeID(), CommentID: ic.GetID(), HTMLURL: ic.GetHTMLURL(), Author: safeGetLogin(ic.GetUser()), Body: body, Action: marker, CreatedAt: ic.GetCreatedAt().Time, UpdatedAt: ic.GetUpdatedAt().Time})
	}
	return out, nil
}

func (c *Client) listMentionReviewComments(ctx context.Context, owner, repoName string, since time.Time) ([]mention.Event, error) {
	opts := &gh.PullRequestListCommentsOptions{Sort: "updated", Direction: "asc", Since: since, ListOptions: gh.ListOptions{PerPage: 100}}
	var out []mention.Event
	for {
		comments, resp, err := c.client.PullRequests.ListComments(ctx, owner, repoName, 0, opts)
		if err != nil {
			return nil, fmt.Errorf("listing mention review comments for %s/%s: %w", owner, repoName, err)
		}
		for _, rc := range comments {
			body := rc.GetBody()
			_, marker := mention.ExtractActionMarker(body)
			out = append(out, mention.Event{
				Repo:      owner + "/" + repoName,
				Kind:      "review_comment",
				Number:    issueNumberFromURL(rc.GetPullRequestURL()),
				NodeID:    rc.GetNodeID(),
				CommentID: rc.GetID(),
				HTMLURL:   rc.GetHTMLURL(),
				Author:    safeGetLogin(rc.GetUser()),
				Body:      body,
				Action:    marker,
				CreatedAt: rc.GetCreatedAt().Time,
				UpdatedAt: rc.GetUpdatedAt().Time,
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

func (c *Client) listMentionOpenedIssues(ctx context.Context, owner, repoName string, since time.Time) ([]mention.Event, error) {
	opts := &gh.IssueListByRepoOptions{State: "all", Sort: "updated", Direction: "asc", Since: since, ListOptions: gh.ListOptions{PerPage: 100}}
	var out []mention.Event
	for {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repoName, opts)
		if err != nil {
			return nil, fmt.Errorf("listing mention issues for %s/%s: %w", owner, repoName, err)
		}
		for _, issue := range issues {
			if issue.IsPullRequest() {
				continue
			}
			body := strings.TrimSpace(issue.GetTitle() + "\n\n" + issue.GetBody())
			_, marker := mention.ExtractActionMarker(body)
			out = append(out, mention.Event{
				Repo:      owner + "/" + repoName,
				Kind:      "issue",
				Number:    issue.GetNumber(),
				NodeID:    issue.GetNodeID(),
				HTMLURL:   issue.GetHTMLURL(),
				Author:    safeGetLogin(issue.GetUser()),
				Body:      body,
				Action:    marker,
				CreatedAt: issue.GetCreatedAt().Time,
				UpdatedAt: issue.GetUpdatedAt().Time,
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

func issueNumberFromURL(s string) int {
	parts := strings.Split(strings.TrimRight(s, "/"), "/")
	if len(parts) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(parts[len(parts)-1])
	return n
}

func (c *Client) CreateMentionAck(ctx context.Context, ev mention.Event, reaction string) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(ev.Repo)
	var err error
	if ev.Kind == "review_comment" {
		_, _, err = c.client.Reactions.CreatePullRequestCommentReaction(ctx, owner, repoName, ev.CommentID, reaction)
	} else {
		_, _, err = c.client.Reactions.CreateIssueCommentReaction(ctx, owner, repoName, ev.CommentID, reaction)
	}
	if err != nil {
		return fmt.Errorf("creating mention ack reaction for %s comment %d: %w", owner+"/"+repoName, ev.CommentID, err)
	}
	return nil
}

func (c *Client) CountAppAuthoredComments(ctx context.Context, repo string, number int) (int, error) {
	if c == nil || c.client == nil {
		return 0, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	count := 0
	for {
		comments, resp, err := c.client.Issues.ListComments(ctx, owner, repoName, number, opts)
		if err != nil {
			return 0, fmt.Errorf("counting app comments for %s#%d: %w", owner+"/"+repoName, number, err)
		}
		for _, cm := range comments {
			if strings.EqualFold(safeGetLogin(cm.GetUser()), c.appBotLogin) {
				count++
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return count, nil
}

func (c *Client) RecordMentionAudit(action, repo string, number int, commentID int64, author, agent, guard string) {
	if c == nil {
		return
	}
	extra := []string{"repo", repo, "number", strconv.Itoa(number), "comment_id", strconv.FormatInt(commentID, 10), "author", author}
	if guard != "" {
		extra = append(extra, "guard", guard)
	}
	c.recordCreationAudit(action, InvocationMeta{Agent: agent}, extra...)
}
