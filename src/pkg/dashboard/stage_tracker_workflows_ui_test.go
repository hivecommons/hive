package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestStageTrackerWorkflowColumnsWiring pins the RFC phase-2 surface
// (hivecommons/hive#17620): the tracker reads `workflows` from the payload,
// a bound repository's group header carries the model badge and its own
// column header, and the row strip switches to the model's columns only
// when stageTrackerWorkflowOf resolves one.
func TestStageTrackerWorkflowColumnsWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`ctx.workflows = d.workflows || null;`,
		`function stageTrackerWorkflowOf(ctx, repo)`,
		`function stageTrackerWorkflowColumnIndex(wf, stage)`,
		`function stageTrackerWorkflowStripItems(item, wf, stages)`,
		`function stageTrackerWorkflowHeaderHtml(wf)`,
		`const rowWorkflow = typeof stageTrackerWorkflowOf === 'function' ? stageTrackerWorkflowOf(ctx, item.repo) : null;`,
		`${rowWorkflow ? stageTrackerWorkflowStripItems(shown, rowWorkflow, stages) : stageTrackerStripItems(`,
		`const groupWorkflow = typeof stageTrackerWorkflowOf === 'function' ? stageTrackerWorkflowOf(ctx, g.repo) : null;`,
		`${groupWorkflow && !g.collapsed ? stageTrackerWorkflowHeaderHtml(groupWorkflow) : ''}`,
		`.ham-wf-badge {`,
		`.ham-seg.ham-wf-uncovered {`,
		// Settings → Repos binding select.
		`function renderRepoWorkflowRow(repo)`,
		`async function setRepoWorkflow(repo, value)`,
		`fetch('/api/workflows/bindings', {`,
		`+ ldSub + renderRepoWorkflowRow(r);`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing workflow-columns wiring %q", want)
		}
	}
}

// The pure helpers under node: an unbound hive resolves no model (so the
// built-in strip is untouched), hive-default itself resolves none, a bound
// repository places rows by stage number into the model's columns, a
// terminal stage lights the terminal column, and an uncovered number is
// drawn as "Not covered" rather than dropped.
func TestStageTrackerWorkflowColumnsNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	html := indexHTML(t)
	script := `const assert = require('assert');
const STAGE_TRACKER_TERMINAL = 110;
function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;'); }
` + jsFunc(t, html, "stageTrackerWorkflowOf") + jsFunc(t, html, "stageTrackerWorkflowColumnIndex") +
		jsFunc(t, html, "stageTrackerWorkflowBadgeHtml") + jsFunc(t, html, "stageTrackerWorkflowHeaderHtml") +
		jsFunc(t, html, "stageTrackerWorkflowStripItems") + `
const STAGE_TRACKER_WORKFLOW_UNCOVERED_WORD = 'Not covered';
const def = { name: 'hive-default', version: '1', columns: [{ id: 'open', label: 'Open', states: ['open'], numbers: [] }] };
const td = { name: 'cncf-techdocs', version: '1', columns: [
  { id: 'requested', label: 'Requested', hint: 'Asked for', states: ['requested'], numbers: [10] },
  { id: 'analysis', label: 'Analysis', states: ['analysing', 'analysis-review'], numbers: [20, 45] },
  { id: 'writing', label: 'Writing', states: ['drafting'], numbers: [40, 50, 70] },
  { id: 'done', label: 'Published', terminal: true, states: ['published'], numbers: [] },
]};
const ctx = { workflows: { default: def, repos: { 'cncf/techdocs': td, 'cncf/plain': def, 'cncf/broken': { name: 'x', version: '1', error: 'not found' } } } };
assert.strictEqual(stageTrackerWorkflowOf({}, 'cncf/techdocs'), null, 'no payload block → built-in strip');
assert.strictEqual(stageTrackerWorkflowOf({ workflows: null }, 'cncf/techdocs'), null);
assert.strictEqual(stageTrackerWorkflowOf(ctx, 'cncf/other'), null, 'unbound repo');
assert.strictEqual(stageTrackerWorkflowOf(ctx, 'cncf/plain'), null, 'bound to hive-default is the built-in strip');
assert.strictEqual(stageTrackerWorkflowOf(ctx, 'cncf/broken'), null, 'unusable binding falls back');
assert.strictEqual(stageTrackerWorkflowOf(ctx, 'cncf/techdocs'), td);

assert.strictEqual(stageTrackerWorkflowColumnIndex(td, 0), 0);
assert.strictEqual(stageTrackerWorkflowColumnIndex(td, 45), 1);
assert.strictEqual(stageTrackerWorkflowColumnIndex(td, 70), 2);
assert.strictEqual(stageTrackerWorkflowColumnIndex(td, 110), 3, 'terminal → terminal column');
assert.strictEqual(stageTrackerWorkflowColumnIndex(td, 85), -1, 'uncovered');

const stages = [{ number: 45, label: 'Needs decision', short: 'Decision' }];
const li = html => html.match(/<li class="([^"]+)"/g).map(m => m.slice(11, -1));
const words = html => [...html.matchAll(/<span class="ham-seg-t">([^<]*)<\/span>/g)].map(m => m[1]);
let s = stageTrackerWorkflowStripItems({ stage: 45 }, td, stages);
assert.deepStrictEqual(li(s), ['ham-seg ham-wf-seg done', 'ham-seg ham-wf-seg active', 'ham-seg ham-wf-seg todo', 'ham-seg ham-wf-seg todo cap']);
assert.deepStrictEqual(words(s), ['Requested', 'Decision', 'Writing', 'Published'], 'active column wears the stage word');
s = stageTrackerWorkflowStripItems({ stage: 110 }, td, stages);
assert.deepStrictEqual(li(s), ['ham-seg ham-wf-seg done', 'ham-seg ham-wf-seg done', 'ham-seg ham-wf-seg done', 'ham-seg ham-wf-seg active cap']);
s = stageTrackerWorkflowStripItems({ stage: 85 }, td, stages);
assert.ok(li(s)[0].includes('ham-wf-uncovered') && words(s)[0] === 'Not covered', s);
assert.ok(s.includes('stage/085 is not a state of cncf-techdocs@1'));
s = stageTrackerWorkflowStripItems({ stage: 0 }, td, stages);
assert.deepStrictEqual(li(s)[0], 'ham-seg ham-wf-seg active');

const head = stageTrackerWorkflowHeaderHtml(td);
assert.ok(head.includes('data-ham-workflow="cncf-techdocs@1"') && head.includes('title="Asked for"') && li(head).length === 4, head);
assert.ok(stageTrackerWorkflowBadgeHtml(td).includes('>cncf-techdocs@1<'));
assert.strictEqual(stageTrackerWorkflowBadgeHtml(null), '');
`
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("node workflow columns fixture failed: %v\n%s", err, out)
	}
}
