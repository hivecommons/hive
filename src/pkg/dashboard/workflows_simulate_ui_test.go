package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestWorkflowsSimulateWiring pins the RFC phase-3c surface
// (hivecommons/hive#17620): the detail view grows a Simulate panel whose
// form is generated from WF_SIM_FIELDS, posts to the simulate endpoint,
// and marks the resulting state in the diagram.
func TestWorkflowsSimulateWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`const WF_SIM_FIELDS = [`,
		`function renderWorkflowSimulate(state, owner)`,
		`function renderWorkflowSimulateResult(result)`,
		`function wfSimFactsFromForm(facts, kind, read)`,
		`function wfSimFormFromFacts(facts, field)`,
		`function wfSimMarkDiagram(svg, result)`,
		`function wfSimSet(field, value)`,
		`async function wfSimFetch()`,
		`async function wfSimRun()`,
		`+ '/simulate',`,
		`renderWorkflowSimulate(sim, owner)`,
		`data-action="wfSimRun"`,
		`data-action="wfSimFetch"`,
		`.wf-dg-state.simulated .wf-dg-chip {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing Simulate wiring %q", want)
		}
	}
}

func jsConstUntil(t *testing.T, html, name, end string) string {
	t.Helper()
	start := strings.Index(html, "const "+name+" = ")
	if start < 0 {
		t.Fatalf("index.html does not define %s", name)
	}
	stop := strings.Index(html[start:], end)
	if stop < 0 {
		t.Fatalf("%s has no terminator %q", name, end)
	}
	return html[start:start+stop+len(end)] + "\n"
}

// Pure renderers under node: the form renders only the fields for the
// chosen kind and round-trips facts; the result renders the decision
// chip, highlights the matched rule and shows each predicate's verdict;
// abstentions read "Not covered"; the diagram marks the simulated state.
func TestWorkflowsSimulateNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	html := indexHTML(t)
	script := `const assert = require('assert');
function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;'); }
` + jsConstUntil(t, html, "WF_SIM_KINDS", "];") + jsConstUntil(t, html, "WF_SIM_FIELDS", "\n    ];") +
		jsFunc(t, html, "wfSimDefaultFacts") + jsFunc(t, html, "wfSimGet") + jsFunc(t, html, "wfSimSetPath") +
		jsFunc(t, html, "wfSimSplitList") + jsFunc(t, html, "wfSimFieldsFor") + jsFunc(t, html, "wfSimFormFromFacts") +
		jsFunc(t, html, "wfSimFactsFromForm") + jsFunc(t, html, "wfSimParseItem") + jsFunc(t, html, "wfSimFieldHtml") +
		jsFunc(t, html, "renderWorkflowSimulate") + jsFunc(t, html, "wfSimVerdict") + jsFunc(t, html, "renderWorkflowSimulateResult") +
		jsFunc(t, html, "wfSimMarkDiagram") + `
// Item parsing accepts owner/repo#N and forge URLs.
assert.deepStrictEqual(wfSimParseItem('cncf/techdocs#12'), { repo: 'cncf/techdocs', number: 12 });
assert.deepStrictEqual(wfSimParseItem('https://github.com/cncf/techdocs/pull/7'), { repo: 'cncf/techdocs', number: 7 });
assert.strictEqual(wfSimParseItem('techdocs'), null);

// The form is kind-aware.
const prFields = wfSimFieldsFor('pr').map(f => f.path), issueFields = wfSimFieldsFor('issue').map(f => f.path);
assert.ok(prFields.includes('pr.checks') && !prFields.includes('issue.bucket'));
assert.ok(issueFields.includes('issue.bucket') && !issueFields.includes('pr.checks') && !issueFields.includes('draft'));

const facts = { labels: ['role/trusted', 'bug'], open: true, draft: false, author: { login: 'andy', kind: 'human', role: 'trusted', selfAuthored: false },
  park: { holds: null, needsHuman: true }, issue: { bucket: '' }, pr: { checks: 'green', review: 'approved', dco: 'yes', conflicting: false } };
const state = { kind: 'pr', repo: 'cncf/techdocs#12', facts: facts, result: null, error: '', busy: false, open: false };
const form = renderWorkflowSimulate(state, true);
assert.ok(form.includes('data-section="workflows-simulate"') && !/<details[^>]*\sopen/.test(form), 'closed until used');
assert.ok(form.includes('data-sim-field="pr.checks"') && !form.includes('data-sim-field="issue.bucket"'), 'PR form has no issue fields');
assert.ok(/<option value="green" selected>/.test(form), 'select reflects facts');
assert.ok(/data-sim-field="park.needsHuman"[^>]*checked/.test(form), 'checkbox reflects facts');
assert.ok(/data-sim-field="labels"[^>]*value="role\/trusted bug"/.test(form), 'labels joined');
assert.ok(form.includes('value="pr" checked'), 'kind radio');
assert.ok(form.includes('data-action="wfSimRun"') && form.includes('data-action="wfSimFetch"'));
assert.ok(renderWorkflowSimulate(Object.assign({}, state, { busy: true }), true).includes('Working…'));
assert.ok(/<details[^>]*\sopen/.test(renderWorkflowSimulate(Object.assign({}, state, { error: 'nope' }), true)), 'errors open the panel');

// Form values round-trip through wfSimFactsFromForm.
const back = wfSimFactsFromForm(wfSimDefaultFacts(), 'pr', f => wfSimFormFromFacts(facts, f));
assert.deepStrictEqual(back.labels, ['role/trusted', 'bug']);
assert.strictEqual(back.pr.checks, 'green');
assert.strictEqual(back.pr.review, 'approved');
assert.strictEqual(back.author.role, 'trusted');
assert.strictEqual(back.park.needsHuman, true);
assert.strictEqual(back.park.holds, null, 'empty holds stay derived');
assert.strictEqual(back.open, true);
const one = wfSimFactsFromForm(facts, 'pr', f => f.path === 'labels' ? 'a, b  c' : undefined);
assert.deepStrictEqual(one.labels, ['a', 'b', 'c']);
assert.strictEqual(one.pr.checks, 'green', 'untouched fields keep their value');
assert.deepStrictEqual(facts.labels, ['role/trusted', 'bug'], 'input is not mutated');

// Result: matched rule highlighted, predicates with verdicts, chip text.
const result = { decision: { state: 'accepted', column: 'Accepted', stage: 20, label: 'stage/020-accepted', reason: 'rule accepted', abstain: false, covered: true },
  trace: [
    { id: 'requested', state: 'requested', evaluated: true, matched: false, predicates: [{ fact: 'label', op: 'has', value: 'techdocs/request', holds: true, known: true }, { fact: 'label', op: 'not', value: 'techdocs/accepted', holds: false, known: true, actual: 'techdocs/accepted' }] },
    { id: 'accepted', state: 'accepted', evaluated: true, matched: true, predicates: [{ fact: 'label', op: 'has', value: 'techdocs/accepted', holds: true, known: true }] },
    { id: 'pr-only', state: 'review', kind: 'pr', evaluated: false, matched: false, skipped: 'kind pr' },
    { id: 'later', state: 'done', evaluated: false, matched: false },
  ], filled: ['labels', 'open'], unknown: ['pr.review'], notes: [] };
const out = renderWorkflowSimulateResult(result);
assert.ok(out.includes('data-sim-state="accepted"'));
assert.ok(out.includes('wf-sim-chip-state mono">accepted<') && out.includes('stage/020') && out.includes('Accepted'), 'decision chip');
assert.strictEqual((out.match(/wf-sim-rule matched/g) || []).length, 1, 'exactly one matched rule');
assert.ok(/data-rule="accepted"/.test(out));
assert.ok(out.includes('wf-sim-rule missed') && out.includes('wf-sim-rule skipped') && out.includes('wf-sim-rule unevaluated'));
assert.ok(out.includes('label has techdocs/request</span> → <span class="wf-sim-ok">✓'), 'holding predicate');
assert.ok(out.includes('label not techdocs/accepted</span> → <span class="wf-sim-fail">✗') && out.includes('(techdocs/accepted)'), 'failing predicate with actual');
assert.ok(out.includes('not evaluated') && out.includes('skipped: kind pr'));
assert.ok(out.includes('fetched:</span> labels, open') && out.includes('unknown, defaulted:</span> pr.review'));

const unknownPred = renderWorkflowSimulateResult({ decision: { state: 'x', covered: true }, trace: [{ id: 'r', state: 'x', evaluated: true, matched: true, predicates: [{ fact: 'pr.checks', op: 'eq', value: 'green', holds: false, known: false }] }] });
assert.ok(unknownPred.includes('wf-sim-unknown'), 'unknown predicate shows ?');

const abstain = renderWorkflowSimulateResult({ decision: { abstain: true, covered: false, reason: 'workflow: not covered: no rule matched' }, trace: [] });
assert.ok(abstain.includes('Not covered') && abstain.includes('no rule matched') && !abstain.includes('wf-sim-trace'));
const builtin = renderWorkflowSimulateResult({ decision: { state: 'awaiting-merge-admission', column: 'Merge', stage: 80, covered: true, reason: 'lane' },
  trace: [{ builtin: true, state: 'awaiting-merge-admission', evaluated: true, matched: true, reason: 'lane' }], notes: ['lane synthesized'] });
assert.ok(builtin.includes('built-in deriver') && builtin.includes('lane synthesized'));
const none = renderWorkflowSimulateResult({ decision: { state: '', covered: true, reason: 'draft' }, trace: [] });
assert.ok(none.includes('No stage') && none.includes('draft'));

// Diagram marking.
const svg = '<g class="wf-dg-state initial" data-state="open"><rect class="wf-dg-chip"/></g><g class="wf-dg-state" data-state="accepted"><rect class="wf-dg-chip"/></g>';
const marked = wfSimMarkDiagram(svg, result);
assert.ok(marked.includes('class="wf-dg-state simulated" data-state="accepted"'), marked);
assert.ok(marked.includes('class="wf-dg-state initial" data-state="open"'), 'other states untouched');
assert.strictEqual(wfSimMarkDiagram(svg, abstain), svg);
assert.strictEqual(wfSimMarkDiagram(svg, null), svg);
console.log('ok');
`
	cmd := exec.Command(node, "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
