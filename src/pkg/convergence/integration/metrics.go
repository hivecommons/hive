package integration

import (
	"strings"
	"time"
)

type CostReport struct {
	Repo                         string    `json:"repo"`
	Base                         string    `json:"base"`
	From                         time.Time `json:"from"`
	Through                      time.Time `json:"through"`
	Cost                         Cost      `json:"cost"`
	ObservedMergedPRs            int       `json:"observed_merged_prs"`
	MergedPRs                    *int      `json:"merged_prs"`
	RunnerMinutesPerMergedPR     *float64  `json:"runner_minutes_per_merged_pr"`
	AttributedIntegrationMinutes *float64  `json:"attributed_integration_minutes"`
	UnattributedExecutionJobs    int       `json:"unattributed_execution_jobs"`
	Attribution                  string    `json:"attribution"`
	Reason                       string    `json:"reason"`
}

func (s *Store) CostReport(repo, base string, from, through time.Time) CostReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	coverage := s.coverage[scopeKey(repo, base)]
	out := CostReport{Repo: repo, Base: base, From: from, Through: through, Cost: cost(s.jobs, coverage, repo, base, from, through),
		Attribution: "bound tested-candidate job execution only; unclassified jobs and author-review receipts remain separate"}
	for _, receipt := range s.integrated {
		if strings.EqualFold(receipt.Repo, repo) && receipt.Base == base && !receipt.MergedAt.Before(from) && !receipt.MergedAt.After(through) {
			out.ObservedMergedPRs++
		}
	}
	if coverage.MergedComplete && !coverage.From.After(from) && !coverage.Through.Before(through) {
		count := out.ObservedMergedPRs
		out.MergedPRs = &count
		if count > 0 && out.Cost.RunnerMinutes != nil {
			average := *out.Cost.RunnerMinutes / float64(count)
			out.RunnerMinutesPerMergedPR = &average
		} else {
			out.Reason = "no completed merged-PR denominator or complete job cohort"
		}
	} else {
		out.Reason = "merged-PR cohort incomplete: " + coverage.MergeReason
	}
	for _, job := range s.jobs {
		if !strings.EqualFold(job.Repo, repo) || job.Base != base {
			continue
		}
		one := cost(map[string]Job{job.Key(): job}, coverage, repo, base, from, through)
		if one.Jobs == 0 {
			continue
		}
		bound := false
		for _, id := range job.Candidates {
			receipt, exists := s.evidence[id]
			if exists && receipt.Subject.Validate() == nil && receipt.Source != "github-workflow-run-v1" &&
				receipt.RunID == job.RunID && receipt.Subject.Attempt == job.Attempt {
				bound = true
				break
			}
		}
		if !bound {
			out.UnattributedExecutionJobs++
			continue
		}
		if out.AttributedIntegrationMinutes == nil {
			value := 0.0
			out.AttributedIntegrationMinutes = &value
		}
		*out.AttributedIntegrationMinutes += one.ObservedRunnerMinutes
	}
	return out
}
