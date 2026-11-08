package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestStageTrackerPhoneAttentionFirstJS drives the phone ordering and the
// count line under node: with opts.phone the attention tier leads the whole
// list (ahead of the stage column), without it the order is the desktop one,
// and the stored preference defaults on and survives normalisation.
func TestStageTrackerPhoneAttentionFirstJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — phone attention-first order was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const window = {};",
		jsConstLine(t, html, "STAGE_TRACKER_PAGE_SIZES"),
		jsConstLine(t, html, "STAGE_TRACKER_DEFAULT_PAGE_SIZE"),
		jsConstLine(t, html, "STAGE_TRACKER_AGE_MS"),
		jsConstLine(t, html, "STAGE_TRACKER_ATTENTION_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_SORTS"),
		jsConstLine(t, html, "STAGE_TRACKER_TERMINAL"),
		jsConstLine(t, html, "STAGE_TRACKER_PATH"),
		jsConstLine(t, html, "STAGE_TRACKER_PR_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_MERGE_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_NEEDS_DECISION"),
		jsConstLine(t, html, "STAGE_TRACKER_ISSUE_DECISION_COLUMN"),
		jsConstLine(t, html, "STAGE_TRACKER_TRIAGE_NEEDED"),
		jsConstLine(t, html, "STAGE_TRACKER_POSSIBLY_EXISTS"),
		jsConstLine(t, html, "STAGE_TRACKER_OPEN_FOR_DISCUSSION"),
		jsConstLine(t, html, "STAGE_TRACKER_PRE_MERGE_SOAK"),
		jsConstLine(t, html, "STAGE_TRACKER_HUMAN_MODS"),
		stageTrackerTierJS(t, html),
		jsFunc(t, html, "stageTrackerDefaultFilter"),
		jsFunc(t, html, "stageTrackerAnchor"),
		jsFunc(t, html, "stageTrackerLinkedDrawnHere"),
		jsFunc(t, html, "stageTrackerLinkedSuperseder"),
		jsFunc(t, html, "stageTrackerLinkedSupersededGone"),
		jsFunc(t, html, "stageTrackerLinkedSupersededTip"),
		jsFunc(t, html, "stageTrackerLinkedSorted"),
		jsConstLine(t, html, "STAGE_TRACKER_COVERED_BY_PR"),
		jsFunc(t, html, "stageTrackerLeadStage"),
		jsFunc(t, html, "stageTrackerLead"),
		jsFunc(t, html, "stageTrackerSortKey"),
		jsFunc(t, html, "stageTrackerRegressionHandled"),
		jsFunc(t, html, "stageTrackerNeedsAttention"),
		jsFunc(t, html, "stageTrackerWaitSince"),
		jsConstBlock(t, html, "STAGE_TRACKER_ICONS"),
		stageTrackerActedJS(t, html),
		jsFunc(t, html, "stageTrackerNormalizeFilter"),
		jsFunc(t, html, "stageTrackerSort"),
		jsConstLine(t, html, "STAGE_TRACKER_SHEET_QUERY"),
		jsFunc(t, html, "stageTrackerIsPhoneSheet"),
		jsFunc(t, html, "stageTrackerPhone"),
		jsFunc(t, html, "stageTrackerAttentionLineHtml"),
	}, "\n") + `
const assert = require('assert');
const now = Date.parse('2026-01-01T12:00:00Z');
const H = 3600000;
const ago = (ms) => new Date(now - ms).toISOString();
const stages = [{ number: 50, label: 'Review pending' }, { number: 80, label: 'Merge gate' }, { number: 85, label: 'Needs decision' }, { number: 110, label: 'Merged' }];
const row = (number, over) => Object.assign({ kind: 'pr', repo: 'a/b', number, stage: 50, modifiers: [], created_at: ago(number * H), stage_since: ago(number * H) }, over);
const items = [
  row(1, { stage: 80 }),
  row(2, { stage: 80, modifiers: ['stalled'] }),
  row(3, {}),
  row(4, { modifiers: ['stalled'] }),
  row(5, { modifiers: ['hold'] }),
];
const nums = (rows) => rows.map(it => it.number);
const F = (over) => Object.assign(stageTrackerDefaultFilter(), over);

assert.strictEqual(stageTrackerDefaultFilter().attentionFirst, true);
assert.strictEqual(stageTrackerNormalizeFilter({ attentionFirst: false }).attentionFirst, false);
assert.strictEqual(stageTrackerNormalizeFilter({ attentionFirst: 'x' }).attentionFirst, true);

// Desktop: column first, tier within it.
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({}))), [2, 1, 4, 3, 5]);
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({}), { phone: false })), [2, 1, 4, 3, 5]);
// Phone: every attention row precedes every other row; hold still sinks.
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({}), { phone: true })), [2, 4, 1, 3, 5]);
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'waiting' }), { phone: true })), [4, 2, 3, 1, 5]);
// The preference off keeps the desktop order on a phone; newest is untouched.
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ attentionFirst: false }), { phone: true })), [2, 1, 4, 3, 5]);
assert.deepStrictEqual(nums(stageTrackerSort(items, stages, F({ sort: 'number' }), { phone: true })), [5, 4, 3, 2, 1]);

const line = stageTrackerAttentionLineHtml(5, 2, false);
assert.ok(line.includes('5 waiting on you · 2 need attention'), line);
assert.ok(line.includes('data-action="hamFilter"') && line.includes('data-arg0="waitingOnYou"') && line.includes('data-arg1="toggle"'), line);
assert.ok(line.includes('aria-pressed="false"') && stageTrackerAttentionLineHtml(5, 2, true).includes('aria-pressed="true"'));
assert.strictEqual(stageTrackerAttentionLineHtml(0, 0, false), '');
assert.strictEqual(stageTrackerPhone(), false, 'no matchMedia means desktop');
console.log('ok');
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("node phone attention-first fixture failed: %v\n%s", err, out)
	}
}
