package github

import (
	"context"
	"fmt"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/prstatus"
)

// UpsertPRStatusSection rewrites section key of repo#number's single hive
// status comment to text, creating the comment only when
// the PR has none. Editing a comment notifies nobody; creating one emails
// the author and every watcher, so this is the quiet-mode delivery for
// every note the hive used to post as its own comment. An empty text
// removes the section. Reports whether a comment was created.
//
// The write goes through prstatus.Write, the one status writer the
// auto-merge engine shares, so it is serialised per item, retried when the
// comment changed between read and edit, and duplicate status comments are
// merged into the oldest.
func (c *Client) UpsertPRStatusSection(ctx context.Context, repo string, number int, key, text string) (bool, error) {
	if c == nil {
		return false, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	f := &clientStatusForge{c: c, owner: owner, repo: repoName, number: number}
	res, err := prstatus.Write(ctx, f, prstatus.ItemKey(owner+"/"+repoName, number), key, text)
	return res.Created, err
}

// clientStatusForge adapts the client's comment calls on one item to
// prstatus.Forge. Creates go through CreateIssueComment (canary gate,
// effects journal, created tally); edits are tallied as edited.
type clientStatusForge struct {
	c      *Client
	owner  string
	repo   string
	number int
}

func (f *clientStatusForge) ListComments(ctx context.Context) ([]prstatus.Comment, error) {
	comments, err := f.c.listIssueComments(ctx, f.owner, f.repo, f.number)
	if err != nil {
		return nil, err
	}
	out := make([]prstatus.Comment, 0, len(comments))
	for _, cm := range comments {
		if cm == nil {
			continue
		}
		out = append(out, prstatus.Comment{ID: cm.GetID(), Body: cm.GetBody(), Own: f.c.hiveAuthoredLogin(cm.GetUser().GetLogin())})
	}
	return out, nil
}

func (f *clientStatusForge) EditComment(ctx context.Context, id int64, body string) error {
	if _, _, err := f.c.client.Issues.EditComment(ctx, f.owner, f.repo, id, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		return fmt.Errorf("editing hive status comment on %s/%s#%d: %w", f.owner, f.repo, f.number, err)
	}
	f.c.noteCommentEdited()
	return nil
}

func (f *clientStatusForge) CreateComment(ctx context.Context, body string) error {
	return f.c.CreateIssueComment(ctx, f.owner+"/"+f.repo, f.number, body)
}

func (f *clientStatusForge) DeleteComment(ctx context.Context, id int64) error {
	if _, err := f.c.client.Issues.DeleteComment(ctx, f.owner, f.repo, id); err != nil {
		return fmt.Errorf("deleting hive status comment on %s/%s#%d: %w", f.owner, f.repo, f.number, err)
	}
	return nil
}

// PostNoteOrFold delivers a hive note on repo#number. In quiet mode
// (github.notifications.mode) it rewrites section key of the item's status
// comment, so the only comment ever created is the status comment itself;
// in normal mode it posts body as a comment of its own, as the hive always
// did. The returned bool reports whether a new comment was created.
func (c *Client) PostNoteOrFold(ctx context.Context, repo string, number int, key, body string) (bool, error) {
	if c == nil {
		return false, ErrNoGitHubClient
	}
	if !c.notificationsQuiet() {
		return true, c.CreateIssueComment(ctx, repo, number, body)
	}
	created, err := c.UpsertPRStatusSection(ctx, repo, number, key, body)
	if err == nil && !created {
		c.noteCommentSuppressed()
	}
	return created, err
}

// PostClaimNote posts a contributor's `<!-- hive:claim … -->` comment body
// through PostNoteOrFold under prstatus.KeyClaim: claim readers scan every
// hive comment body for the marker, so a status section carries it as
// well as a comment of its own did.
func (c *Client) PostClaimNote(ctx context.Context, repo string, number int, body string) error {
	_, err := c.PostNoteOrFold(ctx, repo, number, prstatus.KeyClaim, body)
	return err
}
