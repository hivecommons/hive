package github

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

const (
	SetupBaselineCaptureJobDisplayName = "Hive setup baseline capture (isolated target)"
	SetupBaselineVerifyJobDisplayName  = "Hive setup baseline artifact verifier"
)

type SetupBaselineArtifactRequest struct {
	Repository           string
	RepositoryID         string
	WorkflowRunID        int64
	ArtifactID           int64
	ArtifactName         string
	CaptureHeadSHA       string
	DefaultBranch        string
	ExpectedWorkflowName string
	ExpectedWorkflowPath string
	ExpectedRunName      string
	DestinationDir       string
}

type VerifiedSetupBaselineArtifact struct {
	RepositoryID  string `json:"repository_id"`
	WorkflowRunID int64  `json:"workflow_run_id"`
	ArtifactID    int64  `json:"artifact_id"`
	ArtifactName  string `json:"artifact_name"`
	CaptureHead   string `json:"capture_head"`
	HeadBranch    string `json:"head_branch"`
	WorkflowName  string `json:"workflow_name"`
	WorkflowPath  string `json:"workflow_path"`
	RunURL        string `json:"run_url"`
	ArtifactRoot  string `json:"artifact_root"`
}

// FetchAndVerifySetupBaselineArtifact accepts only the dedicated successful
// workflow_dispatch lane at one exact default-branch head. The target capture
// and fresh verifier must be the only non-skipped jobs and must both use the
// workflow's ubuntu-latest runner label.
func (c *Client) FetchAndVerifySetupBaselineArtifact(ctx context.Context, request SetupBaselineArtifactRequest) (VerifiedSetupBaselineArtifact, error) {
	owner, repo, err := splitFullRepository(request.Repository)
	if err != nil {
		return VerifiedSetupBaselineArtifact{}, err
	}
	if request.WorkflowRunID <= 0 || request.ArtifactID <= 0 || strings.TrimSpace(request.RepositoryID) == "" ||
		!exactManagedSHA(strings.ToLower(strings.TrimSpace(request.CaptureHeadSHA))) || strings.TrimSpace(request.DefaultBranch) == "" ||
		strings.TrimSpace(request.ArtifactName) == "" || strings.TrimSpace(request.ExpectedWorkflowName) == "" ||
		strings.TrimSpace(request.ExpectedWorkflowPath) == "" || strings.TrimSpace(request.ExpectedRunName) == "" || strings.TrimSpace(request.DestinationDir) == "" {
		return VerifiedSetupBaselineArtifact{}, fmt.Errorf("setup baseline artifact requires exact repository, run, artifact, head, workflow, ref, name, and destination bindings")
	}
	repository, _, err := c.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return VerifiedSetupBaselineArtifact{}, fmt.Errorf("verify setup baseline repository: %w", err)
	}
	if repository.GetID() <= 0 || strconv.FormatInt(repository.GetID(), 10) != request.RepositoryID || !strings.EqualFold(repository.GetFullName(), request.Repository) {
		return VerifiedSetupBaselineArtifact{}, fmt.Errorf("setup baseline repository identity changed")
	}
	run, _, err := c.client.Actions.GetWorkflowRunByID(ctx, owner, repo, request.WorkflowRunID)
	if err != nil {
		return VerifiedSetupBaselineArtifact{}, fmt.Errorf("read setup baseline workflow run: %w", err)
	}
	if err := validateSetupBaselineWorkflowRun(run, request); err != nil {
		return VerifiedSetupBaselineArtifact{}, err
	}
	definition, _, err := c.client.Actions.GetWorkflowByID(ctx, owner, repo, run.GetWorkflowID())
	if err != nil {
		return VerifiedSetupBaselineArtifact{}, fmt.Errorf("read setup baseline workflow definition: %w", err)
	}
	definitionPath := strings.TrimPrefix(strings.TrimSpace(strings.Split(definition.GetPath(), "@")[0]), "/")
	runPath := strings.TrimPrefix(strings.TrimSpace(strings.Split(run.GetPath(), "@")[0]), "/")
	expectedPath := strings.TrimPrefix(strings.TrimSpace(request.ExpectedWorkflowPath), "/")
	if definition.GetID() != run.GetWorkflowID() || definition.GetName() != request.ExpectedWorkflowName || definitionPath != expectedPath || runPath != expectedPath {
		return VerifiedSetupBaselineArtifact{}, fmt.Errorf("setup baseline workflow definition/path does not match the exact run")
	}
	jobs, err := c.listAllSetupBaselineWorkflowJobs(ctx, owner, repo, request.WorkflowRunID)
	if err != nil {
		return VerifiedSetupBaselineArtifact{}, fmt.Errorf("read setup baseline workflow jobs: %w", err)
	}
	if err := validateSetupBaselineJobInventory(jobs, request.WorkflowRunID, request.CaptureHeadSHA, request.DefaultBranch); err != nil {
		return VerifiedSetupBaselineArtifact{}, err
	}
	artifact, err := c.findRunArtifact(ctx, owner, repo, request.WorkflowRunID, request.ArtifactID)
	if err != nil {
		return VerifiedSetupBaselineArtifact{}, err
	}
	if artifact.GetName() != request.ArtifactName || artifact.GetExpired() || artifact.GetSizeInBytes() <= 0 || artifact.GetSizeInBytes() > maxVisualHiveArtifactBytes {
		return VerifiedSetupBaselineArtifact{}, fmt.Errorf("setup baseline artifact name, expiry, or size is invalid")
	}
	if artifact.WorkflowRun != nil && ((artifact.WorkflowRun.GetID() != 0 && artifact.WorkflowRun.GetID() != request.WorkflowRunID) ||
		(artifact.WorkflowRun.GetRepositoryID() != 0 && artifact.WorkflowRun.GetRepositoryID() != repository.GetID()) ||
		(artifact.WorkflowRun.GetHeadSHA() != "" && !strings.EqualFold(artifact.WorkflowRun.GetHeadSHA(), request.CaptureHeadSHA))) {
		return VerifiedSetupBaselineArtifact{}, fmt.Errorf("setup baseline artifact workflow provenance mismatch")
	}
	root, err := c.downloadAndExtractArtifact(ctx, owner, repo, request.ArtifactID, request.DestinationDir, "setup-baseline-artifact")
	if err != nil {
		return VerifiedSetupBaselineArtifact{}, err
	}
	return VerifiedSetupBaselineArtifact{
		RepositoryID: request.RepositoryID, WorkflowRunID: request.WorkflowRunID, ArtifactID: request.ArtifactID,
		ArtifactName: artifact.GetName(), CaptureHead: strings.ToLower(request.CaptureHeadSHA), HeadBranch: run.GetHeadBranch(),
		WorkflowName: definition.GetName(), WorkflowPath: filepath.ToSlash(definitionPath), RunURL: run.GetHTMLURL(), ArtifactRoot: root,
	}, nil
}

func validateSetupBaselineWorkflowRun(run *gh.WorkflowRun, request SetupBaselineArtifactRequest) error {
	if run == nil || run.GetID() != request.WorkflowRunID || run.GetEvent() != "workflow_dispatch" || run.GetStatus() != "completed" || run.GetConclusion() != "success" ||
		!strings.EqualFold(run.GetHeadSHA(), request.CaptureHeadSHA) || run.GetHeadBranch() != request.DefaultBranch ||
		run.GetDisplayTitle() != request.ExpectedRunName || run.GetWorkflowID() <= 0 {
		return fmt.Errorf("setup baseline workflow run violates its exact event, head, ref, name, or successful conclusion binding")
	}
	return nil
}

func (c *Client) listAllSetupBaselineWorkflowJobs(ctx context.Context, owner, repo string, runID int64) (*gh.Jobs, error) {
	// "all" is mandatory: "latest" hides older rerun attempts and could make a
	// duplicate execution topology appear unique.
	options := &gh.ListWorkflowJobsOptions{Filter: "all", ListOptions: gh.ListOptions{PerPage: 100}}
	all := []*gh.WorkflowJob{}
	seen := map[int64]bool{}
	expectedTotal := -1
	for {
		page, response, err := c.client.Actions.ListWorkflowJobs(ctx, owner, repo, runID, options)
		if err != nil {
			return nil, fmt.Errorf("read setup baseline workflow jobs: %w", err)
		}
		if page == nil || page.GetTotalCount() < 0 || page.GetTotalCount() > 512 {
			return nil, fmt.Errorf("setup baseline workflow job inventory exceeds the strict 512-job bound")
		}
		if expectedTotal < 0 {
			expectedTotal = page.GetTotalCount()
		} else if page.GetTotalCount() != expectedTotal {
			return nil, fmt.Errorf("setup baseline workflow job total changed across pages")
		}
		for _, job := range page.Jobs {
			if job.GetID() <= 0 || seen[job.GetID()] {
				return nil, fmt.Errorf("setup baseline workflow job inventory contains a missing or duplicate job ID")
			}
			seen[job.GetID()] = true
			all = append(all, job)
			if len(all) > 512 {
				return nil, fmt.Errorf("setup baseline workflow job inventory exceeds the strict 512-job bound")
			}
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	if expectedTotal != len(all) {
		return nil, fmt.Errorf("setup baseline workflow job inventory is truncated or paginated incompletely")
	}
	return &gh.Jobs{TotalCount: gh.Ptr(len(all)), Jobs: all}, nil
}

// FindUniqueWorkflowRunArtifactByName exhaustively finds one exact artifact
// name across every page and rejects rerun/name duplicates.
func (c *Client) FindUniqueWorkflowRunArtifactByName(ctx context.Context, owner, repo string, runID int64, name string) (*gh.Artifact, error) {
	page, seen := 1, 0
	expectedTotal := int64(-1)
	seenPages := map[int]bool{}
	var matched *gh.Artifact
	for page > 0 {
		if page > 512 || seenPages[page] {
			return nil, fmt.Errorf("setup baseline artifact pagination is cyclic or exceeds its strict bound")
		}
		seenPages[page] = true
		artifacts, response, err := c.client.Actions.ListWorkflowRunArtifacts(ctx, owner, repo, runID, &gh.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return nil, fmt.Errorf("list setup baseline workflow artifacts: %w", err)
		}
		if artifacts == nil || artifacts.GetTotalCount() < 0 || artifacts.GetTotalCount() > 512 {
			return nil, fmt.Errorf("setup baseline artifact inventory exceeds the strict 512-artifact bound")
		}
		if expectedTotal < 0 {
			expectedTotal = artifacts.GetTotalCount()
		} else if artifacts.GetTotalCount() != expectedTotal {
			return nil, fmt.Errorf("setup baseline artifact total changed across pages")
		}
		seen += len(artifacts.Artifacts)
		if seen > 512 || int64(seen) > expectedTotal {
			return nil, fmt.Errorf("setup baseline artifact inventory exceeds the strict 512-artifact bound")
		}
		for _, artifact := range artifacts.Artifacts {
			if artifact.GetName() != name {
				continue
			}
			if matched != nil {
				return nil, fmt.Errorf("multiple setup baseline workflow artifacts are named %q", name)
			}
			matched = artifact
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		page = response.NextPage
	}
	if expectedTotal != int64(seen) {
		return nil, fmt.Errorf("setup baseline artifact inventory is truncated or paginated incompletely")
	}
	if matched == nil {
		return nil, fmt.Errorf("setup baseline workflow run %d has no artifact named %q", runID, name)
	}
	return matched, nil
}

func validateSetupBaselineJobInventory(jobs *gh.Jobs, workflowRunID int64, captureHead, defaultBranch string) error {
	if jobs == nil {
		return fmt.Errorf("setup baseline workflow job inventory is absent")
	}
	if jobs.GetTotalCount() != len(jobs.Jobs) || jobs.GetTotalCount() <= 0 || jobs.GetTotalCount() > 512 {
		return fmt.Errorf("setup baseline workflow job inventory is truncated, empty, or exceeds the strict 512-job bound")
	}
	wantJobs := map[string]bool{SetupBaselineCaptureJobDisplayName: false, SetupBaselineVerifyJobDisplayName: false}
	for _, job := range jobs.Jobs {
		if job.GetRunID() != 0 && job.GetRunID() != workflowRunID {
			return fmt.Errorf("setup baseline job belongs to a different workflow run")
		}
		if !strings.EqualFold(job.GetHeadSHA(), captureHead) || job.GetHeadBranch() != defaultBranch {
			return fmt.Errorf("setup baseline job head/ref mismatch")
		}
		if _, expected := wantJobs[job.GetName()]; expected {
			if wantJobs[job.GetName()] || job.GetStatus() != "completed" || job.GetConclusion() != "success" || !containsExactString(job.Labels, "ubuntu-latest") {
				return fmt.Errorf("setup baseline job %q lacks unique successful ubuntu-latest provenance", job.GetName())
			}
			wantJobs[job.GetName()] = true
			continue
		}
		if job.GetConclusion() != "skipped" {
			return fmt.Errorf("unexpected non-skipped job %q executed in setup baseline capture lane", job.GetName())
		}
	}
	for name, found := range wantJobs {
		if !found {
			return fmt.Errorf("setup baseline workflow lacks required successful job %q", name)
		}
	}
	return nil
}

func containsExactString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
