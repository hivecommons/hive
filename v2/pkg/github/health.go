package github

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

const (
	healthStatusSuccess  = 1
	healthStatusFailure  = 0
	healthStatusNotFound = -1
	ciRunsLimit          = 10
	pctMultiplier        = 100
)

func (c *Client) FetchWorkflowHealth(ctx context.Context) map[string]any {
	primaryRepo := c.primaryRepo()

	health := make(map[string]any)

	health["ci"] = c.ciPassRate(ctx, primaryRepo)
	health["brew"] = c.brewCheck(ctx, primaryRepo)
	health["helm"] = c.helmCheck(ctx, primaryRepo)

	nightlyWorkflows := map[string]string{
		"nightly":            "Nightly Test Suite",
		"nightlyCompliance":  "Nightly Compliance & Perf",
		"nightlyDashboard":   "Nightly Dashboard Health",
		"nightlyGhaw":        "Nightly gh-aw Version Check",
		"nightlyPlaywright":  "Playwright Cross-Browser (Nightly)",
	}
	for key, wfName := range nightlyWorkflows {
		health[key] = c.checkWorkflow(ctx, primaryRepo, wfName)
	}

	health["nightlyRel"] = c.releaseCheck(ctx, primaryRepo, false)
	health["weeklyRel"] = c.releaseCheck(ctx, primaryRepo, true)
	health["weekly"] = c.checkWorkflow(ctx, primaryRepo, "Weekly Coverage Review")
	health["hourly"] = c.perfCheck(ctx, primaryRepo)
	c.deployChecks(ctx, primaryRepo, health)

	return health
}

func (c *Client) primaryRepo() string {
	repos := c.getRepos()
	if len(repos) > 0 {
		return repos[0]
	}
	return "console"
}

func (c *Client) ciPassRate(ctx context.Context, repo string) int {
	opts := &gh.ListWorkflowRunsOptions{
		Status:      "completed",
		ListOptions: gh.ListOptions{PerPage: ciRunsLimit},
	}

	runs, _, err := c.client.Actions.ListRepositoryWorkflowRuns(ctx, c.org, repo, opts)
	if err != nil || runs == nil || len(runs.WorkflowRuns) == 0 {
		return healthStatusFailure
	}

	total := len(runs.WorkflowRuns)
	passed := 0
	for _, run := range runs.WorkflowRuns {
		conclusion := run.GetConclusion()
		if conclusion == "success" || conclusion == "skipped" {
			passed++
		}
	}

	if total == 0 {
		return healthStatusFailure
	}
	return passed * pctMultiplier / total
}

func (c *Client) checkWorkflow(ctx context.Context, repo, workflowName string) int {
	workflows, _, err := c.client.Actions.ListWorkflows(ctx, c.org, repo, &gh.ListOptions{PerPage: 100})
	if err != nil || workflows == nil {
		return healthStatusNotFound
	}

	var workflowID int64
	for _, wf := range workflows.Workflows {
		if wf.GetName() == workflowName {
			workflowID = wf.GetID()
			break
		}
	}
	if workflowID == 0 {
		return healthStatusNotFound
	}

	runs, _, err := c.client.Actions.ListWorkflowRunsByID(ctx, c.org, repo, workflowID, &gh.ListWorkflowRunsOptions{
		ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil || runs == nil || len(runs.WorkflowRuns) == 0 {
		return healthStatusNotFound
	}

	conclusion := runs.WorkflowRuns[0].GetConclusion()
	if conclusion == "failure" {
		return healthStatusFailure
	}
	return healthStatusSuccess
}

func (c *Client) brewCheck(ctx context.Context, primaryRepo string) int {
	brewTap := "homebrew-tap"
	hasTap := false
	for _, r := range c.getRepos() {
		if r == brewTap {
			hasTap = true
			break
		}
	}
	if !hasTap {
		return healthStatusNotFound
	}

	formulaContent, _, _, err := c.client.Repositories.GetContents(ctx, c.org, brewTap, "Formula/kubestellar-console.rb", nil)
	if err != nil || formulaContent == nil {
		formulaContent, _, _, err = c.client.Repositories.GetContents(ctx, c.org, brewTap, "Formula/kc-agent.rb", nil)
		if err != nil || formulaContent == nil {
			return healthStatusNotFound
		}
	}

	content, err := formulaContent.GetContent()
	if err != nil {
		return healthStatusNotFound
	}

	formulaVer := ""
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "version ") {
			formulaVer = strings.Trim(strings.TrimPrefix(trimmed, "version "), "\"' ")
			formulaVer = strings.TrimPrefix(formulaVer, "v")
			break
		}
	}

	// The tap's formulas track nightly releases, which GetLatestRelease
	// excludes (prereleases). Comparing only against the latest stable made
	// a fresher-than-stable formula fail forever. Accept a match against
	// any recent release tag — stable or nightly.
	const brewReleaseWindow = 20 // recent releases to compare against
	releases, _, err := c.client.Repositories.ListReleases(ctx, c.org, primaryRepo,
		&gh.ListOptions{PerPage: brewReleaseWindow})
	if err != nil || len(releases) == 0 {
		return healthStatusNotFound
	}

	for _, release := range releases {
		if formulaVer == strings.TrimPrefix(release.GetTagName(), "v") {
			return healthStatusSuccess
		}
	}
	return healthStatusFailure
}

func (c *Client) helmCheck(ctx context.Context, repo string) int {
	_, _, _, err := c.client.Repositories.GetContents(ctx, c.org, repo, "deploy/helm/kubestellar-console/Chart.yaml", nil)
	if err != nil {
		return healthStatusNotFound
	}
	return healthStatusSuccess
}

func (c *Client) releaseCheck(ctx context.Context, repo string, weekly bool) int {
	workflows, _, err := c.client.Actions.ListWorkflows(ctx, c.org, repo, &gh.ListOptions{PerPage: 100})
	if err != nil || workflows == nil {
		return healthStatusNotFound
	}

	var workflowID int64
	for _, wf := range workflows.Workflows {
		if wf.GetName() == "Release" {
			workflowID = wf.GetID()
			break
		}
	}
	if workflowID == 0 {
		return healthStatusNotFound
	}

	runs, _, err := c.client.Actions.ListWorkflowRunsByID(ctx, c.org, repo, workflowID, &gh.ListWorkflowRunsOptions{
		Event:       "schedule",
		ListOptions: gh.ListOptions{PerPage: ciRunsLimit},
	})
	if err != nil || runs == nil || len(runs.WorkflowRuns) == 0 {
		return healthStatusNotFound
	}

	for _, run := range runs.WorkflowRuns {
		createdAt := run.GetCreatedAt().Time
		isSunday := createdAt.Weekday() == 0
		if weekly && isSunday {
			if run.GetConclusion() == "success" {
				return healthStatusSuccess
			}
			return healthStatusFailure
		}
		if !weekly && !isSunday {
			if run.GetConclusion() == "success" {
				return healthStatusSuccess
			}
			return healthStatusFailure
		}
	}

	return healthStatusNotFound
}

func (c *Client) perfCheck(ctx context.Context, repo string) int {
	perfWorkflows := []string{
		"Perf — React commits per navigation",
		"Performance TTFI Gate",
	}

	for _, wfName := range perfWorkflows {
		result := c.checkWorkflow(ctx, repo, wfName)
		if result == healthStatusFailure {
			return healthStatusFailure
		}
		// Not-found workflows are ignored (not treated as failure)
	}
	return healthStatusSuccess
}

func (c *Client) deployChecks(ctx context.Context, repo string, health map[string]any) {
	ciWorkflow := "Build and Deploy KC"

	workflows, _, err := c.client.Actions.ListWorkflows(ctx, c.org, repo, &gh.ListOptions{PerPage: 100})
	if err != nil || workflows == nil {
		health["deploy_vllm_d"] = healthStatusNotFound
		health["deploy_pok_prod"] = healthStatusNotFound
		return
	}

	var workflowID int64
	for _, wf := range workflows.Workflows {
		if wf.GetName() == ciWorkflow {
			workflowID = wf.GetID()
			break
		}
	}
	if workflowID == 0 {
		health["deploy_vllm_d"] = healthStatusNotFound
		health["deploy_pok_prod"] = healthStatusNotFound
		return
	}

	runs, _, err := c.client.Actions.ListWorkflowRunsByID(ctx, c.org, repo, workflowID, &gh.ListWorkflowRunsOptions{
		Branch:      "main",
		Event:       "push",
		Status:      "completed",
		ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil || runs == nil || len(runs.WorkflowRuns) == 0 {
		health["deploy_vllm_d"] = healthStatusNotFound
		health["deploy_pok_prod"] = healthStatusNotFound
		return
	}

	runID := runs.WorkflowRuns[0].GetID()
	jobs, _, err := c.client.Actions.ListWorkflowJobs(ctx, c.org, repo, runID, &gh.ListWorkflowJobsOptions{
		ListOptions: gh.ListOptions{PerPage: 50},
	})

	deployJobs := map[string]string{
		"deploy_vllm_d":    "deploy-vllm-d",
		"deploy_pok_prod":  "deploy-pok-prod",
	}

	for key := range deployJobs {
		health[key] = healthStatusNotFound
	}

	if err != nil || jobs == nil {
		return
	}

	for _, job := range jobs.Jobs {
		for key, jobName := range deployJobs {
			if job.GetName() == jobName {
				if job.GetConclusion() == "failure" {
					health[key] = healthStatusFailure
				} else {
					health[key] = healthStatusSuccess
				}
			}
		}
	}
}
