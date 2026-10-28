package dashboard

import (
	"strings"
	"testing"
)

// TestSearchPaneRegisteredAsSection: the Search pane (#14888) is wired
// wherever every other dashboard section is, and starts hidden until
// GET /api/search answers.
func TestSearchPaneRegisteredAsSection(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`<a class="oc-nav-item" data-section="search-section" data-action="ocNavigate" data-arg0="search-section" hidden>`,
		`id="search-section" data-dashboard-section="search-section" style="display:none"`,
		`'knowledge-section','search-section','review-queue-section'`,
		`'search-section': { title: '🔍 Search', summary: 'search' },`,
		`'search-section': { heading: 'Search', help: `,
		`id="search-query" type="search"`,
		`data-keydown-action="searchPaneRun" data-keys="Enter"`,
		`<div id="search-results" aria-live="polite"></div>`,
		"    v4RefreshPanels();\n    searchPaneProbe();\n",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing Search pane wiring %q", want)
		}
	}
	render := jsFunctionBody(t, html, "function renderRepos(repos)")
	for _, want := range []string{"repoHealthEnsure(cardRepo);", "${pausedPill}${repoHealthSlotHTML(cardRepo)}</span>"} {
		if !strings.Contains(render, want) {
			t.Errorf("renderRepos does not render the repo health badge in the card header: missing %q", want)
		}
	}
}

func searchPaneScript(t *testing.T) string {
	t.Helper()
	html := indexHTML(t)
	block := indexSliceBetween(t, html,
		"    // ---- Portfolio search pane and repo health badge (#14888)",
		"    // ---- end portfolio search pane and repo health badge")
	return `
const assert = require('node:assert/strict');
` + jsFunc(t, html, "esc") + `
function formatDashboardDateTime(v) { return String(v); }
function refreshSectionCardShell() {}
const summaries = {};
function setSectionSummary(id, text) { summaries[id] = text; }
function el(id) { return { id, style: { display: 'none' }, hidden: true, innerHTML: '', value: '', attrs: {}, getAttribute(k) { return this.attrs[k]; } }; }
const els = { 'search-section': el('search-section'), 'search-results': el('search-results'), 'search-query': el('search-query') };
const nav = el('nav');
let slots = [];
global.document = {
  getElementById: id => els[id] || null,
  querySelector: sel => sel === '.oc-nav-item[data-section="search-section"]' ? nav : null,
  querySelectorAll: sel => sel === '[data-repo-health]' ? slots : [],
};
let calls = [];
let reply = () => ({ status: 200, body: {} });
global.fetch = async url => { calls.push(url); const r = reply(url); return { status: r.status, ok: r.status >= 200 && r.status < 300, json: async () => r.body }; };
` + block
}

// TestSearchPaneRendersFixtureAndHidesWhenDisabled renders the pane from a
// GET /api/search fixture and checks the 404/403 answers hide it.
func TestSearchPaneRendersFixtureAndHidesWhenDisabled(t *testing.T) {
	runNodeScript(t, searchPaneScript(t)+`
const fixture = {
  query: 'flaky', total: 3,
  hits: [
    { id: 'evidence:o/r#12', repo: 'o/r', kind: 'evidence', at: '2026-10-01T00:00:00Z', url: 'https://github.com/o/r/pull/12', visibility: 'public', title: 'Fix flaky test', fields: { verdict: 'approve' }, score: 5, terms: ['flaky'], highlight: { snippet: 'fix the flaky <b>test</b>', spans: [[8, 13]] } },
    { id: 'sentinel:o/r#13', repo: 'o/r', kind: 'sentinel', at: '2026-10-02T00:00:00Z', url: 'https://github.com/o/r/pull/13', visibility: 'public', title: '[redacted]', fields: { 'sentinel.rule': 'prompt-injection' }, score: 3, terms: ['flaky'], highlight: { snippet: '', spans: [] }, redacted: true },
    { id: 'bead:x', repo: 'o/s', kind: 'bead', at: '2026-10-03T00:00:00Z', url: 'javascript:alert(1)', visibility: 'private', title: 'Bead flaky', fields: { verdict: 'request_changes' }, score: 1, terms: ['flaky'], highlight: { snippet: '', spans: [] } },
  ],
  facets: {
    kind: [{ value: 'evidence', count: 1 }, { value: 'sentinel', count: 1 }, { value: 'bead', count: 1 }],
    repo: [{ value: 'o/r', count: 2 }, { value: 'o/s', count: 1 }],
    verdict: [{ value: 'approve', count: 1 }, { value: 'request_changes', count: 1 }],
    'sentinel.rule': [{ value: 'prompt-injection', count: 1 }],
  },
  coverage: { 'o/broken': 'index unreadable' },
};
(async () => {
  reply = () => ({ status: 200, body: fixture });
  els['search-query'].value = '  flaky ';
  await searchPaneRun();
  assert.equal(calls.pop(), '/api/search?q=flaky&limit=50');
  assert.equal(els['search-section'].style.display, '', 'a 200 shows the section');
  assert.equal(nav.hidden, false, 'a 200 shows the nav item');
  const out = els['search-results'].innerHTML;
  for (const field of ['kind', 'repo', 'verdict', 'sentinel.rule']) assert.ok(out.includes('data-facet="' + field + '"'), 'facet ' + field + ' missing');
  assert.ok(out.includes('data-arg1="prompt-injection">prompt-injection <span class="search-facet-count">1</span>'));
  assert.ok(out.includes('<a href="https://github.com/o/r/pull/12" target="_blank" rel="noopener noreferrer">Fix flaky test</a>'));
  assert.ok(out.includes('fix the flaky &lt;b&gt;test&lt;/b&gt;'), 'snippet is escaped');
  assert.equal(out.split('🛡 redacted').length - 1, 1, 'only the redacted hit carries the marker');
  assert.ok(/data-redacted="1">[^]*?🛡 redacted/.test(out));
  assert.ok(!out.includes('javascript:'), 'non-http URLs are not linked');
  assert.ok(out.includes('Not searched: <span title="index unreadable">o/broken</span>'), 'coverage gap surfaced');
  assert.ok(out.includes('3 shown of 3 matches'));
  assert.equal(summaries['search-section'], '3 matches');

  searchPaneToggleFacet('verdict', 'approve');
  let narrowed = els['search-results'].innerHTML;
  assert.ok(narrowed.includes('1 shown of 3 matches'), 'verdict narrows hits client-side');
  assert.ok(narrowed.includes('class="search-facet-chip active" aria-pressed="true" data-action="searchPaneToggleFacet" data-arg0="verdict" data-arg1="approve"'));
  assert.equal(calls.length, 0, 'verdict facet does not re-query');
  searchPaneToggleFacet('verdict', 'approve');
  assert.ok(els['search-results'].innerHTML.includes('3 shown of 3 matches'), 'clicking the active chip clears it');

  searchPaneToggleFacet('kind', 'evidence');
  await new Promise(r => setImmediate(r));
  assert.equal(calls.pop(), '/api/search?q=flaky&kind=evidence&limit=50', 'kind facet narrows on the server');

  for (const status of [404, 403]) {
    reply = () => ({ status, body: { ok: false, error: status === 404 ? 'search not enabled' : 'forbidden' } });
    els['search-section'].style.display = '';
    nav.hidden = false;
    assert.equal(await searchPaneFetch('/api/search?limit=1'), null);
    assert.equal(els['search-section'].style.display, 'none', status + ' hides the section');
    assert.equal(nav.hidden, true, status + ' hides the nav item');
  }

  reply = () => ({ status: 503, body: { ok: false, error: 'search index unavailable' } });
  await searchPaneProbe();
  assert.equal(calls.pop(), '/api/search?probe=1', 'the availability probe runs no query');
  assert.equal(els['search-section'].style.display, '', 'an enabled but unready index keeps the pane');
  assert.ok(els['search-results'].innerHTML.includes('search index unavailable'));
})().catch(e => { console.error(e); process.exit(1); });
`)
}

// TestRepoHealthBadgeRendersFixtureAndHidesOn503 renders the project-card
// health badge from a GET /api/repos/{owner}/{repo}/health fixture and checks
// a 503 (search disabled) removes it from every card.
func TestRepoHealthBadgeRendersFixtureAndHidesOn503(t *testing.T) {
	runNodeScript(t, searchPaneScript(t)+`
const health = {
  repo: 'o/r', score: 82.4, status: 'reported', window_days: 90, min_n: 10, computed_at: '2026-10-27T00:00:00Z',
  components: [
    { name: 'ci_pass_rate', status: 'reported', score: 0.9, weight: 20, contribution: 18, n: 40, inputs: {} },
    { name: 'time_to_merge', status: 'not_reported', score: null, weight: 15, contribution: null, n: 3, inputs: {}, reason: 'fewer than 10 merged PRs' },
  ],
};
(async () => {
  assert.equal(repoHealthSlotHTML('o/r'), '<span class="repo-health-slot" data-repo-health="o/r"></span>', 'no badge before data');
  reply = () => ({ status: 200, body: health });
  const slot = { attrs: { 'data-repo-health': 'o/r' }, innerHTML: '', getAttribute(k) { return this.attrs[k]; } };
  slots = [slot];
  await repoHealthEnsure('o/r');
  assert.deepEqual(calls, ['/api/repos/o/r/health']);
  assert.ok(slot.innerHTML.startsWith('<span class="repo-health-badge good" title="Repo health 82/100 (reported).'), slot.innerHTML);
  assert.ok(slot.innerHTML.includes('>♥ 82 · reported</span>'), 'score and status are visible');
  assert.ok(slot.innerHTML.includes('&#10;• ci_pass_rate: 90% × weight 20 → 18 pts (n=40)'), 'components are in the tooltip');
  assert.ok(slot.innerHTML.includes('• time_to_merge: not reported — fewer than 10 merged PRs (n=3)'));
  assert.equal(repoHealthSlotHTML('o/r'), '<span class="repo-health-slot" data-repo-health="o/r">' + slot.innerHTML + '</span>', 'a re-render keeps the badge');
  assert.equal(repoHealthEnsure('o/r'), null, 'cached health is not refetched on every render');
  assert.equal(repoHealthEnsure('group/sub/repo'), null, 'paths that are not owner/repo are not fetched');

  assert.ok(repoHealthBadgeHTML({ repo: 'o/s', score: null, status: 'not_reported', components: [] }).includes('>♥ — · not reported</span>'));

  reply = () => ({ status: 503, body: { ok: false, error: 'repo health unavailable: search is not enabled' } });
  calls = [];
  await repoHealthEnsure('o/s');
  assert.equal(slot.innerHTML, '', 'a 503 removes the badge from every card');
  assert.equal(repoHealthSlotHTML('o/r'), '<span class="repo-health-slot" data-repo-health="o/r"></span>');
  assert.equal(repoHealthEnsure('o/t'), null, 'no more health requests once search is off');
  assert.deepEqual(calls, ['/api/repos/o/s/health']);
})().catch(e => { console.error(e); process.exit(1); });
`)
}
