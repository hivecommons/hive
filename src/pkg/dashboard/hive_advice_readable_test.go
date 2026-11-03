package dashboard

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hiveadvisor"
)

// The needs-human advice words each item's reason code in plain language
// (hiveadvisor.gateReasons). A code the dashboard can produce but the advisor
// has no words for still groups, under its raw reason text, which is the
// wording the advice moved away from: keep the two lists in step.
func TestHiveAdviceWordsEveryOwnershipReason(t *testing.T) {
	for code := range ownershipReasonVerbs {
		if !hiveadvisor.DescribesReasonCode(code) {
			t.Errorf("ownership reason %q has no plain-language wording in hiveadvisor.gateReasons", code)
		}
	}
}

// Execute the shipped card renderer: the advice reads as headline, why the
// items wait, how to help, one item per line, then the full list, with the
// raw signals folded away.
func TestHiveAdviceCardReadsAsWhyHelpAndItems(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: hive advice card rendering was not executed")
	}
	html := indexHTML(t)
	var source strings.Builder
	source.WriteString(`const assert = require('node:assert/strict');
const nodes = { 'hive-advice-section': { style: {} }, 'hive-advice-card': { innerHTML: '' } };
const document = { getElementById: (id) => nodes[id] || null };
let _lastHiveAdvice = null;
function refreshSectionCardShell() {}
function setSectionSummary() {}
function applySectionCollapse() {}
function formatDashboardDate() { return '11/2'; }
`)
	for _, name := range []string{"escapeHtml", "renderHiveAdvice"} {
		source.WriteString(jsFunc(t, html, name))
		source.WriteByte('\n')
	}
	source.WriteString(`renderHiveAdvice({
  epoch: { mode: 'BUSY', end: '2026-11-02T00:00:00Z' },
  nextReviewInDays: 6,
  recommendations: [{
    id: 'clear-human-gate-prs', title: 'Clear the human gate on PRs',
    rationale: '59 of 94 open PRs (62%) are waiting on a person; the hive cannot move them on its own.',
    why: ['24 parked until a maintainer decides (20 in utah-packages, 3 in utah, 1 in testsuite).', '21 waiting for an approving review that branch protection requires (16 in common, 5 in actions).'],
    help: 'answer the parked decisions in utah-packages and utah; approve PRs in common and actions',
    items: [{ repo: 'actions', number: 675, url: 'https://github.com/projectbluefin/actions/pull/675', note: "REVIEW — Hive's review is parked for a person" }, { repo: 'actions', number: 544, note: 'REVIEW — Waiting on a person\'s review: a reviewer requested changes' }],
    links: [{ label: 'Your-turn PRs (CSV)', url: '/api/overview/prs.csv?slice=your-turn' }],
    signals: [{ name: 'needs_human', value: '59' }, { name: 'verbs', value: '1 INSPECT, 24 DECIDE, 34 REVIEW' }],
  }, { id: 'widen-hive-repos', title: 'Widen HIVE_REPOS', rationale: 'No longer applies.', cleared: true }],
});
const out = nodes['hive-advice-card'].innerHTML;
const order = ['59 of 94 open PRs', 'Why they wait', '24 parked until a maintainer decides', 'How to help:', 'First', 'actions#675', 'Full list:', 'Numbers behind this'];
let at = -1;
for (const want of order) {
  const i = out.indexOf(want, at + 1);
  assert.ok(i > at, 'expected ' + JSON.stringify(want) + ' after position ' + at + ' in:\n' + out);
  at = i;
}
assert.match(out, /<ul class="hive-advice-why"><li>24 parked until a maintainer decides[^<]*<\/li><li>21 waiting for an approving review/);
assert.match(out, /<strong>How to help:<\/strong> answer the parked decisions in utah-packages and utah; approve PRs in common and actions\.<\/div>/);
assert.match(out, /<ul class="hive-advice-items"><li><a href="https:\/\/github\.com\/projectbluefin\/actions\/pull\/675"[^>]*>actions#675<\/a> — REVIEW — Hive&#39;s review is parked for a person<\/li><li>actions#544 — /);
assert.match(out, /<details class="hive-advice-signals"><summary>Numbers behind this<\/summary>needs_human=59, verbs=1 INSPECT, 24 DECIDE, 34 REVIEW<\/details>/);
assert.ok(!out.includes('First: '), 'items are a list, not a comma run');
assert.ok(!/class="crit-cmp">signals:/.test(out), 'raw signals are folded away');
assert.match(out, /<s>Widen HIVE_REPOS<\/s>/);
assert.equal(out.match(/hive-advice-why/g).length, 1, 'only the needs-human advice carries a why list');
`)
	if out, err := exec.Command(node, "-e", source.String()).CombinedOutput(); err != nil {
		t.Fatalf("hive advice card renderer failed: %v\n%s", err, out)
	}
}

// The header says in plain words that the list is picked once a week and the
// numbers under it are not, instead of "Frozen for BUSY; next review in 7
// days. Numbers refresh every cycle.", and the header badge reads "picked
// weekly" with the same explanation on hover.
func TestHiveAdviceHeaderSaysWhenTheListWasPicked(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: hive advice card rendering was not executed")
	}
	html := indexHTML(t)
	if got := strings.Count(html, `">picked weekly</span>`); got != 2 {
		t.Errorf("the Hive Advice header badge reads \"picked weekly\" in %d places, want 2 (section header and card config)", got)
	}
	if strings.Contains(html, `">weekly</span>`) {
		t.Error("a Hive Advice badge still reads just \"weekly\"")
	}
	if !strings.Contains(html, `class="dash-card-badge" title="Hive picks this list once a week`) {
		t.Error("the Hive Advice card badge does not explain \"picked weekly\" on hover")
	}
	var source strings.Builder
	source.WriteString(`const assert = require('node:assert/strict');
const nodes = { 'hive-advice-section': { style: {} }, 'hive-advice-card': { innerHTML: '' } };
const document = { getElementById: (id) => nodes[id] || null };
let _lastHiveAdvice = null;
const summaries = {};
function refreshSectionCardShell() {}
function setSectionSummary(id, text) { summaries[id] = text; }
function applySectionCollapse() {}
function formatDashboardDate(v) { const d = new Date(v); return (d.getUTCMonth() + 1) + '/' + d.getUTCDate(); }
`)
	for _, name := range []string{"escapeHtml", "renderHiveAdvice"} {
		source.WriteString(jsFunc(t, html, name))
		source.WriteByte('\n')
	}
	source.WriteString(`const rec = { id: 'rebalance-merge-side', title: 'Rebalance toward merge-side agents', rationale: 'The hive is busy.' };
renderHiveAdvice({ epoch: { mode: 'BUSY', start: '2026-10-26T17:27:39Z', end: '2026-11-02T17:27:39Z' }, nextReviewInDays: 7, recommendations: [rec, rec, rec] });
let out = nodes['hive-advice-card'].innerHTML;
assert.ok(out.includes("Hive picked this list on 10/26 and keeps it until 11/2, so it does not jump around. The numbers under each item update every few minutes. If Hive's mode changes from BUSY, it picks a new list sooner."), out);
assert.ok(!/Frozen for|next review in|refresh every cycle/.test(out), 'old wording:\n' + out);
assert.equal(summaries['advisory-section'], '3 advice items this week');

renderHiveAdvice({ epoch: { mode: 'IDLE', end: '2026-11-02T17:27:39Z' }, recommendations: [rec] });
out = nodes['hive-advice-card'].innerHTML;
assert.ok(out.includes('Hive keeps this list until 11/2, so it does not jump around.'), out);
assert.equal(summaries['advisory-section'], '1 advice item this week');

renderHiveAdvice({ recommendations: [rec] });
assert.ok(nodes['hive-advice-card'].innerHTML.includes('Hive keeps this list for a week, so it does not jump around.'));
`)
	if out, err := exec.Command(node, "-e", source.String()).CombinedOutput(); err != nil {
		t.Fatalf("hive advice header renderer failed: %v\n%s", err, out)
	}
}
