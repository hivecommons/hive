package github

import (
	"context"
	"fmt"
	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/forgestate"
	"time"
)

// ReadIntegrationExecutionJobs accounts duplicate/unowned actual executions
// without authenticating a receipt or granting any promotion/recovery authority.
func (c *Client) ReadIntegrationExecutionJobs(ctx context.Context, repo string, workflowID, runID int64, attempt int, now time.Time) (IntegrationCandidateObservation, error) {
	out := IntegrationCandidateObservation{RunID: runID, Attempt: attempt, State: "unknown", ObservedAt: now, JobCompleteness: "unknown"}
	if c == nil || c.client == nil || !integrationPromotionRepo(repo) || workflowID <= 0 || runID <= 0 || attempt < 1 || now.IsZero() {
		return out, fmt.Errorf("incomplete actual execution accounting identity")
	}
	var run gh.WorkflowRun
	path := fmt.Sprintf("repos/%s/actions/runs/%d", repo, runID)
	if err := c.integrationCandidateRead(ctx, path, &run); err != nil {
		return out, err
	}
	if run.GetID() != runID || run.GetWorkflowID() != workflowID || run.GetRepository().GetFullName() != repo || !integrationOID(run.GetHeadSHA()) {
		return out, fmt.Errorf("actual execution run is outside enrolled workflow/repository")
	}
	var jobs struct {
		Total int               `json:"total_count"`
		Jobs  []*gh.WorkflowJob `json:"jobs"`
	}
	if err := c.integrationCandidateRead(ctx, fmt.Sprintf("%s/attempts/%d/jobs?per_page=%d", path, attempt, integrationGroupMaxEntries), &jobs); err != nil {
		return out, err
	}
	for _, job := range jobs.Jobs {
		if job == nil || job.GetID() <= 0 || job.GetRunID() != runID || job.GetHeadSHA() != run.GetHeadSHA() {
			return out, fmt.Errorf("actual job does not belong to exact run/head")
		}
		out.Jobs = append(out.Jobs, forgestate.IntegrationRunnerJob{
			Repo: repo, RunID: runID, Attempt: attempt, JobID: job.GetID(), RunnerID: job.GetRunnerID(), Name: job.GetName(),
			Status: job.GetStatus(), Conclusion: job.GetConclusion(), StartedAt: job.GetStartedAt().Time,
			CompletedAt: job.GetCompletedAt().Time, ObservedAt: now})
	}
	if jobs.Total != len(jobs.Jobs) || jobs.Total == 0 || run.GetRunAttempt() < attempt {
		return out, fmt.Errorf("actual execution cohort truncated, empty or superseded")
	}
	out.JobCompleteness = "complete"
	return out, nil
}
