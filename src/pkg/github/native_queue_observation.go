package github

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	forgequeue "github.com/hivecommons/hive/pkg/forgestate"
)

const (
	nativeObservationPageSize   = 100
	nativeObservationMaxGroups  = 4
	nativeObservationMaxMembers = 8
)

// ReadNativeQueueObservation is bounded, read-only and never grants merge
// authority. Only signed merge_group event identities supply a group's base;
// workflow runs can discover candidate SHAs but cannot prove membership.
func (c *Client) ReadNativeQueueObservation(ctx context.Context, repo, base string, hints []forgequeue.QueueCandidate) (forgequeue.QueueObservation, error) {
	out := forgequeue.QueueObservation{Provider: "github-native", Repo: repo, Base: base,
		State: "unavailable", ObservedAt: time.Now(), Entries: []forgequeue.QueueEntry{},
		Candidates: []forgequeue.QueueCandidate{}, Outcomes: []forgequeue.QueueOutcome{}}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || base == "" {
		return out, fmt.Errorf("native queue scope unavailable")
	}
	var data struct {
		Repository *struct {
			MergeQueue *struct {
				Entries struct {
					PageInfo struct{ HasNextPage bool }
					Nodes    []struct {
						ID          string
						Position    int
						State       string
						EnqueuedAt  time.Time
						PullRequest struct {
							Number      int
							HeadRefOID  string
							BaseRefName string
						}
					}
				}
			}
		}
	}
	if err := c.graphQL(ctx, `query($owner:String!,$name:String!,$base:String!,$limit:Int!) {
 repository(owner:$owner,name:$name) { mergeQueue(branch:$base) {
 entries(first:$limit) { pageInfo { hasNextPage } nodes {
 id position state enqueuedAt pullRequest { number headRefOid baseRefName }
 } } } } }`, map[string]any{"owner": parts[0], "name": parts[1], "base": base, "limit": nativeObservationPageSize}, &data); err != nil {
		return out, err
	}
	if data.Repository == nil {
		return out, fmt.Errorf("native queue repository evidence unavailable")
	}
	if data.Repository.MergeQueue == nil {
		out.State, out.Complete = "not-enabled", true
		out.EntriesComplete = true
		return out, nil
	}
	out.State, out.Complete = "observed", !data.Repository.MergeQueue.Entries.PageInfo.HasNextPage
	out.EntriesComplete = out.Complete
	if !out.Complete {
		out.Reason = "Queue exceeds bounded page; order and membership coverage are partial."
	}
	seenIDs, seenPRs := map[string]bool{}, map[int]bool{}
	for _, entry := range data.Repository.MergeQueue.Entries.Nodes {
		if entry.ID == "" || entry.Position < 1 || entry.PullRequest.Number < 1 || !integrationOID(entry.PullRequest.HeadRefOID) ||
			entry.PullRequest.BaseRefName != base || seenIDs[entry.ID] || seenPRs[entry.PullRequest.Number] {
			out.Complete = false
			out.EntriesComplete = false
			out.Reason = "Queue contains incomplete entries."
			continue
		}
		seenIDs[entry.ID], seenPRs[entry.PullRequest.Number] = true, true
		e := forgequeue.QueueEntry{ID: entry.ID, Position: entry.Position, PR: entry.PullRequest.Number,
			Head: entry.PullRequest.HeadRefOID, State: entry.State}
		if !entry.EnqueuedAt.IsZero() {
			at := entry.EnqueuedAt
			e.EnqueuedAt = &at
		}
		out.Entries = append(out.Entries, e)
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Position < out.Entries[j].Position })
	for index, entry := range out.Entries {
		if entry.Position != index+1 {
			out.Complete = false
			out.EntriesComplete = false
			out.Reason = "Queue positions contain gaps; snapshot coverage is partial."
		}
	}
	// Candidate identity comes from actual merge_group runs/events, never FIFO
	// seats or the PR number encoded in a synthetic ref.
	candidates := make(map[string]forgequeue.QueueCandidate)
	order := []string{}
	hintBySHA := make(map[string]forgequeue.QueueCandidate)
	for _, hint := range hints {
		if integrationOID(hint.SHA) && strings.HasPrefix(hint.Ref, "refs/heads/gh-readonly-queue/"+base+"/") {
			hintBySHA[hint.SHA] = hint
		}
	}
	runs, response, runErr := c.client.Actions.ListRepositoryWorkflowRuns(ctx, parts[0], parts[1], &gh.ListWorkflowRunsOptions{
		Event: "merge_group", ListOptions: gh.ListOptions{PerPage: nativeObservationPageSize},
	})
	if runErr != nil || runs == nil {
		out.Complete = false
		out.Reason = "Candidate workflow discovery unavailable; signed-event candidates may still be observed."
	} else {
		if response != nil && response.NextPage != 0 {
			out.Complete = false
			out.Reason = "Candidate workflow listing exceeds bounded page."
		}
		for _, run := range runs.WorkflowRuns {
			if run.GetEvent() != "merge_group" || !integrationOID(run.GetHeadSHA()) ||
				!strings.HasPrefix(run.GetHeadBranch(), "gh-readonly-queue/"+base+"/") {
				continue
			}
			if _, exists := candidates[run.GetHeadSHA()]; exists {
				continue
			}
			if len(candidates) >= nativeObservationMaxGroups {
				out.Complete = false
				out.Reason = "Candidate cohort exceeds the bounded observation budget."
				break
			}
			candidates[run.GetHeadSHA()] = forgequeue.QueueCandidate{SHA: run.GetHeadSHA(), Ref: "refs/heads/" + run.GetHeadBranch()}
			if hint, exists := hintBySHA[run.GetHeadSHA()]; exists {
				candidates[run.GetHeadSHA()] = hint
			}
			order = append(order, run.GetHeadSHA())
		}
	}
	for _, hint := range hints {
		if _, valid := hintBySHA[hint.SHA]; !valid {
			continue
		}
		if _, exists := candidates[hint.SHA]; exists {
			continue
		}
		if len(candidates) >= nativeObservationMaxGroups {
			out.Complete = false
			out.Reason = "Candidate cohort exceeds the bounded observation budget."
			break
		}
		candidates[hint.SHA] = hint
		order = append(order, hint.SHA)
	}
	required, requiredKnown := c.nativeObservationRequired(ctx, parts[0], parts[1], base)
	for _, key := range order {
		candidate := candidates[key]
		candidate.Membership, candidate.Members = "unknown", []forgequeue.QueueMember{}
		candidate.CI, candidate.Checks = "unknown", []forgequeue.QueueCheck{}
		// Expired refs are not proof of failure, rebuild or merge.
		ref, _, refErr := c.client.Git.GetRef(ctx, parts[0], parts[1], strings.TrimPrefix(candidate.Ref, "refs/"))
		if refErr != nil || ref.GetObject().GetSHA() != candidate.SHA {
			candidate.Reason = "Synthetic ref unavailable or changed; lifecycle outcome unknown."
			out.Candidates = append(out.Candidates, candidate)
			continue
		}
		if integrationOID(candidate.BaseSHA) {
			group, groupErr := c.ReadNativeIntegrationGroup(ctx, repo, base, candidate.BaseSHA, candidate.Ref, candidate.SHA)
			if groupErr == nil && len(group.Members) <= nativeObservationMaxMembers && len(group.Members) > 0 {
				proven := true
				for _, member := range group.Members {
					// Queue-chain facts alone are insufficient: prove every exact
					// member head is in this synthetic candidate.
					comparison, _, err := c.client.Repositories.CompareCommits(ctx, parts[0], parts[1], member.Head, candidate.SHA, nil)
					matches := false
					for _, entry := range out.Entries {
						matches = matches || entry.ID == member.Entry && entry.PR == member.PR && entry.Head == member.Head
					}
					if err != nil || comparison.GetMergeBaseCommit().GetSHA() != member.Head ||
						(comparison.GetStatus() != "ahead" && comparison.GetStatus() != "identical") || !matches {
						proven = false
						break
					}
				}
				finalRef, _, err := c.client.Git.GetRef(ctx, parts[0], parts[1], strings.TrimPrefix(candidate.Ref, "refs/"))
				if err != nil || finalRef.GetObject().GetSHA() != candidate.SHA {
					proven = false
				}
				if proven {
					candidate.Membership, candidate.Tree = "proven", group.Tree
					for _, member := range group.Members {
						candidate.Members = append(candidate.Members, forgequeue.QueueMember{PR: member.PR, Head: member.Head, Entry: member.Entry, Position: member.Position, BaseCommit: member.BaseCommit, HeadCommit: member.HeadCommit})
					}
				}
			}
		}
		if candidate.Membership != "proven" {
			candidate.Reason = "Exact membership unavailable: needs signed group base, live queue chain, member ancestry and unchanged synthetic ref."
		}
		candidate.Checks, candidate.CI, candidate.ChecksKnown = c.nativeObservationChecks(ctx, parts[0], parts[1], candidate.SHA, required, requiredKnown)
		out.Candidates = append(out.Candidates, candidate)
	}
	return out, nil
}

// Both classic protection and applicable rules must be readable. No configured
// fallback is presented as GitHub's actual required-check policy.
func (c *Client) nativeObservationRequired(ctx context.Context, owner, repo, base string) (map[string]int64, bool) {
	required := map[string]int64{}
	classic, _, err := c.client.Repositories.GetRequiredStatusChecks(ctx, owner, repo, base)
	known := err == nil
	if classic != nil {
		if classic.Contexts != nil {
			for _, name := range *classic.Contexts {
				required[name] = 0
			}
		}
		if classic.Checks != nil {
			for _, check := range *classic.Checks {
				if check == nil || strings.TrimSpace(check.Context) == "" {
					known = false
					continue
				}
				required[check.Context] = check.GetAppID()
			}
		}
	}
	request, err := c.client.NewRequest("GET", fmt.Sprintf("repos/%s/%s/rules/branches/%s", owner, repo, url.PathEscape(base)), nil)
	if err != nil {
		return required, false
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			Checks []struct {
				Context       string `json:"context"`
				IntegrationID int64  `json:"integration_id"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	if _, err := c.client.Do(ctx, request, &rules); err != nil {
		return required, false
	}
	for _, rule := range rules {
		if rule.Type == "required_status_checks" {
			for _, check := range rule.Parameters.Checks {
				if prior, exists := required[check.Context]; exists && prior != 0 && check.IntegrationID != 0 && prior != check.IntegrationID {
					known = false
				}
				if check.IntegrationID != 0 || required[check.Context] == 0 {
					required[check.Context] = check.IntegrationID
				}
			}
		}
	}
	return required, known
}

func (c *Client) nativeObservationChecks(ctx context.Context, owner, repo, sha string, required map[string]int64, known bool) ([]forgequeue.QueueCheck, string, bool) {
	checks := []forgequeue.QueueCheck{}
	if len(required) == 0 {
		return checks, "unknown", known
	}
	runs, response, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, &gh.ListCheckRunsOptions{
		Filter: gh.Ptr("latest"), ListOptions: gh.ListOptions{PerPage: nativeObservationPageSize},
	})
	if err != nil || runs == nil || response != nil && response.NextPage != 0 {
		return checks, "unavailable", false
	}
	statuses := &gh.CombinedStatus{}
	for _, app := range required {
		if app == 0 {
			var statusResponse *gh.Response
			statuses, statusResponse, err = c.client.Repositories.GetCombinedStatus(ctx, owner, repo, sha, &gh.ListOptions{PerPage: nativeObservationPageSize})
			if err != nil || statuses == nil || statusResponse != nil && statusResponse.NextPage != 0 {
				return checks, "unavailable", false
			}
			break
		}
	}
	names := make([]string, 0, len(required))
	for name := range required {
		names = append(names, name)
	}
	sort.Strings(names)
	state := "passed"
	if !known || len(names) == 0 {
		state = "unknown"
	}
	for _, name := range names {
		check := forgequeue.QueueCheck{Name: name, Status: "missing"}
		var latest *gh.CheckRun
		for _, run := range runs.CheckRuns {
			if run.GetName() == name && run.GetHeadSHA() == sha && (required[name] == 0 || run.GetApp().GetID() == required[name]) &&
				(latest == nil || run.GetID() > latest.GetID()) {
				latest = run
			}
		}
		if latest != nil {
			check.Status, check.Conclusion, check.URL, check.AppID = latest.GetStatus(), latest.GetConclusion(), latest.GetHTMLURL(), latest.GetApp().GetID()
		} else if required[name] == 0 {
			var latestStatus *gh.RepoStatus
			for _, status := range statuses.Statuses {
				if status.GetContext() == name && (latestStatus == nil || status.GetID() > latestStatus.GetID()) {
					latestStatus = status
				}
			}
			if latestStatus != nil {
				check.Status, check.Conclusion, check.URL = "completed", latestStatus.GetState(), latestStatus.GetTargetURL()
			}
		}
		switch {
		case check.Conclusion == "failure" || check.Conclusion == "error" || check.Conclusion == "timed_out" || check.Conclusion == "cancelled":
			state = "failed"
		case check.Status == "completed" && check.Conclusion != "success":
			if state != "failed" {
				state = "blocked"
			}
		case check.Status != "completed" || check.Conclusion != "success":
			if state != "failed" && state != "blocked" {
				state = "pending"
			}
		}
		checks = append(checks, check)
	}
	if !known && state != "failed" {
		state = "unknown"
	}
	return checks, state, known
}
