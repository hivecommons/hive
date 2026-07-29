package hub

import (
	"strings"
	"testing"
)

// The Version column in the My Hives table stacks its contents ONE ELEMENT PER
// LINE. That constraint is the whole point of the layout and it is invisible to
// the compiler: putting two controls back on one line would silently restore
// the wide column these tests exist to prevent, because a column is as wide as
// its widest line.
//
// The behavioural proof — that all four lines render and every control still
// fires — is driven in jsdom against the real dashboardHTML. These tests guard
// the structure that proof depends on, so a later edit cannot quietly undo it.

// versionCellExpr isolates the expression that assembles the stacked Version
// cell, so the assertions below cannot be satisfied by an unrelated part of the
// 13k-line dashboard string.
func versionCellExpr(t *testing.T) string {
	t.Helper()
	const start = "var shaLine = "
	i := strings.Index(dashboardHTML, start)
	if i < 0 {
		t.Fatal("could not find the Version cell assembly (var shaLine)")
	}
	const end = "} else { versionCell ="
	j := strings.Index(dashboardHTML[i:], end)
	if j < 0 {
		t.Fatal("could not find the end of the Version cell assembly")
	}
	return dashboardHTML[i : i+j]
}

// Four separate lines, each wrapping exactly one thing. The upgrade affordance
// and the auto-upgrade select must NOT share a line: that pairing is what made
// #2200's three-line stack fail to narrow the column at all.
func TestVersionCellPutsEachElementOnItsOwnLine(t *testing.T) {
	expr := versionCellExpr(t)

	// Each of the four must appear inside its own STACKED_LINE_STYLE wrapper.
	for _, frag := range []string{
		`'<div style="' + STACKED_LINE_STYLE + '">' + branch + '</div>'`,
		`'<div style="' + STACKED_LINE_STYLE + '">' + shaLine + '</div>'`,
		`'<div style="' + STACKED_LINE_STYLE + '">' + upgradeIcon + '</div>'`,
		`'<div style="' + STACKED_LINE_STYLE + '">' + autoUpgradeCheck + '</div>'`,
	} {
		if !strings.Contains(expr, frag) {
			t.Errorf("the Version cell does not wrap each element in its own line; missing:\n  %s", frag)
		}
	}

	// The regression guard proper: the two controls must never be concatenated
	// into a single line variable again.
	for _, bad := range []string{
		"upgradeIcon + autoUpgradeCheck",
		"autoUpgradeCheck + upgradeIcon",
	} {
		if strings.Contains(expr, bad) {
			t.Errorf("the upgrade affordance and the auto-upgrade select share a line (%q); "+
				"the column is then as wide as the PAIR and the stacking buys nothing", bad)
		}
	}
}

// Lines 3 and 4 are conditional. A read-only viewer has neither an upgrade
// action nor a mode select, and must get a two-line cell — not a four-line cell
// containing two empty rows, which would pad every non-owner row for nothing.
func TestVersionCellOmitsEmptyLines(t *testing.T) {
	expr := versionCellExpr(t)
	for _, guard := range []string{"(upgradeIcon ?", "(autoUpgradeCheck ?"} {
		if !strings.Contains(expr, guard) {
			t.Errorf("the Version cell emits a line unconditionally (missing guard %q); "+
				"a non-owner would get a blank line", guard)
		}
	}
}

// The auto-upgrade control is icon-only, so the meaning MUST live in the
// accessible name and the tooltip. Dropping either would delete information
// from the UI rather than just compacting it.
func TestAutoUpgradeSelectIsIconOnlyButStillLabelled(t *testing.T) {
	html := dashboardHTML

	// Icon-only labels, with the short values the product owner specified.
	for _, want := range []string{"'⦸ off'", "'⚡ instant'", "'🕔 5p'"} {
		if !strings.Contains(html, want) {
			t.Errorf("auto-upgrade option label %s is missing; the control is not icon-only", want)
		}
	}
	// The verbose inline label must be gone from the OPTIONS. Its words now live
	// in the tooltip and the accessible name instead.
	for _, gone := range []string{"'Auto: off'", "'Auto: instant'", "'Auto: daily 5pm ET'"} {
		if strings.Contains(html, gone) {
			t.Errorf("auto-upgrade option still carries the verbose label %s; that string "+
				"was the widest single element in the Version column", gone)
		}
	}
	// Every option must carry a full-meaning accessible label.
	for _, want := range []string{
		"ariaLabel: 'Auto-upgrade: off'",
		"ariaLabel: 'Auto-upgrade: instantly when a new version lands'",
		"ariaLabel: 'Auto-upgrade: daily at 5pm ET'",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("an auto-upgrade option has no full-meaning label (%s); icon-only "+
				"without an accessible name loses the information entirely", want)
		}
	}
	// The select itself must be given both an aria-label and a title.
	body := funcBody(t, "var buildRow = function(")
	for _, want := range []string{
		`d.setAttribute('aria-label', autoUpgradeAriaLabel(mode))`,
		"d.title = autoUpgradeAriaLabel(mode)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the auto-upgrade select is missing %q; an icon-only control with no "+
				"accessible name is unusable with a screen reader", want)
		}
	}
	// It must remain a real <select> whose change is persisted by delegation.
	if !strings.Contains(body, "document.createElement('select')") {
		t.Error("the auto-upgrade control is no longer a <select>; the delegated change " +
			"handler in bindAutoUpgradeSelects would never fire")
	}
	if !strings.Contains(body, "d.className = 'auto-upgrade-select'") {
		t.Error("the auto-upgrade select lost the class the delegated handler matches on; " +
			"changing the mode would silently do nothing")
	}
}

// Sizes are named constants, and the tap target stays usable. An icon-only
// control that shrinks to the width of a glyph is a target nobody can hit.
func TestAutoUpgradeSelectGeometryIsNamedAndTappable(t *testing.T) {
	html := dashboardHTML
	for _, decl := range []string{
		"var AUTO_SELECT_H_PX = 20;",
		"var AUTO_SELECT_MIN_W_PX = 56;",
		"var AUTO_SELECT_PAD_X_PX = 4;",
		"var UPGRADE_LINK_STYLE = ",
	} {
		if !strings.Contains(html, decl) {
			t.Errorf("missing named geometry constant: %s (no magic numbers in this file)", decl)
		}
	}
	body := funcBody(t, "var buildRow = function(")
	for _, want := range []string{
		"'height:' + AUTO_SELECT_H_PX + 'px;min-width:' + AUTO_SELECT_MIN_W_PX + 'px;padding:0 ' + AUTO_SELECT_PAD_X_PX + 'px",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the auto-upgrade select does not apply its named geometry (%q); it "+
				"would collapse to the width of a glyph", want)
		}
	}
}

// The upgrade affordance became text-with-an-underline to reclaim the 8px of
// button padding on its own line. It must stay operable by BOTH pointer and
// keyboard: a <span> that replaced a <button> silently loses keyboard
// activation and focusability unless they are restored explicitly.
func TestUpgradeAffordanceStaysOperable(t *testing.T) {
	body := funcBody(t, "var buildRow = function(")
	for _, want := range []string{
		`role="button" tabindex="0"`,
		"onclick=\"upgradeHive(' + jsArg(h.id) + ',' + jsArg(sha) + ',' + jsArg(branchName) + ')\"",
		"event.key===\\'Enter\\'",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the upgrade affordance is missing %q; replacing the <button> with a "+
				"<span> drops keyboard activation unless it is restored", want)
		}
	}
	// escAttr/jsArg, never bare esc(), in the attribute and handler contexts —
	// esc() does not escape quotes, so a hive id containing one would break out.
	if strings.Contains(body, `'<span id="upgrade-' + esc(h.id)`) {
		t.Error("the upgrade affordance interpolates esc(h.id) into an attribute; esc() " +
			"does not escape quotes — use escAttr()")
	}
	if !strings.Contains(body, `'<span id="upgrade-' + escAttr(h.id)`) {
		t.Error("the upgrade affordance does not use escAttr() for its id attribute")
	}
}
