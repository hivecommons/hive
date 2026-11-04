package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#16481: a decomposed parent at 010 whose reason carries
// the engine's "waiting on N open child issue(s)" suffix is tracking its
// children, not waiting on the scanner; the row must say so.

func TestReasonOpenChildren(t *testing.T) {
	cases := map[string]int{
		"actionable; waiting on 1 open child issue(s)": 1,
		"open PR addresses part of it (references it without a closing keyword; PR at 050-review-pending); waiting on 2 open child issue(s)": 2,
		"actionable": 0,
		"":           0,
		"actionable; waiting on x open child issue(s)": 0,
	}
	for reason, want := range cases {
		if got := stageengine.ReasonOpenChildren(reason); got != want {
			t.Errorf("ReasonOpenChildren(%q) = %d, want %d", reason, got, want)
		}
	}
}

func TestStageTrackerTrackingParentIsNotWaitingOnScanner(t *testing.T) {
	now := time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC)
	stage := stagelabels.Actionable
	it := StageTrackerItem{Kind: "issue", Number: 16242, Stage: stage.Number(), IsTracker: true,
		Children: &github.IssueChildren{Total: 4, Completed: 3},
		Reason:   "actionable; waiting on 1 open child issue(s)"}
	it.Caption, it.Actor = stageTrackerCaption(stage, it.Reason, "hanthor", it.Kind, false, stagelabels.Record{}, false, 0, "")
	stageTrackerApplyTracking(&it, stage)
	if it.OpenChildren != 1 {
		t.Fatalf("OpenChildren = %d, want 1", it.OpenChildren)
	}
	if !strings.HasPrefix(it.Caption, "Tracking — 3 of 4 children done; waiting on 1 open child issue(s)") || strings.Contains(it.Caption, "scanner to pick it up") {
		t.Fatalf("caption still blames the scanner: %q", it.Caption)
	}
	if it.Actor != stageTrackerTrackingActor {
		t.Fatalf("actor = %q, want %q", it.Actor, stageTrackerTrackingActor)
	}
	it.StageActor = stageTrackerStageActor(it, nil, now)
	if it.StageActor.Kind != StageActorNone || !strings.Contains(it.StageActor.Why, "1 open child issue(s)") || strings.Contains(it.StageActor.Why, "pick it up") {
		t.Fatalf("stage actor still waits on the scanner: %+v", it.StageActor)
	}

	// No children known: the caption still counts the open ones.
	only := StageTrackerItem{Kind: "issue", Stage: stage.Number(), Reason: "actionable; waiting on 2 open child issue(s)"}
	stageTrackerApplyTracking(&only, stage)
	if only.OpenChildren != 2 || !strings.HasPrefix(only.Caption, "Tracking; waiting on 2 open child issue(s)") {
		t.Fatalf("count-only parent: %+v", only)
	}

	// A plain 010 issue, a PR, and a 040 parent are untouched.
	for _, keep := range []struct {
		it    StageTrackerItem
		stage stagelabels.Stage
	}{
		{StageTrackerItem{Kind: "issue", Stage: stage.Number(), Reason: "actionable", Caption: "c", Actor: "scanner"}, stage},
		{StageTrackerItem{Kind: "pr", Stage: stage.Number(), Reason: "actionable; waiting on 1 open child issue(s)", Caption: "c", Actor: "scanner"}, stage},
		{StageTrackerItem{Kind: "issue", Stage: 40, Reason: "open PR covers it; waiting on 1 open child issue(s)", Caption: "c", Actor: "its PR"}, stagelabels.CoveredByPR},
	} {
		before := keep.it
		stageTrackerApplyTracking(&keep.it, keep.stage)
		if keep.it.OpenChildren != 0 || keep.it.Caption != before.Caption || keep.it.Actor != before.Actor {
			t.Fatalf("row rewritten that should not be: before %+v after %+v", before, keep.it)
		}
	}
	plain := StageTrackerItem{Kind: "issue", Stage: stage.Number(), Reason: "actionable"}
	if a := stageTrackerStageActor(plain, nil, now); a.Kind != StageActorHive || !strings.Contains(a.Why, "scanner") {
		t.Fatalf("plain 010 issue lost its scanner actor: %+v", a)
	}
}

func TestStageTrackerTrackingWordJS(t *testing.T) {
	html := indexHTML(t)
	if !strings.Contains(html, `stageTrackerTrackingWord(item)`) {
		t.Fatal("tracker-row.js does not consult stageTrackerTrackingWord for the lit 010 chevron")
	}
	runStageTrackerRowActionsScript(t, `const assert = require('assert');
`+jsConstLine(t, html, "STAGE_TRACKER_TRACKING_WORD")+"\n"+
		jsFunc(t, html, "stageTrackerTrackingWord")+`
assert.strictEqual(stageTrackerTrackingWord({ kind: 'issue', stage: 10, open_children: 1, children: { total: 4, completed: 3 } }), 'Tracking · 3/4');
assert.strictEqual(stageTrackerTrackingWord({ kind: 'issue', stage: 10, open_children: 1 }), 'Tracking · 1 open child');
assert.strictEqual(stageTrackerTrackingWord({ kind: 'issue', stage: 10, open_children: 2 }), 'Tracking · 2 open children');
assert.strictEqual(stageTrackerTrackingWord({ kind: 'issue', stage: 10, is_tracker: true, children: { total: 6, completed: 0 } }), '');
assert.strictEqual(stageTrackerTrackingWord({ kind: 'pr', stage: 10, open_children: 1 }), '');
assert.strictEqual(stageTrackerTrackingWord(null), '');
`)
}
