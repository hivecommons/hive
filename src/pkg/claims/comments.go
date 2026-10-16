package claims

import (
	"fmt"
	"strings"
	"time"
)

// Labels the hub sets on GitHub issues.
const (
	// LabelClaimed marks an issue with a live claim.
	LabelClaimed = "claimed"
	// LabelPreemptedPrefix plus the displaced holder's login marks a takeover
	// so pollers that only read labels notice they were preempted.
	LabelPreemptedPrefix = "preempted:"
)

// Marker names used in the HTML comments the hub writes so tooling can parse
// them without scraping prose.
const (
	markerClaim      = "hive:claim"
	markerPreempt    = "hive:preempt"
	markerRelease    = "hive:release"
	markerEscalation = "hive:claim-escalation"
)

// PreemptedLabel is the label a displaced holder should watch for.
func PreemptedLabel(holder string) string { return LabelPreemptedPrefix + holder }

// ClaimComment renders the comment posted when a claim is created. The
// marker's until= is the expiry at that moment; renewals do not edit it, so
// the human-readable line states the time-to-live instead of an end time.
func ClaimComment(c Claim) string {
	var b strings.Builder
	b.WriteString(ClaimMarker(c))
	b.WriteString("\n")
	// The visible text never prints a clock time: the comment is posted once,
	// while renewals only move the ledger's expiry (#10926), so a fixed time
	// would go stale and read as a lapsed claim.
	fmt.Fprintf(&b, "🔒 Claimed by %s (%s%s).", mention(c), c.Kind, hiveSuffix(c))
	if ttl := c.ExpiresAt.Sub(c.ClaimedAt); !c.ClaimedAt.IsZero() && ttl > 0 {
		fmt.Fprintf(&b, " The claim lasts %s without activity and renews while the work continues.", ttl.Round(time.Minute))
	} else {
		b.WriteString(" The claim renews while the work continues and lapses after a period without activity.")
	}
	b.WriteString(" Reply `/unclaim` to release, or `/claim` to take it over if you outrank the holder.")
	return b.String()
}

// ClaimMarker renders the machine-readable claim marker alone. at= is the
// instant the claim was asserted (omitted when unknown) so a reader can tell
// when work started, not only when the hold lapses; pkg/issueclaim parses
// both keys.
func ClaimMarker(c Claim) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- %s who=%s kind=%s", markerClaim, c.Holder, c.Kind)
	if c.HolderID != "" && c.HolderID != c.Holder {
		fmt.Fprintf(&b, " id=%s", c.HolderID)
	}
	if c.Session != "" {
		fmt.Fprintf(&b, " session=%s", c.Session)
	}
	if c.Hive != "" {
		fmt.Fprintf(&b, " hive=%s", c.Hive)
	}
	if c.Kind == KindContributor {
		fmt.Fprintf(&b, " tier=%s verified=%t", markerTier(c.Tier), c.Verified)
	}
	if !c.ClaimedAt.IsZero() {
		fmt.Fprintf(&b, " at=%s", c.ClaimedAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, " until=%s -->", c.ExpiresAt.UTC().Format(time.RFC3339))
	return b.String()
}

// TakeoverComment renders the comment posted when a claim transfers. It
// carries the machine-readable preempt marker the displaced holder polls for.
func TakeoverComment(now, previous Claim) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- %s target=%s target_kind=%s by=%s by_kind=%s issue=%d -->\n",
		markerPreempt, previous.Holder, previous.Kind, now.Holder, now.Kind, now.Issue)
	fmt.Fprintf(&b, "<!-- %s who=%s kind=%s until=%s -->\n",
		markerClaim, now.Holder, now.Kind, now.ExpiresAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "🔁 Claim taken over by %s (%s%s)", mention(now), now.Kind, hiveSuffix(now))
	if now.Forced {
		b.WriteString(" with `--force`")
	}
	fmt.Fprintf(&b, ". %s: stop work on this item and pick up something else.", mention(previous))
	return b.String()
}

// ReleaseComment renders the comment posted when a claim is released.
func ReleaseComment(c Claim, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- %s who=%s kind=%s reason=%s -->\n", markerRelease, c.Holder, c.Kind, sanitizeReason(reason))
	fmt.Fprintf(&b, "🔓 Claim by %s released (%s).", mention(c), reason)
	return b.String()
}

// EscalationComment renders the comment posted instead of another claim when
// an agent's last claims on an issue moved nothing (#10527, ADR-0019). label
// is the label the issue is parked with.
func EscalationComment(s Stall, label string) string {
	agent := Claim{Holder: s.Holder, Kind: KindAgent}
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- %s who=%s kind=%s attempts=%d since=%s -->\n",
		markerEscalation, s.Holder, KindAgent, len(s.Attempts), s.Since().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "🚨 Not claiming this again: %s claimed it %d times and nothing moved in between — no linked pull request, referencing commit, label or assignee change, or close/reopen since %s UTC.\n\n",
		mention(agent), len(s.Attempts), s.Since().UTC().Format("2006-01-02 15:04"))
	b.WriteString("| Claimed | How it ended |\n| --- | --- |\n")
	for _, a := range s.Attempts {
		ended := "no end recorded"
		if !a.EndedAt.IsZero() {
			ended = fmt.Sprintf("%s at %s UTC", tableCell(a.Ended), a.EndedAt.UTC().Format("2006-01-02 15:04"))
		}
		fmt.Fprintf(&b, "| %s UTC | %s |\n", a.ClaimedAt.UTC().Format("2006-01-02 15:04"), ended)
	}
	fmt.Fprintf(&b, "\nLabelled `%s` and left out of every work list. Removing the label (or replying `/hive approve`) counts as progress, so the next claim goes ahead.", label)
	return b.String()
}

// tableCell keeps a free-text reason inside one markdown table cell.
func tableCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "ended"
	}
	return strings.ReplaceAll(s, "|", `\|`)
}

func mention(c Claim) string {
	switch c.Kind {
	case KindHuman, KindExternal, KindContributor:
		if strings.HasPrefix(c.Holder, "@") {
			return c.Holder
		}
		return "@" + c.Holder
	}
	return "agent `" + c.Holder + "`"
}

func hiveSuffix(c Claim) string {
	if c.Hive == "" {
		return ""
	}
	return " on hive " + c.Hive
}

// markerTier renders a contributor tier as a single marker token. Anything
// empty or not a plain lowercase word becomes the lowest tier so a malformed
// value can never read as an elevated one.
func markerTier(tier string) string {
	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier == "" {
		return contributorTierDefault
	}
	for _, r := range tier {
		if (r < 'a' || r > 'z') && r != '-' && r != '_' {
			return contributorTierDefault
		}
	}
	return tier
}

// contributorTierDefault mirrors issueclaim.ContributorTierDefault.
const contributorTierDefault = "newcomer"

func sanitizeReason(r string) string {
	r = strings.ReplaceAll(strings.TrimSpace(r), "-->", "")
	return strings.Join(strings.Fields(r), "_")
}
