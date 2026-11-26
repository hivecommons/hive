package github

import (
	"strings"
	"testing"
)

func TestParseAttributionTrailerFooterLines(t *testing.T) {
	const trailer = "-- hive: agent=scanner backend=codex model=gpt-6-astra"
	const identity = "\n\n---\n🐝 **Hive Agent**: `contributor` | **SHA:** `671737a3b`"
	for _, tail := range []string{
		"Closes #1", "Refs #1 — remaining: docs", "Fixes owner/repo#7, #8 and owner/repo#9",
		"Fixes https://github.com/owner/repo/issues/7", "<!-- marker -->", "🤖 Generated with tool",
		"Closes #1" + identity, identity + "\n<!-- hive:origin hive=test -->\nFixes #2",
	} {
		body := "Summary\n\n" + trailer + "\n" + tail
		for _, input := range []string{body, strings.ReplaceAll(body, "\n", "\r\n")} {
			meta, ok := ParseAttributionTrailer(input)
			if !ok || meta.Backend != "codex" || meta.Model != "gpt-6-astra" {
				t.Errorf("ParseAttributionTrailer(%q) = %+v, %v", input, meta, ok)
			}
		}
	}
	for _, tail := range []string{"Fixes the flaky test in #7", "Closes #1\nMore prose", "```text\nRefs #1\n```", "<!-- unclosed", "<!-- marker --> text", identity + "\nFixes #1" + identity} {
		body := trailer + "\n" + tail
		if meta, ok := ParseAttributionTrailer(body); ok {
			t.Errorf("accepted %q: %+v", body, meta)
		}
	}
}

func TestRewriteRefsPreservesAttributionTrailer(t *testing.T) {
	body := "Summary\n\n-- hive: backend=codex model=gpt-6-astra\n\nRefs #7"
	got, ok := rewriteRefsIssueLinkToClosing(body, "hivecommons/hive", 7)
	if !ok {
		t.Fatal("rewrite refused")
	}
	meta, ok := ParseAttributionTrailer(got)
	if !ok || meta.Model != "gpt-6-astra" || !strings.Contains(got, "Fixes #7") || strings.Index(got, issueLinkFixedMarker) > strings.Index(got, "-- hive:") {
		t.Fatalf("rewrite lost attribution: %q, %+v", got, meta)
	}
}
