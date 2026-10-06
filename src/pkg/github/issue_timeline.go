package github

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// IssueEvent is one entry of an issue's timeline, reduced to the fields the
// claim escalation and verify-once gates (#10527) read to decide whether
// anything moved.
type IssueEvent struct {
	// Event is GitHub's timeline event name: "labeled", "cross-referenced",
	// "referenced", "assigned", "closed", ...
	Event string
	// At is when the event happened.
	At time.Time
	// Label is the label name on labeled / unlabeled events.
	Label string
	// SourcePR is the referencing pull request's number on a cross-referenced
	// event whose source is a pull request; 0 otherwise.
	SourcePR int
	// CommitID is the referencing commit on a referenced event.
	CommitID string
	// Actor is the login that caused the event (the commenter on a
	// "commented" event); empty when GitHub names none.
	Actor string
	// ActorIsBot reports that Actor is a bot or GitHub App account, so the
	// verify-once gate (#10527) can tell the hive's own comments from a
	// reporter or maintainer acting.
	ActorIsBot bool
}

// IssueTimelineSince returns the issue's timeline events that happened after
// since, oldest first. Events without a timestamp (commits and reviews on a
// pull request's timeline) are skipped.
func (c *Client) IssueTimelineSince(ctx context.Context, repo string, number int, since time.Time) ([]IssueEvent, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	opts := &gh.ListOptions{PerPage: 100}
	var out []IssueEvent
	for {
		page, resp, err := c.client.Issues.ListIssueTimeline(ctx, owner, repoName, number, opts)
		if err != nil {
			return nil, err
		}
		for _, t := range page {
			if ev, ok := issueEventFrom(t, since); ok {
				out = append(out, ev)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

func issueEventFrom(t *gh.Timeline, since time.Time) (IssueEvent, bool) {
	if t == nil || t.CreatedAt == nil || !t.CreatedAt.After(since) {
		return IssueEvent{}, false
	}
	ev := IssueEvent{Event: t.GetEvent(), At: t.CreatedAt.Time, CommitID: t.GetCommitID()}
	if t.Label != nil {
		ev.Label = t.Label.GetName()
	}
	if src := t.GetSource(); src != nil && src.Issue != nil && src.Issue.IsPullRequest() {
		ev.SourcePR = src.Issue.GetNumber()
	}
	actor := t.GetActor()
	if actor == nil {
		actor = t.GetUser()
	}
	if actor != nil {
		ev.Actor = actor.GetLogin()
		ev.ActorIsBot = strings.EqualFold(actor.GetType(), "Bot") || strings.HasSuffix(strings.ToLower(ev.Actor), "[bot]")
	}
	return ev, true
}
