package github

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const (
	repositoryTestJobPrefix      = "Hive repository test "
	repositoryTestCommandStep    = "Execute exact repository test command"
	maxRepositoryTestJobCommands = 256
	maxRepositoryTestJobs        = 512
	maxRepositoryTestJobPages    = 8
)

var immutableRepositoryTestHead = regexp.MustCompile(`^[a-f0-9]{40}$`)

// RepositoryTestJobsRequest binds runner-owned job conclusions to the exact
// installed repository test plan for one already durably selected workflow
// run. Commands remain structured until their canonical presentation identity
// is derived locally; repository-controlled artifact text is not an authority.
type RepositoryTestJobsRequest struct {
	Repository                 string
	WorkflowRunID              int64
	ExpectedHeadSHA            string
	ExpectedWorkflowName       string
	ExpectedWorkflowPath       string
	Commands                   [][]string
	VisualJobName              string
	RequiredSuccessfulJobNames []string
}

// RepositoryTestJobEvidence is the canonical repository-test lifecycle input
// derived from GitHub's runner-owned job records.
type RepositoryTestJobEvidence struct {
	Repository               string                            `json:"repository"`
	WorkflowRunID            int64                             `json:"workflow_run_id"`
	WorkflowID               int64                             `json:"workflow_id"`
	HeadSHA                  string                            `json:"head_sha"`
	VisualJobID              int64                             `json:"visual_job_id"`
	RequiredSuccessfulJobIDs map[string]int64                  `json:"required_successful_job_ids"`
	Results                  []visualhive.RepositoryTestResult `json:"results"`
	Overall                  int                               `json:"overall"`
	EvidenceDigest           string                            `json:"evidence_digest"`
	PagesRead                int                               `json:"pages_read"`
	JobsScanned              int                               `json:"jobs_scanned"`
}

// VerifyRepositoryTestJobs exhaustively reads every attempt's jobs for an
// exact workflow_dispatch run. It accepts exactly one of every required successful job
// and one terminal runner-owned job for every configured command, in the
// deterministic Hive repository-test namespace. Any missing, duplicate, or
// unexpected namespaced job fails closed.
func (c *Client) VerifyRepositoryTestJobs(ctx context.Context, request RepositoryTestJobsRequest) (RepositoryTestJobEvidence, error) {
	result := RepositoryTestJobEvidence{}
	owner, repo, err := splitFullRepository(request.Repository)
	if err != nil {
		return result, err
	}
	expectedHead := strings.ToLower(strings.TrimSpace(request.ExpectedHeadSHA))
	expectedWorkflowName := strings.TrimSpace(request.ExpectedWorkflowName)
	expectedWorkflowPath := strings.TrimSpace(request.ExpectedWorkflowPath)
	visualJobName := strings.TrimSpace(request.VisualJobName)
	if request.WorkflowRunID <= 0 || !immutableRepositoryTestHead.MatchString(expectedHead) || expectedWorkflowName == "" || expectedWorkflowPath == "" || visualJobName == "" {
		return result, fmt.Errorf("exact run, immutable head, workflow name/path, and primary Visual job name are required")
	}
	requiredSuccessfulJobs := make(map[string]bool, len(request.RequiredSuccessfulJobNames)+1)
	requiredSuccessfulJobs[visualJobName] = true
	for index, value := range request.RequiredSuccessfulJobNames {
		name := strings.TrimSpace(value)
		if name == "" {
			return result, fmt.Errorf("required successful job name %d is empty", index+1)
		}
		requiredSuccessfulJobs[name] = true
	}
	for name := range requiredSuccessfulJobs {
		if strings.HasPrefix(name, repositoryTestJobPrefix) {
			return result, fmt.Errorf("required successful job %q collides with the repository-test job namespace", name)
		}
	}
	if len(request.Commands) > maxRepositoryTestJobCommands {
		return result, fmt.Errorf("repository test plan exceeds %d commands", maxRepositoryTestJobCommands)
	}

	canonical := make([]visualhive.RepositoryTestResult, len(request.Commands))
	expectedJobs := make(map[string]int, len(request.Commands))
	seenCommands := make(map[string]bool, len(request.Commands))
	for index, command := range request.Commands {
		if len(command) == 0 {
			return result, fmt.Errorf("repository test command %d is empty", index+1)
		}
		presentation := strings.Join(command, " ")
		entry, entryErr := visualhive.NewRepositoryTestResult(presentation, 0)
		if entryErr != nil {
			return result, fmt.Errorf("repository test command %d is not canonically safe: %w", index+1, entryErr)
		}
		if seenCommands[entry.Command] {
			return result, fmt.Errorf("repository test plan repeats canonical command %q", entry.Command)
		}
		seenCommands[entry.Command] = true
		canonical[index] = entry
		expectedJobs[fmt.Sprintf("%s%03d", repositoryTestJobPrefix, index+1)] = index
	}

	run, _, err := c.client.Actions.GetWorkflowRunByID(ctx, owner, repo, request.WorkflowRunID)
	if err != nil {
		return result, fmt.Errorf("read exact repository-test workflow run: %w", err)
	}
	runName := strings.TrimSpace(run.GetName())
	if run.GetID() != request.WorkflowRunID || run.GetWorkflowID() <= 0 || runName == "" ||
		!workflowPathMatches(run.GetPath(), expectedWorkflowPath) || run.GetEvent() != "workflow_dispatch" ||
		!strings.EqualFold(strings.TrimSpace(run.GetHeadSHA()), expectedHead) || run.GetStatus() != "completed" ||
		(run.GetConclusion() != "success" && run.GetConclusion() != "failure") ||
		(run.GetRepository().GetFullName() != "" && !strings.EqualFold(run.GetRepository().GetFullName(), request.Repository)) {
		return result, fmt.Errorf("repository-test run does not match the exact completed workflow_dispatch binding")
	}
	definition, _, err := c.client.Actions.GetWorkflowByID(ctx, owner, repo, run.GetWorkflowID())
	if err != nil {
		return result, fmt.Errorf("read exact repository-test workflow definition: %w", err)
	}
	if definition.GetID() != run.GetWorkflowID() || definition.GetName() != expectedWorkflowName ||
		!workflowPathMatches(definition.GetPath(), expectedWorkflowPath) || definition.GetState() != "active" {
		return result, fmt.Errorf("repository-test workflow definition does not match the active installed workflow")
	}

	jobResults := make([]int, len(request.Commands))
	jobSeen := make([]bool, len(request.Commands))
	requiredSuccessfulJobIDs := make(map[string]int64, len(requiredSuccessfulJobs))
	seenJobIDs := make(map[int64]bool)
	seenPages := make(map[int]bool)
	page := 1
	for page > 0 {
		if seenPages[page] || len(seenPages) >= maxRepositoryTestJobPages {
			return result, fmt.Errorf("repository-test job pagination exceeded its bounded unique-page limit")
		}
		seenPages[page] = true
		jobs, response, listErr := c.client.Actions.ListWorkflowJobs(ctx, owner, repo, request.WorkflowRunID, &gh.ListWorkflowJobsOptions{
			Filter: "all", ListOptions: gh.ListOptions{Page: page, PerPage: 100},
		})
		if listErr != nil {
			return result, fmt.Errorf("list exact repository-test workflow jobs: %w", listErr)
		}
		result.PagesRead++
		for _, job := range jobs.Jobs {
			if job == nil || job.GetID() <= 0 || seenJobIDs[job.GetID()] {
				return result, fmt.Errorf("repository-test workflow returned a missing or duplicate job identity")
			}
			seenJobIDs[job.GetID()] = true
			result.JobsScanned++
			if result.JobsScanned > maxRepositoryTestJobs {
				return result, fmt.Errorf("repository-test workflow exceeded %d jobs", maxRepositoryTestJobs)
			}

			name := strings.TrimSpace(job.GetName())
			index, repositoryJob := expectedJobs[name]
			requiredSuccessfulJob := requiredSuccessfulJobs[name]
			if strings.HasPrefix(name, repositoryTestJobPrefix) && !repositoryJob {
				return result, fmt.Errorf("repository-test workflow emitted unexpected namespaced job %q", name)
			}
			if !repositoryJob && !requiredSuccessfulJob {
				continue
			}
			if job.GetRunID() != request.WorkflowRunID || !strings.EqualFold(strings.TrimSpace(job.GetHeadSHA()), expectedHead) ||
				strings.TrimSpace(job.GetWorkflowName()) != runName || job.GetStatus() != "completed" {
				return result, fmt.Errorf("workflow job %q does not match the exact run/head/workflow binding", name)
			}
			if requiredSuccessfulJob {
				if requiredSuccessfulJobIDs[name] != 0 || job.GetConclusion() != "success" {
					return result, fmt.Errorf("repository-test workflow must contain exactly one successful required job %q", name)
				}
				requiredSuccessfulJobIDs[name] = job.GetID()
				continue
			}
			if jobSeen[index] {
				return result, fmt.Errorf("repository-test workflow duplicated job %q", name)
			}
			jobSeen[index] = true
			commandExit, commandErr := repositoryTestCommandExit(job)
			if commandErr != nil {
				return result, fmt.Errorf("repository-test job %q has invalid runner step evidence: %w", name, commandErr)
			}
			jobResults[index] = commandExit
		}
		if response == nil {
			page = 0
		} else {
			page = response.NextPage
		}
	}
	for name := range requiredSuccessfulJobs {
		if requiredSuccessfulJobIDs[name] == 0 {
			return result, fmt.Errorf("repository-test workflow is missing required successful job %q", name)
		}
	}
	overall := 0
	for index := range canonical {
		if !jobSeen[index] {
			return result, fmt.Errorf("repository-test workflow is missing job %q", fmt.Sprintf("%s%03d", repositoryTestJobPrefix, index+1))
		}
		entry, entryErr := visualhive.NewRepositoryTestResult(canonical[index].Command, jobResults[index])
		if entryErr != nil {
			return result, entryErr
		}
		canonical[index] = entry
		if overall == 0 && entry.ExitCode != 0 {
			overall = entry.ExitCode
		}
	}
	_, _, evidenceDigest, err := visualhive.EncodeRepositoryTestEvidence(canonical, overall)
	if err != nil {
		return result, fmt.Errorf("encode canonical repository-test job evidence: %w", err)
	}
	result.Repository = request.Repository
	result.WorkflowRunID = request.WorkflowRunID
	result.WorkflowID = run.GetWorkflowID()
	result.HeadSHA = expectedHead
	result.VisualJobID = requiredSuccessfulJobIDs[visualJobName]
	result.RequiredSuccessfulJobIDs = requiredSuccessfulJobIDs
	result.Results = canonical
	result.Overall = overall
	result.EvidenceDigest = evidenceDigest
	return result, nil
}

// repositoryTestCommandExit distinguishes a deterministic command result from
// runner/setup infrastructure. The generated workflow gives the command one
// exact step name. Every checkout, runtime setup, dependency install, and post
// cleanup step must complete successfully; only that exact command step may
// fail, and the runner-owned job conclusion must agree with it.
func repositoryTestCommandExit(job *gh.WorkflowJob) (int, error) {
	if job == nil || len(job.Steps) == 0 {
		return 0, fmt.Errorf("exact command step is missing")
	}
	commandSteps := 0
	commandConclusion := ""
	previousNumber := int64(0)
	for index, step := range job.Steps {
		if step == nil {
			return 0, fmt.Errorf("runner step %d is missing", index+1)
		}
		name := strings.TrimSpace(step.GetName())
		number := step.GetNumber()
		if name == "" || number <= previousNumber {
			return 0, fmt.Errorf("runner steps lack a strictly ordered identity at position %d", index+1)
		}
		previousNumber = number
		if step.GetStatus() != "completed" {
			return 0, fmt.Errorf("runner step %q is not completed", name)
		}
		conclusion := strings.TrimSpace(step.GetConclusion())
		if name == repositoryTestCommandStep {
			commandSteps++
			if commandSteps > 1 {
				return 0, fmt.Errorf("exact command step is duplicated")
			}
			if conclusion != "success" && conclusion != "failure" {
				return 0, fmt.Errorf("exact command step has non-deterministic conclusion %q", conclusion)
			}
			commandConclusion = conclusion
			continue
		}
		if conclusion != "success" {
			return 0, fmt.Errorf("infrastructure step %q concluded %q", name, conclusion)
		}
	}
	if commandSteps != 1 {
		return 0, fmt.Errorf("exact command step is missing")
	}
	if job.GetConclusion() != commandConclusion {
		return 0, fmt.Errorf("job conclusion %q does not match exact command conclusion %q", job.GetConclusion(), commandConclusion)
	}
	if commandConclusion == "failure" {
		return 1, nil
	}
	return 0, nil
}
