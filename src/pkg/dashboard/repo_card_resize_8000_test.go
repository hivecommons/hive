package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hivecommons/hive#8000: every Repositories card was exactly as wide as every
// other one, because `.repo-grid` was a CSS grid with
// `repeat(auto-fit, minmax(240px, 1fr))` and grid sizes TRACKS, not items. On
// a hive watching six repos with ~30 pills on a card, a pill column read
// `#192 Pa…`, `FIX #1109 fix…`, `TESTS #106…`, and the only way to learn what
// one was was to hover it, one at a time.
//
// The fix gives each card a drag handle on its right edge, stores the width
// per repo in localStorage, reapplies it on every repaint (the grid HTML is
// rebuilt each governor cycle, so a width set once on the node would not
// survive the minute), and grows the pill title budget with the card — a
// wider card with the same 50-character clip would gain nothing.
//
// These tests pin the layout switch and the markup; TestRepoCardWidthStore
// executes the width and truncation rules themselves.

// repoCardConstLine returns the whole `const NAME = ...;` line from
// index.html, so the node-executed tests below run against the real constants
// rather than copies that can drift from them.
func repoCardConstLine(t *testing.T, html, name string) string {
	t.Helper()
	needle := "const " + name + " = "
	start := strings.Index(html, needle)
	if start < 0 {
		t.Fatalf("index.html does not define %s", name)
	}
	end := strings.Index(html[start:], "\n")
	if end < 0 {
		t.Fatalf("unterminated %s declaration in index.html", name)
	}
	return strings.TrimSpace(html[start : start+end])
}

// The container is a flex-wrap layout, not a grid: per-card width is the whole
// point and a grid cannot express it.
func TestRepoGridSizesItemsNotTracks(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		".repo-grid { --repo-cols: 4; --repo-gap: 8px; display: flex; flex-wrap: wrap; align-items: stretch; gap: var(--repo-gap); }",
		// The default basis is a quarter of the row: at most four cards
		// across, so pill rows are readable instead of squashed six-wide.
		"flex: 1 1 calc((100% - (var(--repo-cols) - 1) * var(--repo-gap)) / var(--repo-cols));",
		"@media (max-width: 1400px) { .repo-grid { --repo-cols: 3; } }",
		"@media (max-width: 640px) { .repo-grid { --repo-cols: 1; } }",
		// A sized card keeps its width instead of sharing the row's slack.
		".repo-card.repo-card-sized { flex-grow: 0; }",
		// The title budget scales from the layout width, not a fixed 240px.
		"const MAX_PILL_TITLE_LEN = pillTitleLimit(cardW || layoutW);",
		"function repoCardLayoutWidth(grid)",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
	// The track-sizing rules are gone. Either one left behind would pin every
	// card to the same width again and the drag would do nothing visible.
	for _, gone := range []string{
		".repo-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(240px, 1fr)); gap: 8px; }",
		"body.light-mode .repo-grid { grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); }",
		"flex: 1 1 240px;",
		"body.light-mode .repo-card { flex-basis: 200px; }",
	} {
		if strings.Contains(html, gone) {
			t.Errorf("index.html still sizes repo cards by grid track: %q", gone)
		}
	}
}

// The handle: on every card, focusable, reporting the width it controls, and
// hidden on the phone layout where there is no width to trade.
func TestRepoCardResizeHandleMarkup(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`<div class="repo-card-resize" role="separator" aria-orientation="vertical" tabindex="0" data-repo="${esc(cardRepo)}"`,
		`aria-valuemin="${REPO_CARD_MIN_W}" aria-valuemax="${REPO_CARD_MAX_W}" aria-valuenow="${cardW || REPO_CARD_DEFAULT_W}"`,
		// The handle is rendered inside the card it resizes.
		"${resizeHandle}",
		"cursor: col-resize; touch-action: none;",
		// Keyboard focus has to be visible for a focusable control.
		".repo-card-resize:focus-visible {",
		// Hidden in the phone layout, like the sidebar splitter next door.
		".repo-card-resize { display: none; }",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
	// The width reaches the card as an inline flex-basis emitted WITH the
	// card HTML, so the governor's repaint reapplies it from the stored map
	// instead of throwing it away.
	if !strings.Contains(html, "<div class=\"repo-card${cardW ? ' repo-card-sized' : ''}\"${cardW ? ` style=\"flex-basis:${cardW}px\"` : ''}>") {
		t.Error("the repo card does not carry its stored width in the rendered HTML")
	}
	if !strings.Contains(html, "const cardW = repoCardWidth(cardRepo);") {
		t.Error("renderRepos does not read the stored width for the card it is drawing")
	}
	// The pill title budget must follow the card width; a wider card with the
	// same 50-character clip is the bug this issue is about.
	if !strings.Contains(html, "const MAX_PILL_TITLE_LEN = pillTitleLimit(cardW || layoutW);") {
		t.Error("pill titles are still cut to a fixed length regardless of card width")
	}
	if strings.Contains(html, "const MAX_PILL_TITLE_LEN = 50;") {
		t.Error("the fixed 50-character pill title clip is still in place")
	}
	// A repaint landing mid-drag would replace the node under the pointer and
	// drop the pointer capture with it.
	if !strings.Contains(html, "      if (_repoResizeDrag) return;") {
		t.Error("renderRepos does not hold its repaint while a card is being dragged")
	}
}

// Drag, double-click reset, reset-all and the keyboard nudge are all wired,
// and none of them uses an inline handler attribute (CSP script-src-attr is
// 'none' — see the generic dispatcher's note).
func TestRepoCardResizeInteractionsAreWired(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"(function initRepoCardResize() {",
		"grid.addEventListener('pointerdown', (e) => {",
		"grid.addEventListener('pointermove', (e) => {",
		"grid.addEventListener('pointerup', endDrag);",
		"grid.addEventListener('pointercancel', endDrag);",
		// Double-click on the handle resets that one card.
		"grid.addEventListener('dblclick', (e) => {",
		// Keyboard: arrows nudge, Shift takes a bigger step, Escape resets.
		"grid.addEventListener('keydown', (e) => {",
		"const step = e.shiftKey ? REPO_CARD_NUDGE_BIG_W : REPO_CARD_NUDGE_W;",
		"if (e.key === 'ArrowRight') next = cur + step;",
		"else if (e.key === 'ArrowLeft') next = Math.max(REPO_CARD_MIN_W, cur - step);",
		"else if (e.key === 'Escape') next = 0;",
		// Reset-all sits beside Rescan and goes through the generic
		// data-action dispatcher, not an inline onclick.
		`id="repos-reset-layout-btn" data-action="resetRepoCardWidths"`,
		"function resetRepoCardWidths() {",
		// It only appears once there is a layout to reset.
		"if (resetBtn) resetBtn.style.display = Object.keys(repoCardWidths()).length ? 'inline-block' : 'none';",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
	if strings.Contains(html, "repo-card-resize\" onmousedown") || strings.Contains(html, "repo-card-resize\" onclick") {
		t.Error("the resize handle uses an inline event attribute; CSP script-src-attr is 'none'")
	}
}

// The width store and the truncation rule, EXECUTED. Pattern-matching them
// would catch a rule that was changed and miss one that was always wrong —
// and "the width survives a reload" is exactly a rule that has to be run to
// be believed.
func TestRepoCardWidthStore(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the width-store rules were NOT executed by this run; the structural tests above still ran")
	}
	html := indexHTML(t)
	var script strings.Builder
	script.WriteString(repoCardWidthHarness)
	for _, name := range []string{
		"REPO_CARD_DEFAULT_W", "REPO_CARD_MIN_W", "REPO_CARD_MAX_W",
		"REPO_CARD_WIDTH_KEY_PREFIX", "PILL_TITLE_BASE_LEN", "PILL_TITLE_MAX_LEN",
	} {
		script.WriteString(repoCardConstLine(t, html, name) + "\n")
	}
	for _, name := range []string{
		"safeJsonParse", "repoCardWidthKey", "clampRepoCardWidth",
		"repoCardWidths", "repoCardWidth", "setRepoCardWidth", "pillTitleLimit",
	} {
		script.WriteString(jsFunc(t, html, name) + "\n")
	}
	script.WriteString(repoCardWidthAssertions)

	path := filepath.Join(t.TempDir(), "repo-card-width.js")
	if err := os.WriteFile(path, []byte(script.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("repo card width check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

// A minimal browser: the localStorage the store persists into, and the
// _lastStatus the per-hive key is derived from.
const repoCardWidthHarness = `
const _store = new Map();
const localStorage = {
  getItem: (k) => (_store.has(k) ? _store.get(k) : null),
  setItem: (k, v) => { _store.set(k, String(v)); },
  removeItem: (k) => { _store.delete(k); },
};
const window = { _lastStatus: { hiveId: 'hive-bold-hawk' } };
`

const repoCardWidthAssertions = `
let fails = 0;
function check(name, cond) {
  if (!cond) { fails++; console.log('FAIL ' + name); }
}

// ── the clamp ────────────────────────────────────────────────────────────
check('unset is 0', clampRepoCardWidth(undefined) === 0);
check('zero is 0', clampRepoCardWidth(0) === 0);
check('negative is 0', clampRepoCardWidth(-40) === 0);
check('garbage is 0', clampRepoCardWidth('wide') === 0);
check('below min clamps up', clampRepoCardWidth(40) === REPO_CARD_MIN_W);
check('above max clamps down', clampRepoCardWidth(99999) === REPO_CARD_MAX_W);
check('in range survives', clampRepoCardWidth(512) === 512);
check('fractional drag rounds', clampRepoCardWidth(512.6) === 513);

// ── the per-hive key ─────────────────────────────────────────────────────
check('key names the hive', repoCardWidthKey() === REPO_CARD_WIDTH_KEY_PREFIX + 'hive-bold-hawk');
window._lastStatus = {};
check('key falls back without a hive id', repoCardWidthKey() === REPO_CARD_WIDTH_KEY_PREFIX + 'default');
window._lastStatus = { hiveId: 'hive-bold-hawk' };

// ── store round trip: the width survives a reload ────────────────────────
check('unset repo has no width', repoCardWidth('Danathar/arch-bootc') === 0);
setRepoCardWidth('Danathar/arch-bootc', 520);
check('stored width reads back', repoCardWidth('Danathar/arch-bootc') === 520);
check('a sibling repo is unaffected', repoCardWidth('Danathar/sensi') === 0);
setRepoCardWidth('Danathar/sensi', 190);
check('two repos coexist', repoCardWidth('Danathar/arch-bootc') === 520 && repoCardWidth('Danathar/sensi') === 190);
// Per repo, not per column position: reordering the list cannot move a width.
check('width is keyed by repo full name', Object.keys(repoCardWidths()).sort().join() === 'Danathar/arch-bootc,Danathar/sensi');
// Stored values are clamped on the way in, so a hand-edited or stale entry
// cannot paint a 3px card.
setRepoCardWidth('Danathar/tiny', 4);
check('stored width is clamped', repoCardWidth('Danathar/tiny') === REPO_CARD_MIN_W);

// ── two hives in one browser do not collide ──────────────────────────────
window._lastStatus = { hiveId: 'hive-quiet-fox' };
check('other hive starts clean', repoCardWidth('Danathar/arch-bootc') === 0);
setRepoCardWidth('Danathar/arch-bootc', 300);
window._lastStatus = { hiveId: 'hive-bold-hawk' };
check('first hive keeps its own width', repoCardWidth('Danathar/arch-bootc') === 520);

// ── reset ────────────────────────────────────────────────────────────────
setRepoCardWidth('Danathar/arch-bootc', 0);
check('reset clears the one card', repoCardWidth('Danathar/arch-bootc') === 0);
check('reset leaves the others alone', repoCardWidth('Danathar/sensi') === 190);
check('reset deletes rather than storing a zero', !('Danathar/arch-bootc' in repoCardWidths()));
setRepoCardWidth('Danathar/sensi', 0);
setRepoCardWidth('Danathar/tiny', 0);
// With nothing sized, the key itself is gone — that is what makes the
// "Reset layout" button's visibility a cheap question.
check('the last reset drops the key', _store.get(REPO_CARD_WIDTH_KEY_PREFIX + 'hive-bold-hawk') === undefined);
check('an empty store reads as an empty map', Object.keys(repoCardWidths()).length === 0);

// ── a corrupt store must not break the page ──────────────────────────────
_store.set(REPO_CARD_WIDTH_KEY_PREFIX + 'hive-bold-hawk', 'not json');
check('unparseable store reads empty', Object.keys(repoCardWidths()).length === 0);
_store.set(REPO_CARD_WIDTH_KEY_PREFIX + 'hive-bold-hawk', '[1,2,3]');
check('a non-object store reads empty', Object.keys(repoCardWidths()).length === 0);
_store.set(REPO_CARD_WIDTH_KEY_PREFIX + 'hive-bold-hawk', 'null');
check('a null store reads empty', Object.keys(repoCardWidths()).length === 0);
_store.delete(REPO_CARD_WIDTH_KEY_PREFIX + 'hive-bold-hawk');

// ── the pill title budget grows with the card ────────────────────────────
// This is the half of the issue a wider card alone does not fix: the title
// was cut to 50 characters before the CSS ellipsis ever saw it.
check('default card keeps the old budget', pillTitleLimit(0) === PILL_TITLE_BASE_LEN);
check('at the default width, unchanged', pillTitleLimit(REPO_CARD_DEFAULT_W) === PILL_TITLE_BASE_LEN);
check('narrower than default does not cut further', pillTitleLimit(REPO_CARD_MIN_W) === PILL_TITLE_BASE_LEN);
check('double width, double budget', pillTitleLimit(REPO_CARD_DEFAULT_W * 2) === PILL_TITLE_BASE_LEN * 2);
check('budget grows monotonically', pillTitleLimit(600) > pillTitleLimit(400) && pillTitleLimit(400) > pillTitleLimit(300));
check('budget is capped', pillTitleLimit(REPO_CARD_MAX_W) === PILL_TITLE_MAX_LEN);
check('a widest card can hold a long title whole', pillTitleLimit(REPO_CARD_MAX_W) >= 200);

if (fails) { console.log(fails + ' failure(s)'); process.exit(1); }
console.log('ok');
`
