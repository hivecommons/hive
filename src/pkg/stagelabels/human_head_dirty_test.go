package stagelabels

import (
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
)

// TestRouteHumanGatedConflictGoesToAuthor: a gated head whose evidence
// carries the conflict goes to the author's fixing stage by kind, with the
// gate riding as Gates only (it is a person's to lift, not the author's
// instruction) and the conflict as the lane reason the nudge reads.
func TestRouteHumanGatedConflictGoesToAuthor(t *testing.T) {
	cases := []struct {
		name  string
		kind  authorkind.Kind
		self  bool
		gate  func(*Evidence)
		want  Stage
		gated string
	}{
		{"clanker sentinel", authorkind.Clanker, false, func(e *Evidence) { e.Sentinel = true }, ReturnedToContributor, "a sentinel alert is open on this PR"},
		{"clanker hold", authorkind.Clanker, false, func(e *Evidence) { e.Holds = Holds([]string{"hold"}) }, ReturnedToContributor, "the `hold` label is set"},
		{"person sentinel", authorkind.Human, false, func(e *Evidence) { e.Sentinel = true }, NeedsAuthor, "a sentinel alert is open on this PR"},
		{"hive hold", authorkind.Hive, true, func(e *Evidence) { e.Holds = Holds([]string{"hold"}) }, Fixing, "the `hold` label is set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := readyEvidence()
			e.AuthorKind, e.SelfAuthored = tc.kind, tc.self
			tc.gate(&e)
			e.Conflict, e.HeadDirty = true, true
			r := RouteHuman(e)
			if r.Stage != tc.want || r.LaneReason != LaneReasonConflicting {
				t.Fatalf("route = %+v, want %v for the conflict", r, tc.want)
			}
			if !containsString(r.Reasons, "merge conflict with the base branch") || containsString(r.Reasons, tc.gated) {
				t.Fatalf("reasons = %v: the conflict is the author's instruction, the gate is not", r.Reasons)
			}
			if !containsString(r.Gates, tc.gated) {
				t.Fatalf("gates = %v, want the gate %q to ride the 07x route", r.Gates, tc.gated)
			}
		})
	}
}

// TestRouteHumanDirtyHeadNeverReady: a head GitHub reports `dirty` is never
// 090 (or 060), gated or not, whatever else the evidence says, because no
// merge click can land it. A decision still owed is asked first, as it
// is for a lane-reported conflict on a contributor's PR
// (hivecommons/hive#14607).
func TestRouteHumanDirtyHeadNeverReady(t *testing.T) {
	for _, gated := range []bool{false, true} {
		e := readyEvidence()
		e.AuthorKind = authorkind.Clanker
		e.Sentinel = gated
		e.HeadDirty = true
		if r := RouteHuman(e); r.Stage != ReturnedToContributor || r.LaneReason != LaneReasonConflicting {
			t.Fatalf("gated=%v green reviewed dirty head route = %+v, want 072 for the conflict", gated, r)
		}
		pending := e
		pending.Checks = CheckSummary{Total: 55, Green: 54, Pending: 1, Known: true}
		if r := RouteHuman(pending); r.Stage == Reviewed || r.Stage == ReadyForHumanMerge {
			t.Fatalf("gated=%v dirty head with checks running route = %+v, want it off 060/090", gated, r)
		}
		manual := e
		manual.Cause, manual.ManualMergeRule, manual.CauseReason, manual.HumanApprover = CauseDecision, true, "the PR edits CI workflow / action files", "octocat"
		if r := RouteHuman(manual); r.Stage == ReadyForHumanMerge {
			t.Fatalf("gated=%v approved manual-merge dirty head route = %+v, want it off 090", gated, r)
		}
		decision := e
		decision.Verdict = VerdictRequiresHuman
		if r := RouteHuman(decision); r.Stage != NeedsDecision {
			t.Fatalf("gated=%v requires_human dirty head route = %+v, want the decision asked first (085)", gated, r)
		}
	}
	// A clean head is unchanged.
	if r := RouteHuman(readyEvidence()); r.Stage != ReadyForHumanMerge {
		t.Fatalf("clean head route = %+v", r)
	}
}
