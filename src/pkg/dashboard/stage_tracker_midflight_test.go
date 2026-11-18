package dashboard

import (
	"strings"
	"testing"
)

const midFlightTestHead = "abc1234def5678"

func midFlightPR(mods ...func(*StageTrackerItem)) StageTrackerItem {
	it := StageTrackerItem{Repo: "acme/api", Number: 7, Kind: "pr", Stage: 85, Head: midFlightTestHead, Modifiers: []string{modNeedsDecision},
		Reason: "contributor-lane:no-linked-issue"}
	for _, m := range mods {
		m(&it)
	}
	return it
}

// hivecommons/hive#18889: a PR is mid-flight while the CURRENT head's CI
// runs or its review is queued or unfinished, or while the CI / review on
// file belong to an earlier head.
func TestStageTrackerMidFlightFor(t *testing.T) {
	ciRunning := &StageTrackerActivity{Head: midFlightTestHead, State: "running", ChecksPassed: 2, ChecksTotal: 5, ChecksPending: []string{"a", "b", "c"}}
	cases := []struct {
		name     string
		item     StageTrackerItem
		wantKind string
		wantText string
	}{
		{"CI running on the current head", midFlightPR(func(it *StageTrackerItem) { it.CIActivity = ciRunning }), midFlightKindCI, "CI running 2/5"},
		{"CI queued, no tally", midFlightPR(func(it *StageTrackerItem) {
			it.CIActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "queued"}
		}), midFlightKindCI, "CI running"},
		{"required checks not reported", midFlightPR(func(it *StageTrackerItem) {
			it.CIActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "missing", ChecksPending: []string{"build"}}
		}), midFlightKindCI, "CI running"},
		{"review queued", midFlightPR(func(it *StageTrackerItem) {
			it.ReviewActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "unassigned", PerspectivesDone: 0, PerspectivesTotal: 5}
		}), midFlightKindReview, "review queued on abc1234 (0/5)"},
		{"review being worked", midFlightPR(func(it *StageTrackerItem) {
			it.ReviewActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "reviewing", PerspectivesDone: 2, PerspectivesTotal: 5}
		}), midFlightKindReview, "review in progress on abc1234 (2/5)"},
		{"CI and review both", midFlightPR(func(it *StageTrackerItem) {
			it.CIActivity = ciRunning
			it.ReviewActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "assigned"}
		}), midFlightKindCI, "CI running 2/5 · review queued on abc1234"},
		{"stale park: the review on file is for an earlier head", midFlightPR(func(it *StageTrackerItem) {
			it.ReviewActivity = &StageTrackerActivity{Head: "0ld0ld0ld0ld", State: "human_decision"}
			it.CIActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "passing"}
		}), midFlightKindHead, "review not yet run on abc1234"},
		{"stale park: CI and review both on file for an earlier head", midFlightPR(func(it *StageTrackerItem) {
			it.ReviewActivity = &StageTrackerActivity{Head: "0ld0ld0ld0ld", State: "approved"}
			it.CIActivity = &StageTrackerActivity{Head: "0ld0ld0ld0ld", State: "passing"}
		}), midFlightKindHead, "CI and review not yet run on abc1234"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mf := stageTrackerMidFlightFor(tc.item)
			if mf == nil {
				t.Fatalf("want mid-flight %q, got nil", tc.wantText)
			}
			if mf.Kind != tc.wantKind || !strings.Contains(mf.Text, tc.wantText) {
				t.Fatalf("mid-flight = %+v, want kind %s text containing %q", mf, tc.wantKind, tc.wantText)
			}
			if mf.Head != midFlightTestHead {
				t.Fatalf("head = %q", mf.Head)
			}
			if mf.Park != "it links no issue, so the hive cannot tell which claim it fulfils and does not trust its author yet" {
				t.Fatalf("a parked PR names why it was parked: %q", mf.Park)
			}
		})
	}
}

func TestStageTrackerMidFlightSettledAndInert(t *testing.T) {
	settled := []StageTrackerItem{
		midFlightPR(),
		midFlightPR(func(it *StageTrackerItem) {
			it.CIActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "passing"}
			it.ReviewActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "approved"}
		}),
		midFlightPR(func(it *StageTrackerItem) {
			it.CIActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "failing", ChecksFailing: []string{"build"}}
			it.ReviewActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "changes_requested"}
		}),
		midFlightPR(func(it *StageTrackerItem) {
			it.ReviewActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "human_decision"}
		}),
		// Checks that have run far past a normal run will not finish on
		// their own: the person may have to act, so the row is not held.
		midFlightPR(func(it *StageTrackerItem) {
			it.CIActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "running", ChecksPending: []string{"a"}, ChecksStuck: []string{"a"}}
		}),
		// No review swarm at all: nothing to wait for.
		midFlightPR(func(it *StageTrackerItem) {
			it.ReviewActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "unknown"}
		}),
		// An issue is not a PR.
		{Repo: "acme/api", Number: 1, Kind: "issue", Stage: 45, CIActivity: &StageTrackerActivity{State: "running"}},
		// A merged PR has no decision to hold back.
		midFlightPR(func(it *StageTrackerItem) {
			it.Modifiers = []string{"merged"}
			it.CIActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "running"}
		}),
	}
	for i, it := range settled {
		if mf := stageTrackerMidFlightFor(it); mf != nil {
			t.Errorf("case %d: settled row reads mid-flight: %+v", i, mf)
		}
	}
}

// The unparked PR still reads mid-flight (delegate-all offers it), but names
// no park reason.
func TestStageTrackerMidFlightUnparkedNamesNoPark(t *testing.T) {
	it := midFlightPR(func(it *StageTrackerItem) {
		it.Stage, it.Modifiers, it.Reason = 60, nil, ""
		it.CIActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "running"}
	})
	mf := stageTrackerMidFlightFor(it)
	if mf == nil || mf.Park != "" {
		t.Fatalf("mid-flight = %+v", mf)
	}
}

// The resolver sets the field on every row, mirrors it onto nested PRs from
// their own tracks, and disables the options that rest on the head being
// checked — on the row's decision and on its nested PRs' — with the reason.
func TestStageTrackerResolveMidFlightGuardsDecisionOptions(t *testing.T) {
	pr := midFlightPR(func(it *StageTrackerItem) {
		it.CIActivity = &StageTrackerActivity{Head: midFlightTestHead, State: "running", ChecksPassed: 2, ChecksTotal: 5}
		it.DecisionNeeded = decisionForPR(*it, nil, "sentinel", false)
	})
	issue := StageTrackerItem{Repo: "acme/api", Number: 5, Kind: "issue", Stage: 50, LinkedPRs: []StageTrackerLinkedPR{{
		Number: 7, Stage: 85, Head: midFlightTestHead, Modifiers: []string{modNeedsDecision},
		CIActivity: pr.CIActivity, DecisionNeeded: decisionForPR(pr, nil, "sentinel", false)},
		{Number: 8, Stage: 60, Head: "fff", CIActivity: &StageTrackerActivity{Head: "fff", State: "passing"}}}}
	items := []StageTrackerItem{pr, issue}
	stageTrackerResolveMidFlight(items)

	if items[0].MidFlight == nil || items[0].MidFlight.Text != "CI running 2/5" {
		t.Fatalf("row mid_flight = %+v", items[0].MidFlight)
	}
	if lp := items[1].LinkedPRs[0]; lp.MidFlight == nil || lp.MidFlight.Text != "CI running 2/5" {
		t.Fatalf("nested #7 mid_flight = %+v", lp.MidFlight)
	}
	if items[1].LinkedPRs[1].MidFlight != nil {
		t.Fatalf("nested #8 has a green head: %+v", items[1].LinkedPRs[1].MidFlight)
	}
	for _, d := range []*StageTrackerDecision{items[0].DecisionNeeded, items[1].LinkedPRs[0].DecisionNeeded} {
		guarded := map[string]bool{}
		for _, o := range d.Options {
			if o.Action == decisionActionDelegate || o.Action == decisionActionApprove || o.Action == decisionActionDirection {
				guarded[o.Action] = true
				if o.Enabled || !strings.Contains(o.ReasonIfDisabled, "CI running 2/5") {
					t.Errorf("%s should be disabled with the reason: %+v", o.Action, o)
				}
				continue
			}
			if !o.Enabled && o.Action == decisionActionCloseWontfix {
				t.Errorf("closing stays available while CI runs: %+v", o)
			}
		}
		if len(guarded) != 3 {
			t.Errorf("options guarded = %v", guarded)
		}
	}
}
