package attentionbudget

import (
	"context"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/gfi"
)

// SectionKey is the hive:status section the item's Attention card uses for
// asks the budget held back, so they stay visible on the item itself.
const SectionKey = "attention-overflow"

// MarkerUpserter is the existing marker-comment upsert boundary
// (*github.Client.UpsertIssueMarkerComment, gfi.Forge): find the first
// comment containing marker, edit it when body differs, create only when
// create is set. The digest needs nothing else.
type MarkerUpserter interface {
	UpsertIssueMarkerComment(ctx context.Context, repo string, number int, marker, body string, create bool) (gfi.MarkerComment, error)
}

// DigestMarker identifies recipient's single digest comment in a repo.
func DigestMarker(recipient string) string {
	return fmt.Sprintf("<!-- hive:attention-digest recipient=%s -->", normLogin(recipient))
}

// RenderDigest renders recipient's digest body for asks (already filtered to
// that recipient and repo). Edits of it notify nobody; it is the reminder for
// every non-urgent ask it lists.
func RenderDigest(recipient string, asks []Ask) string {
	recipient = normLogin(recipient)
	var sb strings.Builder
	sb.WriteString(DigestMarker(recipient) + "\n")
	fmt.Fprintf(&sb, "### Attention digest for @%s\n\n", recipient)
	if len(asks) == 0 {
		sb.WriteString("Nothing outstanding. This comment is edited in place; it is not reposted.\n")
		return sb.String()
	}
	sb.WriteString("Non-urgent asks held back by the per-recipient attention budget. Security, required-review and hold notices are never held here.\n\n")
	sb.WriteString(renderLines(asks, false))
	sb.WriteString("\nThis comment is edited in place; it is not reposted.\n")
	return sb.String()
}

// UpsertDigest writes recipient's digest on issue number of repo (typically
// the repo's action-queue issue): created once, edited in place thereafter.
// With no asks it never creates a comment, only clears an existing one.
func UpsertDigest(ctx context.Context, f MarkerUpserter, repo string, number int, recipient string, asks []Ask) (gfi.MarkerComment, error) {
	if f == nil {
		return gfi.MarkerComment{}, fmt.Errorf("attentionbudget: no forge for digest")
	}
	if normLogin(recipient) == "" || number <= 0 {
		return gfi.MarkerComment{}, fmt.Errorf("attentionbudget: digest needs a recipient and an issue number")
	}
	return f.UpsertIssueMarkerComment(ctx, repo, number, DigestMarker(recipient), RenderDigest(recipient, asks), len(asks) > 0)
}

// RenderSection renders the item's status-card / action-queue lines for
// overflowed asks; "" (which removes the section) when there are none.
func RenderSection(asks []Ask) string {
	if len(asks) == 0 {
		return ""
	}
	return "**Attention (held by recipient budget)**\n\n" + renderLines(asks, true)
}

func renderLines(asks []Ask, withRecipient bool) string {
	sorted := append([]Ask(nil), asks...)
	sortAsks(sorted)
	var sb strings.Builder
	for _, a := range sorted {
		ref := fmt.Sprintf("#%d", a.Item)
		if a.URL != "" {
			ref = fmt.Sprintf("[#%d](%s)", a.Item, a.URL)
		}
		summary := strings.Join(strings.Fields(a.Summary), " ")
		if summary == "" {
			summary = string(a.Class)
		}
		who := ""
		if withRecipient {
			// Backticks keep the status card from mentioning anyone.
			who = fmt.Sprintf(" — `%s`", a.Recipient)
		}
		fmt.Fprintf(&sb, "- %s %s (%s since %s)%s\n", ref, summary, a.Class, a.At.UTC().Format("2006-01-02 15:04Z"), who)
	}
	return sb.String()
}
