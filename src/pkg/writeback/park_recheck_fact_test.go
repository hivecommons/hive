package writeback

import (
	"testing"
	"time"
)

// The park re-check posts one note on a parked issue when the work its park
// waits on has moved. It asks the gate as needs_decision_comment, which
// needs the review opt-in label on an issue the hive did not open. A
// contributor relay parks issues with the hive's own token, though, so the
// hive's label is already on an issue a person opened and nobody labelled:
// the note was refused on every pass and the maintainer kept deciding from a
// park reason whose blocker had merged hours before. The re-check now states
// that the park is the hive's (Item.HiveAppliedPark) and the backfill guard
// and the opt-in label stop applying to that one write.

// hiveParked is it with the re-check's finding that the hive applied the
// label its current decision park is dated from.
func hiveParked(it Item) Item {
	it.HiveAppliedPark = true
	return it
}

func TestParkRecheckFactSkipsBackfillAndOptInForAHiveAppliedPark(t *testing.T) {
	ownIssue := Item{Number: 43, Author: "hive[bot]", HiveAuthored: true, CreatedAt: t0.Add(time.Hour)}
	cases := []struct {
		name   string
		level  int
		item   Item
		kind   Kind
		allow  bool
		reason string
	}{
		{"old issue, hive applied the park, L5", 5, hiveParked(oldIssue), KindNeedsDecisionComment, true, ReasonHiveAppliedPark},
		{"old issue, hive applied the park, L6", 6, hiveParked(oldIssue), KindNeedsDecisionComment, true, ReasonHiveAppliedPark},
		{"old issue, fact not stated", 6, oldIssue, KindNeedsDecisionComment, false, ReasonBackfill},
		{"unlabelled issue, hive applied the park, L5", 5, hiveParked(newIssue), KindNeedsDecisionComment, true, ReasonHiveAppliedPark},
		{"unlabelled issue, hive applied the park, L6", 6, hiveParked(newIssue), KindNeedsDecisionComment, true, ReasonHiveAppliedPark},
		{"unlabelled issue, fact not stated, L5", 5, newIssue, KindNeedsDecisionComment, false, ReasonNeedsLabel},
		{"unlabelled issue, fact not stated, L6", 6, newIssue, KindNeedsDecisionComment, false, ReasonNeedsLabel},

		// The kind's floor is unchanged: the fact is no way round the level.
		{"below the floor, hive applied the park", 4, hiveParked(newIssue), KindNeedsDecisionComment, false, ReasonBelowLevel},
		{"below the floor, old issue", 4, hiveParked(oldIssue), KindNeedsDecisionComment, false, ReasonBelowLevel},

		// An item that was already allowed keeps the answer it had.
		{"opted-in issue, hive applied the park", 5, hiveParked(optedIssue), KindNeedsDecisionComment, true, ReasonLabelOptIn},
		{"opted-in issue, fact not stated", 5, optedIssue, KindNeedsDecisionComment, true, ReasonLabelOptIn},
		{"opted-in issue below the floor, hive applied the park", 4, hiveParked(optedIssue), KindNeedsDecisionComment, false, ReasonBelowLevel},
		{"the hive's own issue, hive applied the park", 1, hiveParked(ownIssue), KindNeedsDecisionComment, true, ReasonOwnItem},

		// The fact belongs to one kind, and the blocked repair's fact is not
		// a substitute for it.
		{"gate_comment ignores the fact", 6, hiveParked(newIssue), KindGateComment, false, ReasonNeedsLabel},
		{"stage_label ignores the fact", 6, hiveParked(newIssue), KindStageLabel, false, ReasonNeedsLabel},
		{"blocked_repair ignores the fact", 6, hiveParked(newIssue), KindBlockedRepair, false, ReasonNeedsLabel},
		{"gate_comment on an old issue ignores the fact", 6, hiveParked(oldIssue), KindGateComment, false, ReasonBackfill},
		{"the blocked repair's fact does not admit the note", 6, hiveBlocked(newIssue), KindNeedsDecisionComment, false, ReasonNeedsLabel},
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
// membership, own_only and the kind's minimum level refuse the note on a
// hive-applied park as they refuse any other write, and an operator's
// per-kind settings that already admitted the note answer as they did.
func TestParkRecheckFactStillHeldByPauseModeProjectOwnOnlyAndLevel(t *testing.T) {
	item := hiveParked(oldIssue)
	mode := func(m RepoMode) Config {
		return Config{PerRepo: map[string]RepoOverride{"org/repo": {Mode: m}}}
	}
	kind := func(ov KindPolicyOverride) Config {
		return Config{Kinds: map[string]KindPolicyOverride{"needs_decision_comment": ov}}
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
		{"repo mode labels-only: the note is a comment", mode(RepoModeLabelsOnly), 6, false, true, false, ReasonRepoLabels},
		{"repo mode full", mode(RepoModeFull), 6, false, true, true, ReasonRepoFull},
		{"repo not in the project", Config{}, 6, false, false, false, ReasonRepoNotListed},
		{"own_only set by an operator", kind(KindPolicyOverride{OwnOnly: boolp(true)}), 6, false, true, false, ReasonOwnOnly},
		{"level unset", Config{}, 0, false, true, false, ReasonLevelUnknown},
		{"below the default floor", Config{}, 4, false, true, false, ReasonBelowLevel},
		{"below a raised kind floor", kind(KindPolicyOverride{MinLevel: intp(6)}), 5, false, true, false, ReasonBelowLevel},
		{"at a raised kind floor", kind(KindPolicyOverride{MinLevel: intp(6)}), 6, false, true, true, ReasonHiveAppliedPark},
		{"at a lowered kind floor", kind(KindPolicyOverride{MinLevel: intp(3)}), 3, false, true, true, ReasonHiveAppliedPark},
		{"below the global floor", Config{Unsolicited: UnsolicitedConfig{MinLevel: intp(6)}}, 5, false, true, false, ReasonBelowLevel},
		{"require_labels off answers as before", Config{Unsolicited: UnsolicitedConfig{RequireLabels: boolp(false)}}, 5, false, true, true, ReasonLevel},
		{"unlabeled_min_level reached answers as before", kind(KindPolicyOverride{UnlabeledMinLevel: intp(6)}), 6, false, true, true, ReasonLevel},
		{"unlabeled_min_level not reached", kind(KindPolicyOverride{UnlabeledMinLevel: intp(6)}), 5, false, true, true, ReasonHiveAppliedPark},
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
			d := g.Allowed("org/repo", item, KindNeedsDecisionComment)
			if d.Allowed != tc.allow || d.Reason != tc.reason {
				t.Fatalf("got %+v, want allow=%v reason=%s", d, tc.allow, tc.reason)
			}
		})
	}
}

// Nothing but the re-check's own kind reads the fact: for every other kind,
// at every level, on an old, a new and an opted-in issue, the gate answers
// the same with it as without it.
func TestParkRecheckFactChangesNoOtherKind(t *testing.T) {
	for _, kind := range Kinds() {
		if kind == KindNeedsDecisionComment {
			continue
		}
		for level := 0; level <= 6; level++ {
			for _, it := range []Item{oldIssue, newIssue, optedIssue} {
				g, _ := newGate(t, Config{}, level)
				without := g.Decide("org/repo", it, kind)
				with := g.Decide("org/repo", hiveParked(it), kind)
				if with != without {
					t.Fatalf("%s at L%d on #%d: with the fact %+v, without %+v", kind, level, it.Number, with, without)
				}
			}
		}
	}
}

// An allowed note on an old issue is not one of the "pre-existing items left
// untouched" the backfill banner counts; a refused one still is.
func TestParkRecheckFactDoesNotCountAsBackfillSkip(t *testing.T) {
	g, _ := newGate(t, Config{}, 6)
	if d := g.Allowed("org/repo", hiveParked(oldIssue), KindNeedsDecisionComment); !d.Allowed {
		t.Fatalf("hive-applied park on an old issue = %+v, want allowed", d)
	}
	if n := g.Snapshot().Repos["org/repo"].PreExisting; n != 0 {
		t.Fatalf("pre-existing untouched = %d after an allowed note, want 0", n)
	}
	if d := g.Allowed("org/repo", oldIssue, KindNeedsDecisionComment); d.Allowed || d.Reason != ReasonBackfill {
		t.Fatalf("fact not stated = %+v, want refused by backfill", d)
	}
	if n := g.Snapshot().Repos["org/repo"].PreExisting; n != 1 {
		t.Fatalf("pre-existing untouched = %d after a backfill refusal, want 1", n)
	}
}

// The content layer still sees the note: the same body on the same issue is
// a duplicate, and the same body on a second issue inside the burst window
// is held, exactly as for a note on an opted-in issue.
func TestParkRecheckFactKeepsDuplicateAndBurstHolds(t *testing.T) {
	const note = "<!-- hive:park-recheck -->\n**The reason this issue is parked may be out of date.**"
	g, clk, _ := newDedupeGate(t, Config{}, "")
	first, second := hiveParked(oldIssue), hiveParked(newIssue)
	if d := g.AllowedPreview("org/repo", first, KindNeedsDecisionComment, note); !d.Allowed || d.Reason != ReasonHiveAppliedPark {
		t.Fatalf("first issue = %+v, want allowed", d)
	}
	if d := g.AllowedPreview("org/repo", first, KindNeedsDecisionComment, note); d.Allowed || d.Reason != ReasonDuplicate {
		t.Fatalf("same issue again = %+v, want held as a duplicate", d)
	}
	if d := g.AllowedPreview("org/repo", second, KindNeedsDecisionComment, note); d.Allowed || d.Reason != ReasonBurst {
		t.Fatalf("second issue in the burst window = %+v, want held as a burst", d)
	}
	clk.advance(2 * time.Minute)
	if d := g.AllowedPreview("org/repo", second, KindNeedsDecisionComment, note); !d.Allowed {
		t.Fatalf("second issue after the window = %+v, want allowed", d)
	}
}
