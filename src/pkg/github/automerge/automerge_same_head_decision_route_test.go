package automerge

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestSameHeadDecisionRouteNamesReviewerFinding pins
// projectbluefin/fsdk-containers#553: the same-head 085 re-park carried an
// empty route, and the entry comment read "Needs a decision, not a merge:"
// with no reason, while the stage pass held the reviewer's requires_human
// verdict and its finding. The re-park's route now names them.
func TestSameHeadDecisionRouteNamesReviewerFinding(t *testing.T) {
	head := "0afe7c60b570d5b04dd2967459ab41797f4af639"
	obs := stageObservation{HeadSHA: head, Lane: stageLaneContributor}
	obs.Detail.VerdictKnown = true
	obs.Detail.Verdict = ContributorLaneVerdict{
		Configured:        true,
		Verdict:           "requires_human",
		DecisionRequested: true,
		DecisionFinding:   "the reviewer marked this change high-risk",
	}
	route := sameHeadDecisionRoute(obs)
	if route.Stage != stagelabels.NeedsDecision || route.Head != head || route.Verdict != stagelabels.VerdictRequiresHuman || route.SelfAuthored {
		t.Fatalf("route = %+v", route)
	}
	body := stagelabels.DecisionComment(route)
	for _, want := range []string{
		"- the reviewer swarm returned `requires_human` @0afe7c6\n",
		"- the review found: the reviewer marked this change high-risk\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("DecisionComment missing %q:\n%s", want, body)
		}
	}

	self := stageObservation{HeadSHA: head, Lane: stageLaneSelf}
	if r := sameHeadDecisionRoute(self); !r.SelfAuthored || len(r.Reasons) != 0 {
		t.Fatalf("self-authored route with no verdict = %+v", r)
	} else if body := stagelabels.DecisionComment(r); !strings.Contains(body, "Needs a decision, not a merge: "+stagelabels.DecisionReasonUnnamed+".") {
		t.Fatalf("no verdict known: DecisionComment =\n%s", body)
	}
}
