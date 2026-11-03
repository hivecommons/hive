package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/attention"
)

// replyAttentionCommands answers `/hive ack` and `/hive snooze <duration>`
// on an item that has unresolved attention episodes (hivecommons/hive#16359).
// Only the admitting hive answers, so a fleet replies once. The commands
// record reminder state on the episode and refresh the attention status
// section; they never add or remove a label, never release a security,
// merge or review hold and never open a merge gate. Like the un-park
// commands, a caller who is neither the episode's recipient nor a
// write/maintain/admin maintainer is ignored in silence, and the reply's
// ID marker keeps every command answered at most once.
func (c *Client) replyAttentionCommands(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, cutoff time.Time) (int, error) {
	if c.attentionConfig == nil || c.attentionHiveID == "" || c.attentionConfig().Owner(displayRepo) != c.attentionHiveID {
		return 0, nil
	}
	number := issue.GetNumber()
	store, err := c.WritebackGate().AttentionStore()
	if err != nil {
		return 0, err
	}
	open := false
	for _, e := range store.Snapshot(displayRepo, number) {
		open = open || e.State != attention.Resolved
	}
	if !open {
		return 0, nil
	}
	comments, err := c.listIssueCommentsSince(ctx, owner, repo, number, cutoff)
	if err != nil {
		return 0, err
	}
	answered := c.answeredUnparkCommentIDs(comments)
	replies := 0
	for _, cm := range comments {
		if cm == nil || cm.GetID() <= 0 || answered[cm.GetID()] || cm.GetCreatedAt().Time.Before(cutoff) || cm.GetUpdatedAt().Time.After(cm.GetCreatedAt().Time) {
			continue
		}
		cmd, ok, parseErr := attention.ParseCommand(cm.GetBody())
		if !ok || !c.isHumanAuthor(cm.GetUser()) {
			continue
		}
		actor := safeGetLogin(cm.GetUser())
		maintainer, err := c.commenterMayUnpark(ctx, owner, repo, actor, false)
		if err != nil {
			return replies, err
		}
		var body string
		if parseErr != nil {
			if !maintainer && !recipientOf(store.Snapshot(displayRepo, number), actor) {
				continue
			}
			body = fmt.Sprintf("@%s nothing was recorded: %s.\n\n%s", actor, strings.TrimSuffix(parseErr.Error(), "."), attention.NotApproval)
		} else {
			episodes, applyErr := store.Apply(displayRepo, number, actor, maintainer, cmd)
			switch {
			case errors.Is(applyErr, attention.ErrNotPermitted):
				continue
			case errors.Is(applyErr, attention.ErrNoEpisode):
				if !maintainer && !recipientOf(store.Snapshot(displayRepo, number), actor) {
					continue
				}
				body = fmt.Sprintf("@%s nothing was recorded: no open attention episode on this item matches `%s`.", actor, cmd.Target)
				if cmd.Target == "" {
					body = fmt.Sprintf("@%s nothing was recorded: this item has no open attention episode.", actor)
				}
			case applyErr != nil:
				return replies, applyErr
			default:
				body = renderAttentionCommandReply(actor, cmd, episodes)
				// The ack is already durable; a stale card must not cost the reply.
				if _, err := c.UpsertPRStatusSection(ctx, displayRepo, number, "attention", attentionStatus(store.Snapshot(displayRepo, number))); err != nil && c.logger != nil {
					c.logger.Warn("attention status refresh after command failed", "repo", displayRepo, "item", number, "error", err)
				}
			}
		}
		if err := c.postUnparkReply(ctx, owner, repo, number, cm.GetID(), body); err != nil {
			return replies, err
		}
		replies++
	}
	return replies, nil
}

func recipientOf(episodes []attention.Episode, actor string) bool {
	for _, e := range episodes {
		if e.State != attention.Resolved && attention.MayAct(e, actor, false) {
			return true
		}
	}
	return false
}

func renderAttentionCommandReply(actor string, cmd attention.Command, episodes []attention.Episode) string {
	var b strings.Builder
	switch cmd.Kind {
	case attention.CommandAck:
		fmt.Fprintf(&b, "@%s acknowledged %d attention episode(s); reminders for them stop.\n", actor, len(episodes))
	case attention.CommandSnooze:
		until := time.Time{}
		if len(episodes) > 0 {
			until = episodes[0].SnoozedUntil
		}
		fmt.Fprintf(&b, "@%s snoozed reminders for %d attention episode(s) until %s.\n", actor, len(episodes), until.UTC().Format("2006-01-02T15:04Z"))
	}
	for _, e := range episodes {
		fmt.Fprintf(&b, "- `%s`: %s → %s\n", e.Key.Hash()[:12], statusText(e.Key.Action), statusText(e.Key.Recipient))
	}
	b.WriteString("\n" + attention.NotApproval + "\n")
	return b.String()
}
