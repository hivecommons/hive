package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoTileReorderMarkupAndWiring(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"const REPO_ORDER_KEY = 'hive.repoOrder.v1';",
		"function repoOrderApply(repos)",
		"function repoOrderNormalize(repos)",
		"repoOrderRead().forEach(name => { if (valid.has(name) && !seen.has(name)) { seen.add(name); out.push(name); } });",
		"def.forEach(name => { if (!seen.has(name)) { seen.add(name); out.push(name); } });",
		"repoOrderWrite(out, def);",
		"if (_repoResizeDrag || _repoOrderDrag) return;",
		`<button type="button" class="dashboard-grip repo-card-order-handle" draggable="true" data-repo-order-grip data-repo="${esc(cardRepoKey)}" data-stop="1" aria-label="Drag to reorder" title="Drag to reorder" aria-pressed="false">⠿</button>`,
		`<div class="repo-name">${orderHandle}<a href="${esc(repoUrl)}"`,
		"grid.addEventListener('dragstart', (e) => {",
		"grid.addEventListener('dragover', (e) => {",
		"grid.addEventListener('drop', (e) => {",
		"grid.addEventListener('dragend', cleanup);",
		"grid.addEventListener('keydown', (e) => {",
		"indicator.className = 'dashboard-drop-indicator';",
		"card.classList.add('dashboard-drag-source');",
		"repoOrderSaveFromDom(grid);",
		"localStorage.removeItem(REPO_ORDER_KEY);",
		"repoOrderHasCustom(repos)",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
	if strings.Contains(html, "repo-card\" draggable=\"true\"") {
		t.Fatal("repo card is draggable; only the handle should start tile drags")
	}
}

func TestRepoOrderStore(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the repo order store rules were NOT executed by this run")
	}
	html := indexHTML(t)
	var script strings.Builder
	script.WriteString(`
const _store = new Map();
const localStorage = {
  getItem: (k) => (_store.has(k) ? _store.get(k) : null),
  setItem: (k, v) => { _store.set(k, String(v)); },
  removeItem: (k) => { _store.delete(k); },
};
function safeJsonParse(s, fallback) { try { return JSON.parse(s); } catch { return fallback; } }
`)
	script.WriteString(repoCardConstLine(t, html, "REPO_ORDER_KEY") + "\n")
	for _, name := range []string{"repoOrderName", "repoOrderDefault", "repoOrderRead", "repoOrderWrite", "repoOrderNormalize", "repoOrderApply", "repoOrderHasCustom"} {
		script.WriteString(jsFunc(t, html, name) + "\n")
	}
	script.WriteString(`
let fails = 0;
function check(name, cond) { if (!cond) { fails++; console.log('FAIL ' + name); } }
const repos = [{full:'HiveCommons/Hive'}, {full:'KubeStellar/Console'}, {name:'LooseRepo'}];
check('default order', repoOrderNormalize(repos).join('|') === 'hivecommons/hive|kubestellar/console|looserepo');
repoOrderWrite(['kubestellar/console','missing/repo','HIVECOMMONS/HIVE'], repoOrderDefault(repos));
check('stored custom survives and stale drops', repoOrderNormalize(repos).join('|') === 'kubestellar/console|hivecommons/hive|looserepo');
check('custom is detected', repoOrderHasCustom(repos));
check('apply returns repos in stored order', repoOrderApply(repos).map(r => r.full || r.name).join('|') === 'KubeStellar/Console|HiveCommons/Hive|LooseRepo');
repoOrderWrite(repoOrderDefault(repos), repoOrderDefault(repos));
check('default removes storage', !_store.has(REPO_ORDER_KEY));
if (fails) process.exit(1);
`)
	path := filepath.Join(t.TempDir(), "repo-order.js")
	if err := os.WriteFile(path, []byte(script.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path).CombinedOutput(); err != nil {
		t.Fatalf("repo order check failed:\n%s", strings.TrimSpace(string(out)))
	}
}
