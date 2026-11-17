package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestStageTrackerWorkflowDispatchWiring pins the owner's "Send to
// <profile>" on a model-bound row (cncf/techdocs#395): the ⋯ menu offers it
// from the payload's workflows.repos[repo].dispatch block and it posts to
// POST /api/workflows/dispatch.
func TestStageTrackerWorkflowDispatchWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`function stageTrackerWorkflowDispatchOf(item, workflows)`,
		`function stageTrackerWorkflowDispatchAction(item, workflows, ctx)`,
		`stageTrackerWorkflowDispatchAction(item, _stageTracker.workflows, ctx)`,
		`case 'wf-dispatch': return { url: '/api/workflows/dispatch', body: { repo: item.repo, number: n } };`,
		`result.action === 'dispatched') stageTrackerClearPending(pend);`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing workflow-dispatch wiring %q", want)
		}
	}
}

// The pure helpers under node: an Accepted issue of a bound repository
// offers Send to techdocs-drafter, enabled for an owner only; a draft PR in
// Drafting offers the verifier and names its /verify; an unbound
// repository, a state the model keeps for people, a row of the wrong kind
// and a PR drawn under its issue offer nothing.
func TestStageTrackerWorkflowDispatchNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	html := indexHTML(t)
	script := `const assert = require('assert');
` + jsFunc(t, html, "stageTrackerWorkflowOf") + jsFunc(t, html, "stageTrackerWorkflowDispatchOf") + jsFunc(t, html, "stageTrackerWorkflowDispatchAction") + `
const td = { name: 'cncf-techdocs-assessment', version: 'curated-2026.11.2',
  columns: [{ id: 'accepted', label: 'Accepted', states: ['accepted'], numbers: [20] }, { id: 'drafting', label: 'Drafting', states: ['drafting'], numbers: [70] }],
  dispatch: { '20': { profile: 'techdocs-drafter', state: 'accepted', kinds: ['issue'] }, '70': { profile: 'techdocs-verifier', state: 'drafting', kinds: ['pr'], reply: '/verify' } } };
const workflows = { default: { name: 'hive-default', version: '1', columns: [{ id: 'open', label: 'Open', states: ['open'], numbers: [] }] }, repos: { 'cncf/techdocs': td } };

const accepted = { repo: 'cncf/techdocs', kind: 'issue', number: 395, stage: 20 };
assert.deepStrictEqual(stageTrackerWorkflowDispatchOf(accepted, workflows), { profile: 'techdocs-drafter', state: 'accepted', reply: '', agent: '', problem: '' });
const owner = stageTrackerWorkflowDispatchAction(accepted, workflows, { owner: true });
assert.strictEqual(owner.id, 'wf-dispatch');
assert.strictEqual(owner.label, 'Send to techdocs-drafter');
assert.strictEqual(owner.enabled, true);
assert.strictEqual(owner.hint, 'hand off now');
const reader = stageTrackerWorkflowDispatchAction(accepted, workflows, { owner: false });
assert.strictEqual(reader.enabled, false);
assert.strictEqual(reader.why, 'Needs the owner role');

const draft = { repo: 'cncf/techdocs', kind: 'pr', number: 400, stage: 70 };
const v = stageTrackerWorkflowDispatchAction(draft, workflows, { owner: true });
assert.strictEqual(v.label, 'Send to techdocs-verifier');
assert.strictEqual(v.hint, 'also runs on /verify');

assert.strictEqual(stageTrackerWorkflowDispatchOf({ repo: 'cncf/other', kind: 'issue', number: 1, stage: 20 }, workflows), null, 'unbound repo');
assert.strictEqual(stageTrackerWorkflowDispatchOf({ repo: 'cncf/techdocs', kind: 'issue', number: 2, stage: 10 }, workflows), null, 'a state kept for people');
assert.strictEqual(stageTrackerWorkflowDispatchOf({ repo: 'cncf/techdocs', kind: 'pr', number: 3, stage: 20 }, workflows), null, 'wrong kind');
assert.strictEqual(stageTrackerWorkflowDispatchOf(Object.assign({ via_pr: true }, draft), workflows), null, 'PR drawn under its issue');
assert.strictEqual(stageTrackerWorkflowDispatchOf(accepted, null), null, 'no workflows block');
assert.strictEqual(stageTrackerWorkflowDispatchAction(accepted, null, { owner: true }), null);
`
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("node workflow dispatch fixture failed: %v\n%s", err, out)
	}
}
