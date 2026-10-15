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
assert(hfLabelToneClass('needs-direction') === 'hf-label-direction' && hfLabelToneClass('hold/review') === 'hf-label-hold' && hfLabelToneClass('bug') === '', 'label tones');
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
