package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestStageTrackerLabelFacetsWiring pins the label facet surface: the
// hive.tracker.labels storage key, the needs-human facet constants, the
// "+N more…" button that opens the in-app modal (no native dialog), the
// modal's shared recipe (.hive-dialog + .modal-scroll) and the token-only
// CSS block.
func TestStageTrackerLabelFacetsWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`const STAGE_TRACKER_LABELS_KEY = 'hive.tracker.labels';`,
		`const STAGE_TRACKER_NEEDS_HUMAN_FACET = '@needs-human';`,
		`const STAGE_TRACKER_NEEDS_HUMAN_LABELS = ['needs-human', 'stage/045-needs-decision', 'stage/085-needs-decision', 'stage/071-needs-author', 'sentinel-alert'];`,
		`const STAGE_TRACKER_NEEDS_HUMAN_PATTERNS = [/^needs-human/, /needs-decision$/, /needs-author$/];`,
		`function stageTrackerIsNeedsHumanLabel(label)`,
		`function stageTrackerLabelHit(have, sel)`,
		`function stageTrackerLabelCompare(a, b)`,
		`function stageTrackerLoadLabels()`,
		`function stageTrackerSaveLabels(labels)`,
		`function stageTrackerFacetModalHtml(facets, draft, q)`,
		`function hamLabelsMore()`,
		`data-action="hamLabelsMore" data-stop="1" aria-haspopup="dialog"`,
		`if (!byFamily[fam].some(l => stageTrackerLabelHit(have, l))) return false;`,
		`case 'labels': f.labels = (Array.isArray(value) ? value : []).map(l => String(l || '').toLowerCase()).filter(Boolean); break;`,
		`hiveWireDialog(overlay, () => {}, null, '.primary', '.cancel', close => { hamFilter('labels', draft.slice()); close(true); });`,
		`<div class="hive-dialog-body long modal-scroll ham-facet-list">`,
		`.ham-facet-dialog { width: 560px; }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing label facet wiring %q", want)
		}
	}
	// The "+N more" affordance used to be an inert <span>; it must be a button now.
	if strings.Contains(html, `<span class="ham-labels-more"`) {
		t.Error("the +N more affordance is still an inert span")
	}
	// No native dialogs anywhere in the facet modal path.
	body := jsFunc(t, html, "hamLabelsMore") + jsFunc(t, html, "stageTrackerFacetModalHtml")
	for _, bad := range []string{"window.prompt", "window.confirm", "window.alert", " prompt(", " confirm(", " alert("} {
		if strings.Contains(body, bad) {
			t.Errorf("facet modal uses a native dialog: %q", bad)
		}
	}
	// Token-only CSS (ADR-0018): no literal colour in the facet block.
	start := strings.Index(html, "/* Facet modal (hive-dialog + .modal-scroll recipe")
	end := strings.Index(html, ".ham-meta { display: flex;")
	if start < 0 || end < start {
		t.Fatal("facet modal CSS block not found")
	}
	if css := html[start:end]; strings.Contains(css, "#") || strings.Contains(css, "rgb(") || strings.Contains(css, "rgba(") {
		t.Errorf("facet modal CSS must use tokens only, found a literal colour:\n%s", css)
	}
}

// TestStageTrackerLabelFacets runs the facet model under node: ordering
// (stage/NNN by number, then alpha, case-insensitive), the overflow set
// (inline row capped at STAGE_TRACKER_LABEL_CHIP_LIMIT, selected labels
// always inline, the remainder counted and listed in the modal), the
// needs-human expansion and its AND/OR semantics with real labels, the
// modal markup (groups, counts, checkboxes, search), and the
// hive.tracker.labels persistence.
func TestStageTrackerLabelFacets(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — label facets were NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		"const store = {}; const localStorage = { getItem: k => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); }, removeItem: k => { delete store[k]; } };",
		jsConstLine(t, html, "STAGE_TRACKER_FILTER_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_PAGE_SIZES"),
		jsConstLine(t, html, "STAGE_TRACKER_DEFAULT_PAGE_SIZE"),
		jsConstLine(t, html, "STAGE_TRACKER_LABEL_CHIP_LIMIT"),
		jsConstLine(t, html, "STAGE_TRACKER_LABELS_KEY"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_FACET"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_LABELS"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_HUMAN_PATTERNS"),
		jsConstLine(t, html, "STAGE_TRACKER_AGE_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_ATTENTION_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_LABEL_FAMILIES"),
		jsConstLine(t, html, "STAGE_TRACKER_SORTS"),
		jsFunc(t, html, "esc"),
		jsFunc(t, html, "stageTrackerDefaultFilter"),
		"var _stageTrackerFilter = stageTrackerDefaultFilter();",
		jsFunc(t, html, "stageTrackerLabelFamily"),
		jsFunc(t, html, "stageTrackerIsNeedsHumanLabel"),
		jsFunc(t, html, "stageTrackerLabelHit"),
		jsFunc(t, html, "stageTrackerLabelStageNumber"),
		jsFunc(t, html, "stageTrackerLabelCompare"),
		jsFunc(t, html, "stageTrackerNormalizeFilter"),
		jsFunc(t, html, "stageTrackerWaitSince"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
		jsFunc(t, html, "stageTrackerMatches"),
		jsFunc(t, html, "stageTrackerLabelFacets"),
		jsFunc(t, html, "stageTrackerLabelsHtml"),
		jsFunc(t, html, "stageTrackerFacetText"),
		jsFunc(t, html, "stageTrackerFacetHit"),
		jsFunc(t, html, "stageTrackerFacetRowHtml"),
		jsFunc(t, html, "stageTrackerFacetModalHtml"),
		jsFunc(t, html, "stageTrackerLoadFilter"),
		jsFunc(t, html, "stageTrackerSaveFilter"),
		jsFunc(t, html, "stageTrackerLoadLabels"),
		jsFunc(t, html, "stageTrackerSaveLabels"),
	}, "\n") + `
const F = o => stageTrackerNormalizeFilter(Object.assign(stageTrackerDefaultFilter(), o || {}));
const it = (n, labels, extra) => Object.assign({ kind: 'issue', repo: 'a/b', number: n, stage: 10, title: 'I' + n, labels, modifiers: [] }, extra || {});

// Ordering: stage/NNN by number (not string), then alpha case-insensitive.
const unordered = ['Zeta', 'stage/100-merge-queue', 'bug', 'stage/020-claimed', 'alpha', 'stage/010-actionable', 'Needs-rebase', 'author/hive', 'stage/085-needs-decision'];
assert.deepStrictEqual(unordered.slice().sort(stageTrackerLabelCompare), ['stage/010-actionable', 'stage/020-claimed', 'stage/085-needs-decision', 'stage/100-merge-queue', 'alpha', 'author/hive', 'bug', 'Needs-rebase', 'Zeta']);
assert.strictEqual(stageTrackerLabelStageNumber('stage/045-needs-decision'), 45);
assert.ok(isNaN(stageTrackerLabelStageNumber('staged')));

// Every label in the snapshot is in all, in order; singletons included.
let items = [it(1, ['bug', 'stage/010-actionable']), it(2, ['stage/100-merge-queue', 'lonely']), it(3, ['stage/020-claimed', 'bug'])];
let fac = stageTrackerLabelFacets(items, F());
assert.deepStrictEqual(fac.all.map(x => x.label), ['stage/010-actionable', 'stage/020-claimed', 'stage/100-merge-queue', 'bug', 'lonely']);
assert.deepStrictEqual(fac.all.map(x => x.count), [1, 1, 1, 2, 1]);
assert.strictEqual(fac.hidden, 0);
assert.strictEqual(fac.needsHuman.count, 0);
assert.ok(!fac.chips.some(x => x.label === STAGE_TRACKER_NEEDS_HUMAN_FACET), 'no needs-human chip when nothing matches');

// Overflow: many labels → the row stops at the limit, the rest are counted,
// a selected label past the limit still shows inline, and the modal lists all.
const many = [];
for (let i = 0; i < 40; i++) many.push(it(100 + i, ['stage/0' + (10 + i) + '-x', 'other-' + String(i).padStart(2, '0')]));
fac = stageTrackerLabelFacets(many, F());
assert.strictEqual(fac.all.length, 80);
assert.strictEqual(fac.chips.length, STAGE_TRACKER_LABEL_CHIP_LIMIT);
assert.strictEqual(fac.hidden, 80 - STAGE_TRACKER_LABEL_CHIP_LIMIT);
assert.ok(fac.chips.every(x => x.label.startsWith('stage/')), 'stage labels fill the inline row first');
fac = stageTrackerLabelFacets(many, F({ labels: ['other-39'] }));
assert.strictEqual(fac.chips.length, STAGE_TRACKER_LABEL_CHIP_LIMIT + 1);
assert.ok(fac.chips[fac.chips.length - 1].label === 'other-39' && fac.chips[fac.chips.length - 1].on, 'the selected overflow label is appended inline');
assert.strictEqual(fac.hidden, 80 - STAGE_TRACKER_LABEL_CHIP_LIMIT - 1, 'the inlined selection no longer counts as hidden');
let row = stageTrackerLabelsHtml(fac);
assert.ok(row.includes('<button type="button" class="ham-chip ham-labels-more" data-action="hamLabelsMore"'), 'the +N more affordance is a button: ' + row.slice(-300));
assert.ok(row.includes('>+' + fac.hidden + ' more…</button>'), row.slice(-200));
assert.ok(!row.includes('<span class="ham-labels-more"'));
let modal = stageTrackerFacetModalHtml(fac, ['other-39'], '');
assert.strictEqual((modal.match(/class="ham-facet-check"/g) || []).length, 80, 'the modal lists every label');
assert.ok(modal.includes('data-label="other-39" checked'), 'the current selection is pre-ticked');
assert.ok(modal.includes('class="hive-dialog-body long modal-scroll ham-facet-list"'), 'shared modal scroll recipe');
assert.ok(modal.includes('>Stage labels<') && modal.includes('>Other labels<'));
assert.ok(!modal.includes('>Needs human<'), 'no needs-human group when nothing matches it');
assert.ok(modal.includes('1 selected · 80 of 80 shown'));
assert.ok(modal.includes('class="hv-btn btn-secondary ham-facet-clear"') && modal.includes('hive-dialog-btn primary">Apply<') && modal.includes('hive-dialog-btn cancel">Cancel<'));
// Search hides non-matching rows and reports the count.
modal = stageTrackerFacetModalHtml(fac, [], 'other-3');
assert.strictEqual((modal.match(/<label class="ham-facet-row" hidden>/g) || []).length, 70);
assert.ok(modal.includes('0 selected · 10 of 80 shown'));
modal = stageTrackerFacetModalHtml(fac, [], 'zzz');
assert.ok(modal.includes('0 of 80 shown') && modal.includes('<div class="ham-facet-empty ham-facet-nomatch">No label matches the search.</div>'));
assert.ok(stageTrackerFacetModalHtml(fac, [], 'a<b').includes('value="a&lt;b"'), 'query is escaped');

// Needs-human: exact names, prefix / suffix rules, and nothing else.
for (const l of ['needs-human', 'Needs-Human', 'needs-human-review', 'stage/045-needs-decision', 'stage/085-needs-decision', 'stage/071-needs-author', 'sentinel-alert', 'foo/needs-decision', 'x-needs-author']) assert.ok(stageTrackerIsNeedsHumanLabel(l), l);
for (const l of ['needs-rebase', 'stage/010-actionable', 'bug', 'author/human', 'humans-needed', '']) assert.ok(!stageTrackerIsNeedsHumanLabel(l), l);
items = [
  it(1, ['stage/045-needs-decision', 'bug']),
  it(2, ['needs-human', 'author/hive']),
  it(3, ['sentinel-alert']),
  it(4, ['stage/010-actionable', 'bug']),
  it(5, ['needs-rebase']),
];
fac = stageTrackerLabelFacets(items, F());
assert.strictEqual(fac.needsHuman.count, 3);
assert.deepStrictEqual(fac.needsHuman.expands, ['stage/045-needs-decision', 'needs-human', 'sentinel-alert'], 'expansion lists only the member labels present, in row order');
assert.strictEqual(fac.chips[0].label, STAGE_TRACKER_NEEDS_HUMAN_FACET, 'the synthetic facet leads the row');
assert.strictEqual(fac.chips[0].text, 'Needs human (any)');
row = stageTrackerLabelsHtml(fac);
assert.ok(row.startsWith('<button type="button" class="ham-chip ham-label-chip ham-facet-chip" data-action="hamFilter" data-arg0="label" data-arg1="@needs-human" aria-pressed="false"'), row.slice(0, 300));
assert.ok(row.includes('>Needs human (any)<span class="ham-chip-n">3</span></button>'), row.slice(0, 400));
assert.ok(row.includes('title="Items carrying any needs-human label (stage/045-needs-decision, needs-human, sentinel-alert)"'), row.slice(0, 400));
modal = stageTrackerFacetModalHtml(fac, [STAGE_TRACKER_NEEDS_HUMAN_FACET], '');
assert.ok(modal.includes('>Needs human<span class="ham-facet-group-hint">expands to: stage/045-needs-decision, needs-human, sentinel-alert</span></div>'), 'group header shows the expansion: ' + modal.slice(0, 900));
assert.ok(modal.includes('data-label="@needs-human" checked'));
assert.ok(modal.includes('>Needs human (any)</span><span class="ham-chip-n">3</span>'));
assert.ok(stageTrackerFacetHit(fac.needsHuman, 'sentinel'), 'searching a member label keeps the facet row');
// Matching: the facet alone; AND with another family; OR within a family.
const nums = rows => rows.map(x => x.number).sort((a, b) => a - b);
const vis = f => nums(items.filter(x => stageTrackerMatches(x, F(f), { nowMs: Date.now() })));
assert.deepStrictEqual(vis({ labels: [STAGE_TRACKER_NEEDS_HUMAN_FACET] }), [1, 2, 3]);
assert.deepStrictEqual(vis({ labels: [STAGE_TRACKER_NEEDS_HUMAN_FACET, 'bug'] }), [1], 'facet AND a label of another family');
assert.deepStrictEqual(vis({ labels: [STAGE_TRACKER_NEEDS_HUMAN_FACET, 'stage/010-actionable'] }), [], 'facet AND an unrelated stage label');
assert.deepStrictEqual(vis({ labels: ['stage/010-actionable', 'stage/045-needs-decision'] }), [1, 4], 'two stage labels OR together');
assert.deepStrictEqual(vis({ labels: [STAGE_TRACKER_NEEDS_HUMAN_FACET], repo: 'zz/zz' }), [], 'repo filter still ANDs');
assert.deepStrictEqual(vis({ labels: ['@NEEDS-HUMAN'] }), [1, 2, 3], 'facet id normalises like a label');

// Persistence: hive.tracker.labels mirrors the selection and wins on load.
_stageTrackerFilter = F({ labels: ['bug', STAGE_TRACKER_NEEDS_HUMAN_FACET], repo: 'a/b' });
stageTrackerSaveFilter();
assert.strictEqual(store[STAGE_TRACKER_LABELS_KEY], JSON.stringify(['bug', '@needs-human']));
store[STAGE_TRACKER_LABELS_KEY] = JSON.stringify(['Role/Docs']);
stageTrackerLoadFilter();
assert.deepStrictEqual(_stageTrackerFilter.labels, ['role/docs'], 'the labels key overrides the filter blob');
assert.strictEqual(_stageTrackerFilter.repo, 'a/b', 'other filter fields come from the blob');
delete store[STAGE_TRACKER_LABELS_KEY];
stageTrackerLoadFilter();
assert.deepStrictEqual(_stageTrackerFilter.labels, ['bug', '@needs-human'], 'without the key the blob is used');
store[STAGE_TRACKER_LABELS_KEY] = '{bad json';
assert.strictEqual(stageTrackerLoadLabels(), null);
store[STAGE_TRACKER_LABELS_KEY] = '"not an array"';
assert.strictEqual(stageTrackerLoadLabels(), null);
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node label facets fixture failed: %v\n%s", err, out)
	}
}
