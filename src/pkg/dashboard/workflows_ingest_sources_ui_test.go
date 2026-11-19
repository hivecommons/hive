package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkflowsIngestSourcesWiring pins the RFC phase-6 surface
// (hivecommons/hive#17620): the Ingest view appends the cluster and
// written-spec panels through one hook, the panels post to the two new
// routes, read capabilities from the one that gates them, and the
// stylesheet rides on the shared tokens.
func TestWorkflowsIngestSourcesWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`(typeof renderWorkflowIngestSources === 'function' ? renderWorkflowIngestSources(state, owner) : '')`,
		`function renderWorkflowIngestSources(state, owner)`,
		`function renderWorkflowIngestClusterPanel(caps, owner)`,
		`function renderWorkflowIngestSpecPanel(caps, owner)`,
		`fetch('/api/workflows/ingest/capabilities', {`,
		`fetch('/api/workflows/ingest/' + which, {`,
		`.wf-src-panel {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing ingest-sources wiring %q", want)
		}
	}
	// The hook is the only touch on the sibling-owned Ingest renderer.
	lib, err := os.ReadFile(filepath.Join("static-src", "js", "config-workflows-library.js"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(lib), "renderWorkflowIngestSources"); n != 2 {
		t.Errorf("config-workflows-library.js references renderWorkflowIngestSources %d times, want the one guarded hook", n)
	}
}

// The renderers under node: capability gating (cluster disabled with its
// reason, the AI checkbox only when a gateway is configured), the three spec
// input modes, request-body validation, the result block with the observed
// runs / LLM note / section list, and the state setters.
func TestWorkflowsIngestSourcesNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	html := indexHTML(t)
	script := `const assert = require('assert');
function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;'); }
function renderWorkflowDiagnostics(d) { return '<ul class="wf-diags">' + (d || []).length + '</ul>'; }
function workflowDiagramSvg() { return '<svg class="wf-diagram"></svg>'; }
let rerenders = 0; function wfLibRerender() { rerenders++; }
const WF_INGEST_SPEC_MODES = [['text', 'Paste the spec'], ['url', 'Fetch from a URL'], ['repo', 'Read a repository file']];
const WF_INGEST_SRC_CAPS_UNKNOWN = { cluster: false, clusterReason: '', contexts: [], spec: { llm: false, model: '' } };
const _wfIngestSrc = {
  caps: null, capsRequested: true, capsError: '',
  cluster: { open: false, context: '', namespace: '', allNamespaces: false, selector: '', name: '', busy: false, result: null, error: '' },
  spec: { open: false, mode: 'text', text: '', url: '', repo: '', path: '', useLLM: true, name: '', busy: false, result: null, error: '' },
};
` + jsFunc(t, html, "wfIngestSrcCaps") + jsFunc(t, html, "renderWorkflowIngestSourceResult") + jsFunc(t, html, "renderWorkflowIngestClusterRuns") +
		jsFunc(t, html, "renderWorkflowIngestSpecNotes") + jsFunc(t, html, "renderWorkflowIngestClusterPanel") + jsFunc(t, html, "renderWorkflowIngestSpecPanel") +
		jsFunc(t, html, "renderWorkflowIngestSources") + jsFunc(t, html, "wfIngestSrcToggle") + jsFunc(t, html, "wfIngestSrcSet") + jsFunc(t, html, "wfIngestSrcRequest") + `
// Unknown capabilities: cluster panel disabled with the install hint, no AI checkbox.
let out = renderWorkflowIngestSources(null, true);
assert.ok(out.includes('data-section="workflows-ingest-sources"'));
assert.ok(out.includes('data-section="workflows-ingest-cluster"') && out.includes('data-section="workflows-ingest-spec"'));
assert.ok(out.includes('No kube access on this spoke'));
assert.ok(out.includes('id="wf-src-cluster-namespace"') && /id="wf-src-cluster-namespace"[^>]*disabled/.test(out));
assert.ok(!out.includes('id="wf-src-spec-llm"'));
assert.ok(out.includes('No model gateway is configured'));
assert.ok(out.includes('<textarea id="wf-src-spec-text"'));
assert.ok(!out.includes('<details class="wf-src-panel" data-section="workflows-ingest-cluster" open'));

// Reader: the buttons are disabled.
out = renderWorkflowIngestSources(null, false);
assert.ok(out.includes('Owner role is required to ingest.'));
assert.ok(/data-action="wfIngestSrcRun" data-arg0="spec" data-arg1="false" data-arg-types="s,j" disabled/.test(out));

// Full capabilities: context select from the listed contexts, AI checkbox naming the model.
_wfIngestSrc.caps = { cluster: true, contexts: ['kind-hive', 'prod'], spec: { llm: true, model: 'gw/reviewer' } };
_wfIngestSrc.cluster.context = 'prod';
out = renderWorkflowIngestSources(null, true);
assert.ok(out.includes('<select id="wf-src-cluster-context"'));
assert.ok(out.includes('<option value="prod" selected>prod</option>'));
assert.ok(out.includes('id="wf-src-spec-llm" checked'));
assert.ok(out.includes('gw/reviewer'));
assert.ok(!out.includes('No kube access'));
assert.ok(!/id="wf-src-cluster-namespace"[^>]*disabled/.test(out));

// Cluster without a context list falls back to a text input.
_wfIngestSrc.caps = { cluster: true, clusterReason: 'kubectl config get-contexts failed', contexts: [], spec: { llm: false } };
out = renderWorkflowIngestClusterPanel(wfIngestSrcCaps(), true);
assert.ok(out.includes('<input type="text" id="wf-src-cluster-context" value="prod"'));

// Spec input modes.
_wfIngestSrc.spec.mode = 'url'; _wfIngestSrc.spec.url = 'https://x.example/p.md';
out = renderWorkflowIngestSpecPanel(wfIngestSrcCaps(), true);
assert.ok(out.includes('id="wf-src-spec-url" value="https://x.example/p.md"') && !out.includes('wf-src-spec-text'));
_wfIngestSrc.spec.mode = 'repo'; _wfIngestSrc.spec.repo = 'cncf/techdocs'; _wfIngestSrc.spec.path = 'docs/p.md';
out = renderWorkflowIngestSpecPanel(wfIngestSrcCaps(), true);
assert.ok(out.includes('id="wf-src-spec-repo" value="cncf/techdocs"') && out.includes('id="wf-src-spec-path" value="docs/p.md"'));
assert.ok(out.includes('<option value="repo" selected>'));

// Setters: strings, booleans, guarded fields; mode change resets the result and rerenders.
wfIngestSrcSet('cluster', 'namespace', ' ci ');
assert.strictEqual(_wfIngestSrc.cluster.namespace, ' ci ');
wfIngestSrcSet('cluster', 'allNamespaces', true);
assert.strictEqual(_wfIngestSrc.cluster.allNamespaces, true);
wfIngestSrcSet('cluster', 'busy', true);
assert.strictEqual(_wfIngestSrc.cluster.busy, false);
wfIngestSrcSet('nope', 'x', 1);
_wfIngestSrc.spec.result = { compiled: true };
wfIngestSrcSet('spec', 'mode', 'text');
assert.strictEqual(_wfIngestSrc.spec.result, null);
assert.strictEqual(rerenders, 1);
wfIngestSrcToggle('spec', true);
assert.strictEqual(_wfIngestSrc.spec.open, true);
assert.ok(renderWorkflowIngestSpecPanel(wfIngestSrcCaps(), true).includes('data-section="workflows-ingest-spec" open'));

// Request bodies and their validation.
const caps = { cluster: true, contexts: [], spec: { llm: true, model: 'm' } };
let c = { context: ' prod ', namespace: '', allNamespaces: false, selector: '', name: '' };
assert.ok(wfIngestSrcRequest('cluster', c, false, caps).error.includes('namespace'));
c.namespace = 'ci'; c.selector = 'a=b'; c.name = 'ci-model';
assert.deepStrictEqual(wfIngestSrcRequest('cluster', c, true, caps).body, { save: true, context: 'prod', namespace: 'ci', selector: 'a=b', name: 'ci-model' });
c.allNamespaces = true;
assert.deepStrictEqual(wfIngestSrcRequest('cluster', c, false, caps).body, { save: false, context: 'prod', allNamespaces: true, selector: 'a=b', name: 'ci-model' });
let s = { mode: 'text', text: '  ', url: '', repo: '', path: '', useLLM: true, name: '' };
assert.ok(wfIngestSrcRequest('spec', s, false, caps).error.includes('Paste'));
s.text = '# P\n1. a';
assert.deepStrictEqual(wfIngestSrcRequest('spec', s, false, caps).body, { save: false, useLLM: true, text: '# P\n1. a' });
// The AI flag is never sent when no gateway is configured, whatever the checkbox remembers.
assert.strictEqual(wfIngestSrcRequest('spec', s, false, WF_INGEST_SRC_CAPS_UNKNOWN).body.useLLM, false);
s.useLLM = false;
assert.strictEqual(wfIngestSrcRequest('spec', s, false, caps).body.useLLM, false);
s.mode = 'url'; s.url = 'http://x/p.md';
assert.ok(wfIngestSrcRequest('spec', s, false, caps).error.includes('https'));
s.url = 'https://x/p.md'; s.name = 'n';
assert.deepStrictEqual(wfIngestSrcRequest('spec', s, true, caps).body, { save: true, useLLM: false, url: 'https://x/p.md', name: 'n' });
s.mode = 'repo'; s.repo = 'bad';
assert.ok(wfIngestSrcRequest('spec', s, false, caps).error.includes('owner/repo'));
s.repo = 'o/r';
assert.ok(wfIngestSrcRequest('spec', s, false, caps).error.includes('path'));
s.path = 'd/p.md';
assert.deepStrictEqual(wfIngestSrcRequest('spec', s, false, caps).body, { save: false, useLLM: false, repo: 'o/r', path: 'd/p.md', name: 'n' });

// Results: shared summary plus the source extras.
const cl = { compiled: true, source: 'cluster:prod/ci', files: 3, pipelines: [{ system: 'tekton', name: 'build', path: 'cluster/ci/pipeline/build.yaml' }],
  runs: [{ pipeline: 'build', namespace: 'ci', run: 'build-x1', status: 'Succeeded' }, { pipeline: 'deploy', namespace: 'ci', run: 'deploy-y', status: 'Failed' }],
  namespaces: ['ci', 'ops'], diagnostics: [{ code: 'x' }], model: {}, profile: { columns: [1, 2, 3] }, entry: { name: 'ci', version: '1' } };
_wfIngestSrc.cluster.result = cl; _wfIngestSrc.cluster.error = '';
out = renderWorkflowIngestClusterPanel(caps, true);
assert.ok(out.includes('data-wf-ingest-result="1"'));
assert.ok(out.includes('wf-compiled-ok') && out.includes('cluster:prod/ci') && out.includes('3 files') && out.includes('1 pipeline ') && out.includes('3 columns'));
assert.ok(out.includes('saved as') && out.includes('data-action="wfLibOpen" data-arg0="ci" data-arg1="1"'));
assert.ok(out.includes('Observed runs') && out.includes('wf-src-run-succeeded">Succeeded') && out.includes('wf-src-run-failed">Failed'));
assert.ok(out.includes('Namespaces:') && out.includes('<span class="mono">ops</span>'));
assert.ok(out.includes('cluster/ci/pipeline/build.yaml'));
assert.ok(out.includes('<ul class="wf-diags">1</ul>') && out.includes('wf-diagram'));
assert.ok(renderWorkflowIngestClusterRuns([]).includes('No PipelineRuns observed'));
_wfIngestSrc.cluster.error = 'Ingest failed: boom';
assert.ok(renderWorkflowIngestClusterPanel(caps, true).includes('<div class="wf-ingest-error">Ingest failed: boom</div>'));

const sp = { compiled: false, source: 'spec:pasted', diagnostics: [], model: {}, profile: { columns: [1] },
  llm: { used: true, model: 'gw/reviewer', attempts: 2, accepted: true }, sections: [{ heading: 'Release', level: 1, startLine: 1, endLine: 9 }, { heading: 'Review <b>', level: 2, startLine: 10, endLine: 12 }] };
_wfIngestSrc.spec.result = sp;
out = renderWorkflowIngestSpecPanel(caps, true);
assert.ok(out.includes('wf-compiled-no'));
assert.ok(!out.includes(' files') && !out.includes(' pipeline'));
assert.ok(out.includes('wf-src-llm-on">LLM-assisted') && out.includes('gw/reviewer') && out.includes('2 attempts'));
assert.ok(out.includes('Spec sections') && out.includes('L1–L9</span> Release') && out.includes('Review &lt;b'));
assert.ok(renderWorkflowIngestSpecNotes({ llm: { used: false } }).includes('wf-src-llm-off">Deterministic only'));
const fb = renderWorkflowIngestSpecNotes({ llm: { used: true, model: 'm', attempts: 2, accepted: false, error: 'did not compile' } });
assert.ok(fb.includes('wf-src-llm-fallback">LLM fell back') && fb.includes('did not compile') && !fb.includes('Spec sections'));
console.log('ok');
`
	cmd := exec.Command(node, "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
