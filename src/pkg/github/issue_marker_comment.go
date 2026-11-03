package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// IssueMarkerCommentResult reports what EditIssueMarkerComment did.
type IssueMarkerCommentResult struct {
	// Found is set when a comment carrying the marker already existed;
	// Previous is its body before the edit.
	Found    bool
	Previous string
	// Created is set when the body was posted as a new comment.
	Created bool
	// Edited is set when the existing comment was rewritten.
	Edited bool
}

// EditIssueMarkerComment finds the hive's own comment on repo#number that
// carries marker, asks render for the body that should replace it (render
// receives the previous body, or "" when none exists) and edits it in
// place; when no such comment exists the rendered body is posted as a new
// comment through the per-issue comment budget. An empty rendered body is
// a no-op, as is an unchanged one.
//
// Editing notifies nobody, which is the point: the claim ledger
// (hivecommons/hive#16433) replaces one comment per claim transition with
// one comment per issue. Only a comment one of the hive's own accounts
// wrote is edited; when the hive cannot name its accounts, any author is
// accepted, the same fallback the sentinel's marker reads use.
func (c *Client) EditIssueMarkerComment(ctx context.Context, repo string, number int, marker string, render func(previous string) string) (IssueMarkerCommentResult, error) {
	if c == nil {
		return IssueMarkerCommentResult{}, ErrNoGitHubClient
	}
	if render == nil {
		return IssueMarkerCommentResult{}, fmt.Errorf("editing marker comment on %s#%d: no renderer", repo, number)
	}
	owner, repoName := c.splitRepo(repo)
	comments, err := c.listIssueComments(ctx, owner, repoName, number)
	if err != nil {
		return IssueMarkerCommentResult{}, err
	}
	var existing *gh.IssueComment
	for _, cm := range comments {
		if cm == nil || !strings.Contains(cm.GetBody(), marker) || !c.markerAuthorTrusted(cm.GetUser()) {
			continue
		}
		existing = cm
		break
	}
	if existing == nil {
		body := render("")
		if strings.TrimSpace(body) == "" {
			return IssueMarkerCommentResult{}, nil
		}
		if err := c.CreateIssueCommentBudgeted(ctx, owner+"/"+repoName, number, body); err != nil {
			return IssueMarkerCommentResult{}, err
		}
		return IssueMarkerCommentResult{Created: true}, nil
	}
	res := IssueMarkerCommentResult{Found: true, Previous: existing.GetBody()}
	body := render(res.Previous)
	if strings.TrimSpace(body) == "" || body == res.Previous {
		return res, nil
	}
	// Rewrites a comment the hive already owns; an edit notifies nobody.
	if _, _, err := c.client.Issues.EditComment(ctx, owner, repoName, existing.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
		return res, fmt.Errorf("editing marker comment on %s/%s#%d: %w", owner, repoName, number, err)
	}
	c.noteCommentEdited()
	res.Edited = true
	return res, nil
}
