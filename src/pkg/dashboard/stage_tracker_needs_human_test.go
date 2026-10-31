package dashboard

import "testing"

// hivecommons/hive#15008: the "Needs human" chip count and the filter share
// stageTrackerNeedsHuman — only rows a person alone can advance qualify;
// hold is "On hold", and 040 "PR in progress" / stalled / blocked /
// ready-for-automerge rows never count.
func TestStageTrackerNeedsHuman(t *testing.T) {
	cases := []struct {
		name             string
		it               StageTrackerItem
		needsHuman, hold bool
	}{
		{"040 covered, no mods (#14976)", StageTrackerItem{Stage: 40, Modifiers: []string{}}, false, false},
		{"015 triage", StageTrackerItem{Stage: 15}, true, false},
		{"045 needs decision", StageTrackerItem{Stage: 45}, true, false},
		{"085 needs decision", StageTrackerItem{Stage: 85}, true, false},
		{"090 human merge", StageTrackerItem{Stage: 90}, true, false},
		{"071 human author", StageTrackerItem{Stage: 71, AuthorKind: authorKindHuman}, true, false},
		{"071 clanker author", StageTrackerItem{Stage: 71, AuthorKind: "clanker"}, false, false},
		{"072 human author", StageTrackerItem{Stage: 72, AuthorKind: authorKindHuman}, true, false},
		// hivecommons/hive#16021: the author pushed; CI / the re-review for
		// the new head are the hive's to run.
		{"071 human author who pushed", StageTrackerItem{Stage: 71, AuthorKind: authorKindHuman, Push: &StageTrackerPush{State: pushStatePushed}}, false, false},
		{"071 human author still waited on", StageTrackerItem{Stage: 71, AuthorKind: authorKindHuman, Push: &StageTrackerPush{State: pushStateWaiting}}, true, false},
		{"needs-human pill", StageTrackerItem{Stage: 20, Modifiers: []string{modNeedsHuman}}, true, false},
		{"needs-decision pill", StageTrackerItem{Stage: 50, Modifiers: []string{modNeedsDecision}}, true, false},
		{"unauthorized pill", StageTrackerItem{Stage: 50, Modifiers: []string{modUnauthorized}}, true, false},
		{"hold on 045 wins", StageTrackerItem{Stage: 45, Modifiers: []string{modHold}}, false, true},
		{"ready for auto merge + hold (#7563)", StageTrackerItem{Stage: 80, Modifiers: []string{modHold}}, false, true},
		{"stalled", StageTrackerItem{Stage: 60, Modifiers: []string{modStalled}}, false, false},
		{"blocked on pr", StageTrackerItem{Stage: 60, Modifiers: []string{"blocked-on-pr"}}, false, false},
		{"regressed handled", StageTrackerItem{Stage: 30, Modifiers: []string{"regressed"}}, false, false},
		{"merge queue", StageTrackerItem{Stage: 100}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stageTrackerNeedsHuman(c.it); got != c.needsHuman {
				t.Fatalf("needsHuman = %v, want %v", got, c.needsHuman)
			}
			if got := stageTrackerOnHold(c.it); got != c.hold {
				t.Fatalf("onHold = %v, want %v", got, c.hold)
			}
		})
	}

	items := make([]StageTrackerItem, 0, len(cases))
	for _, c := range cases {
		items = append(items, c.it)
	}
	nh, oh := stageTrackerHumanCounts(items)
	if nh != 10 || oh != 2 {
		t.Fatalf("counts = (%d, %d), want (10, 2)", nh, oh)
	}
	for i, c := range cases {
		if items[i].NeedsHuman != c.needsHuman || items[i].OnHold != c.hold {
			t.Fatalf("%s: flags (%v,%v) want (%v,%v)", c.name, items[i].NeedsHuman, items[i].OnHold, c.needsHuman, c.hold)
		}
	}
}
