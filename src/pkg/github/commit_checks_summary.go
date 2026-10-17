package github

import (
	"context"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// CommitChecksSummary is the human-facing CI digest for one head: how many
// gating checks exist, how many are green, which are red, and what the DCO
// context said. It feeds the recommend/* label (hivecommons/hive#12528),
// not the merge gate — EvaluateCommitCI stays the gate. Merge-metadata
// contexts (tide, enforce-guardrails, …) and the ignorable CI classes are
// excluded from the counts; the DCO context is reported separately.
type CommitChecksSummary struct {
	Total, Green, Pending int
	Red                   []string
	// DCO is 1 when a DCO context succeeded, -1 when one failed, 0 when the
	// head carries none.
	DCO int
}

// SummarizeCommitChecks reads the combined status and check runs for sha
// and folds them into a CommitChecksSummary. Check runs are deduplicated to
// the latest per (name, app) the way the gate does, so a re-run replaces
// its predecessor rather than counting twice.
func SummarizeCommitChecks(ctx context.Context, client *gh.Client, owner, repo, sha string) (CommitChecksSummary, error) {
	var out CommitChecksSummary
	if client == nil {
		return out, ErrNoGitHubClient
	}
	statusOpts := &gh.ListOptions{PerPage: 100}
	var statuses []*gh.RepoStatus
	for {
		status, resp, err := client.Repositories.GetCombinedStatus(ctx, owner, repo, sha, statusOpts)
		if err != nil {
			return out, err
		}
		statuses = append(statuses, status.Statuses...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		statusOpts.Page = resp.NextPage
	}
	checkOpts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var runs []*gh.CheckRun
	for {
		checkRuns, resp, err := client.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, checkOpts)
		if err != nil {
			return out, err
		}
		if checkRuns != nil {
			runs = append(runs, checkRuns.CheckRuns...)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		checkOpts.Page = resp.NextPage
	}
	return summarizeChecks(statuses, runs), nil
}

func summarizeChecks(statuses []*gh.RepoStatus, runs []*gh.CheckRun) CommitChecksSummary {
	var out CommitChecksSummary
	if dcoContextSucceeded(statuses, runs) {
		out.DCO = 1
	}
	red := map[string]bool{}
	for _, s := range statuses {
		name := strings.TrimSpace(s.GetContext())
		if isDCOContext(name) {
			if out.DCO == 0 && (s.GetState() == "failure" || s.GetState() == "error") {
				out.DCO = -1
			}
			continue
		}
		if isIgnorableCICheck(name) {
			continue
		}
		out.Total++
		switch s.GetState() {
		case "success":
			out.Green++
		case "pending":
			out.Pending++
		default:
			red[name] = true
		}
	}
	for _, cr := range latestCheckRunsByNameAndApp(runs) {
		name := strings.TrimSpace(cr.GetName())
		if isDCOContext(name) {
			if out.DCO == 0 && cr.GetStatus() == "completed" && checkRunConclusionRed(cr.GetConclusion()) {
				out.DCO = -1
			}
			continue
		}
		if isIgnorableCICheck(name) {
			continue
		}
		out.Total++
		switch {
		case cr.GetStatus() != "completed":
			out.Pending++
		case checkRunConclusionRed(cr.GetConclusion()):
			red[name] = true
		default:
			// success, neutral, skipped: nothing left for a human to fix.
			out.Green++
		}
	}
	for name := range red {
		out.Red = append(out.Red, name)
	}
	sort.Strings(out.Red)
	return out
}

// checkRunConclusionRed reports whether a completed check-run conclusion is
// one a human would read as failed.
func checkRunConclusionRed(conclusion string) bool {
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale":
		return true
	}
	return false
}
