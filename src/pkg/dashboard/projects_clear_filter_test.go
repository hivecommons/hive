package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestProjectsOverviewClearFilterBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Projects clear-filter behavior was not executed")
	}
	html := indexHTML(t)
	if !strings.Contains(html, `data-action="clearOverviewProjectFilter"`) {
		t.Fatal("overview filter chip is missing the clearOverviewProjectFilter action")
	}
	renderRepos := jsFunctionBody(t, html, "function renderRepos(repos)")
	for _, want := range []string{
		"clearFilterBtn.hidden = !(pillFilterIsActive || overviewFilter);",
		"clearFilterBtn.dataset.action = overviewFilter ? 'clearProjectsFilter' : 'clearRepoPillFilter';",
	} {
		if !strings.Contains(renderRepos, want) {
			t.Fatalf("Projects toolbar clear button is not wired for overview filters: missing %q", want)
		}
	}

	script := `
const assert = require('node:assert/strict');
const OVERVIEW_PROJECT_FILTERS = {
  'open-issues': { label: 'Total open issues', kinds: ['issue'] }
};
let _overviewProjectFilterKey = null;
let _overviewProjectFilterCount = null;
const window = { location: {} };
const location = window.location;
function setURL(raw) {
  const u = new URL(raw);
  window.location.href = u.href;
  window.location.pathname = u.pathname;
  window.location.search = u.search;
  window.location.hash = u.hash;
}
const history = {
  replaceState: function(_, __, raw) {
    const u = new URL(String(raw), window.location.href);
    setURL(u.href);
  }
};
function repaintReposForWidth() { repaintReposForWidth.calls = (repaintReposForWidth.calls || 0) + 1; }
function ocNavigate(section) { ocNavigate.section = section; }
let repoClearState = null;
function repoPillFilterState() { return { mode: 'all', kinds: ['waiting'], repo: 'hive' }; }
function writeRepoPillFilterState(state) { repoClearState = state; }
` + extractJSFunc(t, html, "function overviewProjectFilterSpec(key)") + "\n" +
		extractJSFunc(t, html, "function overviewProjectFilterKeyFromURL()") + "\n" +
		extractJSFunc(t, html, "function overviewProjectFilterState()") + "\n" +
		extractJSFunc(t, html, "function overviewSetProjectFilterHash(key)") + "\n" +
		extractJSFunc(t, html, "function overviewApplyKPIProjectFilter(key, count)") + "\n" +
		extractJSFunc(t, html, "function clearOverviewProjectFilter()") + "\n" +
		extractJSFunc(t, html, "function clearProjectsFilter()") + `
setURL('https://dash.example.test/dashboard?style=spoke&band=open-issues');
assert.equal(overviewProjectFilterState().key, 'open-issues', 'URL band should seed the overview filter');
clearOverviewProjectFilter();
assert.equal(overviewProjectFilterState(), null, 'clearOverviewProjectFilter must not re-read the stale URL band');
assert.equal(location.search, '?style=spoke', 'clearing keeps unrelated query params but removes band/kpi');
assert.equal(location.hash, '#section-repos-section');

_overviewProjectFilterKey = null;
_overviewProjectFilterCount = null;
setURL('https://dash.example.test/dashboard?style=spoke&band=held');
overviewApplyKPIProjectFilter('open-issues', 20);
assert.equal(overviewProjectFilterState().key, 'open-issues');
assert.equal(overviewProjectFilterState().selectedCount, 20);
assert.equal(location.search, '?style=spoke', 'applying a KPI filter removes stale query filter params');
assert.equal(location.hash, '#projects?band=open-issues');
assert.equal(ocNavigate.section, 'repos-section');

clearProjectsFilter();
assert.deepEqual(repoClearState, { mode: 'all', kinds: [], repo: '' });
assert.equal(overviewProjectFilterState(), null, 'toolbar Clear filter clears the overview filter too');
assert.ok(repaintReposForWidth.calls >= 3, 'clearing/applying filters should repaint Projects');
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
}
