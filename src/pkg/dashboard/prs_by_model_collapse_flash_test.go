package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// A collapsed "PRs by model" subsection flashed its ranking open on every
// Governor re-render (a window or sort pill click, a status refresh): the
// markup was emitted expanded, renderGovernor then read layout (sparkline
// replay) and only afterwards applySectionCollapse added .collapsed, which
// ran the .section-body max-height/opacity transition. The tile must render
// its saved state from the start, and a pill click on the collapsed
// subsection expands it instead of changing data nobody can see.

func TestGovernorPRModelsRendersSavedCollapseStateInMarkup(t *testing.T) {
	html := indexHTML(t)
	body := jsFunctionBody(t, html, "function renderGovernorPRModelsTile()")
	for _, want := range []string{
		"isSectionCollapsed('governor-pr-models-section')",
		`class="advisory-subsection governor-subsection${collapsedCls}"`,
		`aria-expanded="${collapsed ? 'false' : 'true'}"`,
		`<span class="section-chevron${collapsedCls}"`,
		`<div class="section-body${collapsedCls}" id="governor-pr-models-section-body">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderGovernorPRModelsTile must emit the saved collapse state; missing %q", want)
		}
	}
	if strings.Contains(body, `aria-expanded="true" aria-controls="governor-pr-models-section-body"`) {
		t.Error("PRs-by-model header hard-codes aria-expanded=\"true\"; a collapsed subsection then flashes open on re-render")
	}
	for _, fn := range []string{"function setGovernorPRModelsWindow(windowName)", "function setGovernorPRModelsSort(sortName)"} {
		if !strings.Contains(jsFunctionBody(t, html, fn), "expandGovernorPRModelsSection();") {
			t.Errorf("%s must expand a collapsed PRs-by-model subsection", fn)
		}
	}
}

func TestGovernorPRModelsPillsExpandCollapsedSubsectionWithoutFlash(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; the PRs-by-model collapse rule was NOT executed")
	}
	html := indexHTML(t)
	var script strings.Builder
	script.WriteString(`
const assert = require('node:assert/strict');
let govPRModelsWindow = '7d';
let govPRModelsSort = 'effectiveness';
let govPRModelsColumnSort = { key: '', dir: 'desc' };
let govPRModelsData = null;
let collapsed = true;
const toggles = [];
const loads = [];
let renders = 0;
const window = { _lastStatus: { governor: {}, cadenceMatrix: [] } };
const escapeHtml = s => String(s);
const dashboardSectionHelpMark = () => '';
const governorPRModelRows = () => [];
const governorPRModelsHiveCostNote = () => '';
function isSectionCollapsed(id) { assert.equal(id, 'governor-pr-models-section'); return collapsed; }
function toggleSection(id, opts) { toggles.push([id, opts]); collapsed = !!opts.collapsed; }
function loadGovernorPRModels(force, w) { loads.push([force, w]); }
function renderGovernor() { renders++; }
let windowRenders = 0;
function renderGovernorFromLastStatus() { windowRenders++; }
`)
	for _, name := range []string{"normalizeGovernorPRModelsWindowName", "expandGovernorPRModelsSection", "setGovernorPRModelsWindow", "setGovernorPRModelsSort", "renderGovernorPRModelsTile"} {
		script.WriteString(jsFunc(t, html, name) + "\n")
	}
	script.WriteString(`
const sectionTag = h => h.match(/<div id="governor-pr-models-section"[^>]*>/)[0];
const headerTag = h => h.match(/<div class="section-label section-header-toggle[^>]*>/)[0];
const chevronTag = h => h.match(/<span class="section-chevron[^>]*>/)[0];
const bodyTag = h => h.match(/<div class="section-body[^>]*>/)[0];

// Collapsed: the very first markup is already collapsed, so no transition runs.
let html = renderGovernorPRModelsTile();
assert.match(sectionTag(html), /class="[^"]*\bcollapsed\b/);
assert.match(headerTag(html), /aria-expanded="false"/);
assert.match(chevronTag(html), /class="section-chevron collapsed"/);
assert.match(bodyTag(html), /class="section-body collapsed"/);

// A window pill on the collapsed subsection expands it (persisted like the header toggle) and loads the window.
setGovernorPRModelsWindow('30d');
assert.deepEqual(toggles, [['governor-pr-models-section', { collapsed: false }]]);
assert.deepEqual(loads, [[true, '30d']]);
assert.equal(windowRenders, 1, 'a window pill re-renders at once so the new window shows as loading');
html = renderGovernorPRModelsTile();
assert.doesNotMatch(sectionTag(html), /\bcollapsed\b/);
assert.match(headerTag(html), /aria-expanded="true"/);
assert.match(chevronTag(html), /class="section-chevron"/);
assert.match(bodyTag(html), /class="section-body"/);

// Pills on an expanded subsection never toggle it.
setGovernorPRModelsWindow('all');
setGovernorPRModelsSort('count');
assert.equal(toggles.length, 1, 'a pill click must not collapse an expanded subsection');
assert.equal(govPRModelsSort, 'count');
assert.equal(renders, 1);
assert.equal(windowRenders, 2);

// A sort pill on the collapsed subsection expands it too.
collapsed = true;
setGovernorPRModelsSort('effectiveness');
assert.deepEqual(toggles[1], ['governor-pr-models-section', { collapsed: false }]);
assert.equal(renders, 2);
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("PRs-by-model collapse check failed:\n%s", strings.TrimSpace(string(out)))
	}
}
