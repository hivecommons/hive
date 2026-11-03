package hub

import (
	"strings"
	"testing"
)

// The Version column in the My Hives table is a WIDTH-BOUNDED, WRAPPING
// two-line block: line 1 is the identity (channel pill, running SHA, behind
// chip, drift count), line 2 the status badge plus every action and the
// auto-upgrade select. Each line is a wrapping flex row inside a td whose
// width is capped by CSS, so a long status folds onto a second visual row
// instead of widening the column. Before this layout the column rendered its
// pieces on unwrapped nowrap lines and measured ~1300px, pushing the Repos,
// Maturity and later columns off a 1600px screen.
//
// The compiler cannot see any of this: a later edit that drops the class, the
// bound or the wrap would silently restore the wide column. These tests guard
// the structure.

// versionCellExpr isolates the expression that assembles the Version cell, so
// the assertions below cannot be satisfied by an unrelated part of the
// dashboard string.
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

// The cell is two classed, wrapping lines: identity and status. The pieces
// must land on the right line and no line may be an inline nowrap flex row
// (STACKED_LINE_STYLE), which is what made the column as wide as its longest
// status string.
func TestVersionCellIsTwoWrappingLines(t *testing.T) {
	expr := versionCellExpr(t)
	for _, frag := range []string{
		`'<div class="version-stack">'`,
		`'<div class="version-line version-line-id">' + branch + ' ' + shaLine + '</div>'`,
		`'<div class="version-line version-line-status">' + statusLine + '</div>'`,
		`var statusLine = (upgradeIcon || '') + (autoUpgradeCheck ? ' ' + autoUpgradeCheck : '');`,
	} {
		if !strings.Contains(expr, frag) {
			t.Errorf("the Version cell is not the two-line wrapping block; missing:\n  %s", frag)
		}
	}
	if strings.Contains(expr, "STACKED_LINE_STYLE") {
		t.Error("the Version cell still uses STACKED_LINE_STYLE (white-space:nowrap); " +
			"a nowrap line makes the column as wide as its longest status string")
	}
}

// The status line is conditional: a read-only viewer has neither an action
// nor a mode select and must get a one-line cell, not a blank second row.
func TestVersionCellOmitsEmptyStatusLine(t *testing.T) {
	expr := versionCellExpr(t)
	if !strings.Contains(expr, "(statusLine ?") {
		t.Error("the Version cell emits the status line unconditionally; a non-owner would get a blank line")
	}
}

// The td carries the class the width bound hangs on, and the CSS actually
// bounds and wraps it. Without the max-width the flex-wrap never triggers;
// without white-space:normal the inherited nowrap wins.
func TestVersionColumnIsWidthBoundedAndWraps(t *testing.T) {
	html := dashboardHTML
	if !strings.Contains(html, `'<td class="version-cell" style="font-size:0.7rem">' + versionCell + '</td>'`) {
		t.Error("the Version <td> lost the version-cell class the width bound is keyed on")
	}
	css := cssRule(t, ".hive-table td.version-cell")
	for _, want := range []string{"min-width:", "max-width:", "white-space: normal", "overflow-wrap: anywhere"} {
		if !strings.Contains(css, want) {
			t.Errorf(".hive-table td.version-cell is missing %q; the column can grow unbounded again", want)
		}
	}
	line := cssRule(t, ".version-line")
	for _, want := range []string{"flex-wrap: wrap", "white-space: normal"} {
		if !strings.Contains(line, want) {
			t.Errorf(".version-line is missing %q; a crowded status line widens the column instead of folding", want)
		}
	}
}

// The Repos cell had the same one-line problem ("as: <identity>" never
// wrapped). It is bounded and wraps the same way.
func TestReposColumnIsWidthBoundedAndWraps(t *testing.T) {
	css := cssRule(t, ".hive-table td.repos-cell")
	for _, want := range []string{"max-width:", "white-space: normal", "overflow-wrap: anywhere"} {
		if !strings.Contains(css, want) {
			t.Errorf(".hive-table td.repos-cell is missing %q", want)
		}
	}
	body := funcBody(t, "var buildRow = function(")
	if !strings.Contains(body, `'<td class="repos-cell" title="`) {
		t.Error("the Repos <td> lost the repos-cell class the width bound is keyed on")
	}
	if !strings.Contains(body, `'<div class="repos-as" style="' + STACKED_LINE_STYLE + ';white-space:normal;overflow-wrap:anywhere;color:var(--muted)"`) {
		t.Error("the Repos \"as:\" line no longer overrides the nowrap of STACKED_LINE_STYLE")
	}
}

// Long explanations live in hover titles, not inline: the queued badge shows
// only "behind by N · next check …" and the "why not now" sentence rides in
// its title; the progress pill shows the state (and time left on a drain)
// while the agents holding the drain and a missed window's reason go to the
// title. Inline, those sentences were the widest things in the column.
func TestVersionColumnLongExplanationsMoveToTitles(t *testing.T) {
	body := funcBody(t, "var buildRow = function(")
	if !strings.Contains(body, "var queuedLabel = behindLabel + ' · ' + nextCheck;") {
		t.Error("the queued badge label is no longer the short state; keep it to behind/next-check")
	}
	if strings.Contains(body, "' · why not now: ' + whyNotNow") {
		t.Error("the queued badge renders the 'why not now' sentence inline again")
	}
	for _, want := range []string{
		"var whyNotNowNote = ' Why not now: ' + whyNotNow + '.';",
		"'.' + whyNotNowNote;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the queued badge title lost the 'why not now' explanation (%q); moving it off the badge must not delete it", want)
		}
	}
	prog := funcBody(t, "function renderUpgradeProgress(")
	for _, want := range []string{
		"if (agents) tipExtra = '\\n' + agents;",
		"tipExtra = '\\n' + st.reason;",
		"var tip = (st.detail || label) + tipExtra;",
	} {
		if !strings.Contains(prog, want) {
			t.Errorf("renderUpgradeProgress is missing %q; the agents/reason must survive in the pill title", want)
		}
	}
	for _, bad := range []string{"(agents ? ' · ' + agents : '')", "extra = ' · ' + st.reason;"} {
		if strings.Contains(prog, bad) {
			t.Errorf("renderUpgradeProgress renders %q inline again; that sentence widens the column", bad)
		}
	}
}

// cssRule returns the declaration block of the first rule whose selector
// list starts with sel.
func cssRule(t *testing.T, sel string) string {
	t.Helper()
	i := strings.Index(dashboardHTML, "\n      "+sel+" {")
	if i < 0 {
		i = strings.Index(dashboardHTML, "\n    "+sel+" {")
	}
	if i < 0 {
		t.Fatalf("CSS rule %q not found in dashboard.html", sel)
	}
	rest := dashboardHTML[i:]
	j := strings.Index(rest, "}")
	return rest[:j]
}

// The auto-upgrade control is icon-only, so the meaning MUST live in the
// accessible name and the tooltip. Dropping either would delete information
// from the UI rather than just compacting it.
func TestAutoUpgradeSelectIsIconOnlyButStillLabelled(t *testing.T) {
	html := dashboardHTML

	// Icon-only labels, with the short values the product owner specified.
	for _, want := range []string{"'⦸ off'", "'⚡ instant'", "'🕐 1p'"} {
		if !strings.Contains(html, want) {
			t.Errorf("auto-upgrade option label %s is missing; the control is not icon-only", want)
		}
	}
	// The verbose inline label must be gone from the OPTIONS. Its words now live
	// in the tooltip and the accessible name instead.
	for _, gone := range []string{"'Auto: off'", "'Auto: instant'", "'Auto: daily 1pm ET'"} {
		if strings.Contains(html, gone) {
			t.Errorf("auto-upgrade option still carries the verbose label %s; that string "+
				"was the widest single element in the Version column", gone)
		}
	}
	// Every option must carry a full-meaning accessible label.
	for _, want := range []string{
		"ariaLabel: 'Auto-upgrade: off'",
		"ariaLabel: 'Auto-upgrade: instantly when a new version lands'",
		"ariaLabel: 'Auto-upgrade: daily at 1pm ET'",
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
		"onclick=\"upgradeHive(' + jsArg(h.id) + ',' + jsArg(sha) + ',' + jsArg(branchName) + ',' + jsArg(branchLatest || '') + ')\"",
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
