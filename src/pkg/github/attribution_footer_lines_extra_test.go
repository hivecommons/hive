package github

import (
	"strings"
	"testing"
)

// Extra regression cases for hivecommons/hive#19364 not covered by
// attribution_footer_lines_test.go.

func TestParseAttributionTrailerFooterLinesExtraStrictCases(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"unclosed-html-comment", "Done\n\n" + footerLinesTrailer + "\n\n<!-- started a comment"},
		{"lang-code-block", "Done\n\n" + footerLinesTrailer + "\n\n```text\nRefs #1\n```"},
		{"keyword-inside-prose", "Done\n\n" + footerLinesTrailer + "\nThis Fixes #3 by accident"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if meta, ok := ParseAttributionTrailer(tc.body); ok {
				t.Errorf("ParseAttributionTrailer(%q) = %+v, want no trailer", tc.body, meta)
			}
		})
	}
}

func TestParseAttributionTrailerMixedFooterTailCRLF(t *testing.T) {
	body := "Summary\n\n" + footerLinesTrailer + "\nCloses #1000\n" + issueLinkFixedMarker +
		"\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\nRefs #9" +
		"\n\n---\n🐝 **Hive Agent**: `scanner` | **SHA:** `671737a3b`"
	assertFooterLinesTrailer(t, strings.ReplaceAll(body, "\n", "\r\n"))
}
