package writeback

import (
	"testing"
	"time"
)

// The stage labeler states when an item's stage labels are the hive's own
// (Item.HiveAppliedStage). For stage_label that lifts the backfill guard and
// nothing else: the hive keeps its own label current on an old PR, while a
// first stage label on an old item, every other kind, and every other check
// answer as before.

var (
	// oldPR predates the gate's first sight of org/repo (t0); newPR does not.
	oldPR = Item{Number: 60, IsPR: true, Author: "contributor", Labels: []string{"stage/100-merge-queue"}, CreatedAt: t0.Add(-48 * time.Hour)}
	newPR = Item{Number: 61, IsPR: true, Author: "contributor", Labels: []string{"stage/100-merge-queue"}, CreatedAt: t0.Add(time.Hour)}
)

// hiveStaged is it with the labeler's finding that the hive applied its
// stage labels.
func hiveStaged(it Item) Item {
	it.HiveAppliedStage = true
	return it
}

func TestHiveAppliedStageLiftsOnlyTheBackfillGuard(t *testing.T) {
	labelOff := Config{Kinds: map[string]KindPolicyOverride{"stage_label": {RequireLabel: boolp(false)}}}
	full := Config{PerRepo: map[string]RepoOverride{"org/repo": {Mode: RepoModeFull}}}
	cases := []struct {
		name   string
		cfg    Config
		item   Item
		kind   Kind
		allow  bool
		reason string
	}{
		// require_label off: the level alone decides once the guard is lifted.
		{"old PR, hive stage label", labelOff, hiveStaged(oldPR), KindStageLabel, true, ReasonHiveAppliedStage},
		{"old PR, fact not stated", labelOff, oldPR, KindStageLabel, false, ReasonBackfill},
		{"old PR, a comment kind ignores the fact", labelOff, hiveStaged(oldPR), KindGateComment, false, ReasonBackfill},
		{"old PR, needs_decision_comment ignores the fact", labelOff, hiveStaged(oldPR), KindNeedsDecisionComment, false, ReasonBackfill},
		{"new PR, hive stage label: decided as before", labelOff, hiveStaged(newPR), KindStageLabel, true, ReasonLevel},
		{"new PR, fact not stated", labelOff, newPR, KindStageLabel, true, ReasonLevel},

		// The default policy still wants the opt-in label after the guard.
		{"old PR, hive stage label, no opt-in label", Config{}, hiveStaged(oldPR), KindStageLabel, false, ReasonNeedsLabel},
		{"old PR, fact not stated, default policy", Config{}, oldPR, KindStageLabel, false, ReasonBackfill},

		// Mode full allows any write the guard does not hold.
		{"old PR, hive stage label, mode full", full, hiveStaged(oldPR), KindStageLabel, true, ReasonHiveAppliedStage},
		{"old PR, fact not stated, mode full", full, oldPR, KindStageLabel, false, ReasonBackfill},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := newGate(t, tc.cfg, 6)
			d := g.Allowed("org/repo", tc.item, tc.kind)
			if d.Allowed != tc.allow || d.Reason != tc.reason {
				t.Fatalf("got %+v, want allow=%v reason=%s", d, tc.allow, tc.reason)
			}
		})
	}
}

// The fact lifts the guard and no other check: a pause, the repo mode,
// project membership, own_only and the kind's minimum level refuse the label
// write as they refuse any other.
func TestHiveAppliedStageStillHeldByPauseModeProjectOwnOnlyAndLevel(t *testing.T) {
	item := hiveStaged(oldPR)
	base := map[string]KindPolicyOverride{"stage_label": {RequireLabel: boolp(false)}}
	mode := func(m RepoMode) Config {
		return Config{Kinds: base, PerRepo: map[string]RepoOverride{"org/repo": {Mode: m}}}
	}
	cases := []struct {
		name   string
		cfg    Config
		level  int
		paused bool
		listed bool
		allow  bool
		reason string
	}{
		{"paused repo", Config{Kinds: base}, 6, true, true, false, ReasonRepoPaused},
		{"repo mode off", mode(RepoModeOff), 6, false, true, false, ReasonRepoOff},
		{"repo mode issues-only", mode(RepoModeIssuesOnly), 6, false, true, false, ReasonRepoIssues},
		{"repo mode labels-only", mode(RepoModeLabelsOnly), 6, false, true, true, ReasonHiveAppliedStage},
		{"repo not in the project", Config{Kinds: base}, 6, false, false, false, ReasonRepoNotListed},
		{"own_only set by an operator", Config{Kinds: map[string]KindPolicyOverride{"stage_label": {RequireLabel: boolp(false), OwnOnly: boolp(true)}}}, 6, false, true, false, ReasonOwnOnly},
		{"level unset", Config{Kinds: base}, 0, false, true, false, ReasonLevelUnknown},
		{"below the kind's floor", Config{Kinds: base}, 4, false, true, false, ReasonBelowLevel},
		{"at the kind's floor", Config{Kinds: base}, 5, false, true, true, ReasonHiveAppliedStage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := New(tc.cfg, Deps{
				LevelFor:   func(string) int { return tc.level },
				RepoListed: func(string) bool { return tc.listed },
				RepoPause:  func(string) (RepoPauseInfo, bool) { return RepoPauseInfo{By: "owner"}, tc.paused },
				Now:        func() time.Time { return t0 },
			}, "")
			g.ObserveRepo("org/repo")
			d := g.Allowed("org/repo", item, KindStageLabel)
			if d.Allowed != tc.allow || d.Reason != tc.reason {
				t.Fatalf("got %+v, want allow=%v reason=%s", d, tc.allow, tc.reason)
			}
		})
	}
}

// A stage write the fact let through is not one of the "pre-existing items
// left untouched" the backfill banner counts; a refused one still is. Asking
// BackfillHolds records nothing.
func TestHiveAppliedStageDoesNotCountAsBackfillSkip(t *testing.T) {
	cfg := Config{Kinds: map[string]KindPolicyOverride{"stage_label": {RequireLabel: boolp(false)}}}
	g, _ := newGate(t, cfg, 6)
	if !g.BackfillHolds("org/repo", oldPR) || g.BackfillHolds("org/repo", newPR) {
		t.Fatal("BackfillHolds: want true for the old PR only")
	}
	opted := oldPR
	opted.Labels = append([]string{"hive-review"}, oldPR.Labels...)
	if g.BackfillHolds("org/repo", opted) {
		t.Fatal("BackfillHolds on an opted-in item: the guard never applies to it")
	}
	if n := g.Snapshot().Repos["org/repo"].PreExisting; n != 0 {
		t.Fatalf("pre-existing untouched = %d after BackfillHolds, want 0", n)
	}
	if d := g.Allowed("org/repo", hiveStaged(oldPR), KindStageLabel); !d.Allowed || d.Reason != ReasonHiveAppliedStage {
		t.Fatalf("hive stage label on an old PR = %+v, want allowed", d)
	}
	if n := g.Snapshot().Repos["org/repo"].PreExisting; n != 0 {
		t.Fatalf("pre-existing untouched = %d after an allowed stage move, want 0", n)
	}
	if d := g.Allowed("org/repo", oldPR, KindStageLabel); d.Allowed || d.Reason != ReasonBackfill {
		t.Fatalf("fact not stated = %+v, want refused by backfill", d)
	}
	if n := g.Snapshot().Repos["org/repo"].PreExisting; n != 1 {
		t.Fatalf("pre-existing untouched = %d after a backfill refusal, want 1", n)
	}
}

func TestBackfillHoldsNilGate(t *testing.T) {
	var g *Gate
	if g.BackfillHolds("org/repo", oldPR) {
		t.Fatal("a nil gate holds nothing")
	}
}
