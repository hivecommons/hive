package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestWorkflowsLibraryTabWiring pins the RFC phase-3b surface
// (hivecommons/hive#17620): Settings grows a Workflows tab that renders the
// library, the governor dispatcher routes to it, the Activity Monitor model
// badge deep-links into it, and the tab never binds (binding stays on
// Settings → Repos).
func TestWorkflowsLibraryTabWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`'Repos', 'Workflows'`,
		`case 'Workflows': return renderGovWorkflows();`,
		`function renderGovWorkflows()`,
		`function renderWorkflowLibraryRows(workflows, bindings, owner)`,
		`function renderWorkflowDetail(detail, bindings, owner, sim)`,
		`function workflowDiagramSvg(model, profile)`,
		`function renderWorkflowIngest(state, owner)`,
		`function openWorkflowLibraryEntry(name, version)`,
		`fetch('/api/workflows/ingest', {`,
		`data-action="openWorkflowLibraryEntry"`,
		`.wf-diagram {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing Workflows tab wiring %q", want)
		}
	}
}

// Pure helpers under node: provenance resolution, the sort order and badge
// text of the library rows, bound-repository lookup, the ingest default
// name, and the diagram's lanes, chips, inferred marking and edges.
func TestWorkflowsLibraryNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	html := indexHTML(t)
	script := `const assert = require('assert');
function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;'); }
const WF_PROVENANCE_LABELS = { 'built-in': 'built-in', 'curated': 'curated', 'local': 'local', 'ingested-unreviewed': 'ingested · unreviewed' };
const WF_DIAGRAM = { colW: 172, colGap: 28, chipH: 30, chipGap: 10, headH: 30, pad: 14, textPad: 10, maxLabel: 22 };
const WF_EDGE = { chan: 8, radius: 6, fanMax: 4, selfR: 12 };
` + jsFunc(t, html, "wfEdgeCommonEvent") + jsFunc(t, html, "wfEdgeChannels") + jsFunc(t, html, "wfEdgeCorridor") +
		jsFunc(t, html, "wfOrthoPath") + jsFunc(t, html, "wfRouteEdges") + jsFunc(t, html, "wfEdgeLabelSvg") +
		jsFunc(t, html, "wfProvenanceOf") + jsFunc(t, html, "wfProvenanceBadge") + jsFunc(t, html, "wfCompiledBadge") +
		jsFunc(t, html, "wfBoundRepos") + jsFunc(t, html, "renderWorkflowLibraryRows") + jsFunc(t, html, "wfTrim") +
		jsFunc(t, html, "workflowDiagramSvg") + jsFunc(t, html, "wfIngestDefaultName") + `
assert.strictEqual(wfProvenanceOf(null), 'local');
assert.strictEqual(wfProvenanceOf({ name: 'hive-default' }), 'built-in');
assert.strictEqual(wfProvenanceOf({ name: 'x', readOnly: true }), 'built-in');
assert.strictEqual(wfProvenanceOf({ name: 'x', provenance: 'ingested-unreviewed' }), 'ingested-unreviewed');
assert.strictEqual(wfProvenanceOf({ name: 'x' }), 'local');

const bindings = [{ repo: 'cncf/techdocs', name: 'techdocs', version: '2' }, { repo: 'cncf/other', name: 'techdocs', version: '1' }];
assert.deepStrictEqual(wfBoundRepos(bindings, 'techdocs', '2'), ['cncf/techdocs']);
assert.deepStrictEqual(wfBoundRepos(bindings, 'techdocs', '3'), []);
assert.deepStrictEqual(wfBoundRepos(null, 'techdocs', '1'), []);

assert.strictEqual(wfIngestDefaultName(' CNCF/TechDocs '), 'cncf-techdocs');
assert.strictEqual(wfIngestDefaultName(''), '');
assert.strictEqual(wfIngestDefaultName('a b/c'), 'a-b-c');

assert.ok(/colspan="7"/.test(renderWorkflowLibraryRows([], [], true)), 'empty state spans every column');

const entries = [
  { name: 'techdocs', version: '2', provenance: 'ingested-unreviewed', compiled: true, diagnostics: [{ severity: 'warning', code: 'W1', message: 'inferred' }], states: 9, columns: 7 },
  { name: 'hive-default', version: '1', readOnly: true, compiled: true, states: 8, columns: 7 },
  { name: 'broken', version: '1', compiled: false, diagnostics: [{ severity: 'error', code: 'E1', message: 'no exit' }], states: 2, columns: 1 },
];
const rows = renderWorkflowLibraryRows(entries, bindings, true);
const keys = [...rows.matchAll(/data-wf-entry="([^"]+)"/g)].map(m => m[1]);
assert.deepStrictEqual(keys, ['hive-default@1', 'broken@1', 'techdocs@2'], 'built-in first, then by name');
assert.ok(rows.includes('compiles · 1 warning'));
assert.ok(rows.includes('does not compile'));
assert.ok(rows.includes('ingested · unreviewed'));
assert.ok(rows.includes('>1 repo<'), 'techdocs@2 shows its one bound repo');
const hd = rows.slice(rows.indexOf('hive-default@1'), rows.indexOf('broken@1'));
assert.ok(!hd.includes('wfLibDelete'), 'built-in has no Delete');
const td = rows.slice(rows.indexOf('data-wf-entry="techdocs@2"'));
assert.ok(!td.includes('wfLibDelete') && td.includes('>bound<'), 'bound model shows bound instead of Delete');
const br = rows.slice(rows.indexOf('data-wf-entry="broken@1"'), rows.indexOf('data-wf-entry="techdocs@2"'));
assert.ok(br.includes('wfLibDelete'), 'unbound local model can be deleted');
const ro = renderWorkflowLibraryRows(entries, bindings, false);
assert.ok(!ro.includes('wfLibDuplicate') && !ro.includes('wfLibDelete'), 'non-owners get View/Export only');

assert.ok(workflowDiagramSvg({ spec: {} }, null).includes('declares no columns'));
const model = { spec: {
  initial: 'open',
  states: [{ id: 'open', label: 'Open', lifecycle: { number: 10 } }, { id: 'review', label: 'Review', lifecycle: { number: 50 } },
           { id: 'merged', label: 'Merged', lifecycle: { number: 100 } }, { id: 'deploy', label: 'Deploy prod', lifecycle: { confidence: 'inferred' } }],
  transitions: [{ from: 'open', to: 'review', on: 'pr-opened' }, { from: 'review', to: 'open', on: 'changes-requested' },
                { from: 'review', to: 'merged', on: 'merged' }, { from: 'merged', to: 'deploy', on: 'pipeline-succeeded' }, { from: 'open', to: 'review', on: 'dup' }, { from: 'x', to: 'review' }],
} };
const profile = { columns: [{ id: 'c1', label: 'Open', states: ['open'] }, { id: 'c2', label: 'Review', states: ['review'] },
  { id: 'c3', label: 'Merged', states: ['merged', 'deploy'], terminal: true }], terminal: { merged: true } };
const svg = workflowDiagramSvg(model, profile);
assert.strictEqual((svg.match(/class="wf-dg-lane"/g) || []).length, 3, 'one lane per column');
assert.strictEqual((svg.match(/class="wf-dg-chip"/g) || []).length, 4, 'one chip per state');
assert.strictEqual((svg.match(/<g class="wf-dg-edge[ "]/g) || []).length, 4, 'duplicate and dangling edges dropped');
assert.strictEqual((svg.match(/wf-dg-edge back/g) || []).length, 1, 'backward edge marked');
assert.ok(svg.includes('data-wf-focus-root="1"') && svg.includes('class="wf-edge-layer"'), 'diagram opts into hover focus');
assert.ok(svg.includes('data-wf-from="review" data-wf-to="open"'), 'edges carry endpoints for focus mode');
assert.strictEqual((svg.match(/class="wf-dg-edge-label"/g) || []).length, 4, 'labels are rendered (CSS hides them until hover)');
assert.ok(!svg.includes('<path d="M') || !/wf-dg-edge-line" marker-end="url\(#wf-arrow\)"\/><text/.test(svg), 'no bare text labels on edges');
assert.ok(svg.includes('fill="context-stroke"'), 'arrowheads follow the edge colour');
assert.ok(svg.includes('wf-dg-state inferred'), 'inferred state marked');
assert.ok(svg.includes('wf-dg-state initial'), 'initial state marked');
assert.ok(svg.includes('>010<') && svg.includes('>100<'), 'stage numbers padded');
assert.ok(svg.includes('wf-dg-col terminal'), 'terminal column marked');
console.log('ok');
`
	cmd := exec.Command(node, "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("unexpected node output: %s", out)
	}
}

// The Library header's workflow.enabled switch (hivecommons/hive#17620):
// wired to PUT /api/workflows/enabled with a confirm on the way on, and the
// Repos binding row's "columns only" hint when a binding exists while
// derivation is off.
func TestWorkflowsEnabledToggleWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`function renderWorkflowEnabledToggle(enabled, owner, busy)`,
		`async function wfToggleEnabled(el)`,
		`fetch('/api/workflows/enabled', { method: 'PUT'`,
		`title: 'Derive stages from bound models?'`,
		`data-action="wfToggleEnabled"`,
		`renderWorkflowLibraryTable(_workflowState.workflows, _workflowState.bindings, owner, _workflowState.enabled, _wfLib.enabledBusy)`,
		`columns only — turn on derivation in Workflows`,
		`.wf-enabled-toggle {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing workflow.enabled toggle wiring %q", want)
		}
	}
}

func TestWorkflowsEnabledToggleNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	html := indexHTML(t)
	script := `const assert = require('assert');
function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;'); }
function yamlHint(p) { return '<code>' + p + '</code>'; }
` + jsFunc(t, html, "renderWorkflowEnabledToggle") + `
const off = renderWorkflowEnabledToggle(false, true, false);
assert.ok(off.includes('data-wf-enabled="off"') && off.includes('aria-checked="false"'), 'off state');
assert.ok(off.includes('data-action="wfToggleEnabled"'), 'owner can flip');
assert.ok(!off.includes('config-toggle-switch on'), 'switch not on');
assert.ok(off.includes('Off: a binding only changes'), 'off explanation');
assert.ok(off.includes('workflow.enabled'), 'names the yaml key');

const on = renderWorkflowEnabledToggle(true, true, false);
assert.ok(on.includes('config-toggle-switch on') && on.includes('aria-checked="true"'), 'on state');
assert.ok(on.includes('On: a bound repository'), 'on explanation');

const viewer = renderWorkflowEnabledToggle(false, false, false);
assert.ok(!viewer.includes('data-action="wfToggleEnabled"') && viewer.includes('aria-disabled="true"') && viewer.includes(' disabled'), 'non-owner sees a locked switch');
assert.ok(viewer.includes('Owner role is required'), 'non-owner title explains');

const busy = renderWorkflowEnabledToggle(true, true, true);
assert.ok(!busy.includes('data-action="wfToggleEnabled"') && busy.includes(' disabled'), 'busy switch is locked');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
