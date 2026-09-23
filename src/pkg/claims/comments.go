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
	markerClaim   = "hive:claim"
	markerPreempt = "hive:preempt"
	markerRelease = "hive:release"
)

// PreemptedLabel is the label a displaced holder should watch for.
func PreemptedLabel(holder string) string { return LabelPreemptedPrefix + holder }

// ClaimComment renders the comment posted when a claim is created or renewed.
func ClaimComment(c Claim) string {
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
	fmt.Fprintf(&b, " until=%s -->\n", c.ExpiresAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "🔒 Claimed by %s (%s%s) until %s UTC. Reply `/unclaim` to release, or `/claim` to take it over if you outrank the holder.",
		mention(c), c.Kind, hiveSuffix(c), c.ExpiresAt.UTC().Format("2006-01-02 15:04"))
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

func sanitizeReason(r string) string {
	r = strings.ReplaceAll(strings.TrimSpace(r), "-->", "")
	return strings.Join(strings.Fields(r), "_")
}
