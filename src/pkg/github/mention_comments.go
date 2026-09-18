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

// ListMentionComments lists recently-created issue/PR comments across one repo.
func (c *Client) ListMentionComments(ctx context.Context, repo string, since time.Time) ([]mention.Event, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
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
		out = append(out, mention.Event{Repo: owner + "/" + repoName, Kind: kind, Number: number, NodeID: ic.GetNodeID(), CommentID: ic.GetID(), HTMLURL: ic.GetHTMLURL(), Author: safeGetLogin(ic.GetUser()), Body: ic.GetBody(), CreatedAt: ic.GetCreatedAt().Time, UpdatedAt: ic.GetUpdatedAt().Time})
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

func (c *Client) CreateMentionAck(ctx context.Context, repo string, commentID int64, reaction string) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	_, _, err := c.client.Reactions.CreateIssueCommentReaction(ctx, owner, repoName, commentID, reaction)
	if err != nil {
		return fmt.Errorf("creating mention ack reaction for %s comment %d: %w", owner+"/"+repoName, commentID, err)
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
