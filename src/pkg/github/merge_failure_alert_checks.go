package github

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

const mergeAlertEvidenceMaxPages = 3

// missingMergeChecksResolved only retires the missing-check banner. It neither
// grants a CI verdict nor schedules another merge, and never substitutes an
// old head's success for evidence on the current PR head.
func (c *Client) missingMergeChecksResolved(ctx context.Context, owner, repo string, pr *gh.PullRequest, names []string) bool {
	head, base := pr.GetHead().GetSHA(), pr.GetBase().GetRef()
	if head == "" || base == "" || len(names) == 0 {
		return false
	}
	required, known := c.nativeObservationRequired(ctx, owner, repo, base)
	if !known {
		// In particular, a denied protection read cannot establish that an
		// identically named check from another App satisfies this blocker.
		return false
	}
	producers := make(map[string]int64, len(names))
	for _, name := range names {
		appID, exists := required[name]
		if !exists {
			return false
		}
		producers[name] = appID
	}
	var checks []*gh.CheckRun
	checkOpts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for page := 0; ; page++ {
		result, response, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, repo, head, checkOpts)
		if err != nil || result == nil || result.Total == nil {
			return false
		}
		checks = append(checks, result.CheckRuns...)
		if response == nil || response.NextPage == 0 {
			if result.GetTotal() != len(checks) {
				return false
			}
			break
		}
		if page+1 >= mergeAlertEvidenceMaxPages || response.NextPage <= checkOpts.Page {
			return false
		}
		checkOpts.Page = response.NextPage
	}
	passed, blocked := make(map[string]bool), make(map[string]bool)
	for _, check := range latestCheckRunsByNameAndApp(checks) {
		name := check.GetName()
		appID, wanted := producers[name]
		if !wanted || (appID > 0 && check.GetApp().GetID() != appID) {
			continue
		}
		conclusion := check.GetConclusion()
		if check.GetID() > 0 && check.GetHeadSHA() == head && check.GetStatus() == "completed" &&
			(conclusion == "success" || conclusion == "neutral" || conclusion == "skipped") {
			passed[name] = true
		} else {
			blocked[name] = true
		}
	}
	statusOpts := &gh.ListOptions{PerPage: 100}
	var statusCount int
	for page := 0; ; page++ {
		result, response, err := c.client.Repositories.GetCombinedStatus(ctx, owner, repo, head, statusOpts)
		if err != nil || result == nil || result.TotalCount == nil || result.GetSHA() != head {
			return false
		}
		statusCount += len(result.Statuses)
		for _, status := range result.Statuses {
			name := status.GetContext()
			appID, wanted := producers[name]
			if !wanted || appID > 0 {
				continue
			}
			if status.GetID() > 0 && status.GetState() == "success" {
				passed[name] = true
			} else {
				blocked[name] = true
			}
		}
		if response == nil || response.NextPage == 0 {
			if result.GetTotalCount() != statusCount {
				return false
			}
			break
		}
		if page+1 >= mergeAlertEvidenceMaxPages || response.NextPage <= statusOpts.Page {
			return false
		}
		statusOpts.Page = response.NextPage
	}
	for _, name := range names {
		if !passed[name] || blocked[name] {
			return false
		}
	}
	current, _, err := c.client.PullRequests.Get(ctx, owner, repo, pr.GetNumber())
	return err == nil && current != nil && current.GetState() == "open" &&
		strings.EqualFold(current.GetHead().GetSHA(), head) && current.GetBase().GetRef() == base
}
