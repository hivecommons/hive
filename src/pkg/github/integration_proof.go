package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

// IntegrationWorkflowProof is a receipt for ONE pinned workflow, not an
// assertion that all CI/review/trust/merge gates have passed.
type IntegrationWorkflowProof struct {
	Passed   bool
	Rejected bool
	Reason   string
	RunID    int64
	SuiteID  int64
	CheckIDs []int64
}

func (c *Client) ReadIntegrationWorkflowProof(ctx context.Context, policy config.IntegrationPolicy, run *gh.WorkflowRun) (IntegrationWorkflowProof, error) {
	out := IntegrationWorkflowProof{Reason: "workflow evidence incomplete"}
	parts := strings.SplitN(policy.Repo, "/", 2)
	if len(parts) != 2 || run == nil || run.GetRunAttempt() < 1 || run.GetID() <= 0 || run.GetCheckSuiteID() <= 0 || run.GetWorkflowID() <= 0 {
		return out, fmt.Errorf("workflow observation identity incomplete")
	}
	out.RunID, out.SuiteID = run.GetID(), run.GetCheckSuiteID()
	event := "pull_request"
	if policy.Engine == "native" {
		event = "merge_group"
	}
	if run.GetEvent() != event || strings.Split(run.GetPath(), "@")[0] != policy.Workflow ||
		!strings.EqualFold(run.GetRepository().GetFullName(), policy.Repo) || run.GetHeadRepository().GetFullName() == "" ||
		policy.Engine == "native" && !strings.EqualFold(run.GetHeadRepository().GetFullName(), policy.Repo) {
		return out, fmt.Errorf("workflow event, path or source repository not bound to policy")
	}
	content, _, _, err := c.client.Repositories.GetContents(ctx, parts[0], parts[1], policy.Workflow, &gh.RepositoryContentGetOptions{Ref: run.GetHeadSHA()})
	if err != nil {
		return out, err
	}
	if content == nil || content.GetSHA() != policy.Blob {
		return out, fmt.Errorf("tested workflow does not match pinned policy blob")
	}
	trusted, _, _, err := c.client.Repositories.GetContents(ctx, parts[0], parts[1], policy.Workflow, &gh.RepositoryContentGetOptions{Ref: policy.Base})
	if err != nil {
		return out, err
	}
	if trusted == nil || trusted.GetSHA() != policy.Blob {
		return out, fmt.Errorf("pinned workflow is not present on the trusted target base")
	}
	request, err := c.client.NewRequest("GET", fmt.Sprintf("repos/%s/actions/runs/%d/attempts/%d/jobs?per_page=%d", policy.Repo, run.GetID(), run.GetRunAttempt(), integrationGroupMaxEntries), nil)
	if err != nil {
		return out, err
	}
	jobs := new(gh.Jobs)
	response, err := c.client.Do(ctx, request, jobs)
	if err != nil {
		return out, err
	}
	if response != nil && response.NextPage != 0 {
		return out, fmt.Errorf("workflow job proof exceeds bounded page")
	}
	checks, response, err := c.client.Checks.ListCheckRunsForRef(ctx, parts[0], parts[1], run.GetHeadSHA(), &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: integrationGroupMaxEntries}})
	if err != nil {
		return out, err
	}
	if checks == nil || response != nil && response.NextPage != 0 {
		return out, fmt.Errorf("check proof unavailable or truncated")
	}
	if err := bindIntegrationWorkflowJobs(policy, run, jobs.Jobs, checks.CheckRuns, &out); err != nil {
		return out, err
	}
	return out, nil
}

func bindIntegrationWorkflowJobs(policy config.IntegrationPolicy, run *gh.WorkflowRun, jobs []*gh.WorkflowJob, checks []*gh.CheckRun, out *IntegrationWorkflowProof) error {
	if run.GetStatus() != "completed" {
		out.Reason = "pinned integration workflow still running"
		return nil
	}
	for _, name := range policy.Jobs {
		var job *gh.WorkflowJob
		for _, j := range jobs {
			if j.GetName() == name {
				if job != nil {
					return fmt.Errorf("ambiguous required integration job %q", name)
				}
				job = j
			}
		}
		if job == nil || job.GetRunID() != run.GetID() || job.GetHeadSHA() != run.GetHeadSHA() {
			return fmt.Errorf("required integration job %q missing exact run/head binding", name)
		}
		url := strings.Split(strings.TrimRight(job.GetCheckRunURL(), "/"), "/")
		id, err := strconv.ParseInt(url[len(url)-1], 10, 64)
		if err != nil {
			return fmt.Errorf("integration job missing check identity")
		}
		var check *gh.CheckRun
		for _, candidate := range checks {
			if candidate.GetID() == id {
				check = candidate
			}
		}
		if check == nil || check.GetHeadSHA() != run.GetHeadSHA() || check.GetCheckSuite().GetID() != run.GetCheckSuiteID() ||
			check.GetApp().GetID() != policy.AppID || check.GetName() != name {
			return fmt.Errorf("integration check app/suite/job/head binding unavailable")
		}
		out.CheckIDs = append(out.CheckIDs, id)
		if job.GetStatus() != "completed" || check.GetStatus() != "completed" {
			out.Reason = "integration job still running"
			return nil
		}
		if job.GetConclusion() != "success" || check.GetConclusion() != "success" {
			out.Rejected = true
			out.Reason = "pinned integration workflow job rejected"
			return nil
		}
	}
	if run.GetConclusion() != "success" {
		out.Rejected = true
		out.Reason = "pinned integration workflow rejected"
		return nil
	}
	out.Passed, out.Reason = true, "exact tested workflow blob, producer, suite and required job membership verified; other gates remain separate"
	return nil
}
