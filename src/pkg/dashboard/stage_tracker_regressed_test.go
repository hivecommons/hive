package dashboard

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The regressed detail names the hop that booked the fall even when the
// item has moved on since (040 → 020 → 010 reports from 040 to 020 at the
// first fall), and falls back to the record's current stage when the
// history was trimmed away.
func TestStageTrackerRegressionDetail(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	key := stagelabels.Key("acme/console", 269)
	store.Observe(key, stagelabels.CoveredByPR, "open PR #270 covers it")
	clock = now.Add(3 * time.Minute)
	store.Observe(key, stagelabels.Claimed, "live issue claim")
	clock = now.Add(6 * time.Minute)
	store.Observe(key, stagelabels.Actionable, "actionable")
	rec, _ := store.Get(key)

	var it StageTrackerItem
	stageTrackerRegression(&it, rec)
	if it.RegressedFrom != 40 || it.RegressedTo != 20 || it.RegressedReason != "live issue claim" || it.RegressedAt == nil || !it.RegressedAt.Equal(now.Add(3*time.Minute)) {
		t.Fatalf("detail = from %d to %d at %v because %q, want 040 → 020 at +3m because the claim", it.RegressedFrom, it.RegressedTo, it.RegressedAt, it.RegressedReason)
	}

	rec.History = nil
	var trimmed StageTrackerItem
	stageTrackerRegression(&trimmed, rec)
	if trimmed.RegressedFrom != 40 || trimmed.RegressedTo != 10 || trimmed.RegressedReason != "actionable" || trimmed.RegressedAt == nil || !trimmed.RegressedAt.Equal(rec.RegressedAt) {
		t.Fatalf("trimmed detail = %+v, want the record's own stage/reason/regressed_at", trimmed)
	}

	var clean StageTrackerItem
	stageTrackerRegression(&clean, stagelabels.Record{Stage: stagelabels.Actionable})
	if clean.RegressedFrom != 0 || clean.RegressedTo != 0 || clean.RegressedAt != nil || clean.RegressedReason != "" {
		t.Fatalf("non-regressed item got detail %+v", clean)
	}
}

// The pill, the tooltip and the current-stage caption all carry the
// regression text, and the text itself reads "regressed: <from> → <to> at
// <time> because <reason>".
func TestStageTrackerRegressedPillExplainsItself(t *testing.T) {
	html := indexHTML(t)
	chips := jsFunc(t, html, "stageTrackerChipsHtml")
	if !strings.Contains(chips, "stageTrackerRegressionText(item") || !strings.Contains(chips, "title=") {
		t.Fatalf("stageTrackerChipsHtml must title the regressed pill with the regression text:\n%s", chips)
	}
	if tip := jsFunc(t, html, "stageTrackerTooltip"); !strings.Contains(tip, "stageTrackerRegressionText(item, nowMs)") {
		t.Fatalf("stageTrackerTooltip must carry the regression line:\n%s", tip)
	}
	if cap := jsFunc(t, html, "stageTrackerCurrentStageHtml"); !strings.Contains(cap, "stageTrackerRegressionText(item") {
		t.Fatalf("stageTrackerCurrentStageHtml must carry the regression text:\n%s", cap)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — regression text was NOT executed by this run")
	}
	script := strings.Join([]string{
		"const assert = require('assert');",
		"var _stageTracker = { stages: [{ number: 10, short: 'Actionable' }, { number: 20, short: 'Claimed' }, { number: 40, short: 'Covered by PR' }] };",
		"function agentNowClockTime(ms, nowMs) { return 'T' + ms; }",
		"function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }",
		jsConstLine(t, html, "STAGE_TRACKER_MOD_PRIORITY"),
		jsConstLine(t, html, "STAGE_TRACKER_MOD_TEXT"),
		jsFunc(t, html, "stageTrackerStageWord"),
		jsConstLine(t, html, "STAGE_TRACKER_NEEDS_DECISION"),
		stageTrackerManualMergeJS(t, html),
		stageTrackerSentinelJS(t, html),
		jsConstLine(t, html, "STAGE_TRACKER_ATTENTION_STAGES"),
		jsConstLine(t, html, "STAGE_TRACKER_HUMAN_MODS"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_TEXT"),
		jsConstLine(t, html, "STAGE_TRACKER_HANDLED_NOTE"),
		jsFunc(t, html, "stageTrackerRegressionHandled"),
		jsFunc(t, html, "stageTrackerRegressionText"),
		jsFunc(t, html, "stageTrackerChipsHtml"),
	}, "\n") + `
const item = { stage: 10, modifiers: ['regressed'], regressed_from: 40, regressed_to: 20, regressed_at: '2026-10-22T12:44:59Z', regressed_reason: 'live issue claim' };
const text = stageTrackerRegressionText(item, 0);
// On a hive-driven stage with no human gate the explanation ends with the "no action needed" note.
assert.strictEqual(text, 'regressed: 040 Covered by PR → 020 Claimed at T' + Date.parse(item.regressed_at) + ' because live issue claim · now 010 Actionable · ' + STAGE_TRACKER_HANDLED_NOTE);
// No hop detail: falls back to the current stage, no time, no reason.
assert.strictEqual(stageTrackerRegressionText({ stage: 20, modifiers: ['regressed'], regressed_from: 40 }, 0), 'regressed: 040 Covered by PR → 020 Claimed · ' + STAGE_TRACKER_HANDLED_NOTE);
// Unknown stage numbers still read as codes; 072 is parked on a person, so no note.
assert.strictEqual(stageTrackerRegressionText({ stage: 72, modifiers: ['regressed'], regressed_from: 100 }, 0), 'regressed: 100 → 072');
// Not regressed → nothing.
assert.strictEqual(stageTrackerRegressionText({ stage: 10, modifiers: ['stalled'], regressed_from: 40 }, 0), '');
assert.strictEqual(stageTrackerRegressionText({ stage: 10, modifiers: ['regressed'] }, 0), '');
// The pill carries the text as its title; a plain pill has none.
const pill = stageTrackerChipsHtml(item);
assert.ok(pill.includes('title="regressed: 040 Covered by PR → 020 Claimed at T'), pill);
assert.ok(pill.includes('because live issue claim'), pill);
assert.ok(!stageTrackerChipsHtml({ stage: 10, modifiers: ['stalled'] }).includes('title='));
// Hive-handled: muted "stage ↓" pill, never the red regressed look, and it says so on hover.
const handledCases = [
  { name: 'actionable', item: item },
  { name: 'claimed', item: { stage: 20, modifiers: ['regressed'], regressed_from: 30 } },
  { name: 'pr in progress', item: { stage: 30, modifiers: ['regressed'], regressed_from: 40 } },
  { name: 'draft folds in', item: { kind: 'pr', stage: 50, modifiers: ['regressed', 'draft'], regressed_from: 100 } },
];
for (const c of handledCases) {
  assert.ok(stageTrackerRegressionHandled(c.item), c.name);
  const h = stageTrackerChipsHtml(c.item);
  assert.ok(h.includes(STAGE_TRACKER_HANDLED_NOTE), c.name + ': ' + h);
  if (c.item.modifiers.length === 1) {
    assert.ok(h.includes('ham-mod-regressed ham-mod-handled') && h.includes('>' + STAGE_TRACKER_HANDLED_TEXT + '<'), c.name + ': ' + h);
  } else {
    assert.ok(!h.includes('ham-mod-regressed') && h.includes('ham-mod-draft') && h.includes(' +1<'), c.name + ' yields the lead pill: ' + h);
  }
}
// A human gate keeps the red "regressed" pill: a human-gated stage or a human-call modifier.
const redCases = [
  { name: 'needs decision stage', item: { stage: 85, modifiers: ['regressed'], regressed_from: 100 } },
  { name: 'returned to contributor', item: { stage: 72, modifiers: ['regressed'], regressed_from: 100 } },
  { name: 'needs-rebase alongside', item: { kind: 'pr', stage: 70, modifiers: ['regressed', 'needs-rebase'], regressed_from: 100 } },
  { name: 'hold alongside', item: { stage: 10, modifiers: ['regressed', 'hold'], regressed_from: 40 } },
];
for (const c of redCases) {
  assert.ok(!stageTrackerRegressionHandled(c.item), c.name);
  const h = stageTrackerChipsHtml(c.item);
  assert.ok(h.includes('ham-mod-regressed') && !h.includes('ham-mod-handled') && h.includes('>regressed'), c.name + ': ' + h);
  assert.ok(!h.includes(STAGE_TRACKER_HANDLED_NOTE), c.name + ': ' + h);
}
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
