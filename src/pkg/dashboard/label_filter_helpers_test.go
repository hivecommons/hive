package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDashboardLabelFilterHelpers runs the DOM-free half of the project
// cards' faceted label filter (hivecommons/hive#12172) under node: term
// parsing, matching semantics, facet counts, preset-priority sorting and the
// localStorage store defaults.
func TestDashboardLabelFilterHelpers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: label filter helper behavior was not executed")
	}
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(raw)
	const beginMarker = "// ── Label filter (hf) pure helpers (begin)"
	const endMarker = "// ── Label filter (hf) pure helpers (end)"
	start := strings.Index(html, beginMarker)
	if start < 0 {
		t.Fatal("label filter helper block begin marker not found")
	}
	end := strings.Index(html[start:], endMarker)
	if end < 0 {
		t.Fatal("label filter helper block end marker not found")
	}
	block := html[start : start+end]
	for _, forbidden := range []string{"document.", "window.", "localStorage"} {
		if strings.Contains(block, forbidden) {
			t.Fatalf("pure helper block must stay DOM-free; found %q", forbidden)
		}
	}

	script := block + `
function assert(cond, msg) { if (!cond) throw new Error(msg); }
function eq(a, b, msg) { const ja = JSON.stringify(a), jb = JSON.stringify(b); if (ja !== jb) throw new Error(msg + ': got ' + ja + ' want ' + jb); }

// --- term parsing ---
eq(hfParseTerm(' "Rate  Limit" '), { raw: '"rate limit"', negate: false, phrase: true, words: ['rate limit'] }, 'quoted phrase');
eq(hfParseTerm('-flaky'), { raw: '-flaky', negate: true, phrase: false, words: ['flaky'] }, 'exclusion');
eq(hfParseTerm('Rate limit'), { raw: 'rate limit', negate: false, phrase: false, words: ['rate', 'limit'] }, 'words');
eq(hfParseTerm('-"ci red"').words, ['ci red'], 'negated phrase');
assert(hfParseTerm('') === null && hfParseTerm('-') === null && hfParseTerm('""') === null, 'empty terms parse to null');

// --- entries ---
const mk = (kind, number, title, labels, extra) => hfEntry(kind, Object.assign({ number, title, labels, created_at: new Date(2026, 0, number).toISOString(), author: 'alice' }, extra || {}), 'acme/repo', (extra && extra.__opts) || {});
const e1 = mk('issue', 1, 'Rate limit exceeded on login', ['bug', 'needs-human']);
const e2 = mk('issue', 2, 'Limit the rate of retries', ['needs-direction'], { assignees: ['bob'] });
const e3 = mk('pr', 3, 'Fix flaky CI', ['needs-signal'], { app_authored: true });
const e4 = mk('issue', 4, 'Docs typo', ['docs'], { author: 'renovate[bot]', __opts: { held: true } });
const e5 = mk('issue', 5, 'Covered thing', ['bug'], { __opts: { coveringPR: true, bucket: 'claimed_by_pr' } });
const all = [e1, e2, e3, e4, e5];
assert(e3.authorKind === 'hive' && e4.authorKind === 'bot' && e1.authorKind === 'human', 'author kinds');
assert(e4.states.has('parked') && !e5.states.has('parked') && e5.states.has('covered-by-pr') && e5.states.has('claimed'), 'derived states');
assert(e1.states.has('unclaimed') && e2.states.has('claimed') && !e3.states.has('unclaimed'), 'claim states only on issues');

// --- matching ---
const nums = list => list.map(e => e.number);
eq(nums(hfMatchingEntries({ terms: ['"rate limit"'] }, all)), [1], 'phrase matches as phrase only');
eq(nums(hfMatchingEntries({ terms: ['rate limit'] }, all)), [1, 2], 'unquoted words AND within the term, any order');
eq(nums(hfMatchingEntries({ terms: ['#3'] }, all)), [3], 'number is searchable');
eq(nums(hfMatchingEntries({ terms: ['needs-'] }, all)), [1, 2, 3], 'labels are searchable');
eq(nums(hfMatchingEntries({ terms: ['-rate'] }, all)), [3, 4, 5], 'exclusion term removes matches');
eq(nums(hfMatchingEntries({ labels: ['bug'], terms: ['-login'] }, all)), [5], 'exclusion always applies, even with other criteria');
eq(nums(hfMatchingEntries({ labels: ['needs-human', 'docs'] }, all)), [1, 4], 'labels OR by default');
eq(nums(hfMatchingEntries({ labels: ['bug', 'needs-human'], matchAll: true }, all)), [1], 'match all ANDs labels');
eq(nums(hfMatchingEntries({ labels: ['bug'], terms: ['flaky'] }, all)), [1, 3, 5], 'labels and text share the any/all toggle');
eq(nums(hfMatchingEntries({ labels: ['bug'], terms: ['login'], matchAll: true }, all)), [1], 'match all across labels and text');
eq(nums(hfMatchingEntries({ kinds: ['pr'] }, all)), [3], 'kind facet');
eq(nums(hfMatchingEntries({ states: ['parked'] }, all)), [4], 'state facet');
eq(nums(hfMatchingEntries({ authors: ['bot', 'hive'] }, all)), [3, 4], 'author facet OR within group');
eq(nums(hfMatchingEntries({ authors: ['bot'], kinds: ['pr'] }, all)), [], 'facet groups AND across groups');
eq(nums(hfMatchingEntries({ labels: ['NEEDS-HUMAN'] }, all)), [1], 'labels match case-insensitively');
eq(nums(hfMatchingEntries({}, all)), [1, 2, 3, 4, 5], 'empty filter matches everything');

// --- sorting: preset priority first, then newest-first ---
const needsHuman = { labels: ['needs-direction', 'needs-decision', 'needs-spec', 'needs-signal', 'needs-human', 'hold'] };
eq(nums(hfSortEntries(hfMatchingEntries(needsHuman, all), needsHuman)), [2, 3, 1], 'direction > signal > human');
eq(nums(hfSortEntries(all, {})), [5, 4, 3, 2, 1], 'no labels: newest first');

// --- facet counts ---
const counts = hfFacetCounts({ labels: ['bug'] }, all);
assert(counts.labels.get('bug').count === 2 && counts.labels.get('docs').count === 1, 'label counts are independent of the positive criteria');
eq(counts.kinds, { issue: 2 }, 'kind counts respect the criteria');
assert(counts.states['covered-by-pr'] === 1 && counts.states.unclaimed === 1 && counts.states.claimed === 1, 'state counts');
const excluded = hfFacetCounts({ terms: ['-login'] }, all);
assert(excluded.labels.get('bug').count === 1, 'negative terms shrink label counts');
const typed = hfFacetCounts({ kinds: ['pr'] }, all);
assert(!typed.labels.has('bug') && typed.labels.get('needs-signal').count === 1 && typed.kinds.issue === 4, 'group facets restrict labels but the kind facet counts its alternatives');
assert(hfFacetCounts({ labels: ['missing-label'] }, all).labels.get('missing-label').count === 0, 'selected labels always listed');

// --- normalisation / signatures ---
eq(hfNormalizeFilter({ labels: ['Bug', 'bug', ' '], terms: ['  Rate   Limit ', 'x', 'x'], kinds: ['pr', 'nope'], states: ['parked'], authors: ['bot'], matchAll: 1 }),
  { labels: ['bug'], terms: ['rate limit', 'x'], kinds: ['pr'], states: ['parked'], authors: ['bot'], matchAll: true }, 'normalisation dedupes and validates');
assert(hfFilterSignature({ labels: ['a', 'b'], terms: ['x', 'y'] }) === hfFilterSignature({ labels: ['a', 'b'], terms: ['y', 'x'] }), 'term order is irrelevant');
assert(hfFilterSignature({ labels: ['a', 'b'] }) !== hfFilterSignature({ labels: ['b', 'a'] }), 'label order is the sort priority and part of the identity');
assert(hfFilterIsEmpty({}) && hfFilterIsEmpty({ matchAll: true }) && !hfFilterIsEmpty({ terms: ['x'] }), 'emptiness');

// --- store ---
const presets = [{ key: 'needs-human', label: 'Needs human', labels: needsHuman.labels }, { key: 'help-wanted', label: 'Help wanted', labels: ['help wanted'] }];
const fresh = hfNormalizeStore(null);
assert(fresh.activeKey === 'preset:needs-human' && fresh.active === null, 'first load selects the Needs human preset');
eq(hfActiveFilter(fresh, presets).labels, needsHuman.labels, 'active filter resolves to the preset labels');
eq(hfPinnedKeys(fresh, presets), ['preset:needs-human'], 'Needs human pinned by default');
const edited = hfNormalizeStore({ activeKey: 'saved:abc', saved: [{ id: 'abc', name: 'Mine', filter: { labels: ['x'] }, pinned: true }, { bogus: true }], presetPins: { 'needs-human': false }, hiddenPresets: ['help-wanted'] });
eq(hfPinnedKeys(edited, presets), ['saved:abc'], 'unpinned preset drops out, pinned saved filter stays');
eq(hfResolveKey(edited, presets, 'saved:abc').filter.labels, ['x'], 'saved filter resolves');
assert(hfResolveKey(edited, presets, 'saved:nope').source === 'custom', 'unknown key falls back to custom');
assert(hfResolveKey(hfNormalizeStore(null), [], 'preset:needs-human').source === 'custom', 'missing payload presets degrade to custom, not a crash');
const ordered = hfNormalizeStore({ saved: [{ id: 'a', name: 'A', pinned: true }, { id: 'b', name: 'B', pinned: true }], order: ['saved:b', 'preset:needs-human'] });
eq(hfPinnedKeys(ordered, presets), ['saved:b', 'preset:needs-human', 'saved:a'], 'explicit order wins, the rest keep discovery order');
assert(hfLabelToneClass('stage/085-needs-decision') === 'hf-label-decision' && hfLabelToneClass('hold/review') === 'hf-label-hold' && hfLabelToneClass('bug') === '', 'label tones');
console.log('ok');
`
	dir := t.TempDir()
	path := filepath.Join(dir, "label_filter_helpers.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("writing script: %v", err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("unexpected node output: %s", out)
	}
}

// TestDashboardLabelFilterPinnedSidebar drives the pinned-filter sidebar and
// the cross-repo view (hivecommons/hive#12172, PR 2) through a tiny DOM shim:
// pinned entries render with live cross-repo counts, opening one swaps the
// card grid for the grouped list, and navigating elsewhere restores the grid.
func TestDashboardLabelFilterPinnedSidebar(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: pinned sidebar behavior was not executed")
	}
	raw, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(raw)
	const beginMarker = "// ── Label filter (hf) pure helpers (begin)"
	const endMarker = "    function renderRepos(repos) {"
	start := strings.Index(html, beginMarker)
	end := strings.Index(html, endMarker)
	if start < 0 || end < start {
		t.Fatal("label filter block boundaries not found")
	}
	for _, id := range []string{`id="hf-nav-group"`, `id="hf-nav-items"`, `id="hf-crossrepo"`} {
		if !strings.Contains(html, id) {
			t.Fatalf("index.html is missing %s", id)
		}
	}
	if !strings.Contains(html, ".repo-grid[hidden], .swarm-panel[hidden] { display: none; }") {
		t.Fatal(".repo-grid is display:flex, so the hidden attribute needs an explicit override or the cards stack under the cross-repo list")
	}
	if strings.Index(html, `id="hf-crossrepo"`) > strings.Index(html, `id="swarm-panel"`) || strings.Index(html, `id="swarm-panel"`) > strings.Index(html, `<div class="repo-grid" id="repos">`) {
		t.Fatal("Projects order must be: cross-repo list, swarm header, repo cards")
	}
	if !strings.Contains(html, "if (typeof hfOnNavigate === 'function') hfOnNavigate(section);") {
		t.Fatal("ocNavigate must notify the label filter so the cross-repo view closes on navigation")
	}

	shim := `
const store = {};
const localStorage = { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); }, removeItem: k => { delete store[k]; } };
global.localStorage = localStorage; global.window = global;
window._lastStatus = { hiveId: 'h1', labelPresets: [{ key: 'needs-human', label: 'Needs human', labels: ['needs-direction', 'needs-decision', 'needs-spec', 'needs-signal', 'needs-human', 'hold'] }],
  repos: [
    { name: 'acme/repo', actionableIssues: [{ number: 1, title: 'A', labels: ['needs-human'], created_at: '2026-01-01T00:00:00Z' }], openPrs: [{ number: 2, title: 'P', labels: ['needs-signal'], created_at: '2026-01-02T00:00:00Z' }] },
    { name: 'acme/two', actionableIssues: [{ number: 9, title: 'Z', labels: ['needs-direction'], created_at: '2026-01-05T00:00:00Z' }] },
    { name: 'acme/none', actionableIssues: [{ number: 4, title: 'plain', labels: ['bug'], created_at: '2026-01-05T00:00:00Z' }] }
  ] };
function mkEl(id) { return { id, innerHTML: '', value: '', hidden: false, classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } }, setAttribute() {}, getAttribute() { return null; }, contains() { return false; }, querySelector() { return null; }, querySelectorAll() { return []; }, focus() {}, dataset: {} }; }
const els = {};
global.document = { getElementById: id => els[id] || (els[id] = mkEl(id)), activeElement: null, querySelectorAll: () => [], querySelector: () => null };
function esc(s) { return String(s == null ? '' : s); }
function pillAge() { return ''; } function issueCoveringPR() { return null; } function setIfChanged(el, h) { el.innerHTML = h; } function showToast() {}
function miniHiveKey() { return 'h1'; } function writeSectionCollapsedKey(id, c) { localStorage.setItem('sec:' + id, c ? '1' : '0'); } const SECTION_LS_PREFIX = 'sec:';
const navCalls = [];
// Mirrors the real ocNavigate contract: it notifies hfOnNavigate after switching sections.
function ocNavigate(section) { navCalls.push(section); hfOnNavigate(section); }
function renderRepos() { hfRenderEditor(); hfRenderPinned(); hfRenderCrossRepo(); }
`
	assertions := `
function assert(cond, msg) { if (!cond) throw new Error(msg); }
renderRepos();
assert(els['hf-nav-group'].hidden === false, 'Needs human is pinned by default so the Filters group shows');
assert(/data-action="hfOpenCrossRepo"/.test(els['hf-nav-items'].innerHTML), 'pinned entry opens the cross-repo view');
assert(/badge-count[^>]*>3</.test(els['hf-nav-items'].innerHTML), 'count badge sums matches across all repos');
hfOpenCrossRepo('preset:needs-human');
assert(navCalls.join(',') === 'repos-section', 'opening navigates to Projects');
assert(els['repos'].hidden === true && els['hf-crossrepo'].hidden === false, 'grid swapped for the cross-repo panel');
assert(els['swarm-panel'].hidden === true, 'swarm header hides with the grid it describes');
const panel = els['hf-crossrepo'].innerHTML;
assert((panel.match(/hf-crossrepo-group/g) || []).length === 2, 'only repos with matches get a group');
assert(/id="hf-x-acme-repo"/.test(panel) && /id="hf-x-acme-two"/.test(panel) && !/acme-none/.test(panel), 'groups keyed per repo');
assert(/data-action="hfToggleSection"/.test(panel), 'groups are collapsible via the shared hf toggle');
assert(/hfCloseCrossRepo/.test(panel), 'panel offers a way back to the cards');
assert(/id="hf-crossrepo-all"/.test(panel) && /data-arg0="hf-crossrepo-all"/.test(panel) && /aria-expanded="true"/.test(panel), 'the whole list has its own collapse chevron, open by default when it has matches');
assert(/collapse with the chevron/.test(panel) && /← Project cards/.test(panel), 'header hint explains how to collapse or close the list');
hfToggleSection('hf-x-acme-repo');
hfToggleSection('hf-crossrepo-all');
assert(store['sec:hf-crossrepo-all'] === '1', 'collapsing the whole list persists like a repo group');
renderRepos();
assert(/id="hf-crossrepo-all"[^>]*>[^]*?aria-expanded="false"/.test(els['hf-crossrepo'].innerHTML), 'collapsed state survives a re-render');
assert(store['sec:hf-x-acme-repo'] === '1', 'group collapse state persists with the section collapse keys');
hfOnNavigate('overview-section');
assert(els['hf-crossrepo'].hidden === true && els['repos'].hidden === false, 'navigating elsewhere restores the card grid');
hfOpenCrossRepo('preset:needs-human');
hfCloseCrossRepo();
assert(els['hf-crossrepo'].hidden === true && els['repos'].hidden === false, 'close button restores the card grid');
assert(els['swarm-panel'].hidden === false, 'close button restores the swarm header above the cards');
hfTogglePin('preset:needs-human');
assert(els['hf-nav-group'].hidden === true, 'unpinning the last filter hides the Filters group');
assert(JSON.parse(store['hive-label-filters:h1']).presetPins['needs-human'] === false, 'pin state persists with the store');
console.log('ok');
`
	dir := t.TempDir()
	path := filepath.Join(dir, "label_filter_pinned.js")
	if err := os.WriteFile(path, []byte(shim+html[start:end]+assertions), 0o600); err != nil {
		t.Fatalf("writing script: %v", err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("unexpected node output: %s", out)
	}
}
