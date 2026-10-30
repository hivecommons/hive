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
