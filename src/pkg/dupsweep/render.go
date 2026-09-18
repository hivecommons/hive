package dupsweep

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/advisory"
)

// Marker is the invisible tag every rendered suggestion carries as its first
// line, mirroring taskListSweepMarker in pkg/github/task_list_sweep.go. It
// exists so a later sweep finds its own prior comment and EDITS it in place
// rather than stacking a fresh suggestion on every pass — a sweep that ran on
// a cadence without this would be indistinguishable from a spam bot on a
// 423-PR queue.
const Marker = "<!-- hive:duplicate-sweep -->"

// MarkerFor returns the per-cluster marker line. Scoping the marker to the
// cluster key means two genuinely different duplicate groups on one PR (a PR
// can only be in one file-set cluster today, but the data model does not
// forbid a future second pass) each own their own comment instead of
// overwriting each other.
func MarkerFor(c Cluster) string {
	return fmt.Sprintf("<!-- hive:duplicate-sweep key=%s -->", c.Key())
}

// maxRenderedFiles bounds the evidence file list. The whole set is the
// cluster's identity, but a reader only needs enough of it to recognise the
// change; the count is always stated so a truncated list never reads as the
// complete one.
const maxRenderedFiles = 10

// Render produces the suggestion to post on target.
//
// Three properties this body must have, each load-bearing:
//
//  1. It states plainly that it is a SUGGESTION and that hive will not act on
//     it. File-set identity is a candidate generator; the known false
//     positives (renovate bumping three different image digests in one file,
//     three unrelated fixes to one file) are indistinguishable from true
//     positives at this layer, so a body that reads as a verdict would be
//     lying about its own evidence.
//  2. It is DETERMINISTIC. The same cluster must render byte-identically on
//     every sweep, because the create-or-edit path elides the write when the
//     body is unchanged. A timestamp in here would turn a quiet sweep into a
//     write every cycle.
//  3. It carries no live @mention. Titles are author-supplied prose and land
//     in this body verbatim; a title containing "@someone" would re-notify
//     that person on every render. NeutralizeMentions is applied to the
//     finished body (it is idempotent, and pkg/github applies it again on the
//     way out, so neither pass weakens the other).
func Render(c Cluster, target PR) string {
	var b strings.Builder
	b.WriteString(MarkerFor(c))
	b.WriteString("\n")

	if c.BotSeries {
		renderBotSeries(&b, c)
	} else {
		renderOrdinary(&b, c, target)
	}

	b.WriteString("\n**Evidence — identical changed-file set")
	if c.Confidence == ConfidenceIdenticalDiff {
		b.WriteString(", identical diff")
	}
	fmt.Fprintf(&b, " (%d file(s)):**\n\n", len(c.Files))
	shown := c.Files
	if len(shown) > maxRenderedFiles {
		shown = shown[:maxRenderedFiles]
	}
	for _, f := range shown {
		fmt.Fprintf(&b, "- `%s`\n", f)
	}
	if len(shown) < len(c.Files) {
		fmt.Fprintf(&b, "- …and %d more\n", len(c.Files)-len(shown))
	}

	b.WriteString("\n")
	b.WriteString(disclaimer(c))
	return advisory.NeutralizeMentions(b.String())
}

func renderOrdinary(b *strings.Builder, c Cluster, target PR) {
	b.WriteString("### 🐝 Possible duplicate\n\n")
	fmt.Fprintf(b, "This PR changes exactly the same files as #%d, which was opened first:\n\n", c.Survivor.Number)
	fmt.Fprintf(b, "- **survivor (suggested):** #%d — %s\n", c.Survivor.Number, cleanTitle(c.Survivor.Title))
	for _, m := range c.Superseded {
		marker := ""
		if m.Number == target.Number {
			marker = " ← this PR"
		}
		fmt.Fprintf(b, "- #%d — %s%s\n", m.Number, cleanTitle(m.Title), marker)
	}
	if ident := c.IdenticalTo(target); len(ident) > 0 {
		fmt.Fprintf(b, "\nThe diff here is **byte-identical** to %s.\n", numberList(ident))
	}
}

func renderBotSeries(b *strings.Builder, c Cluster) {
	b.WriteString("### 🐝 Superseded regeneration series\n\n")
	fmt.Fprintf(b, "`%s` has %d open PRs regenerating the same file(s). This one is the newest, so the %d older one(s) are superseded by construction:\n\n",
		strings.TrimSpace(c.Survivor.Author), len(c.Superseded)+1, len(c.Superseded))
	older := append([]PR(nil), c.Superseded...)
	sort.Slice(older, func(i, j int) bool { return older[i].Number < older[j].Number })
	for _, m := range older {
		fmt.Fprintf(b, "- #%d — %s\n", m.Number, cleanTitle(m.Title))
	}
	b.WriteString("\nPosted once on the newest PR rather than on each of them, so a daily regeneration does not produce a daily comment on every PR in the series.\n")
}

// disclaimer is the part that must not be softened. It names the mechanism,
// its known failure mode, and the fact that hive takes no action — so a
// reader can discount the suggestion without having to reverse-engineer how
// it was produced.
func disclaimer(c Cluster) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("**This is a suggestion, not a verdict.** Hive will not close, label, approve or merge anything here — a human decides.\n\n")
	b.WriteString("It was produced by comparing changed-file sets across open PRs, which is a *candidate generator*: PRs that share a file are not necessarily the same work. ")
	switch c.Confidence {
	case ConfidenceIdenticalDiff:
		b.WriteString("In this case the patches are byte-identical, which is the strongest corroboration available without reading intent — but confirm before closing anything.\n")
	default:
		b.WriteString("Here the patches DIFFER, so this is the weaker tier: dependency-bump PRs that edit one shared manifest, and unrelated fixes to one busy file, both land in it. Read the diffs before acting.\n")
	}
	return b.String()
}

// cleanTitle flattens a PR title to a single line. A title cannot legitimately
// contain a newline, but it arrives from the API as author-controlled text and
// an embedded newline would let one entry forge extra list items.
func cleanTitle(title string) string {
	t := strings.TrimSpace(title)
	t = strings.ReplaceAll(t, "\r", " ")
	t = strings.ReplaceAll(t, "\n", " ")
	if t == "" {
		return "(no title)"
	}
	return t
}

func numberList(nums []int) string {
	parts := make([]string, 0, len(nums))
	for _, n := range nums {
		parts = append(parts, fmt.Sprintf("#%d", n))
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}
