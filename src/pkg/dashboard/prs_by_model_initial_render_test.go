package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func asyncJSFunc(t *testing.T, html, name string) string {
	t.Helper()
	start := strings.Index(html, "async function "+name+"(")
	if start < 0 {
		t.Fatalf("index.html does not define async %s()", name)
	}
	depth := 0
	for i := strings.Index(html[start:], "{") + start; i < len(html); i++ {
		switch html[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return html[start : i+1]
			}
		}
	}
	t.Fatalf("unbalanced braces extracting async %s() from index.html", name)
	return ""
}

func TestGovernorPRModelsInitialRenderUsesCurrentWindow(t *testing.T) {
	html := indexHTML(t)
	body := jsFunc(t, html, "renderGovernor")
	if !strings.Contains(body, "loadGovernorPRModels(false, govPRModelsWindow);") {
		t.Fatal("renderGovernor must kick the PRs-by-model loader with the current/default window on initial paint")
	}
}

func TestGovernorPRModelsLoaderRerendersInitialStatusPayload(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the PRs-by-model initial-render loader rule was NOT executed")
	}
	html := indexHTML(t)
	var script strings.Builder
	script.WriteString(`
const assert = require('node:assert/strict');
let govPRModelsWindow = '7d';
let govPRModelsLoading = false;
let govPRModelsData = null;
const fetchURLs = [];
const renders = [];
const window = { _lastStatus: { cadenceMatrix: ['cadence-row'], hiveId: 'hive-without-governor-field' } };
async function fetch(url) {
  fetchURLs.push(url);
  return { ok: true, json: async () => ({ total: 1, buckets: [{ model: 'claude', backend: 'copilot', prs: 1 }] }) };
}
function renderGovernor(gov, cadenceMatrix, data) {
  renders.push({ gov, cadenceMatrix, data, loaded: govPRModelsData });
}
`)
	for _, name := range []string{"normalizeGovernorPRModelsWindowName", "renderGovernorFromLastStatus", "setGovernorPRModelsWindow"} {
		script.WriteString(jsFunc(t, html, name) + "\n")
	}
	script.WriteString(asyncJSFunc(t, html, "loadGovernorPRModels") + "\n")
	script.WriteString(`
(async () => {
  await loadGovernorPRModels(false, govPRModelsWindow);
  assert.deepEqual(fetchURLs, ['/api/governor/pr-models?window=7d']);
  assert.equal(govPRModelsData.window, '7d');
  assert.equal(renders.length, 1, 'initial load completion must repaint the panel');
  assert.deepEqual(renders[0].gov, {}, 'status payloads without a governor object still repaint the rendered governor tile');
  assert.deepEqual(renders[0].cadenceMatrix, ['cadence-row']);

  await setGovernorPRModelsWindow('30d');
  assert.equal(govPRModelsWindow, '30d');
  assert.equal(fetchURLs[1], '/api/governor/pr-models?window=30d');
  assert.equal(govPRModelsData.window, '30d');
  assert.equal(renders.length, 2, 'filter-change path and initial path share the same loader/rerender path');
})().catch(err => { console.error(err && err.stack || err); process.exit(1); });
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("PRs-by-model initial-render loader check failed:\n%s", strings.TrimSpace(string(out)))
	}
}
