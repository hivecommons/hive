package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRepoPillFilterStaticWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"repos-clear-pill-filter-btn",
		"function repoPillFilterMatches(rowKinds, state)",
		"function toggleRepoPillFilter(kind)",
		"function clearRepoPillFilter()",
		"function setRepoPillFilterMode(mode)",
		"repo-pill-row-filtered-hidden",
		"repo-filtered-chip",
		"data-pill-kinds",
		"aria-pressed=\"${active ? 'true' : 'false'}\"",
		"repoPillFilterRowCounts(repoIssueFilterRows.concat(repoPRFilterRows), pillFilter)",
		"e.key === 'Escape' && repoPillFilterActive() && e.target && e.target.closest && e.target.closest('#repos-section')",
		"k.indexOf('hive-repos-pill-filter:')===0",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("repository pill filter missing %q", want)
		}
	}
}

func TestRepoPillFilterORANDAndClear(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: repository pill filter was not executed")
	}
	html := indexHTML(t)
	funcs := []string{
		"repoPillFilterKey",
		"normalizeRepoPillFilter",
		"repoPillFilterState",
		"writeRepoPillFilterState",
		"repoPillFilterActive",
		"repoPillFilterMatches",
		"repoPillFilterAttrs",
		"repoPillKindAttr",
		"repoPillFilterHiddenClass",
		"toggleRepoPillFilter",
		"setRepoPillFilterMode",
		"clearRepoPillFilter",
		"repoPillFilterRowCounts",
	}
	var script strings.Builder
	script.WriteString(`
const assert = require('node:assert/strict');
const store = new Map();
const localStorage = { getItem: k => store.has(k) ? store.get(k) : null, setItem: (k, v) => store.set(k, String(v)), removeItem: k => store.delete(k) };
const window = { _lastStatus: { hiveId: 'filter-test' } };
const REPO_PILL_FILTER_KEY_PREFIX = 'hive-repos-pill-filter:';
let repaints = 0;
function repaintReposForWidth() { repaints++; }
function esc(v) { return String(v == null ? '' : v).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); }
`)
	for _, name := range funcs {
		script.WriteString(jsFunc(t, html, name))
		script.WriteByte('\n')
	}
	script.WriteString(`
const rows = [
  { kinds: ['needs-human', 'pr:needs-human', 'pr:ci'] },
  { kinds: ['pr:ci'] },
  { kinds: ['needs-human', 'issue:waiting', 'issue:needs-human'] },
  { kinds: ['issue:ready'] }
];
let state = writeRepoPillFilterState({ mode: 'any', kinds: ['needs-human'] });
assert.deepEqual(repoPillFilterRowCounts(rows, state), { shown: 2, total: 4 });
assert.equal(repoPillFilterMatches(['pr:ci'], state), false);
assert.equal(repoPillFilterMatches(['needs-human', 'pr:needs-human', 'pr:ci'], state), true);
state = writeRepoPillFilterState({ mode: 'all', kinds: ['pr:needs-human', 'pr:ci'] });
assert.deepEqual(repoPillFilterRowCounts(rows, state), { shown: 1, total: 4 });
assert.equal(repoPillFilterHiddenClass(['pr:ci'], state), ' repo-pill-row-filtered-hidden');
assert.match(repoPillKindAttr(['pr:ci', 'pr:needs-human']), /data-pill-kinds="pr:ci pr:needs-human"/);
toggleRepoPillFilter('issue:ready');
assert.equal(repoPillFilterState().kinds.includes('issue:ready'), true);
setRepoPillFilterMode('any');
assert.equal(repoPillFilterState().mode, 'any');
clearRepoPillFilter();
state = repoPillFilterState();
assert.deepEqual(state.kinds, []);
assert.deepEqual(repoPillFilterRowCounts(rows, state), { shown: 4, total: 4 });
assert.equal(repoPillFilterActive(state), false);
assert.equal(repaints, 3);
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("repository pill filter check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
