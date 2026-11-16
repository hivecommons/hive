package github

import (
	"context"
	"strings"
	"time"
)

// IssueCommentEntry is one issue comment with the authorship and time a
// caller outside this package needs to reason about who said what, and in
// which order. The contribute queue reads it to see whether an issue's author
// answered a question a contributor asked (pkg/dashboard
// contribute_author_reply.go).
type IssueCommentEntry struct {
	ID     int64
	Author string
	// AuthorIsBot is true for a GitHub Bot account or a "[bot]" login.
	AuthorIsBot bool
	Body        string
	CreatedAt   time.Time
}

// IssueCommentsSince lists repo#number's comments updated at or after since,
// oldest first; a zero since lists every comment. GitHub's since filter is on
// updated_at and is rounded down to the hour for the ETag cache, so callers
// get a few older comments and must apply their own cutoff on CreatedAt.
// One paginated REST read, normally a single page; a repeat read of an
// unchanged issue is a conditional 304.
func (c *Client) IssueCommentsSince(ctx context.Context, repo string, number int, since time.Time) ([]IssueCommentEntry, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	comments, err := c.listIssueCommentsSince(ctx, owner, name, number, since)
	if err != nil {
		return nil, err
	}
	out := make([]IssueCommentEntry, 0, len(comments))
	for _, cm := range comments {
		if cm == nil {
			continue
		}
		login := strings.TrimSpace(cm.GetUser().GetLogin())
		out = append(out, IssueCommentEntry{
			ID:          cm.GetID(),
			Author:      login,
			AuthorIsBot: strings.EqualFold(cm.GetUser().GetType(), "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]"),
			Body:        cm.GetBody(),
			CreatedAt:   cm.GetCreatedAt().Time,
		})
	}
	return out, nil
}
