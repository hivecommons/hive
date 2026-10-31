package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const (
	defaultCIReportingRecheck = 5 * time.Minute
	defaultCIReportingGrace   = 5 * time.Minute
	ciReportingEventRecheck   = time.Minute
	ciReportingMaxEntries     = 1024
	ciReportingMaxRunPages    = 3
	ciReportingMaxJobPages    = 3
	ciReportingMaxChecks      = 64
	ciReportingMaxRuns        = 12
)

// CIReportingObservation describes reporting, never a replacement CI verdict.
// Even a successful owning workflow cannot satisfy an unfinished required check.
type CIReportingObservation struct {
	Repo          string    `json:"repo"`
	PRNumber      int       `json:"pr_number"`
	State         string    `json:"state"`
	HeadSHA       string    `json:"head_sha"`
	Check         string    `json:"check"`
	CheckRunID    int64     `json:"check_run_id,omitempty"`
	CheckStatus   string    `json:"check_status"`
	RunID         int64     `json:"run_id,omitempty"`
	Attempt       int       `json:"attempt,omitempty"`
	JobID         int64     `json:"job_id,omitempty"`
	JobStatus     string    `json:"job_status,omitempty"`
	JobConclusion string    `json:"job_conclusion,omitempty"`
	RunConclusion string    `json:"run_conclusion,omitempty"`
	RunUpdatedAt  time.Time `json:"run_updated_at,omitzero"`
	RunURL        string    `json:"run_url,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
	Detail        string    `json:"detail,omitempty"`
	Recovery      string    `json:"recovery"`
}

type ciReportingEntry struct {
	repo, head   string
	checkedAt    time.Time
	busy, dirty  bool
	observations []CIReportingObservation
	suspicions   map[string]time.Time
}

type ciReportingCache struct {
	mu      sync.Mutex
	entries map[string]*ciReportingEntry
}

func newCIReportingCache() *ciReportingCache {
	return &ciReportingCache{entries: make(map[string]*ciReportingEntry)}
}

var sharedCIReportingCache = newCIReportingCache()

func ciReportingKey(client *gh.Client, owner, repo, sha string, number int, checks []*gh.CheckRun) string {
	var fingerprint strings.Builder
	for _, check := range checks {
		fmt.Fprintf(&fingerprint, "/%d:%s:%s", check.GetID(), check.GetName(), check.GetStatus())
	}
	return fmt.Sprintf("%p:%s:%s/%s#%d@%s%s", client, client.BaseURL,
		strings.ToLower(owner), strings.ToLower(repo), number, sha, fingerprint.String())
}

func (c *ciReportingCache) invalidate(repo, sha string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		if entry.repo == strings.ToLower(repo) && entry.head == sha {
			entry.dirty = true
		}
	}
}

func (c *ciReportingCache) observe(ctx context.Context, client *gh.Client, owner, repo, sha string, opts CommitCIOptions, checks []*gh.CheckRun) []CIReportingObservation {
	if opts.PRNumber <= 0 || !opts.RequiredKnown || len(checks) == 0 {
		return nil
	}
	if len(checks) > ciReportingMaxChecks {
		return []CIReportingObservation{{State: "unverified", HeadSHA: sha,
			Detail: "observation-check-capacity", Recovery: "owner-action-required"}}
	}
	sort.Slice(checks, func(i, j int) bool {
		if checks[i].GetName() != checks[j].GetName() {
			return checks[i].GetName() < checks[j].GetName()
		}
		return checks[i].GetID() < checks[j].GetID()
	})
	now := time.Now()
	if opts.Now != nil {
		now = opts.Now()
	}
	recheck := durationFromEnv("HIVE_CI_REPORTING_RECHECK", defaultCIReportingRecheck)
	grace := durationFromEnv("HIVE_CI_REPORTING_GRACE", defaultCIReportingGrace)
	if recheck <= 0 {
		recheck = defaultCIReportingRecheck
	}
	if grace <= 0 {
		grace = defaultCIReportingGrace
	}
	key := ciReportingKey(client, owner, repo, sha, opts.PRNumber, checks)
	c.mu.Lock()
	entry := c.entries[key]
	if entry != nil {
		if entry.busy {
			out := append([]CIReportingObservation(nil), entry.observations...)
			c.mu.Unlock()
			return out
		}
		interval := recheck
		if entry.dirty && interval > ciReportingEventRecheck {
			interval = ciReportingEventRecheck
		}
		if !now.Before(entry.checkedAt) && now.Sub(entry.checkedAt) < interval {
			out := append([]CIReportingObservation(nil), entry.observations...)
			c.mu.Unlock()
			return out
		}
		if now.Before(entry.checkedAt) {
			entry.suspicions = nil
		}
	} else {
		if len(c.entries) >= ciReportingMaxEntries {
			var oldestKey string
			var oldest time.Time
			for candidate, value := range c.entries {
				if !value.busy && (oldestKey == "" || value.checkedAt.Before(oldest)) {
					oldestKey, oldest = candidate, value.checkedAt
				}
			}
			if oldestKey == "" {
				c.mu.Unlock()
				return []CIReportingObservation{{State: "unverified", HeadSHA: sha, ObservedAt: now,
					Detail: "observation-capacity", Recovery: "owner-action-required"}}
			}
			delete(c.entries, oldestKey)
		}
		entry = &ciReportingEntry{repo: strings.ToLower(owner + "/" + repo), head: sha}
		c.entries[key] = entry
	}
	entry.busy = true
	entry.dirty = false
	c.mu.Unlock()

	observations := observeCIReporting(ctx, client, owner, repo, sha, opts.PRNumber, checks, now)
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]time.Time)
	for i := range observations {
		observation := &observations[i]
		if observation.State != "contradiction" {
			continue
		}
		signature := fmt.Sprintf("%d:%d:%d:%d:%s:%s:%s:%s:%s", observation.CheckRunID,
			observation.RunID, observation.Attempt, observation.JobID, observation.CheckStatus,
			observation.JobStatus, observation.JobConclusion, observation.RunConclusion,
			observation.RunUpdatedAt.UTC().Format(time.RFC3339Nano))
		first, seen := entry.suspicions[signature]
		if !seen || now.Before(first) {
			first = now
		}
		next[signature] = first
		if seen && now.Sub(first) >= grace {
			observation.State = "stalled"
			observation.Recovery = "owner-action-required"
			observation.Detail = "terminal workflow still reports an unfinished required check; inspect the latest attempt before one owner-authorized affected-workflow rerun"
		} else {
			observation.State = "propagation-grace"
			observation.Recovery = "awaiting-consistency"
			observation.Detail = "terminal workflow/check mismatch awaiting an independent consistency observation"
		}
	}
	entry.checkedAt, entry.busy = now, false
	entry.suspicions = next
	entry.observations = append([]CIReportingObservation(nil), observations...)
	return observations
}

func observeCIReporting(ctx context.Context, client *gh.Client, owner, repo, sha string, number int, checks []*gh.CheckRun, now time.Time) []CIReportingObservation {
	ctx = WithRESTCaller(ctx, "hive:ci_reporting")
	out := make([]CIReportingObservation, len(checks))
	actions := false
	for i, check := range checks {
		out[i] = CIReportingObservation{Repo: owner + "/" + repo, PRNumber: number,
			State: "unverified", HeadSHA: sha, Check: check.GetName(),
			CheckRunID: check.GetID(), CheckStatus: check.GetStatus(), ObservedAt: now,
			Detail: "missing-actions-provenance", Recovery: "owner-inspection-required"}
		if check.GetApp().GetSlug() != "github-actions" {
			if check.GetApp().GetSlug() != "" {
				out[i].State = "unsupported-provider"
				out[i].Recovery = "unsupported-provider"
			}
			continue
		}
		if check.GetHeadSHA() == sha && check.GetID() > 0 && check.GetCheckSuite().GetID() > 0 {
			actions = true
		}
	}
	if !actions {
		return out
	}
	unverified := func(detail string) []CIReportingObservation {
		for i := range out {
			out[i].State, out[i].Detail = "unverified", detail
			out[i].Recovery = "owner-inspection-required"
		}
		return out
	}
	pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return unverified("head-read: " + ciReportingAPIError(err))
	}
	if pr.GetHead().GetSHA() != sha || pr.GetState() != "open" {
		return unverified("head-changed-or-closed")
	}
	base := pr.GetBase().GetRef()
	var runs []*gh.WorkflowRun
	options := &gh.ListWorkflowRunsOptions{HeadSHA: sha, ListOptions: gh.ListOptions{PerPage: workflowRunsPerPage}}
	for page := 0; page < ciReportingMaxRunPages; page++ {
		list, response, err := client.Actions.ListRepositoryWorkflowRuns(ctx, owner, repo, options)
		if err != nil {
			return unverified("workflow-runs: " + ciReportingAPIError(err))
		}
		if list != nil {
			runs = append(runs, list.WorkflowRuns...)
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		if page == ciReportingMaxRunPages-1 {
			return unverified("workflow-run-pagination-incomplete")
		}
		options.Page = response.NextPage
	}
	jobsByRun := make(map[int64][]*gh.WorkflowJob)
	currentByRun := make(map[int64]*gh.WorkflowRun)
	contradiction := false
	for i, check := range checks {
		if check.GetApp().GetSlug() != "github-actions" || check.GetHeadSHA() != sha || check.GetID() <= 0 || check.GetCheckSuite().GetID() <= 0 {
			continue
		}
		var candidate *gh.WorkflowRun
		for _, run := range runs {
			if run != nil && run.GetHeadSHA() == sha && run.GetCheckSuiteID() == check.GetCheckSuite().GetID() {
				candidate = run
				break
			}
		}
		if candidate == nil || candidate.GetID() <= 0 || candidate.GetRunAttempt() <= 0 ||
			candidate.GetWorkflowID() <= 0 || candidate.GetRunNumber() <= 0 || candidate.GetEvent() == "" ||
			!strings.EqualFold(candidate.GetRepository().GetFullName(), owner+"/"+repo) {
			out[i].Detail = "owning-workflow-not-verified"
			continue
		}
		out[i].RunID, out[i].Attempt, out[i].RunURL = candidate.GetID(), candidate.GetRunAttempt(), candidate.GetHTMLURL()
		superseded := false
		for _, other := range runs {
			if other == nil || other.GetHeadSHA() != sha || other.GetWorkflowID() != candidate.GetWorkflowID() ||
				other.GetEvent() != candidate.GetEvent() {
				continue
			}
			if other.GetRunNumber() > candidate.GetRunNumber() ||
				(other.GetID() == candidate.GetID() && other.GetRunAttempt() > candidate.GetRunAttempt()) {
				superseded = true
			}
		}
		if superseded {
			out[i].State, out[i].Detail = "superseded", "newer workflow execution owns current evidence"
			out[i].Recovery = "awaiting-current-evidence"
			continue
		}
		if candidate.GetStatus() != "completed" {
			switch candidate.GetStatus() {
			case "queued", "in_progress", "waiting", "requested", "pending":
				out[i].State, out[i].Detail = "active", "owning workflow is queued or running; elapsed time alone is not stale"
				out[i].Recovery = "none"
			default:
				out[i].Detail = "owning-workflow-status-unknown"
			}
			continue
		}
		if candidate.GetConclusion() == "" || candidate.GetUpdatedAt().IsZero() {
			out[i].Detail = "terminal-workflow-conclusion-unknown"
			continue
		}
		switch candidate.GetConclusion() {
		case "success", "failure", "cancelled", "timed_out", "neutral", "skipped", "action_required", "startup_failure":
		default:
			out[i].Detail = "terminal-workflow-conclusion-unknown"
			continue
		}
		jobs, known := jobsByRun[candidate.GetID()]
		if !known {
			if len(jobsByRun) >= ciReportingMaxRuns {
				out[i].Detail = "observation-workflow-capacity"
				continue
			}
			jobOptions := &gh.ListOptions{PerPage: workflowRunsPerPage}
			for page := 0; page < ciReportingMaxJobPages; page++ {
				list, response, err := client.Actions.ListWorkflowJobsAttempt(ctx, owner, repo, candidate.GetID(), int64(candidate.GetRunAttempt()), jobOptions)
				if err != nil {
					return unverified("latest-attempt-jobs: " + ciReportingAPIError(err))
				}
				if list != nil {
					jobs = append(jobs, list.Jobs...)
				}
				if response == nil || response.NextPage == 0 {
					break
				}
				if page == ciReportingMaxJobPages-1 {
					return unverified("workflow-job-pagination-incomplete")
				}
				jobOptions.Page = response.NextPage
			}
			jobsByRun[candidate.GetID()] = jobs
		}
		var matched *gh.WorkflowJob
		for _, job := range jobs {
			if job != nil && job.GetID() > 0 && job.GetRunID() == candidate.GetID() &&
				job.GetRunAttempt() == int64(candidate.GetRunAttempt()) && job.GetHeadSHA() == sha &&
				ciReportingJobCheck(client, owner, repo, job.GetCheckRunURL(), check.GetID()) {
				matched = job
				break
			}
		}
		if matched == nil || matched.GetStatus() == "" {
			out[i].Detail = "check-not-in-latest-attempt"
			continue
		}
		switch matched.GetStatus() {
		case "queued", "in_progress", "completed", "waiting", "requested", "pending":
		default:
			out[i].Detail = "owning-job-status-unknown"
			continue
		}
		current, known := currentByRun[candidate.GetID()]
		if !known {
			var err error
			current, _, err = client.Actions.GetWorkflowRunByID(ctx, owner, repo, candidate.GetID())
			if err != nil {
				return unverified("workflow-consistency-read: " + ciReportingAPIError(err))
			}
			currentByRun[candidate.GetID()] = current
		}
		if current.GetID() != candidate.GetID() || current.GetHeadSHA() != sha ||
			!strings.EqualFold(current.GetRepository().GetFullName(), owner+"/"+repo) ||
			current.GetWorkflowID() != candidate.GetWorkflowID() || current.GetEvent() != candidate.GetEvent() ||
			current.GetRunNumber() != candidate.GetRunNumber() ||
			current.GetCheckSuiteID() != candidate.GetCheckSuiteID() ||
			current.GetRunAttempt() != candidate.GetRunAttempt() || current.GetStatus() != "completed" ||
			current.GetConclusion() != candidate.GetConclusion() ||
			!current.GetUpdatedAt().Time.Equal(candidate.GetUpdatedAt().Time) {
			out[i].Detail = "workflow-attempt-changed-during-observation"
			continue
		}
		out[i].JobID, out[i].JobStatus = matched.GetID(), matched.GetStatus()
		out[i].JobConclusion = matched.GetConclusion()
		out[i].RunConclusion = current.GetConclusion()
		out[i].RunUpdatedAt = current.GetUpdatedAt().Time
		out[i].State = "contradiction"
		contradiction = true
	}
	if contradiction {
		pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
		if err != nil {
			return unverified("final-head-read: " + ciReportingAPIError(err))
		}
		if pr.GetHead().GetSHA() != sha || pr.GetState() != "open" || pr.GetBase().GetRef() != base {
			return unverified("head-changed-or-closed-during-observation")
		}
	}
	return out
}

func ciReportingJobCheck(client *gh.Client, owner, repo, raw string, checkID int64) bool {
	link, err := url.Parse(raw)
	if err != nil || link.User != nil || client.BaseURL == nil || link.Host != client.BaseURL.Host || link.Scheme != client.BaseURL.Scheme {
		return false
	}
	expected := strings.TrimSuffix(client.BaseURL.Path, "/") + "/repos/" + owner + "/" + repo + "/check-runs/" + strconv.FormatInt(checkID, 10)
	return link.Path == expected && link.RawQuery == "" && link.Fragment == ""
}

func ciReportingAPIError(err error) string {
	var response *gh.ErrorResponse
	if errors.As(err, &response) && response.Response != nil {
		return fmt.Sprintf("api-http-%d (permission/rate-limit/unavailable; not a CI verdict)", response.Response.StatusCode)
	}
	return "api-unavailable (not a CI verdict)"
}

func ciReportingReason(observations []CIReportingObservation) string {
	for _, observation := range observations {
		if observation.State == "stalled" {
			return fmt.Sprintf("ci-reporting-stalled:check=%q check-id=%d run=%d attempt=%d job=%d check-state=%s job-state=%s job-result=%q workflow=%s owner-action-required",
				observation.Check, observation.CheckRunID, observation.RunID, observation.Attempt,
				observation.JobID, observation.CheckStatus, observation.JobStatus, observation.JobConclusion, observation.RunConclusion)
		}
	}
	for _, observation := range observations {
		if observation.State == "unverified" && observation.Detail != "missing-actions-provenance" {
			return fmt.Sprintf("ci-reporting-unverified:check=%q %s owner-action-required", observation.Check, observation.Detail)
		}
	}
	return ""
}
