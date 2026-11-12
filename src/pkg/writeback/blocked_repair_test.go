package writeback

import (
	"testing"
	"time"
)

// The blocked sweep repairs a `blocked` label the hive applied with nothing
// naming what the issue waits on: needs-human on, blocked off, one note. It
// used to ask the gate as gate_comment, which held the repair back on every
// issue older than the hive's write access (backfill off) and on every issue
// without the opt-in label, although the hive had already labelled each one.
// blocked_repair is the kind for it: the caller states the label is the
// hive's (Item.HiveAppliedBlock) and those two checks stop applying.

var (
	// oldIssue predates the gate's first sight of org/repo (t0), the
	// backfill guard's case; newIssue is newer and carries no opt-in label.
	oldIssue = Item{Number: 40, Author: "maintainer", CreatedAt: t0.Add(-48 * time.Hour)}
	newIssue = Item{Number: 41, Author: "maintainer", CreatedAt: t0.Add(time.Hour)}
	// optedIssue carries the opt-in label.
	optedIssue = Item{Number: 42, Author: "maintainer", Labels: []string{"hive-review"}, CreatedAt: t0.Add(time.Hour)}
)

// hiveBlocked is it with the caller's finding that the hive's own bot
// applied its `blocked` label.
func hiveBlocked(it Item) Item {
	it.HiveAppliedBlock = true
	return it
}

func TestBlockedRepairKindDefaults(t *testing.T) {
	if !IsKind("blocked_repair") {
		t.Fatal("blocked_repair is not a known kind")
	}
	var c Config
	if p := c.Policy(KindBlockedRepair); p.MinLevel != 1 || !p.RequireLabel || p.UnlabeledMinLevel != 0 || p.OwnOnly {
		t.Fatalf("blocked_repair default = %+v, want L1 with the opt-in label for a call that does not state the hive applied the block", p)
	}
	if KindBlockedRepair.IsLabelKind() || KindBlockedRepair.IsIssueFileKind() {
		t.Fatal("blocked_repair posts a note on an existing issue: neither a label kind (labels-only refuses it) nor an issue-file kind")
	}
	if !KindBlockedRepair.ContentDeduped() || KindBlockedRepair.RateCapExempt() {
		t.Fatal("blocked_repair keeps the content dedupe, burst hold and hourly cap its note had under gate_comment")
	}
	listed := false
	for _, row := range c.EffectiveKinds() {
		listed = listed || row.Kind == KindBlockedRepair
	}
	if !listed {
		t.Fatal("blocked_repair missing from EffectiveKinds (the Settings kind table)")
	}
	if err := (Config{Kinds: map[string]KindPolicyOverride{"blocked_repair": {MinLevel: intp(3)}}}).Validate(); err != nil {
		t.Fatalf("writeback.kinds.blocked_repair override refused: %v", err)
	}
}

// With the fact stated, neither the backfill guard nor the opt-in label
// holds the repair back. Without it the kind is gated like gate_comment for
// the same item, and the fact does nothing for any other kind.
func TestBlockedRepairSkipsBackfillAndOptInOnlyForAHiveAppliedBlock(t *testing.T) {
	cases := []struct {
		name   string
		level  int
		item   Item
		kind   Kind
		allow  bool
		reason string
	}{
		{"old issue, repair, hive applied the block", 6, hiveBlocked(oldIssue), KindBlockedRepair, true, ReasonHiveAppliedBlock},
		{"old issue, repair, fact not stated", 6, oldIssue, KindBlockedRepair, false, ReasonBackfill},
		{"old issue, gate_comment", 6, oldIssue, KindGateComment, false, ReasonBackfill},
		{"old issue, gate_comment ignores the fact", 6, hiveBlocked(oldIssue), KindGateComment, false, ReasonBackfill},

		{"unlabelled issue, repair, hive applied the block", 6, hiveBlocked(newIssue), KindBlockedRepair, true, ReasonHiveAppliedBlock},
		{"unlabelled issue, repair, fact not stated", 6, newIssue, KindBlockedRepair, false, ReasonNeedsLabel},
		{"unlabelled issue, gate_comment", 6, newIssue, KindGateComment, false, ReasonNeedsLabel},
		{"unlabelled issue, gate_comment ignores the fact", 6, hiveBlocked(newIssue), KindGateComment, false, ReasonNeedsLabel},
		{"unlabelled issue, stage_label ignores the fact", 6, hiveBlocked(newIssue), KindStageLabel, false, ReasonNeedsLabel},

		// The label is applied at any level, so the repair's floor is L1.
		{"lowest level, hive applied the block", 1, hiveBlocked(oldIssue), KindBlockedRepair, true, ReasonHiveAppliedBlock},
		{"opted-in issue, hive applied the block", 1, hiveBlocked(optedIssue), KindBlockedRepair, true, ReasonHiveAppliedBlock},
		{"opted-in issue, fact not stated", 1, optedIssue, KindBlockedRepair, true, ReasonLabelOptIn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := newGate(t, Config{}, tc.level)
			d := g.Allowed("org/repo", tc.item, tc.kind)
			if d.Allowed != tc.allow || d.Reason != tc.reason {
				t.Fatalf("got %+v, want allow=%v reason=%s", d, tc.allow, tc.reason)
			}
		})
	}
}

// The fact lifts two checks and no others: a pause, the repo mode, project
// membership, own_only and the kind's minimum level refuse a hive-applied
// block's repair as they refuse any other write.
func TestBlockedRepairStillHeldByPauseModeProjectOwnOnlyAndLevel(t *testing.T) {
	item := hiveBlocked(oldIssue)
	mode := func(m RepoMode) Config {
		return Config{PerRepo: map[string]RepoOverride{"org/repo": {Mode: m}}}
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
		{"paused repo", Config{}, 6, true, true, false, ReasonRepoPaused},
		{"repo mode off", mode(RepoModeOff), 6, false, true, false, ReasonRepoOff},
		{"repo mode issues-only", mode(RepoModeIssuesOnly), 6, false, true, false, ReasonRepoIssues},
		{"repo mode labels-only: the repair posts a note", mode(RepoModeLabelsOnly), 6, false, true, false, ReasonRepoLabels},
		{"repo mode full", mode(RepoModeFull), 6, false, true, true, ReasonRepoFull},
		{"repo not in the project", Config{}, 6, false, false, false, ReasonRepoNotListed},
		{"own_only set by an operator", Config{Kinds: map[string]KindPolicyOverride{"blocked_repair": {OwnOnly: boolp(true)}}}, 6, false, true, false, ReasonOwnOnly},
		{"level unset", Config{}, 0, false, true, false, ReasonLevelUnknown},
		{"below a raised kind floor", Config{Kinds: map[string]KindPolicyOverride{"blocked_repair": {MinLevel: intp(3)}}}, 2, false, true, false, ReasonBelowLevel},
		{"at a raised kind floor", Config{Kinds: map[string]KindPolicyOverride{"blocked_repair": {MinLevel: intp(3)}}}, 3, false, true, true, ReasonHiveAppliedBlock},
		{"below the global floor", Config{Unsolicited: UnsolicitedConfig{MinLevel: intp(5)}}, 4, false, true, false, ReasonBelowLevel},
		{"require_labels off changes nothing", Config{Unsolicited: UnsolicitedConfig{RequireLabels: boolp(false)}}, 1, false, true, true, ReasonHiveAppliedBlock},
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
			d := g.Allowed("org/repo", item, KindBlockedRepair)
			if d.Allowed != tc.allow || d.Reason != tc.reason {
				t.Fatalf("got %+v, want allow=%v reason=%s", d, tc.allow, tc.reason)
			}
		})
	}
}

// An allowed repair of an old issue is not one of the "pre-existing items
// left untouched" the backfill banner counts; a refused one still is.
func TestBlockedRepairDoesNotCountAsBackfillSkip(t *testing.T) {
	g, _ := newGate(t, Config{}, 6)
	if d := g.Allowed("org/repo", hiveBlocked(oldIssue), KindBlockedRepair); !d.Allowed {
		t.Fatalf("hive-applied block on an old issue = %+v, want allowed", d)
	}
	if n := g.Snapshot().Repos["org/repo"].PreExisting; n != 0 {
		t.Fatalf("pre-existing untouched = %d after an allowed repair, want 0", n)
	}
	if d := g.Allowed("org/repo", oldIssue, KindBlockedRepair); d.Allowed || d.Reason != ReasonBackfill {
		t.Fatalf("fact not stated = %+v, want refused by backfill", d)
	}
	if n := g.Snapshot().Repos["org/repo"].PreExisting; n != 1 {
		t.Fatalf("pre-existing untouched = %d after a backfill refusal, want 1", n)
	}
}

// The note is one constant body, so the content layer treats it as it did
// under gate_comment: the same issue twice is a duplicate, a second issue in
// the same repo within the burst window is held, and both pass later.
func TestBlockedRepairKeepsDuplicateAndBurstHolds(t *testing.T) {
	const note = "<!-- hive:unblocked -->\nNeeds a maintainer: the `blocked` label named nothing it waits on."
	g, clk, _ := newDedupeGate(t, Config{}, "")
	first, second := hiveBlocked(oldIssue), hiveBlocked(newIssue)
	if d := g.AllowedPreview("org/repo", first, KindBlockedRepair, note); !d.Allowed || d.Reason != ReasonHiveAppliedBlock {
		t.Fatalf("first issue = %+v, want allowed", d)
	}
	if d := g.AllowedPreview("org/repo", first, KindBlockedRepair, note); d.Allowed || d.Reason != ReasonDuplicate {
		t.Fatalf("same issue again = %+v, want held as a duplicate", d)
	}
	if d := g.AllowedPreview("org/repo", second, KindBlockedRepair, note); d.Allowed || d.Reason != ReasonBurst {
		t.Fatalf("second issue in the burst window = %+v, want held as a burst", d)
	}
	clk.advance(2 * time.Minute)
	if d := g.AllowedPreview("org/repo", second, KindBlockedRepair, note); !d.Allowed {
		t.Fatalf("second issue after the window = %+v, want allowed", d)
	}
}

// NoteHeld is news once per item and reason; ForgetHeld and RetainHeld make
// a later hold news again. Kinds and repos are remembered apart.
func TestHeldMemory(t *testing.T) {
	g, _ := newGate(t, Config{}, 6)
	const kind = KindBlockedRepair
	if !g.NoteHeld("Org/Repo", 80, kind, ReasonRepoPaused) {
		t.Fatal("a first hold must be news")
	}
	if g.NoteHeld("org/repo", 80, kind, ReasonRepoPaused) {
		t.Fatal("the same hold again must not be news, whatever the repo's case")
	}
	if !g.NoteHeld("org/repo", 80, kind, ReasonRepoOff) {
		t.Fatal("a changed reason must be news")
	}
	if g.NoteHeld("org/repo", 80, kind, ReasonRepoOff) {
		t.Fatal("the changed reason again must not be news")
	}
	if !g.NoteHeld("org/repo", 81, kind, ReasonRepoOff) || !g.NoteHeld("org/other", 80, kind, ReasonRepoOff) || !g.NoteHeld("org/repo", 80, KindGateComment, ReasonRepoOff) {
		t.Fatal("another issue, repo or kind is its own hold")
	}

	g.ForgetHeld("ORG/repo", 80, kind)
	if !g.NoteHeld("org/repo", 80, kind, ReasonRepoOff) {
		t.Fatal("a hold after ForgetHeld must be news again")
	}

	// Only org/repo#81 is still a target of this kind.
	g.RetainHeld(kind, func(repo string, number int) bool { return repo == "org/repo" && number == 81 })
	if g.NoteHeld("org/repo", 81, kind, ReasonRepoOff) {
		t.Fatal("RetainHeld forgot an item it was told to keep")
	}
	if !g.NoteHeld("org/repo", 80, kind, ReasonRepoOff) || !g.NoteHeld("org/other", 80, kind, ReasonRepoOff) {
		t.Fatal("RetainHeld kept an item that is no longer a target")
	}
	if g.NoteHeld("org/repo", 80, KindGateComment, ReasonRepoOff) {
		t.Fatal("RetainHeld for one kind forgot another kind's hold")
	}
	g.RetainHeld(kind, nil)
	if g.NoteHeld("org/repo", 81, kind, ReasonRepoOff) {
		t.Fatal("RetainHeld with no keep function must forget nothing")
	}

	var none *Gate
	if !none.NoteHeld("org/repo", 80, kind, ReasonRepoOff) {
		t.Fatal("a nil gate remembers nothing: every hold is news")
	}
	none.ForgetHeld("org/repo", 80, kind)
	none.RetainHeld(kind, func(string, int) bool { return false })
}
