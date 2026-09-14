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

// ciStreakRunsLimit is how many completed default-branch runs the green-streak
// scan reads. It is deliberately larger than ciRunsLimit (which powers the
// pass-rate percentage over a short recent window): the ACMM advisor's highest
// streak threshold is 12 consecutive green runs, so a 10-run page could never
// satisfy it and the criterion would be unreachable no matter how green the
// repo actually is. One page of this size covers every threshold with headroom
// while remaining a single API request.
const ciStreakRunsLimit = 30

// ciConclusionSuccess / ciConclusionSkipped are the run conclusions that count
// as "not red" for streak purposes. A skipped run means no gate ran, so it
// neither proves nor breaks greenness — it is transparent to the streak, the
// same treatment ciPassRate already gives it.
const (
	ciConclusionSuccess = "success"
	ciConclusionSkipped = "skipped"
)

func (c *Client) FetchWorkflowHealth(ctx context.Context) map[string]any {
	primaryRepo := c.primaryRepo()

	health := make(map[string]any)

	health["ci"] = c.ciPassRate(ctx, primaryRepo)
	health["brew"] = c.brewCheck(ctx, primaryRepo)
	health["helm"] = c.helmCheck(ctx, primaryRepo)

	nightlyWorkflows := map[string]string{
		"nightly":           "Nightly Test Suite",
		"nightlyCompliance": "Nightly Compliance & Perf",
		"nightlyDashboard":  "Nightly Dashboard Health",
		"nightlyGhaw":       "Nightly gh-aw Version Check",
		"nightlyPlaywright": "Playwright Cross-Browser (Nightly)",
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
		if conclusion == ciConclusionSuccess || conclusion == ciConclusionSkipped {
			passed++
			continue
		}
		// #6936: a completed run that never scheduled a JOB executed none of
		// this repository's code, so it is not evidence about CI health in
		// either direction — it is dropped from the sample rather than counted
		// as a failure. `tagged-release.yml` strands five of these per release
		// (see #6084), and with ciRunsLimit at 10 that is half the window: the
		// v4.32.1 release drove this function to 20% at 17:51:36Z and held it
		// at 40% for over a minute while CI was in fact entirely healthy.
		if c.runScheduledNoJobs(ctx, repo, run.GetID()) {
			total--
		}
	}

	// Either the API returned nothing, or every run in the window was dropped
	// as no-evidence. Reported the same way an empty result already is.
	if total == 0 {
		return healthStatusFailure
	}
	return passed * pctMultiplier / total
}

// runScheduledNoJobs reports whether a completed run finished without ever
// scheduling a job — GitHub's zero-job "startup failure" shape (#6936).
//
// It FAILS CLOSED: any API error or nil payload returns false, which keeps the
// run counted exactly as it was before this check existed. A transient jobs-API
// error can therefore only restore the old behaviour, never silently inflate
// the pass rate by discarding runs that did real work.
//
// Cost is bounded and small: it is consulted only for runs that would otherwise
// count against the rate, so a healthy window makes zero extra calls and the
// worst case is ciRunsLimit. PerPage is 1 because only the total is read.
func (c *Client) runScheduledNoJobs(ctx context.Context, repo string, runID int64) bool {
	jobs, _, err := c.client.Actions.ListWorkflowJobs(ctx, c.org, repo, runID, &gh.ListWorkflowJobsOptions{
		ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil || jobs == nil {
		return false
	}
	return jobs.GetTotalCount() == 0
}

// GreenCIStreak returns the number of consecutive green CI runs on the primary
// repo's default branch, counting back from the most recent completed run, and
// whether the streak could be measured at all.
//
// measured=false means "unknown" and MUST be treated as such by callers — it is
// returned when there is no GitHub client, when the Actions API call fails, and
// when the repo reports no completed runs at all. That last case is the
// important one: a repo with no CI configured must read as unknown, never as a
// green streak, or the advisor would reward a repo for having no gates to fail.
// Callers leave the advisor signal at its conservative zero when measured is
// false rather than fabricating a number.
//
// What it measures, honestly: consecutive non-red completed runs on the default
// branch, over at most ciStreakRunsLimit runs. Its known weaknesses, stated in
// the same spirit as mergeSuccessRateFromFleetStats:
//   - the streak resets on ANY red, including a flake unrelated to code quality;
//   - it measures nothing about repos whose CI does not run on the default
//     branch (they read as unknown, which is the safe direction);
//   - a repo that rarely commits can hold a stale streak indefinitely, because
//     the signal is count-based rather than time-based;
//   - it is capped at ciStreakRunsLimit, so it reports "at least N" rather than
//     a true unbounded streak. The cap sits above every advisor threshold, so
//     the cap can only ever under-report, never over-report.
func (c *Client) GreenCIStreak(ctx context.Context) (streak int, measured bool) {
	if c == nil || c.client == nil {
		return 0, false
	}
	repo := c.primaryRepo()
	branch, err := c.DefaultBranch(ctx, c.org, repo)
	if err != nil || branch == "" {
		return 0, false
	}
	opts := &gh.ListWorkflowRunsOptions{
		Status:      "completed",
		Branch:      branch,
		ListOptions: gh.ListOptions{PerPage: ciStreakRunsLimit},
	}
	runs, _, err := c.client.Actions.ListRepositoryWorkflowRuns(ctx, c.org, repo, opts)
	if err != nil || runs == nil {
		return 0, false
	}
	// No completed runs at all: the repo has no CI history on its default
	// branch. Unknown, explicitly NOT a zero-length green streak.
	if len(runs.WorkflowRuns) == 0 {
		return 0, false
	}
	// ListRepositoryWorkflowRuns returns runs newest-first, so counting from
	// the head of the slice walks backwards in time from the latest run. Stop
	// at the first red — that is where the streak ended.
	for _, run := range runs.WorkflowRuns {
		if run == nil {
			continue
		}
		switch run.GetConclusion() {
		case ciConclusionSuccess, ciConclusionSkipped:
			streak++
		default:
			return streak, true
		}
	}
	return streak, true
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
		"deploy_vllm_d":   "deploy-vllm-d",
		"deploy_pok_prod": "deploy-pok-prod",
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
