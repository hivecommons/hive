package stageengine

import (
	"testing"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestSupersessionParkSurvivesRepairAndPassOverrides(t *testing.T) {
	for _, kind := range []authorkind.Kind{authorkind.Human, authorkind.Clanker} {
		for _, reason := range []string{"needs-rebase", "check-failing:unit", "label:needs-human", "no-claim"} {
			t.Run(string(kind)+"/"+reason, func(t *testing.T) {
				o := stageObservation{Lane: stageLaneContributor, Reason: reason, AuthorKind: kind, Labels: []string{NeedsHumanLabel, prstage.SupersessionPendingLabel, "needs-rebase"}}
				f := FactsFromPRObservations([]stageObservation{o})
				f.PR.Pass = &PRPassFacts{AuthorKind: kind, DCOKnown: true, DCO: stagelabels.DCONo, DCORequired: true,
					Route: &stagelabels.Evidence{}}
				d := DerivePR(f)
				if d.Stage != stagelabels.NeedsDecision || d.Reason != prstage.ReasonSupersessionPending {
					t.Fatalf("superseded PR sent to repair: %+v", d)
				}
				// Once the cause or the park is cleared the ordinary lane owns it.
				for _, labels := range [][]string{{NeedsHumanLabel}, {prstage.SupersessionPendingLabel}, {NeedsHumanLabel, prstage.SupersessionPendingLabel, "hive/keep-open"}} {
					o.Labels = labels
					if v := ClassifyStage(o); v.Reason == prstage.ReasonSupersessionPending {
						t.Fatalf("stale park retained: %+v", v)
					}
				}
				o.Labels = []string{NeedsHumanLabel, prstage.SupersessionPendingLabel}
				o.Reason = "closed"
				if v := ClassifyStage(o); !v.Terminal {
					t.Fatalf("closed PR still parked: %+v", v)
				}
			})
		}
	}
}
