package github

import (
	"strings"
	"testing"
)

// Lines tools and the hive itself write below the attribution trailer
// (closing keywords, HTML markers, a coding tool's "Generated with" line)
// must not hide it from the PR-models count: 112 of 707 trailer-carrying
// projectbluefin PRs were dropped that way.

const footerLinesTrailer = "-- hive: agent=scanner backend=codex model=gpt-6-astra"

func assertFooterLinesTrailer(t *testing.T, body string) {
	t.Helper()
	meta, ok := ParseAttributionTrailer(body)
	if !ok || meta.Agent != "scanner" || meta.Backend != "codex" || meta.Model != "gpt-6-astra" {
		t.Errorf("ParseAttributionTrailer(%q) = %+v, %v; want scanner/codex/gpt-6-astra", body, meta, ok)
	}
}

func TestParseAttributionTrailerSkipsFooterLines(t *testing.T) {
	for _, tc := range []struct{ name, tail string }{
		{"refs", "Refs #489"},
		{"ref", "Ref #489"},
		{"closes", "Closes #1000"},
		{"close", "close #1000"},
		{"closed", "Closed #1000"},
		{"fixes", "Fixes #1031"},
		{"fix", "FIX #1031"},
		{"fixed", "Fixed #1031"},
		{"resolves", "Resolves #12"},
		{"resolve", "resolve #12"},
		{"resolved", "Resolved #12"},
		{"part-of", "Part of #7"},
		{"colon", "Fixes: #7"},
		{"cross-repo", "Refs owner/repo#7"},
		{"url", "Fixes https://github.com/owner/repo/issues/7"},
		{"list", "Fixes #7, #8 and owner/repo#9"},
		{"repeated-keyword", "Fixes #7, fixes #8"},
		{"period", "Closes #7."},
		{"dash-note", "Refs #503 - remaining: lockdev"},
		{"em-dash-note", "Refs #503 — remaining: lockdev"},
		{"paren-note", "Refs #7 (closed by #9; this PR adds docs)"},
		{"hive-marker", issueLinkFixedMarker},
		{"html-comment", "<!-- some-tool: id=42 -->"},
		{"generated-with", "🤖 Generated with [Claude Code](https://claude.com/claude-code)"},
		{"several", "Refs #1\n\n<!-- marker -->\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\nCloses #2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertFooterLinesTrailer(t, "Done\n\n"+footerLinesTrailer+"\n\n"+tc.tail)
			assertFooterLinesTrailer(t, "Done\n\n"+footerLinesTrailer+"\n"+tc.tail+"\n\n")
		})
	}
}

func TestParseAttributionTrailerRealFooterTails(t *testing.T) {
	legacy := strings.Replace(footerLinesTrailer, AttributionTrailerPrefix, LegacyAttributionTrailerPrefix, 1)
	for _, tc := range []struct{ name, body string }{
		// Refs lines, the trailer, then the hive's own issue-link marker.
		{"refs-then-marker", "## Summary\n\nPackages lockdev.\n\nRefs #489\nRefs #503 — remaining: lockdev, uucp\n\n" + legacy + " effort=high\n\n" + issueLinkFixedMarker},
		// Trailer, Generated with, Closes, then the identity footer.
		{"closes-then-identity-footer", "Summary\n\n" + footerLinesTrailer + "\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\nCloses #1000\n\n---\n🐝 **Hive Agent**: `contributor` | **SHA:** `d6177bc3`"},
		// Fixes above the trailer, Generated with and the identity footer below.
		{"generated-then-identity-footer", "Fixes #1031\n\n" + footerLinesTrailer + "\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\n---\n🐝 **Hive Agent**: `contributor` | **SHA:** `d6177bc3`"},
		// A line appended below the identity footer (Fixes #N added to a
		// contributor PR), with and without the origin marker.
		{"fixes-after-identity-footer", contributorAttributionBodyFor(footerLinesTrailer) + "\n\nFixes #7"},
		{"fixes-after-identity-footer-origin", contributorAttributionBodyFor(footerLinesTrailer) + "\n<!-- hive:origin hive=hive-bold-fox at=2026-10-16T20:00:00Z -->\n\nFixes #7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertFooterLinesTrailer(t, tc.body)
			assertFooterLinesTrailer(t, strings.ReplaceAll(tc.body, "\n", "\r\n"))
		})
	}
}

func contributorAttributionBodyFor(trailer string) string {
	return "Done\n\n" + trailer + "\n\n---\n🐝 **Hive Agent**: `contributor` | **SHA:** `671737a3b`"
}

func TestParseAttributionTrailerFooterLinesStayStrict(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"prose-after", "Done\n\n" + footerLinesTrailer + "\n\nFixes #7\n\nThanks for reviewing."},
		{"prose-starting-with-keyword", "Done\n\n" + footerLinesTrailer + "\n\nFixes the flaky test in #7 as well"},
		{"keyword-without-ref", "Done\n\n" + footerLinesTrailer + "\n\nCloses the gap"},
		{"code-after", "Done\n\n" + footerLinesTrailer + "\n\n```\nFixes #7\n```"},
		{"quoted-in-prose", "The trailer looks like `" + footerLinesTrailer + "` in a body.\n\nSee the docs."},
		{"quoted-line-then-prose", "Example:\n" + footerLinesTrailer + "\n\nThat line above is only an example.\n\nRefs #7"},
		{"blockquoted", "Done\n\n> " + footerLinesTrailer + "\n\nRefs #7"},
		{"multi-line-comment", "Done\n\n" + footerLinesTrailer + "\n\n<!--\nhidden\n-->"},
		{"text-after-comment", "Done\n\n" + footerLinesTrailer + "\n\n<!-- marker --> visible text"},
		{"identity-line-without-rule", "Done\n\n" + footerLinesTrailer + "\n\n🐝 **Hive Agent**: `contributor` | **SHA:** `671737a3b`\n\nFixes #7"},
		{"two-identity-footers", contributorAttributionBodyFor(footerLinesTrailer) + "\n\nFixes #7\n\n---\n🐝 **Hive Agent**: `contributor` | **SHA:** `unknown`"},
		{"footer-lines-only", "Refs #7\n\n" + issueLinkFixedMarker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if meta, ok := ParseAttributionTrailer(tc.body); ok {
				t.Errorf("ParseAttributionTrailer(%q) = %+v, want no trailer", tc.body, meta)
			}
		})
	}
}

func TestParseAttributionTrailerFooterLinesLastTrailerWins(t *testing.T) {
	body := "-- hive: agent=reviewer backend=claude model=claude-fable-5\n\nRefs #6\n\n" +
		"— hive: agent=scanner backend=codex model=gpt-6-astra\n\nFixes #7\n" + issueLinkFixedMarker
	assertFooterLinesTrailer(t, body)
}
