package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestWorkflowsEditorWiring pins the RFC phase-4 editor surface
// (hivecommons/hive#17620): the Workflows nav grows an Editor item, the
// library and detail grow Edit / Edit-a-copy buttons for owners, the ingest
// result can open on the canvas, the editor module is bundled with its
// stylesheet, and the live-compile / YAML / simulate endpoints are called.
func TestWorkflowsEditorWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`['editor', 'Editor']`,
		`body = renderWorkflowEditor(owner);`,
		`function renderWorkflowEditor(owner)`,
		`function wfEdAttach(root)`,
		`data-action="wfEdOpen"`,
		`data-action="wfEdCopyOf"`,
		`data-action="wfEdOpenIngestResult"`,
		`fetch('/api/workflows/compile', {`,
		`fetch('/api/workflows/yaml', {`,
		`fetch('/api/workflows/simulate', {`,
		`if (tabId === 'Workflows' && typeof wfEdAfterRender === 'function') wfEdAfterRender();`,
		`.wf-ed-canvas {`,
		`.wf-ed-state.wf-ed-sim-hit`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing workflow editor wiring %q", want)
		}
	}
	for _, banned := range []string{"toLocaleString", ".download =", "createObjectURL("} {
		src := html[strings.Index(html, "function wfEdNewModel("):]
		if end := strings.Index(src, "function renderGovWorkflows("); end > 0 {
			src = src[:end]
		}
		if strings.Contains(src, banned) {
			t.Errorf("editor module must not use %q", banned)
		}
	}
}

// The editor's model-mutation, layout, diff and validation core is pure:
// every helper returns a new model and never touches its input, so the
// undo stack can be a plain snapshot list. Executed under node.
func TestWorkflowsEditorNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	html := indexHTML(t)
	script := `const assert = require('assert');
function esc(s) { return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;'); }
function wfTrim(s, n) { s = String(s || ''); return s.length > n ? s.slice(0, n - 1) + '…' : s; }
const WF_ED = { colW: 200, colGap: 32, chipH: 34, chipGap: 12, headH: 36, pad: 16, grid: 8, minRows: 4, portR: 5, minColW: 120, maxColW: 480 };
const WF_ED_UNASSIGNED = '__unassigned';
const WF_ED_API_VERSION = 'workflow.hivecommons.dev/v1alpha1';
const WF_ED_KINDS = ['issue', 'pr'];
const WF_EDGE = { chan: 8, radius: 6, fanMax: 4, selfR: 12 };
` + jsFunc(t, html, "wfEdgeCommonEvent") + jsFunc(t, html, "wfEdgeChannels") + jsFunc(t, html, "wfEdgeCorridor") +
		jsFunc(t, html, "wfOrthoPath") + jsFunc(t, html, "wfRouteEdges") + jsFunc(t, html, "wfEdgeLabelSvg") +
		jsFunc(t, html, "wfEdClone") + jsFunc(t, html, "wfEdSlug") + jsFunc(t, html, "wfEdSpec") +
		jsFunc(t, html, "wfEdStates") + jsFunc(t, html, "wfEdFindState") + jsFunc(t, html, "wfEdFindColumn") +
		jsFunc(t, html, "wfEdUniqueId") + jsFunc(t, html, "wfEdNewModel") + jsFunc(t, html, "wfEdNormalize") +
		jsFunc(t, html, "wfEdAddColumn") + jsFunc(t, html, "wfEdMoveColumn") + jsFunc(t, html, "wfEdRenameColumn") +
		jsFunc(t, html, "wfEdRemoveColumn") + jsFunc(t, html, "wfEdAddState") + jsFunc(t, html, "wfEdMoveState") +
		jsFunc(t, html, "wfEdRenameState") + jsFunc(t, html, "wfEdRemoveState") + jsFunc(t, html, "wfEdAddTransition") +
		jsFunc(t, html, "wfEdRemoveTransition") + jsFunc(t, html, "wfEdPath") + jsFunc(t, html, "wfEdUpdate") +
		jsFunc(t, html, "wfEdDepths") + jsFunc(t, html, "wfEdAutoLayout") + jsFunc(t, html, "wfEdSnap") +
		jsFunc(t, html, "wfEdLayout") + jsFunc(t, html, "wfEdLaneAt") + jsFunc(t, html, "wfEdSlotAt") +
		jsFunc(t, html, "wfEdLocalProblems") + jsFunc(t, html, "wfEdDiff") + jsFunc(t, html, "wfEdSvg") +
		jsFunc(t, html, "wfEdMinimapSvg") + jsFunc(t, html, "wfEdFitView") + jsFunc(t, html, "wfEdCompileModel") +
		jsFunc(t, html, "wfEdStatus") + `
const frozen = m => JSON.stringify(m);

// New model: minimal, compiles-shaped, name slugged.
const m0 = wfEdNewModel(' My Lifecycle ');
assert.strictEqual(m0.metadata.name, 'my-lifecycle');
assert.strictEqual(m0.apiVersion, WF_ED_API_VERSION);
assert.strictEqual(m0.spec.initial, 'open');
assert.deepStrictEqual(m0.spec.columns.map(c => c.id), ['intake']);
assert.strictEqual(m0.spec.states[0].lifecycle.column, 'intake');

// Columns: add (unique id), move, rename (updates lifecycle.column), remove (moves states).
const snap0 = frozen(m0);
let m = wfEdAddColumn(m0, { label: 'Review' });
m = wfEdAddColumn(m, { label: 'Review' });
m = wfEdAddColumn(m, { id: 'done', label: 'Done', terminal: true });
assert.strictEqual(frozen(m0), snap0, 'addColumn must not mutate its input');
assert.deepStrictEqual(m.spec.columns.map(c => c.id), ['intake', 'review', 'review-2', 'done']);
assert.strictEqual(m.spec.columns[3].terminal, true);
m = wfEdMoveColumn(m, 'done', 0);
assert.deepStrictEqual(m.spec.columns.map(c => c.id), ['done', 'intake', 'review', 'review-2']);
m = wfEdMoveColumn(m, 'done', 3);
assert.deepStrictEqual(m.spec.columns.map(c => c.id), ['intake', 'review', 'review-2', 'done']);
m = wfEdRenameColumn(m, 'intake', 'Triage Lane');
assert.deepStrictEqual(m.spec.columns.map(c => c.id), ['triage-lane', 'review', 'review-2', 'done']);
assert.strictEqual(wfEdFindState(m, 'open').lifecycle.column, 'triage-lane');
assert.deepStrictEqual(wfEdFindColumn(m, 'triage-lane').states, ['open']);

// States: add into lane at index, move between lanes, rename with reference update, remove.
m = wfEdAddState(m, { label: 'In review', lifecycle: { number: 50 } }, 'review');
m = wfEdAddState(m, { label: 'Merged', lifecycle: { number: 90 } }, 'done');
m = wfEdAddState(m, { label: 'Triage', lifecycle: { number: 20 } }, 'triage-lane', 0);
assert.deepStrictEqual(wfEdFindColumn(m, 'triage-lane').states, ['triage', 'open']);
assert.strictEqual(wfEdFindState(m, 'in-review').owner, 'human');
m = wfEdMoveState(m, 'triage', 'triage-lane', 1);
assert.deepStrictEqual(wfEdFindColumn(m, 'triage-lane').states, ['open', 'triage']);
m = wfEdMoveState(m, 'triage', 'review', 0);
assert.deepStrictEqual(wfEdFindColumn(m, 'review').states, ['triage', 'in-review']);
assert.strictEqual(wfEdFindState(m, 'triage').lifecycle.column, 'review');
m = wfEdMoveState(m, 'triage', WF_ED_UNASSIGNED);
assert.strictEqual(wfEdFindState(m, 'triage').lifecycle.column, undefined);
assert.deepStrictEqual(wfEdFindColumn(m, 'review').states, ['in-review']);
m = wfEdMoveState(m, 'triage', 'triage-lane');

// Transitions: dedupe, refuse dangling, remove by index.
const before = frozen(m);
m = wfEdAddTransition(m, 'open', 'triage', 'triaged');
m = wfEdAddTransition(m, 'open', 'triage', 'triaged');
m = wfEdAddTransition(m, 'open', 'nope', '');
assert.strictEqual(m.spec.transitions.length, 1, 'duplicate and dangling transitions are refused');
m = wfEdAddTransition(m, 'triage', 'in-review', '');
m = wfEdAddTransition(m, 'in-review', 'merged', 'merged');
m = wfEdAddTransition(m, 'in-review', 'triage', 'changes-requested');
assert.strictEqual(m.spec.transitions.length, 4);
m = wfEdUpdate(m, 'spec.labels.entries', [{ name: 'stage/triage', state: 'triage' }]);
m = wfEdUpdate(m, 'spec.derivation.rules', [{ id: 'r1', state: 'triage', when: { labels: ['triage'] } }]);
m = wfEdUpdate(m, 'spec.initial', 'triage');
const renamed = wfEdRenameState(m, 'triage', 'Needs Triage');
assert.strictEqual(wfEdFindState(renamed, 'triage'), null);
assert.ok(wfEdFindState(renamed, 'needs-triage'));
assert.strictEqual(renamed.spec.initial, 'needs-triage');
assert.deepStrictEqual(wfEdFindColumn(renamed, 'triage-lane').states, ['open', 'needs-triage']);
assert.strictEqual(renamed.spec.transitions.filter(t => t.from === 'needs-triage' || t.to === 'needs-triage').length, 3);
assert.strictEqual(renamed.spec.labels.entries[0].state, 'needs-triage');
assert.strictEqual(renamed.spec.derivation.rules[0].state, 'needs-triage');
assert.strictEqual(frozen(wfEdRenameState(m, 'triage', 'open')), frozen(wfEdNormalize(m)), 'renaming onto a taken id is a no-op');
const removed = wfEdRemoveState(renamed, 'needs-triage');
assert.strictEqual(removed.spec.transitions.length, 1, 'transitions touching the removed state go with it');
assert.strictEqual(removed.spec.initial, 'open', 'initial falls back to the first remaining state');
assert.strictEqual(removed.spec.labels.entries.length, 0);
assert.strictEqual(wfEdRemoveTransition(m, 0).spec.transitions.length, 3);
assert.strictEqual(wfEdRemoveTransition(m, 99).spec.transitions.length, 4);

// Remove a column moving its states elsewhere, or leaving them unassigned.
const moved = wfEdRemoveColumn(m, 'review', 'done');
assert.deepStrictEqual(wfEdFindColumn(moved, 'done').states, ['merged', 'in-review']);
assert.strictEqual(wfEdFindState(moved, 'in-review').lifecycle.column, 'done');
const loose = wfEdRemoveColumn(m, 'review');
assert.strictEqual(wfEdFindState(loose, 'in-review').lifecycle.column, undefined);
assert.strictEqual(frozen(m), frozen(wfEdNormalize(m)), 'remove must not mutate');

// Path updates: keyed selectors, indices, deletes on empty.
let u = wfEdUpdate(m, 'spec.states[id=open].lifecycle.number', 15);
assert.strictEqual(wfEdFindState(u, 'open').lifecycle.number, 15);
assert.strictEqual(wfEdFindState(m, 'open').lifecycle.number, 10, 'update must not mutate');
u = wfEdUpdate(u, 'spec.transitions[1].guard', 'ci.green');
assert.strictEqual(u.spec.transitions[1].guard, 'ci.green');
u = wfEdUpdate(u, 'spec.transitions[1].guard', '');
assert.strictEqual('guard' in u.spec.transitions[1], false);
u = wfEdUpdate(u, 'spec.meta.description', 'hello');
assert.strictEqual(u.spec.meta.description, 'hello');
assert.strictEqual(frozen(wfEdUpdate(u, 'spec.states[id=missing].label', 'x')), frozen(u), 'unknown selector is a no-op');

// Unique ids never collide with states or columns.
assert.strictEqual(wfEdUniqueId(m, 'Open'), 'open-2');
assert.strictEqual(wfEdUniqueId(m, 'done'), 'done-2');
assert.strictEqual(wfEdUniqueId(m, 'Brand new!'), 'brand-new');

// Auto-layout: lanes ordered by depth from initial, terminal last, lanes sorted by stage number.
let al = wfEdNewModel('al');
al = wfEdAddColumn(al, { id: 'c-late' });
al = wfEdAddColumn(al, { id: 'c-end', terminal: true });
al = wfEdAddColumn(al, { id: 'c-mid' });
al = wfEdAddState(al, { id: 'b', label: 'B', lifecycle: { number: 30 } }, 'c-late');
al = wfEdAddState(al, { id: 'a', label: 'A', lifecycle: { number: 20 } }, 'c-late');
al = wfEdAddState(al, { id: 'c', label: 'C', lifecycle: { number: 40 } }, 'c-mid');
al = wfEdAddState(al, { id: 'z', label: 'Z', lifecycle: { number: 99 } }, 'c-end');
al = wfEdAddState(al, { id: 'free', label: 'Free', lifecycle: { number: 45 } });
al = wfEdAddTransition(al, 'open', 'a', '');
al = wfEdAddTransition(al, 'a', 'b', '');
al = wfEdAddTransition(al, 'b', 'c', '');
al = wfEdAddTransition(al, 'c', 'free', '');
al = wfEdAddTransition(al, 'free', 'z', '');
al = wfEdMoveColumn(al, 'c-end', 0);
const laid = wfEdAutoLayout(al);
assert.deepStrictEqual(laid.spec.columns.map(c => c.id), ['intake', 'c-late', 'c-mid', 'c-end']);
assert.deepStrictEqual(wfEdFindColumn(laid, 'c-late').states, ['a', 'b'], 'lane sorted by stage number');
assert.ok(wfEdFindState(laid, 'free').lifecycle.column, 'unassigned states are placed by depth');
assert.strictEqual(frozen(wfEdAutoLayout(laid)), frozen(laid), 'auto-layout is idempotent');
assert.strictEqual(wfEdFindState(al, 'free').lifecycle.column, undefined, 'auto-layout must not mutate');

// Layout geometry: lanes left→right, chips stacked, unassigned lane only when needed, edges classified.
const lay = wfEdLayout(m, null, {});
assert.deepStrictEqual(lay.cols.map(c => c.id), ['triage-lane', 'review', 'review-2', 'done']);
assert.ok(lay.cols[1].x > lay.cols[0].x + lay.cols[0].w);
assert.strictEqual(lay.chips.open.col, 'triage-lane');
assert.ok(lay.chips.triage.y > lay.chips.open.y, 'second chip sits below the first');
assert.strictEqual(lay.edges.length, 4);
assert.strictEqual(lay.edges.find(e => e.from === 'in-review' && e.to === 'triage').back, true, 'right-to-left edges are backward');
assert.strictEqual(lay.edges.find(e => e.from === 'open' && e.to === 'triage').back, false, 'downward same-lane edge is forward');
const layLoose = wfEdLayout(loose, null, {});
assert.strictEqual(layLoose.cols[layLoose.cols.length - 1].id, WF_ED_UNASSIGNED);
assert.strictEqual(layLoose.chips['in-review'].col, WF_ED_UNASSIGNED);
const wide = wfEdLayout(m, null, { laneWidths: { review: 333 } });
assert.strictEqual(wide.cols[1].w, 336, 'lane widths snap to the grid');
assert.strictEqual(wfEdLayout(m, null, { laneWidths: { review: 9999 } }).cols[1].w, WF_ED.maxColW);
assert.strictEqual(wfEdLaneAt(lay, lay.cols[2].x + 5).id, 'review-2');
assert.strictEqual(wfEdLaneAt(lay, -500), null);
assert.strictEqual(wfEdSlotAt(lay, lay.cols[0], lay.chips.triage.y + 2), 1);
assert.strictEqual(wfEdSlotAt(lay, lay.cols[0], -1000), 0);
assert.strictEqual(wfEdSlotAt(lay, lay.cols[0], 1e6), 2);
assert.strictEqual(JSON.stringify(wfEdLayout(m, null, {})), JSON.stringify(lay), 'layout is deterministic');
const fit = wfEdFitView(lay, 800, 600);
assert.ok(fit.zoom > 0 && fit.zoom <= 1.5);

// Local problems mirror the compiler's refusal codes.
const codes = model => wfEdLocalProblems(model).map(p => p.code).sort();
assert.deepStrictEqual(codes(wfEdNewModel('x')), ['no-terminal-column', 'state-no-exit']);
assert.ok(!wfEdLocalProblems(m).some(p => p.code === 'state-no-exit' && p.subject === 'merged'), 'states in a terminal lane need no exit');
assert.ok(wfEdLocalProblems(m).some(p => p.code === 'column-empty' && p.subject === 'review-2'), 'empty lanes warn');
let bad = wfEdNewModel('bad');
bad = wfEdAddColumn(bad, { id: 'end', terminal: true });
bad = wfEdAddState(bad, { id: 'a', lifecycle: { number: 10 } }, 'end');
bad = wfEdUpdate(bad, 'spec.transitions', [{ from: 'open', to: 'ghost', on: '' }]);
bad = wfEdUpdate(bad, 'spec.states[id=a].lifecycle.column', 'elsewhere');
bad = wfEdUpdate(bad, 'spec.initial', 'nobody');
bad = wfEdUpdate(bad, 'spec.states[id=a].lifecycle.confidence', 'inferred');
const badCodes = codes(bad);
for (const want of ['unknown-state', 'duplicate-stage-number', 'state-no-column', 'inferred-element']) {
  assert.ok(badCodes.includes(want), 'expected ' + want + ' in ' + badCodes.join(','));
}
assert.ok(wfEdLocalProblems(bad).some(p => p.code === 'unknown-state' && p.subject === 'nobody'), 'missing initial is reported');
assert.ok(wfEdLocalProblems(bad).some(p => p.code === 'unknown-state' && p.subject === 'open->ghost'), 'dangling transition is reported');
assert.strictEqual(wfEdLocalProblems(bad).find(p => p.code === 'inferred-element').severity, 'warning');
let ok = wfEdNewModel('ok');
ok = wfEdAddColumn(ok, { id: 'end', terminal: true });
ok = wfEdAddState(ok, { id: 'done', lifecycle: { number: 90 } }, 'end');
ok = wfEdAddTransition(ok, 'open', 'done', '');
assert.deepStrictEqual(codes(ok), [], 'a two-lane model with an exit and a terminal lane has no local problems');
assert.deepStrictEqual(codes({}), ['invalid', 'invalid', 'invalid']);

// Structural diff mirrors the Go ModelDiff shape.
const d0 = wfEdDiff(ok, ok);
assert.strictEqual(d0.empty, true);
assert.strictEqual(d0.summary, 'no changes');
let ok2 = wfEdAddColumn(ok, { id: 'extra' });
ok2 = wfEdUpdate(ok2, 'spec.states[id=done].label', 'Finished');
ok2 = wfEdRemoveTransition(ok2, 0);
ok2 = wfEdUpdate(ok2, 'spec.meta.description', 'v2');
const d1 = wfEdDiff(ok, ok2);
assert.deepStrictEqual(d1.columns.added, ['extra']);
assert.deepStrictEqual(d1.transitions.removed, ['open>done']);
assert.deepStrictEqual(d1.states.changed.map(c => c.key), ['done']);
assert.deepStrictEqual(d1.states.changed[0].fields, [{ field: 'label', from: 'done', to: 'Finished' }]);
assert.deepStrictEqual(d1.meta, [{ field: 'description', from: '', to: 'v2' }]);
assert.strictEqual(d1.empty, false);
assert.strictEqual(d1.summary, '+1 columns, \u22121 transitions, 2 changed');
const d2 = wfEdDiff(ok2, ok);
assert.deepStrictEqual(d2.columns.removed, ['extra']);
assert.deepStrictEqual(d2.transitions.added, ['open>done']);

// SVG rendering: deterministic, marks initial/terminal/selected/inferred/sim-hit, strips nothing needed.
const svgState = { selection: { type: 'state', ids: ['open'] }, diags: wfEdLocalProblems(bad), view: { panX: 0, panY: 0, zoom: 1 }, simHit: 'done' };
const svg = wfEdSvg(ok, wfEdLayout(ok, null, {}), svgState);
assert.ok(svg.startsWith('<svg'), 'renders an svg root');
assert.ok(svg.includes('wf-ed-state initial'), 'initial state gets the marker class');
assert.ok(/wf-ed-col terminal/.test(svg), 'terminal column is marked');
assert.ok(/wf-ed-state[^"]*selected/.test(svg), 'selection is reflected');
assert.ok(svg.includes('wf-ed-sim-hit'), 'simulate hit is highlighted');
assert.ok(svg.includes('wf-ed-sim-dim'), 'other states are dimmed during simulate');
assert.ok(svg.includes('tabindex="0"'), 'chips are focusable');
assert.ok(svg.includes('wf-ed-port'), 'chips expose a transition port');
assert.ok(svg.includes('wf-ed-edge'), 'transitions are drawn');
assert.strictEqual(svg, wfEdSvg(ok, wfEdLayout(ok, null, {}), svgState), 'svg is deterministic');
assert.ok(!svg.includes('style='), 'no inline styles leak into the canvas');
const svgBad = wfEdSvg(bad, wfEdLayout(bad, null, {}), { diags: wfEdLocalProblems(bad) });
assert.ok(svgBad.includes('wf-ed-badge'), 'diagnostics render as badges');
assert.ok(svgBad.includes('wf-ed-confirm'), 'inferred states get a confirm affordance');
assert.ok(svgBad.includes('inferred'), 'inferred states are dashed');
// Quiet-by-default edge layer: every edge renders a label pill but nothing
// marks it visible unless selected; the most common event is tagged
// 'common' so the Labels toggle can skip it; a selected state puts the
// canvas in focus mode.
let quiet = wfEdAddTransition(wfEdAddTransition(ok, 'done', 'open', 'reopened'), 'open', 'done', 'stage-change');
quiet = wfEdAddTransition(wfEdAddState(quiet, { id: 'mid', label: 'Mid' }, 'intake'), 'mid', 'done', 'stage-change');
const quietLay = wfEdLayout(quiet, null, {});
const quietSvg = wfEdSvg(quiet, quietLay, { selection: { type: '', ids: [] }, diags: [] });
assert.ok(quietSvg.includes('class="wf-ed-edge-label"'), 'edge labels are rendered as pills');
assert.ok(quietSvg.startsWith('<svg class="wf-ed-svg"') && !quietSvg.includes('show-labels'), 'no focus or label mode by default');
assert.ok(quietSvg.includes('data-wf-from="done" data-wf-to="open"'), 'edges expose endpoints for hover focus');
assert.ok(quietSvg.includes('data-wf-focus-root="1"'), 'canvas is a focus root');
const labelsSvg = wfEdSvg(quiet, quietLay, { selection: { type: '', ids: [] }, diags: [], showLabels: true });
assert.ok(labelsSvg.includes('class="wf-edge-layer show-labels"'), 'labels toggle flags the layer');
assert.strictEqual(quietLay.commonEvent, 'stage-change', 'most common event detected');
assert.ok(/class="wf-ed-edge[^"]* common"[^>]*data-wf-from="open" data-wf-to="done"/.test(labelsSvg), 'common-event edges are tagged so their labels stay hidden');
const focusSvg = wfEdSvg(quiet, quietLay, { selection: { type: 'state', ids: ['done'] }, diags: [] });
assert.ok(focusSvg.startsWith('<svg class="wf-ed-svg wf-focus"'), 'selecting a state enters focus mode');
assert.ok(/class="wf-ed-edge[^"]* focus"[^>]*data-wf-from="done"/.test(focusSvg), 'incident edges are focused');
assert.ok(/class="wf-ed-state[^"]* focus[^"]*"[^>]*data-wf-state="open"/.test(focusSvg), 'neighbour chips are focused');
const selEdgeSvg = wfEdSvg(quiet, quietLay, { selection: { type: 'transition', ids: [String(quietLay.edges.find(e => e.from === 'done').index)] }, diags: [] });
assert.ok(/class="wf-ed-edge back selected"/.test(selEdgeSvg), 'selected edge is marked');

// Orthogonal router: bundling by lane pair, back edges outside, fan collapse.
const geom = { gap: 32, pad: 16, cols: [{ x: 16, y: 40, w: 200, h: 300 }, { x: 248, y: 40, w: 200, h: 300 }, { x: 480, y: 40, w: 200, h: 300 }], chips: {} };
const mkChip = (id, ci, ri) => { geom.chips[id] = { id: id, x: geom.cols[ci].x + 8, y: 40 + 36 + 12 + ri * 46, w: 184, h: 34, colIndex: ci, index: ri }; };
mkChip('a1', 0, 0); mkChip('a2', 0, 1); mkChip('a3', 0, 2); mkChip('b1', 1, 0); mkChip('b2', 1, 1); mkChip('b3', 1, 2); mkChip('c1', 2, 0);
const rt = wfRouteEdges([
  { from: 'a1', to: 'b2', on: 'stage-change' }, { from: 'a2', to: 'b3', on: 'stage-change' }, { from: 'a3', to: 'b1', on: 'rare' },
  { from: 'b2', to: 'a1', on: 'reopened' }, { from: 'c1', to: 'a2', on: 'restart' }, { from: 'a1', to: 'c1', on: 'skip' }, { from: 'a1', to: 'a2', on: 'stage-change' }, { from: 'c1', to: 'c1', on: 'loop' },
], geom, {});
assert.strictEqual(rt.common, 'stage-change');
const fwd = rt.edges.filter(e => e.kind === 'fwd');
assert.strictEqual(fwd.length, 3);
const xs = new Set(fwd.map(e => e.d.match(/ Q([\d.]+) /)[1]));
assert.strictEqual(xs.size, 1, 'forward edges between the same lane pair share one vertical trunk x');
const tx = Number([...xs][0]);
assert.ok(tx > 216 && tx < 248, 'trunk runs through the gutter between the lanes');
assert.ok(fwd.every(e => e.d.includes(' Q' + tx + ' ')), 'every bundled edge turns on the shared trunk');
assert.ok(fwd.every(e => !e.common === (e.on === 'rare')), 'only the common event is tagged');
const ys = d => d.split(/[ML Q,]+/).filter(Boolean).map(Number).filter((v, i) => i % 2 === 1);
const back = rt.edges.find(e => e.from === 'b2');
assert.strictEqual(back.back, true);
assert.ok(back.d.startsWith('M256 '), 'back edge leaves from the left port of its source chip');
assert.ok(Math.min.apply(null, ys(back.d)) >= 40, 'adjacent-lane back edge stays in the shared gutter');
assert.ok(back.d.endsWith(' 105'), 'and enters the target’s right port');
const far = rt.edges.find(e => e.from === 'c1' && e.to === 'a2');
assert.ok(Math.min.apply(null, ys(far.d)) < 40, 'lane-skipping back edge routes above the lane headers instead of across the lanes');
const long = rt.edges.find(e => e.kind === 'fwdlong');
const maxY = Math.max.apply(null, long.d.split(/[ML Q,]+/).filter(Boolean).map(Number).filter((v, i) => i % 2 === 1));
assert.ok(maxY > 340, 'lane-skipping edge routes below the lanes');
const same = rt.edges.find(e => e.kind === 'same');
assert.strictEqual(same.back, false);
assert.ok(same.d.startsWith('M208 ') && same.d.endsWith(' 151'), 'same-lane edge leaves and re-enters on the lane\u2019s right margin');
assert.ok(rt.edges.find(e => e.self).d.startsWith('M'), 'self loop drawn');
assert.strictEqual(rt.fans.length, 0, 'no fan below the threshold');
const chans = wfEdgeChannels([{ from: 'b2', to: 'a1' }, { from: 'a1', to: 'c1' }, { from: 'a1', to: 'b1' }], id => geom.chips[id].colIndex);
assert.deepStrictEqual(chans, { top: 0, bottom: 1 }, 'adjacent back edges need no corridor');
assert.deepStrictEqual(wfEdgeChannels([{ from: 'c1', to: 'a1' }, { from: 'c1', to: 'a2' }], id => geom.chips[id].colIndex), { top: 1, bottom: 0 }, 'one corridor row per lane pair');
assert.strictEqual(wfEdgeCorridor(0), 0);
assert.strictEqual(wfEdgeCorridor(2), 24);
// Fan-out: five edges from one state into one lane collapse to a trunk unless expanded.
for (let i = 0; i < 5; i++) mkChip('f' + i, 1, 3 + i);
const fanT = [0, 1, 2, 3, 4].map(i => ({ from: 'a1', to: 'f' + i, on: 'ev' }));
const fanned = wfRouteEdges(fanT, geom, {});
assert.strictEqual(fanned.fans.length, 1);
assert.strictEqual(fanned.fans[0].count, 5);
assert.strictEqual(fanned.fans[0].key, 'a1>1');
assert.ok(fanned.edges.every(e => e.collapsed && e.fan === 'a1>1'), 'fan edges are collapsed');
const opened = wfRouteEdges(fanT, geom, { expanded: { 'a1>1': true } });
assert.ok(opened.fans[0].open && opened.edges.every(e => !e.collapsed), 'expanded fan shows its edges');
assert.strictEqual(wfRouteEdges(fanT.slice(0, 4), geom, {}).fans.length, 0, 'threshold is > fanMax');
assert.strictEqual(JSON.stringify(wfRouteEdges(fanT, geom, {})), JSON.stringify(fanned), 'router is deterministic');
const fanSvg = wfEdSvg(ok, Object.assign({}, quietLay, { edges: fanned.edges, fans: fanned.fans }), { selection: { type: '', ids: [] }, diags: [] });
assert.ok(fanSvg.includes('class="wf-fan" data-wf-fan="a1&gt;1"') || fanSvg.includes('data-wf-fan="a1>1"'), 'fan group rendered');
assert.ok(fanSvg.includes('>+5<'), 'fan badge shows the collapsed count');
assert.strictEqual((fanSvg.match(/wf-fan-edge/g) || []).length, 5, 'collapsed edges live inside the fan group');
assert.strictEqual(wfOrthoPath([[0, 0], [10, 0], [10, 10]], 2), 'M0 0 L8 0 Q10 0 10 2 L10 10');
assert.strictEqual(wfOrthoPath([[0, 0], [5, 0], [10, 0]], 2), 'M0 0 L10 0', 'collinear points pruned');
const mm = wfEdMinimapSvg(wfEdLayout(ok, null, {}), { panX: 0, panY: 0, zoom: 1 }, 800, 600);
assert.ok(mm.includes('wf-ed-mm-view'));

// Compile body + status pill.
const body = wfEdCompileModel({ metadata: { name: 'x' }, spec: { meta: { version: '' } } });
assert.strictEqual(body.spec.meta.version, 'draft');
assert.strictEqual(wfEdStatus([], false, true).cls, 'ok');
assert.strictEqual(wfEdStatus([{ severity: 'warning' }, { severity: 'warning' }], false, true).cls, 'warn');
assert.strictEqual(wfEdStatus([{ severity: 'error' }], false, false).cls, 'error');
assert.strictEqual(wfEdStatus([], true, null).busy, true);
assert.strictEqual(wfEdStatus([], true, null).cls, 'ok');
console.log('ok');
`
	cmd := exec.Command(node, "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("unexpected node output: %s", out)
	}
}

// renderGovWorkflows must never throw with empty state, including the
// editor view with no model loaded (the settings-search index renders every
// governor tab under node with stubbed fetch).
func TestWorkflowsEditorStartScreenRendersEmpty(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`function renderWorkflowEditorStart(owner)`,
		`data-action="wfEdNew"`,
		`data-action="wfEdDuplicateDefault"`,
		`data-action="wfLibShow" data-arg0="ingest"`,
		`coming in phase 7`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing editor start screen %q", want)
		}
	}
}
