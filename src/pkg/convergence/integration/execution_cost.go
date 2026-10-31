package integration

import (
	"fmt"
	"math"
	"time"
)

func executionCostKnown(observed Cost) bool {
	return observed.Complete && observed.RunnerMinutes != nil && *observed.RunnerMinutes >= 0 &&
		!math.IsNaN(*observed.RunnerMinutes) && !math.IsInf(*observed.RunnerMinutes, 0) &&
		observed.Jobs >= 0 && observed.Running >= 0 && observed.Running <= observed.Jobs
}

// ExecutionCohortCost reuses assigned-runner accounting while unioning durable
// observers. A default-branch bootstrap can be seen by both ledgers; physical
// repo/job/attempt keys, not observer ownership, determine execution spend.
func ExecutionCohortCost(repo, base string, from, through time.Time, coverage []Coverage, cohorts ...[]Job) Cost {
	complete := len(coverage) > 0
	for _, observed := range coverage {
		complete = complete && observed.Complete && !observed.From.After(from) && !observed.Through.Before(through)
	}
	jobs := map[string]Job{}
	for _, cohort := range cohorts {
		for _, job := range cohort {
			old, exists := jobs[job.Key()]
			if exists && (old.Repo != job.Repo || old.Head != job.Head || old.RunID != job.RunID) {
				complete = false
				continue
			}
			if !exists || job.ObservedAt.After(old.ObservedAt) {
				jobs[job.Key()] = job
			}
		}
	}
	return cost(jobs, Coverage{Repo: repo, Base: base, From: from, Through: through, Complete: complete,
		Reason: fmt.Sprintf("actual deduplicated workflow cohorts; complete=%t", complete)}, repo, base, from, through)
}
