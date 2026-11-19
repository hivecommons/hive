package github

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/sentinel"
)

// Sentinel notices in the status card (hivecommons/hive#18453).
//
// A sentinel result is one of two kinds:
//
//   - actionable: the finding blocks (untrusted author, or
//     sentinel.trusted_authors_block), so the alert label goes on and a
//     maintainer has to review or clear it. That always gets its own
//     `<!-- hive-sentinel -->` comment, in every notification mode, because
//     a new comment is the only write that notifies anyone.
//   - informational: the author is trusted and the merge is not blocked.
//     Nobody has to act. In github.notifications.mode: quiet this becomes the
//     `sentinel` section of the PR's hive:status card, edited in place, so a
//     trusted author's PR is not emailed a notice per head. In normal mode it
//     is still its own comment, as before.
//
// The card section carries SentinelNoticeCardMarker, never SentinelMarker:
// the sweep reads its own sentinel comment back (which head it alerted on,
// whether a maintainer cleared it), and a card that carried that marker
// would be read as the sentinel comment and could be rewritten into an
// alert that notifies nobody.
const (
	// SentinelStatusKey is the hive:status section holding the
	// informational sentinel notice in quiet mode.
	SentinelStatusKey = "sentinel"
	// SentinelNoticeCardMarker stamps the card section with the head it
	// describes. It deliberately does not contain SentinelMarker.
	SentinelNoticeCardMarker = "<!-- hive-sentinel-notice"
	// sentinelCardMaxPaths bounds how many paths one rule lists in the card
	// section; the full list stays on the dashboard and /api/sentinel.
	sentinelCardMaxPaths = 5
)

// sentinelFoldsNotice reports whether an informational (non-blocking)
// sentinel notice goes into the status card rather than a comment of its
// own. It folds only in quiet mode, and only when the PR carries no earlier
// sentinel comment of the hive's: that comment is edited in place for the
// new head (no notification) so the PR never shows a stale notice next to
// the card.
func (c *Client) sentinelFoldsNotice(hasPriorComment bool) bool {
	return c.notificationsQuiet() && !hasPriorComment
}

// foldSentinelNotice writes the informational notice for head as the
// sentinel section of repo#number's status card. The card is created only
// when the PR has none; every later write is an in-place edit.
func (c *Client) foldSentinelNotice(ctx context.Context, repo string, number int, head string, decision SentinelDecision, findings []sentinel.Finding) error {
	created, err := c.UpsertPRStatusSection(ctx, repo, number, SentinelStatusKey, SentinelStatusSection(decision, head, findings))
	if err != nil {
		return fmt.Errorf("folding sentinel notice into status card: %w", err)
	}
	if !created {
		c.noteCommentSuppressed()
	}
	return nil
}

// clearSentinelNoticeSection drops a stale informational section once the
// same PR is alerted on, so the card never says "merge is not blocked" next
// to an alert label. Removing a section never creates a card. Best effort:
// the alert comment has already been posted and is what matters.
func (c *Client) clearSentinelNoticeSection(ctx context.Context, repo string, number int) {
	if !c.notificationsQuiet() {
		return
	}
	if _, err := c.UpsertPRStatusSection(ctx, repo, number, SentinelStatusKey, ""); err != nil {
		c.logger.Warn("sentinel: clearing informational status-card section failed", "repo", repo, "number", number, "error", err)
	}
}

// SentinelStatusSection is the status-card text for an informational
// sentinel notice: one line naming the trust reason and head, then each
// matched rule with up to sentinelCardMaxPaths paths. It names no login, so
// it notifies nobody even before the writer neutralises mentions.
func SentinelStatusSection(decision SentinelDecision, head string, findings []sentinel.Finding) string {
	reason := strings.TrimSpace(decision.TrustedReason)
	if reason == "" {
		reason = "trusted author"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s -->\n", SentinelNoticeCardMarker, head)
	fmt.Fprintf(&b, "ℹ️ **Sentinel:** informational, author is trusted (%s), merge is not blocked. Head `%s` matched:\n", reason, shortSHA(head))
	for _, f := range findings {
		fmt.Fprintf(&b, "- `%s`: %s", f.Rule, f.Summary)
		paths := append([]string(nil), f.Paths...)
		sort.Strings(paths)
		if len(paths) > 0 {
			shown := paths
			if len(shown) > sentinelCardMaxPaths {
				shown = shown[:sentinelCardMaxPaths]
			}
			quoted := make([]string, 0, len(shown))
			for _, p := range shown {
				quoted = append(quoted, "`"+p+"`")
			}
			fmt.Fprintf(&b, " (%s", strings.Join(quoted, ", "))
			if extra := len(paths) - len(shown); extra > 0 {
				fmt.Fprintf(&b, ", +%d more", extra)
			}
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nNo `%s` label was added. The finding is recorded in the dashboard Security tab and `/api/sentinel`; set `%s.trusted_authors_block: true` to block trusted authors too.", decision.Label, SentinelConfigKey)
	return b.String()
}
