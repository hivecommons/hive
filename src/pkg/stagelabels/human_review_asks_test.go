package stagelabels

import (
	"slices"
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
)

// TestRouteHumanCarriesReviewAsksOnAChangesRequestedRoute: what the review
// asked for rides the fixing route decided on a changes_requested verdict,
// gate or no gate, so the author's nudge can list it when the lane stopped
// before the verdict. It is never a Reason, and no other route carries it.
func TestRouteHumanCarriesReviewAsksOnAChangesRequestedRoute(t *testing.T) {
	const ask = "review finding: `src/widget.go:42` return the error instead of dropping it"
	asks := []string{ask}
	for name, gate := range map[string]func(*Evidence){
		"no gate":  nil,
		"sentinel": func(e *Evidence) { e.Sentinel = true },
		"hold":     func(e *Evidence) { e.Holds = Holds([]string{"hold"}) },
	} {
		e := readyEvidence()
		e.AuthorKind = authorkind.Clanker
		e.Verdict = VerdictChangesRequested
		e.ReviewAsks = []string{ask}
		if gate != nil {
			gate(&e)
		}
		r := RouteHuman(e)
		if r.Stage != ReturnedToContributor || r.LaneReason != LaneReasonChangesRequested {
			t.Fatalf("%s: route = %+v, want 072 for the verdict", name, r)
		}
		if !slices.Equal(r.ReviewAsks, asks) {
			t.Fatalf("%s: review asks = %q, want %q", name, r.ReviewAsks, asks)
		}
		if len(r.Reasons) != 1 || r.Reasons[0] != "verdict changes_requested @abc1234" {
			t.Fatalf("%s: reasons = %q, want the verdict reason alone", name, r.Reasons)
		}
		// The route owns its copy: the evidence is recorded on the pass
		// facts and must not change through the route.
		r.ReviewAsks[0] = "changed"
		if e.ReviewAsks[0] != ask {
			t.Fatalf("%s: the route shares the evidence's review asks", name)
		}
	}

	// A fixing route decided on something else (a red check under an
	// approving verdict) carries no review asks, whatever the evidence holds.
	red := readyEvidence()
	red.AuthorKind = authorkind.Clanker
	red.ReviewAsks = asks
	red.Checks = CheckSummary{Total: 3, Green: 2, Red: []string{"unit"}, Known: true}
	if r := RouteHuman(red); r.Stage != ReturnedToContributor || len(r.ReviewAsks) != 0 {
		t.Fatalf("red-check route = %+v, want 072 with no review asks", r)
	}
	// Neither does a route that is not the author's: ready for a merge,
	// or a person's decision.
	ready := readyEvidence()
	ready.ReviewAsks = asks
	if r := RouteHuman(ready); r.Stage != ReadyForHumanMerge || len(r.ReviewAsks) != 0 {
		t.Fatalf("ready route = %+v, want 090 with no review asks", r)
	}
	decision := readyEvidence()
	decision.Verdict, decision.ReviewAsks = VerdictRequiresHuman, asks
	decision.AuthorKind = authorkind.Clanker
	if r := RouteHuman(decision); r.Stage != NeedsDecision || len(r.ReviewAsks) != 0 {
		t.Fatalf("requires_human route = %+v, want 085 with no review asks", r)
	}
}
