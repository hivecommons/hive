package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/writeback"
)

// The attention pilot (github.notifications.attention_pilot,
// hivecommons/hive#16358) is the opt-in gate every attention-pilot change
// reads. Its first effect is the issue-side status card: the routine notes
// the hive leaves on an issue (claim/release, partial work, dependency
// progress, close/auto-close narration) become sections of the issue's one
// hive:status comment, written through prstatus.Write like the PR card, so
// an issue's lifecycle emails its watchers once instead of once per note.
// A newly required action is the exception: it still gets exactly one
// explicit comment, because an edit notifies nobody.

// SetAttentionPilot wires github.notifications.attention_pilot. Read live so
// a config reload applies to the next write. nil (nothing wired) is off.
func (c *Client) SetAttentionPilot(fn func() bool) {
	if c == nil {
		return
	}
	c.attentionPilotFn = fn
}

// attentionPilot reports whether this hive opted in to the attention pilot.
func (c *Client) attentionPilot() bool {
	return c != nil && c.attentionPilotFn != nil && c.attentionPilotFn()
}

// PostIssueNote delivers a routine issue note. With the attention pilot off
// it runs legacy, the call site's own write, so behaviour is unchanged.
// With it on the note becomes section key of the issue's status card; the
// only comment ever created is the card itself. Reports whether a new
// comment was created.
func (c *Client) PostIssueNote(ctx context.Context, repo string, number int, key, body string, legacy func(context.Context) error) (bool, error) {
	if c == nil {
		return false, ErrNoGitHubClient
	}
	if !c.attentionPilot() {
		return true, legacy(ctx)
	}
	created, err := c.UpsertPRStatusSection(ctx, repo, number, key, body)
	if err == nil && !created {
		c.noteCommentSuppressed()
	}
	return created, err
}

// PostIssueAttention delivers a newly required action (cause names it, e.g.
// the PR whose merge awaits confirmation). With the attention pilot off it
// runs legacy. With it on the action is recorded on the status card and
// announced by exactly one notifying write: creating the card when the
// issue has none, otherwise one explicit comment tagged
// prstatus.IssueAttentionMarker(cause) — a card edit alone would reach
// nobody. A cause already announced is only refreshed on the card. Reports
// whether a new comment was created.
//
// The explicit comment consults the write-back gate. A refusal is returned
// as an error wrapping writeback.ErrSuppressed before anything is written:
// the card carries no marker, so the caller does not treat the action as
// announced and the next pass retries once the policy admits it.
func (c *Client) PostIssueAttention(ctx context.Context, repo string, number int, cause, body string, legacy func(context.Context) error) (bool, error) {
	if c == nil {
		return false, ErrNoGitHubClient
	}
	if !c.attentionPilot() {
		return true, legacy(ctx)
	}
	marker := prstatus.IssueAttentionMarker(cause)
	owner, name := c.splitRepo(repo)
	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		return false, err
	}
	announced, cardExists := false, false
	for _, cm := range comments {
		b := cm.GetBody()
		if strings.Contains(b, marker) {
			announced = true
		}
		if prstatus.IsStatusComment(b) {
			cardExists = true
		}
	}
	posted := false
	// The explicit comment goes first: if the card edit then fails, the
	// next pass finds the marker and only retries the edit, never a second
	// announcement. A gate refusal returns before any write so the marker
	// is never recorded for an announcement that did not happen.
	if !announced && cardExists {
		if err := c.issueAttentionAllowed(ctx, owner, name, number, repo, body); err != nil {
			return false, err
		}
		if err := c.CreateIssueComment(WithNotificationSubject(ctx, prstatus.KeyIssueAttentionPrefix+cause), repo, number, marker+"\n"+body); err != nil { // writeback:gated — issueAttentionAllowed consulted the gate above
			return false, err
		}
		posted = true
	}
	created, err := c.UpsertPRStatusSection(ctx, repo, number, prstatus.KeyIssueAttentionPrefix+cause, body+"\n"+marker)
	if err == nil && !created && !posted {
		c.noteCommentSuppressed()
	}
	return posted || created, err
}

// issueAttentionAllowed consults the write-back gate for the explicit
// attention comment on an issue. A refusal is an error wrapping
// writeback.ErrSuppressed; a failed issue read is returned as is. Either
// way the caller writes nothing and retries on its next pass.
func (c *Client) issueAttentionAllowed(ctx context.Context, owner, name string, number int, repo, body string) error {
	issue, _, _, err := c.getIssueWithBlockedCount(ctx, owner, name, number)
	if err != nil {
		return fmt.Errorf("issue attention %s#%d: reading issue: %w", repo, number, err)
	}
	if issue == nil {
		return fmt.Errorf("issue attention %s#%d: reading issue: empty response", repo, number)
	}
	author := safeGetLogin(issue.GetUser())
	item := writeback.Item{
		Number:       number,
		Author:       author,
		HiveAuthored: issue.GetUser().GetType() == "Bot" && c.IsHiveAuthorLogin(author),
		Labels:       issueLabelNames(issue.Labels),
		CreatedAt:    issue.GetCreatedAt().Time,
	}
	if d := c.WritebackAllowed(repo, item, writeback.KindNeedsDecisionComment, body); !d.Allowed {
		return fmt.Errorf("issue attention %s#%d: %w: %s", repo, number, writeback.ErrSuppressed, d.Reason)
	}
	return nil
}
