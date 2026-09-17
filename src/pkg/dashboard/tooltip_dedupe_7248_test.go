package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// labelWithTitle matches the opening tag of a label element that carries a
// native title attribute.
var labelWithTitle = regexp.MustCompile(`(?s)<label\b[^>]*\btitle=`)

// TestNoLabelHasBothNativeAndCustomTooltip enforces the invariant behind
// #7248: a Settings label must not carry a native `title=` attribute AND a
// nested custom `.config-info` / `.config-tooltip` at the same time.
//
// When it does, hovering the row shows the custom styled tooltip while the
// browser independently pops its own native one on top of it, so the operator
// gets two overlapping tooltips saying the same thing.
//
// This is written as a general invariant rather than a pin on the two labels
// that were wrong, so that a newly added label cannot reintroduce the bug.
// Both forms remain individually fine -- a label with only `title=` (there are
// ~25) and a label with only `.config-info` (there are ~136) are untouched;
// only the combination is rejected.
func TestNoLabelHasBothNativeAndCustomTooltip(t *testing.T) {
	html := indexHTML(t)

	var offenders []string
	for _, loc := range labelWithTitle.FindAllStringIndex(html, -1) {
		// Scope the search to this label's own content, so a .config-info
		// belonging to some *later* label cannot be misattributed to it.
		rest := html[loc[0]:]
		if end := strings.Index(rest, "</label>"); end >= 0 {
			rest = rest[:end]
		}
		if strings.Contains(rest, "config-info") || strings.Contains(rest, "config-tooltip") {
			snippet := rest
			if len(snippet) > 120 {
				snippet = snippet[:120] + "..."
			}
			offenders = append(offenders, snippet)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("%d label(s) carry both a native title= and a nested custom tooltip, "+
			"which renders two overlapping tooltips on hover (#7248). Drop the title= and keep "+
			"the richer .config-tooltip:", len(offenders))
		for _, o := range offenders {
			t.Errorf("  %s", o)
		}
	}
}

// TestConfigInfoTooltipsStillPresent is a counterweight to the test above.
// Deleting every .config-info icon in the file would satisfy that invariant
// trivially, so pin that the custom tooltip mechanism -- the one we chose to
// keep -- is still widely in use.
func TestConfigInfoTooltipsStillPresent(t *testing.T) {
	html := indexHTML(t)
	if got := strings.Count(html, `class="config-info"`); got < 100 {
		t.Errorf("only %d .config-info tooltip icons remain, want >= 100 — "+
			"the custom tooltip mechanism appears to have been removed rather than deduplicated (#7248)", got)
	}
}

// TestDedupedTooltipLabelsKeepTheirExplanation asserts the two labels fixed in
// #7248 still explain themselves after losing their title attribute. The point
// of the fix was to remove a *duplicate*, not to remove the help text.
func TestDedupedTooltipLabelsKeepTheirExplanation(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"<label>Cadence mode scope <span class=\"config-info\">i<span class=\"config-tooltip\">",
		"<label>Multi-repo threshold scaling <span class=\"config-info\">i<span class=\"config-tooltip\">",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing %q — the deduplicated label lost its remaining tooltip (#7248)", want)
		}
	}
}
