package dashboard

import (
	"strings"
	"testing"
)

// TestStageTrackerMergeFailedColumnNode: the operator's row
// (hivecommons/docs#496 → docs#512, 2026-10-29 11:04 ET). The lane fell
// back to 060 after the automerge lane's merge call drew GitHub's 405 "4
// of 4 required status checks are expected", and the tracker drew the amber
// "⟳ Merge failed · retrying ⬡" chip in CI / Review with the raw
// "PUT https://api.github.com/…" error spilling out of it. An actual queue
// outcome lights the Merge gate; a premature call waiting for head checks
// does not admit a 050/060 PR. The chip is a fixed short phrase
// (no URL, no verb, ≤ 5 words) with GitHub's words in the tooltip's
// Details line; pending CI is "CI running", never "red"; and the cue
// names the stage in plain words, not "stage/060-reviewed".
func TestStageTrackerMergeFailedColumnNode(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`if (mg && (mg.state === 'merging' || mg.state === 'merge-failed') && cur > STAGE_TRACKER_PR_COLUMN) return STAGE_TRACKER_MERGE_COLUMN;`,
		`const anchor = stageTrackerAnchor(cur, stages, item.via_pr ? 'pr' : item.kind, item);`,
		`stageTrackerAnchor(item.stage, stages, item.via_pr ? 'pr' : item.kind, item);`,
		`label: (typeof stageTrackerStageName === 'function' && (stageTrackerStageName(tipStage) || stageTrackerStageName(st))) || tipStage.label || st.label || String(n)`,
		`.ham-gate-detail { font-weight: normal; opacity: 0.85; white-space: nowrap; font-variant-numeric: tabular-nums; display: inline-block; vertical-align: bottom; max-width: 12em; overflow: hidden; text-overflow: ellipsis; }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing merge-outcome column wiring %q", want)
		}
	}
	runStageTrackerRowActionsScript(t, "function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_PATH")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_PR_COLUMN")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_COLUMN")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_NEEDS_DECISION")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_DECISION_COLUMN")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_NEEDED")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_POSSIBLY_EXISTS")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_GATE_WORD")+"\n"+
		jsConstBlock(t, html, "STAGE_TRACKER_GATE_REASON_WORDS")+"\n"+
		jsConstBlock(t, html, "STAGE_TRACKER_EVENT_MOD_WORDS")+"\n"+
		jsFunc(t, html, "stageTrackerAnchor")+"\n"+
		jsFunc(t, html, "stageTrackerAgoText")+"\n"+
		jsFunc(t, html, "stageTrackerSoakLeft")+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateStateOf")+"\n"+
		jsFunc(t, html, "stageTrackerShortLeft")+"\n"+
		jsFunc(t, html, "stageTrackerGateReasonText")+"\n"+
		stageTrackerMergeGateInfoJS(t, html)+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateWordHtml")+"\n"+
		jsFunc(t, html, "stageTrackerStageName")+"\n"+
		jsFunc(t, html, "stageTrackerStageFell")+"\n"+
		jsConstLine(t, html, "STAGE_TRACKER_EVENT_COVERED_BY_PR")+"\n"+
		jsFunc(t, html, "stageTrackerEventText")+"\n"+`
const assert = require('assert');
const now = Date.parse('2026-10-29T15:04:00Z');
const iso = ms => new Date(ms).toISOString();
const stages = [{ number: 50, label: 'stage/050-review-pending', short: 'CI / Review' }, { number: 60, label: 'stage/060-reviewed', short: '' }, { number: 80, label: 'stage/080-awaiting-merge', short: 'Merge gate' }];
const raw = 'PUT https://api.github.com/repos/hivecommons/docs/pulls/512/merge: 405 4 of 4 required status checks are expected. []';
const gate = (extra) => Object.assign({ state: 'merge-failed', reason: 'check-pending', sweep_running: true, merge_path: 'sweep', merge_failed: raw, merge_failed_at: iso(now - 60000), next_sweep_at: iso(now + 7000), last_sweep_at: iso(now - 22000) }, extra || {});
const row = (mg) => ({ repo: 'hivecommons/docs', number: 512, kind: 'pr', stage: 60, stage_label: 'stage/060-reviewed', reason: 'check-pending', modifiers: [], merge_gate: mg });
const words = s => s.trim().split(/\s+/).filter(w => w !== '·');
const plain = s => !/http|\bPUT\b|api\.github\.com/i.test(s);

// 1. Column: the merge outcome lights Merge gate (80), whatever stage the
//    lane fell back to, except an early refusal waiting for head checks:
//    that head is still in CI / Review, not admitted by the failed call.
assert.strictEqual(stageTrackerAnchor(60, stages, 'pr', row(gate())), 50, 'pending head checks at 060 → CI / Review');
assert.strictEqual(stageTrackerAnchor(60, stages, 'pr', row(gate({ state: 'merging', merge_failed: '' }))), STAGE_TRACKER_MERGE_COLUMN, 'merging at 060 → Merge gate');
assert.strictEqual(stageTrackerAnchor(50, stages, 'pr', row(gate())), 50, 'pending head checks at 050 → CI / Review');
assert.strictEqual(stageTrackerAnchor(60, stages, 'pr', row({ state: 'gate-pending', reason: 'check-pending' })), 50, 'no merge call: 060 stays in CI / Review');
assert.strictEqual(stageTrackerAnchor(60, stages, 'pr'), 50, 'no item: unchanged');
assert.strictEqual(stageTrackerAnchor(STAGE_TRACKER_TERMINAL, stages, 'pr', row(gate())), STAGE_TRACKER_TERMINAL);
assert.strictEqual(stageTrackerAnchor(30, stages, 'pr', row(gate())), 30, 'a stale gate on a PR back in progress does not jump it forward');
const prepared = Object.assign(row(gate({ merge_failed_kind: 'checks-pending', merge_failed_checks: 1 })), {
  stage: 50, preparation: { role: 'waiting', state: 'current', number: 16014, behind: 16007 },
});
assert.strictEqual(stageTrackerAnchor(50, stages, 'pr', prepared), 50, 'preparation is not queue admission');
assert.strictEqual(stageTrackerAnchor(100, stages, 'pr', prepared), STAGE_TRACKER_MERGE_COLUMN, 'an actual queue-stage outcome stays at the gate');

// 2. The chip for this exact payload (Go translated the 405): the sweep
//    called too early, so it is "Waiting for CI · 4 checks", not a failure.
let info = stageTrackerMergeGateInfo(row(gate({ merge_failed_kind: 'checks-pending', merge_failed_text: "4 required checks haven't finished", merge_failed_checks: 4 })), now);
assert.strictEqual(info.word, 'Waiting for CI');
assert.strictEqual(info.detail, '4 checks');
assert.strictEqual(info.cue, 'running', 'the sweep is on it');
assert.ok(info.tip.includes("before its required checks had reported — 4 required checks haven't finished"), info.tip);
assert.ok(info.tip.includes('Details: ' + raw), 'raw error only after Details: ' + info.tip);
assert.ok(!info.tip.startsWith('stage/'), info.tip);
// Older payload without the translated fields: the SPA classifies the raw text itself.
info = stageTrackerMergeGateInfo(row(gate()), now);
assert.strictEqual(info.word + ' · ' + info.detail, 'Waiting for CI · 4 checks', 'raw-only payload');
assert.ok(info.tip.includes("4 required checks haven't finished"), info.tip);

// 3. Every merge-failed chip is a fixed short phrase: no URL, no verb, ≤ 5 words.
const cases = [
  [gate(), 'Waiting for CI · 4 checks'],
  [gate({ sweep_running: false }), 'Waiting for CI · 4 checks'],
  [gate({ merge_failed: '405 Pull Request is not mergeable' }), 'Merge failed · retrying'],
  [gate({ merge_failed: '405 Pull Request is not mergeable', sweep_running: false }), 'Merge failed · branch protection'],
  [gate({ merge_failed: '409 Head branch was modified. Review and try the merge again.', sweep_running: false }), 'Merge failed · needs rebase'],
  [gate({ merge_failed: '403 Resource not accessible by integration', sweep_running: false }), 'Merge failed · not allowed'],
  [gate({ merge_failed: 'context deadline exceeded', sweep_running: false }), 'Merge failed · GitHub refused'],
  [gate({ merge_failed: 'context deadline exceeded', merge_failed_kind: 'refused', merge_failed_text: 'GitHub refused the merge', sweep_running: false }), 'Merge failed · GitHub refused'],
  [gate({ merge_failed: '405 At least 1 approving review is required', merge_failed_kind: 'review', merge_failed_text: "branch protection wants an approving review hive can't give", sweep_running: false }), 'Merge failed · review required'],
  [gate({ merge_failed: '', sweep_running: false }), 'Merge failed · GitHub refused'],
];
for (const [mg, want] of cases) {
  const i = stageTrackerMergeGateInfo(row(mg), now);
  const label = i.word + (i.detail ? ' · ' + i.detail : '');
  assert.strictEqual(label, want);
  assert.ok(plain(label), 'no raw error in the chip: ' + label);
  assert.ok(words(label).length <= 5, 'chip ≤ 5 words: ' + label);
  const html = stageTrackerMergeGateWordHtml(row(mg), now);
  assert.ok(html.includes('<span class="ham-seg-t">') && plain(html), html);
  if (mg.merge_failed) assert.ok(i.tip.includes('Details: ' + mg.merge_failed), 'raw text rides the tooltip: ' + i.tip);
}
// A retry says what GitHub said in the tooltip.
info = stageTrackerMergeGateInfo(row(gate({ merge_failed: '405 Pull Request is not mergeable' })), now);
assert.ok(info.tip.includes('GitHub refused — branch protection blocks it') && info.tip.includes("hive is retrying it now"), info.tip);
assert.strictEqual(info.cue, 'running');

// 4. Pending CI is not red.
assert.strictEqual(stageTrackerGateReasonText('check-pending'), 'CI running');
assert.strictEqual(stageTrackerGateReasonText('required-check-pending:build'), 'CI running (build)');
assert.strictEqual(stageTrackerGateReasonText('check-failing:unit'), 'CI failed: unit');
assert.strictEqual(stageTrackerGateReasonText('required-check-failing'), 'CI failed');

// 5. The cue names the stage in words, never by its slug.
assert.strictEqual(stageTrackerStageName({ label: 'stage/060-reviewed' }), 'Reviewed');
assert.strictEqual(stageTrackerStageName({ label: '060-reviewed', short: 'Review passed' }), 'Review passed');
assert.strictEqual(stageTrackerStageName({ label: 'Claimed' }), 'Claimed');
assert.strictEqual(stageTrackerStageName(null), '');
const snap = (stage, label, caption) => ({ stage, label, caption: caption || '', mods: [] });
assert.strictEqual(stageTrackerEventText(snap(80, 'stage/080-awaiting-merge'), snap(60, 'stage/060-reviewed', 'Review passed; waiting for merge admission — CI running'), stages), 'fell back to Reviewed — Review passed; waiting for merge admission — CI running');
assert.strictEqual(stageTrackerEventText(snap(50, ''), snap(80, ''), stages), 'moved to Merge gate', 'short word when the table has one');
console.log('ok');
`)
}
