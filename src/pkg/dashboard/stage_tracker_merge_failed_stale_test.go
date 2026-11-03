package dashboard

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestStageTrackerMergeFailedScopedToHead: hive#15574 read "⏱ Merge failed
// · branch protection" while its caption said the review swarm was waiting
// on CI for the new head. The sweep's merge call on the old head drew
// GitHub's refusal, the hive merged v5 in (new head), the record regressed
// 080 → 050 required-check-pending:build-and-test, and the chevron kept the
// previous head's word. A refusal is scoped to the head it failed on: once
// the head moves, or the record falls below 080 to wait on CI after the
// refusal, the chevron reads the live stage and the refusal survives only
// as prior_head_merge_failed* hover history.
func TestStageTrackerMergeFailedScopedToHead(t *testing.T) {
	ended := time.Date(2026, 11, 2, 14, 0, 0, 0, time.UTC)
	refused := errors.New("PUT https://api.github.com/repos/hivecommons/hive/pulls/15574/merge: 405 Pull Request is not mergeable []")
	failed := github.MergeAttempt{Repo: "hivecommons/hive", Number: 15574, Path: github.PRAuditPathSweep, Head: "aaaaaaa1", StartedAt: ended.Add(-2 * time.Second), EndedAt: ended, Error: refused.Error()}
	regressed := stagelabels.Record{Stage: stagelabels.ReviewPending, Reason: "required-check-pending:build-and-test", EnteredAt: ended.Add(time.Minute), RegressedFrom: stagelabels.AwaitingMergeAdmission, RegressedAt: ended.Add(time.Minute)}

	// Head moved (merge-from-base → 92d4cefc): no merge-failed word.
	mg := stageTrackerMergeGateAttempt(nil, failed, true, "92d4cefc", stagelabels.ReviewPending, regressed, true)
	if mg == nil || mg.State == MergeGateStateMergeFailed || mg.MergeFailed != "" {
		t.Fatalf("refusal on head aaaaaaa1 must not be the word on head 92d4cefc: %+v", mg)
	}
	if mg.PriorHeadMergeFailed != refused.Error() || mg.PriorHeadMergeFailedKind != MergeRefusalProtection || mg.PriorHeadMergeFailedHead != "aaaaaaa1" || mg.PriorHeadMergeFailedAt == nil || !mg.PriorHeadMergeFailedAt.Equal(ended) {
		t.Errorf("the refusal must survive as prior-head history: %+v", mg)
	}

	// Same head, but the record regressed to 050 waiting on CI after the
	// refusal (older attempts carry no head): still history, not the word.
	sameHead := failed
	sameHead.Head = ""
	if mg = stageTrackerMergeGateAttempt(nil, sameHead, true, "aaaaaaa1", stagelabels.ReviewPending, regressed, true); mg == nil || mg.State == MergeGateStateMergeFailed || mg.PriorHeadMergeFailedKind != MergeRefusalProtection {
		t.Errorf("050 required-check-pending after the refusal must demote it to history: %+v", mg)
	}

	// Control: same head, record still at 080 → the refusal is live.
	live := stagelabels.Record{Stage: stagelabels.AwaitingMergeAdmission, Reason: "", EnteredAt: ended.Add(-time.Hour)}
	if mg = stageTrackerMergeGateAttempt(nil, failed, true, "aaaaaaa1", stagelabels.AwaitingMergeAdmission, live, true); mg == nil || mg.State != MergeGateStateMergeFailed || mg.MergeFailedKind != MergeRefusalProtection || mg.PriorHeadMergeFailed != "" {
		t.Errorf("a refusal on the current head at 080 is still the word: %+v", mg)
	}
	// Control: regressed to 050 before the refusal (the refusal is newer) → live.
	before := regressed
	before.EnteredAt, before.RegressedAt = ended.Add(-time.Minute), ended.Add(-time.Minute)
	if mg = stageTrackerMergeGateAttempt(nil, sameHead, true, "aaaaaaa1", stagelabels.ReviewPending, before, true); mg == nil || mg.State != MergeGateStateMergeFailed {
		t.Errorf("a refusal newer than the regression is still the word: %+v", mg)
	}
	// Control: regressed for a non-CI reason (a human asked for changes) → live.
	changes := regressed
	changes.Reason = "changes-requested"
	if mg = stageTrackerMergeGateAttempt(nil, sameHead, true, "aaaaaaa1", stagelabels.ReviewPending, changes, true); mg == nil || mg.State != MergeGateStateMergeFailed {
		t.Errorf("a non-CI regression does not hide the refusal: %+v", mg)
	}

	for reason, want := range map[string]bool{"required-check-pending:build-and-test": true, "required-check-pending": true, "check-pending": true, "changes-requested": false, "": false} {
		if got := stageTrackerCIPendingReason(reason); got != want {
			t.Errorf("stageTrackerCIPendingReason(%q) = %v, want %v", reason, got, want)
		}
	}
}

// TestStageTrackerMergeFailedScopedToHeadNode: the SPA side of hive#15574.
// A payload whose merge_gate carries only prior_head_merge_failed* (state
// empty, stage 050) draws the 050 word with no "Merge failed", and the
// hover note reads "merge attempt on previous head aaaaaaa failed: branch
// protection".
func TestStageTrackerMergeFailedScopedToHeadNode(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`stageTrackerPriorHeadMergeFailedNote(item) : '';`,
		`if (priorNote) tip += ' — ' + priorNote;`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing prior-head hover wiring %q", want)
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
		jsFunc(t, html, "stageTrackerAnchor")+"\n"+
		jsFunc(t, html, "stageTrackerAgoText")+"\n"+
		jsFunc(t, html, "stageTrackerSoakLeft")+"\n"+
		jsFunc(t, html, "stageTrackerMergeGateStateOf")+"\n"+
		jsFunc(t, html, "stageTrackerShortLeft")+"\n"+
		jsFunc(t, html, "stageTrackerGateReasonText")+"\n"+
		stageTrackerMergeGateInfoJS(t, html)+"\n"+
		jsFunc(t, html, "stageTrackerPriorHeadMergeFailedNote")+"\n"+`
const assert = require('assert');
const now = Date.parse('2026-11-02T14:05:00Z');
const stages = [{ number: 50, label: 'stage/050-review-pending', short: 'CI / Review' }, { number: 80, label: 'stage/080-awaiting-merge', short: 'Merge gate' }];
const raw = 'PUT https://api.github.com/repos/hivecommons/hive/pulls/15574/merge: 405 Pull Request is not mergeable []';
const prior = { prior_head_merge_failed: raw, prior_head_merge_failed_kind: 'protection', prior_head_merge_failed_text: 'branch protection blocks it', prior_head_merge_failed_at: new Date(now - 300000).toISOString(), prior_head_merge_failed_head: 'aaaaaaa1deadbeef' };
const row = (mg) => ({ repo: 'hivecommons/hive', number: 15574, kind: 'pr', stage: 50, stage_label: 'stage/050-review-pending', reason: 'required-check-pending:build-and-test', modifiers: [], merge_gate: mg });

// Record with merge_failed at head A + regression to 050 on head B: the
// gate carries no state, so no gate word (the chevron reads the 050 word)
// and the row stays in CI / Review rather than lighting the Merge gate.
assert.strictEqual(stageTrackerMergeGateStateOf(row(prior)), '', 'no live gate state at 050');
assert.strictEqual(stageTrackerMergeGateInfo(row(prior), now), null, 'no "Merge failed" word from a prior-head refusal');
assert.strictEqual(stageTrackerAnchor(50, stages, 'pr', row(prior)), 50, 'stays in CI / Review');

// The hover keeps the history, in plain words with the short head.
const note = stageTrackerPriorHeadMergeFailedNote(row(prior));
assert.strictEqual(note, 'earlier merge attempt on head aaaaaaa failed: branch protection');
assert.ok(!/http|api\.github\.com/.test(note), note);
// Raw-only payload (no kind): the SPA classifies the text itself.
assert.strictEqual(stageTrackerPriorHeadMergeFailedNote(row({ prior_head_merge_failed: raw })), 'earlier merge attempt failed: branch protection');
assert.strictEqual(stageTrackerPriorHeadMergeFailedNote(row({ prior_head_merge_failed: '405 1 of 3 required status checks are expected.' })), 'earlier merge attempt failed: required checks had not reported');
// Nothing to say without a prior refusal.
assert.strictEqual(stageTrackerPriorHeadMergeFailedNote(row({ state: 'gate-pending' })), '');
assert.strictEqual(stageTrackerPriorHeadMergeFailedNote({}), '');

// A live refusal on the current head still reads "Merge failed".
const liveInfo = stageTrackerMergeGateInfo(row({ state: 'merge-failed', merge_failed: raw, merge_failed_kind: 'protection', merge_failed_text: 'branch protection blocks it', merge_failed_at: new Date(now - 9000).toISOString(), sweep_running: false }), now);
assert.strictEqual(liveInfo.word + ' · ' + liveInfo.detail, 'Merge failed · branch protection');
`)
}
