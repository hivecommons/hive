package recommend

import (
	"fmt"
	"strings"
)

// Marker identifies the hive's recommendations issue so the poster can find
// and rewrite the same issue instead of opening a new one every cycle. It is
// an HTML comment, invisible when rendered, and versioned so a future format
// change can migrate rather than orphan the existing issue.
const Marker = "<!-- hive:recommendations:v1 -->"

// Title is the issue title. The bee makes it findable at a glance in a long
// issue list, and the wording is deliberately about the reader's queue rather
// than about the hive.
const Title = "🐝 Hive recommendations — what to merge next"

// Markdown renders the report as the full body of the recommendations issue.
//
// Three rules shape this output, all learned from the failure mode it
// replaces -- a review comment nobody read:
//
//  1. Lead with the answer. The first line a maintainer sees is how many pull
//     requests they can merge right now, because that is the only number that
//     makes opening the issue worth it.
//  2. Never @-mention anyone. The body is rewritten in place on every cycle,
//     so a mention would re-notify that person every time. Authors appear as
//     plain text.
//  3. No task checkboxes. They look inviting, but the body is rewritten, so
//     any tick a maintainer made would silently vanish -- which is worse than
//     not offering them.
func (r Report) Markdown() string {
	var b strings.Builder
	b.WriteString(Marker)
	b.WriteString("\n\n")

	ready := len(r.Buckets[BucketReady])
	b.WriteString(r.headline(ready))
	b.WriteString("\n\n")
	if start := r.startHere(); start != "" {
		b.WriteString(start)
		b.WriteString("\n\n")
	}
	b.WriteString(r.summaryTable())
	b.WriteString("\n")

	for _, bucket := range bucketOrder {
		items := r.Buckets[bucket]
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## %s (%d)\n\n", bucketTitle[bucket], len(items))
		b.WriteString(bucketLead[bucket])
		b.WriteString("\n\n")
		b.WriteString(renderItems(items, r.MaxPerBucket))
		if q := r.bucketQueryURL(bucket); q != "" {
			fmt.Fprintf(&b, "\n[Open this list in GitHub](%s)\n", q)
		}
		b.WriteString(batchBlock(r.Repo, bucket, items, r.MaxPerBucket))
		b.WriteString("\n")
	}

	b.WriteString(r.footer())
	return b.String()
}

// headline states the queue's bottom line in one sentence.
func (r Report) headline(ready int) string {
	switch {
	case r.TotalOpen == 0:
		return "**The queue is empty.** No open pull requests."
	case ready == 0:
		return fmt.Sprintf("**Nothing is ready to merge right now.** %d open pull requests, all blocked on something — the sections below say what.", r.TotalOpen)
	case ready == 1:
		return fmt.Sprintf("**1 pull request is ready to merge right now**, out of %d open.", r.TotalOpen)
	default:
		return fmt.Sprintf("**%d pull requests are ready to merge right now**, out of %d open.", ready, r.TotalOpen)
	}
}

func (r Report) summaryTable() string {
	var b strings.Builder
	b.WriteString("| | Count | What it means |\n|---|---:|---|\n")
	for _, bucket := range bucketOrder {
		n := len(r.Buckets[bucket])
		if n == 0 {
			continue
		}
		fmt.Fprintf(&b, "| %s | %d | %s |\n", bucketTitle[bucket], n, bucketLead[bucket])
	}
	if r.Unclassified > 0 {
		fmt.Fprintf(&b, "| ❔ Undetermined | %d | The hive could not read the state of these. |\n", r.Unclassified)
	}
	return b.String()
}

func renderItems(items []Item, cap int) string {
	if cap <= 0 {
		cap = DefaultMaxPerBucket
	}
	var b strings.Builder
	shown := items
	if len(shown) > cap {
		shown = shown[:cap]
	}
	for _, it := range shown {
		fmt.Fprintf(&b, "- %s", link(it))
		if it.AgeDays > 0 {
			fmt.Fprintf(&b, " · %s old", humanDays(it.AgeDays))
		}
		if author := plainAuthor(it.Author); author != "" {
			fmt.Fprintf(&b, " · by %s", author)
		}
		if it.Note != "" {
			fmt.Fprintf(&b, " · %s", it.Note)
		}
		b.WriteString("\n")
	}
	if len(items) > len(shown) {
		fmt.Fprintf(&b, "- …and %d more.\n", len(items)-len(shown))
	}
	return b.String()
}

func link(it Item) string {
	title := strings.TrimSpace(it.Title)
	if title == "" {
		title = fmt.Sprintf("#%d", it.Number)
	}
	title = sanitizeTitle(title)
	if it.URL == "" {
		return fmt.Sprintf("#%d %s", it.Number, title)
	}
	return fmt.Sprintf("[#%d](%s) %s", it.Number, it.URL, title)
}

// sanitizeTitle keeps a PR title from breaking the list it is rendered into.
// Titles are author-supplied text: a newline would split the bullet, and a
// stray "]" or "[" can swallow the link that follows it.
func sanitizeTitle(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "[", "(")
	s = strings.ReplaceAll(s, "]", ")")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.TrimSpace(s)
	const maxTitle = 120
	if len(s) > maxTitle {
		s = strings.TrimSpace(s[:maxTitle]) + "…"
	}
	return s
}

// plainAuthor renders an author without turning it into a notification.
//
// The digest is rewritten in place every cycle. An "@handle" here would
// re-notify that person on every rewrite, which is precisely the kind of
// machine-generated nagging that makes a community resent an automation. Bot
// and app authors are dropped entirely: naming them adds noise and no human
// is behind them.
func plainAuthor(author string) string {
	a := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(author), "@"))
	if a == "" || strings.HasSuffix(a, "[bot]") || strings.Contains(a, "/") {
		return ""
	}
	// Backticks make it read as an identifier and guarantee GitHub cannot
	// resolve it into a mention.
	return "`" + a + "`"
}

func humanDays(days int) string {
	switch {
	case days >= 730:
		return fmt.Sprintf("%d years", days/365)
	case days >= 365:
		return "a year"
	case days >= 60:
		return fmt.Sprintf("%d months", days/30)
	case days >= 30:
		return "a month"
	case days == 1:
		return "1 day"
	default:
		return fmt.Sprintf("%d days", days)
	}
}

func (r Report) footer() string {
	var b strings.Builder
	b.WriteString("\n---\n\n")
	withAutonomyNote(&b)
	b.WriteString("\n")
	b.WriteString("This issue is maintained by the hive and rewritten in place, so it never adds to your notifications more than once. ")
	b.WriteString("It reports only what the hive could verify from GitHub's own merge and check state.\n\n")
	fmt.Fprintf(&b, "_Last updated %s._\n", r.GeneratedAt.UTC().Format("2006-01-02 15:04 MST"))
	return b.String()
}
