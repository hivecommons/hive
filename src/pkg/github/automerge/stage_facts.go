package automerge

// Gathering-side previews use the engine's pure PR helpers (ADR-0025 PR-F).
import (
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func factsFrom(list []stageObservation) stageengine.Facts {
	return stageengine.FactsFromPRObservations(list)
}
func reviewFactsFrom(d stageDetail, head string) stageengine.ReviewFacts {
	return stageengine.ReviewFactsFrom(d, head)
}
func verdictStateFrom(d stageDetail) stagelabels.VerdictState { return stageengine.VerdictStateFrom(d) }
func verdictFrom(r stageengine.ReviewFacts) ContributorLaneVerdict {
	return stageengine.VerdictFromReviewFacts(r)
}
func observationsFrom(f stageengine.Facts) []stageObservation {
	return stageengine.ObservationsFromPRFacts(f)
}
