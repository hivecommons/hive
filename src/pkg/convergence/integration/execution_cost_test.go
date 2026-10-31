package integration

import (
	"math"
	"testing"
	"time"
)

func TestExecutionCostUsesPhysicalAssignedJobsAndExplicitCompleteness(t *testing.T) {
	through := executionProofFixture().ObservedAt
	from := through.Add(-time.Minute)
	coverage := []Coverage{{Repo: "o/r", Base: "v5", From: from, Through: through, Complete: true}}
	job := Job{Repo: "o/r", Base: "v5", ID: 1, RunID: 10, Attempt: 1, Head: "head", RunnerID: 9, Status: "completed",
		Conclusion: "cancelled", StartedAt: through.Add(-2 * time.Minute), CompletedAt: through, ObservedAt: through}
	shared := ExecutionCohortCost("o/r", "v5", from, through, coverage, []Job{job}, []Job{job})
	if !shared.Complete || shared.RunnerMinutes == nil || *shared.RunnerMinutes != 1 || shared.Jobs != 1 {
		t.Fatalf("double-counted shared/canceled/window-clipped execution: %+v", shared)
	}
	conflicting := job
	conflicting.Head = "other"
	ambiguous := ExecutionCohortCost("o/r", "v5", from, through, coverage, []Job{job}, []Job{conflicting})
	if ambiguous.Complete || ambiguous.RunnerMinutes != nil {
		t.Fatal("conflicting physical identity became complete")
	}
	unknown := ExecutionCohortCost("o/r", "v5", from, through, nil, []Job{job})
	if unknown.Complete || unknown.RunnerMinutes != nil || unknown.ObservedRunnerMinutes != 1 {
		t.Fatal("missing cohort became zero/complete")
	}
	later := job
	later.ObservedAt = through.Add(time.Second)
	if got := ExecutionCohortCost("o/r", "v5", from, through, coverage, []Job{job}, []Job{later}); !got.Complete {
		t.Fatal("consistent newer physical observation rejected")
	}
	for _, value := range []float64{-1, math.Inf(1), math.NaN()} {
		if executionCostKnown(Cost{Complete: true, RunnerMinutes: &value}) {
			t.Fatal("nonfinite/negative cost authorized growth")
		}
	}
	zero := 0.0
	if executionCostKnown(Cost{Complete: true, RunnerMinutes: &zero, Jobs: 0, Running: 1}) {
		t.Fatal("inconsistent running cohort authorized growth")
	}
}
