package upstreamwatch

import (
	"fmt"
	"strings"
)

const (
	// titlePrefix starts every filed issue title.
	titlePrefix = "upstream: "
	// markerPrefix opens the hidden dedupe marker embedded in every filed
	// issue body. The second dedupe guard searches the fork for it.
	markerPrefix = "<!-- upstream-ref: "
	markerSuffix = " -->"
	// maxRenderedFiles bounds the affected-files list; the rest are counted.
	maxRenderedFiles = 30
	// maxSummaryRunes bounds the upstream summary copied into the body.
	maxSummaryRunes = 1500
)

// Issue is one fork issue the watch files.
type Issue struct {
	Title  string
	Body   string
	Labels []string
}

// MarkerRef returns the upstream-qualified reference the hidden marker
// carries: "owner/repo#123" for a PR and "owner/repo@<tag>" for a release.
func MarkerRef(upstream string, item Item) string {
	if item.Kind == KindRelease {
		return upstream + "@" + strings.TrimPrefix(item.Ref, "release:")
	}
	return upstream + "#" + strings.TrimPrefix(item.Ref, "upstream#")
}

// Marker returns the hidden HTML comment that identifies the upstream item an
// issue was filed for, e.g. "<!-- upstream-ref: owner/repo#123 -->".
func Marker(upstream string, item Item) string {
	return markerPrefix + MarkerRef(upstream, item) + markerSuffix
}

// RenderIssue renders the fork issue for one applicable upstream item. The
// title is "upstream: " plus the upstream title; the body carries the
// upstream link, a summary, the class, the affected files, the difficulty
// estimate and the hidden marker. label is applied as the only label.
func RenderIssue(upstream string, item Item, j Judgement, label string) Issue {
	title := strings.TrimSpace(item.Title)
	if title == "" {
		title = item.Ref
	}
	var b strings.Builder
	kind := "pull request"
	if item.Kind == KindRelease {
		kind = "release"
	}
	fmt.Fprintf(&b, "The upstream watch found an upstream %s that may need porting to this fork.\n\n", kind)
	fmt.Fprintf(&b, "**Upstream:** %s (%s)\n", MarkerRef(upstream, item), item.HTMLURL)
	fmt.Fprintf(&b, "**Class:** %s\n", j.Class)
	fmt.Fprintf(&b, "**Port difficulty:** %s", j.Difficulty)
	if item.Kind == KindPR {
		fmt.Fprintf(&b, " (%d files, +%d/-%d lines)", len(item.Files), item.Additions, item.Deletions)
	}
	b.WriteString("\n")
	if summary := summarize(item.Body); summary != "" {
		b.WriteString("\n### Summary\n\n")
		b.WriteString(summary)
		b.WriteString("\n")
	}
	files := j.Present
	if len(files) == 0 {
		files = item.Files
	}
	if len(files) > 0 {
		fmt.Fprintf(&b, "\n### Affected files (%d)\n\n", len(files))
		for i, f := range files {
			if i >= maxRenderedFiles {
				fmt.Fprintf(&b, "- ... and %d more\n", len(files)-i)
				break
			}
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
	}
	b.WriteString("\nPort the change if it still applies, or close this issue as not planned to dismiss it; a dismissed upstream change is never filed again.\n")
	b.WriteString("\n")
	b.WriteString(Marker(upstream, item))
	b.WriteString("\n")
	var labels []string
	if l := strings.TrimSpace(label); l != "" {
		labels = []string{l}
	}
	return Issue{Title: titlePrefix + title, Body: b.String(), Labels: labels}
}

// summarize trims the upstream body to a bounded summary.
func summarize(body string) string {
	body = strings.TrimSpace(body)
	r := []rune(body)
	if len(r) <= maxSummaryRunes {
		return body
	}
	return strings.TrimSpace(string(r[:maxSummaryRunes])) + "\n\n_(truncated)_"
}
