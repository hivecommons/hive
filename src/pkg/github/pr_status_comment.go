package github

import (
	"context"
	"strings"

	"github.com/hivecommons/hive/pkg/prstatus"
)

// UpsertPRStatusSection rewrites section key of repo#number's single hive
// status comment (prstatus.Marker) to text, creating the comment only when
// the PR has none. Editing a comment notifies nobody; creating one emails
// the author and every watcher, so this is the quiet-mode delivery for
// every note the hive used to post as its own comment. An empty text
// removes the section. Reports whether a comment was created.
func (c *Client) UpsertPRStatusSection(ctx context.Context, repo string, number int, key, text string) (bool, error) {
	if c == nil {
		return false, ErrNoGitHubClient
	}
	current, err := c.UpsertIssueMarkerComment(ctx, repo, number, prstatus.Marker, "", false)
	if err != nil {
		return false, err
	}
	if !current.Found && strings.TrimSpace(text) == "" {
		return false, nil
	}
	next := prstatus.Upsert(current.Previous, key, text)
	if current.Found && next == current.Previous {
		return false, nil
	}
	res, err := c.UpsertIssueMarkerComment(ctx, repo, number, prstatus.Marker, next, true)
	if err != nil {
		return false, err
	}
	return res.Created, nil
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
