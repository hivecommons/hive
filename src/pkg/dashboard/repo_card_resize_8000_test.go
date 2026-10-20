package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Repository card widths persist per hive and are clamped to usable bounds.

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
		// Reset-all sits in the expanded Projects toolbar beside Rescan and
		// goes through the generic data-action dispatcher, not an inline onclick.
		`id="repos-reset-layout-btn" data-action="resetRepoCardWidths"`,
		"function resetRepoCardWidths() {",
		// It only appears once there is a layout to reset.
		"if (resetBtn) resetBtn.style.display = (Object.keys(repoCardWidths()).length || repoOrderHasCustom(repos)) ? 'inline-block' : 'none';",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
	if strings.Contains(html, "repo-card-resize\" onmousedown") || strings.Contains(html, "repo-card-resize\" onclick") {
		t.Error("the resize handle uses an inline event attribute; CSP script-src-attr is 'none'")
	}
}

// Execute the width-store rules against the real dashboard functions.
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
		"REPO_CARD_WIDTH_KEY_PREFIX",
	} {
		script.WriteString(repoCardConstLine(t, html, name) + "\n")
	}
	for _, name := range []string{
		"safeJsonParse", "repoCardWidthKey", "clampRepoCardWidth",
		"repoCardWidths", "repoCardWidth", "setRepoCardWidth",
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
// _lastStatus the per-hive key is derived from. setRepoCardWidth also pokes
// the user-menu layout status (#10005); that is DOM work outside the store
// rules under test, so it is a no-op here.
const repoCardWidthHarness = `
const _store = new Map();
const localStorage = {
  getItem: (k) => (_store.has(k) ? _store.get(k) : null),
  setItem: (k, v) => { _store.set(k, String(v)); },
  removeItem: (k) => { _store.delete(k); },
};
const window = { _lastStatus: { hiveId: 'hive-bold-hawk' } };
function dashboardUpdateLayoutMenuState() {}
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

if (fails) { console.log(fails + ' failure(s)'); process.exit(1); }
console.log('ok');
`
