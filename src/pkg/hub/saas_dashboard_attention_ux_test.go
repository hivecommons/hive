package hub

import (
	"regexp"
	"strings"
	"testing"
)

// jsFunctionBody extracts the source of one top-level inline-script function
// from dashboardHTML, from its declaration to the next top-level declaration.
// Good enough for presence assertions on what a handler does.
func jsFunctionBody(t *testing.T, name string) string {
	t.Helper()
	decl := "function " + name + "("
	start := strings.Index(dashboardHTML, decl)
	if start < 0 {
		t.Fatalf("function %s not found in dashboardHTML", name)
	}
	rest := dashboardHTML[start+len(decl):]
	// Next top-level declaration (column-4 indent) ends the slice.
	next := regexp.MustCompile(`(?m)^ {4}(function |var )`).FindStringIndex(rest)
	if next == nil {
		return rest
	}
	return rest[:next[0]]
}

// Clicking a drift pill must CLEAR the hive search (a stale search silently
// intersecting with the pill filter reads as hives having disappeared), stash
// what was typed, and restore it when the last pill is deselected — unless the
// user typed a new search while a pill was active, in which case the new text
// wins.
func TestDriftPillClearsAndStashesSearch(t *testing.T) {
	// The stash must be top-level render state, like the other filter state.
	if !regexp.MustCompile(`(?m)^ {4}var _driftSearchStash\b`).MatchString(dashboardHTML) {
		t.Fatal("var _driftSearchStash is not declared at top level of the inline script")
	}

	body := jsFunctionBody(t, "toggleDriftFilter")
	for _, want := range []struct{ snippet, why string }{
		{"_driftSearchStash = _dashSearchQuery;",
			"first pill selection must stash the current search text"},
		{"_dashSearchQuery = '';",
			"pill selection must clear the search filter state"},
		{"searchEl.value = '';",
			"pill selection must blank the visible search input, not just the state"},
		{"_dashSearchQuery = _driftSearchStash;",
			"deselecting the last pill must restore the stashed search"},
		{"_driftSearchStash = null;",
			"deselecting the last pill must drop the stash after restoring"},
	} {
		if !strings.Contains(body, want.snippet) {
			t.Errorf("toggleDriftFilter is missing %q — %s", want.snippet, want.why)
		}
	}
}

// Typing a new search while a drift pill is active supersedes the stashed
// text: the stash is dropped so deselecting the pill cannot clobber what the
// user just typed.
func TestSearchInputWhilePillActiveDropsStash(t *testing.T) {
	body := jsFunctionBody(t, "onHiveSearchInput")
	if !strings.Contains(body, "if (_dashDriftFilter) _driftSearchStash = null;") {
		t.Error("onHiveSearchInput must drop _driftSearchStash while a drift pill " +
			"is active, so new text typed by the user survives pill deselection")
	}
}

// The clear-everything paths bypass toggleDriftFilter when resetting the drift
// filter, so they must drop the stash themselves — otherwise a search cleared
// by "Clear filters" could silently come back later.
func TestClearFilterPathsDropSearchStash(t *testing.T) {
	for _, fn := range []string{"clearAllHiveFilters", "clearStatusFilters", "clearHiveSearch"} {
		body := jsFunctionBody(t, fn)
		if !strings.Contains(body, "_driftSearchStash = null;") {
			t.Errorf("%s must drop _driftSearchStash", fn)
		}
	}
}

// The Usage card must render ABOVE the "N hives need attention" strip, so the
// strip's drift pills sit directly adjacent to the hive list they filter, with
// the alerts panel and the view bar between the strip and the list only.
func TestUsagePanelRendersAboveAttentionStrip(t *testing.T) {
	ids := []string{
		`<div id="usage-panel"`,
		`<div id="hive-drift-summary"`,
		`<div id="fleet-alerts-panel"`,
		`<div id="hive-view-bar"`,
		`<div id="hive-filter-bar"`,
	}
	prev := -1
	prevID := ""
	for _, id := range ids {
		idx := strings.Index(dashboardHTML, id)
		if idx < 0 {
			t.Fatalf("%s not found in dashboardHTML", id)
		}
		if idx <= prev {
			t.Errorf("%s (offset %d) must appear after %s (offset %d) in dashboardHTML",
				id, idx, prevID, prev)
		}
		prev = idx
		prevID = id
	}
}
