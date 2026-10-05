package dashboard

import (
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// OverviewBands carries the same ordered taxonomy used by /api/overview exports.
type OverviewBands struct {
	Issues []BandSpec `json:"issues"`
	PRs    []BandSpec `json:"prs"`
}
type overviewStatusIssue struct {
	github.Issue
	IssueBandInfo
}
type overviewStatusHold struct {
	github.HoldItem
	IssueBandInfo
}
type overviewStatusPR struct {
	FrontendPR
	PRBandInfo
}

// statusWithOverviewBands copies response containers, leaving the cached typed
// items untouched for concurrent readers and the export endpoints.
func (s *Server) statusWithOverviewBands(status *StatusPayload, now time.Time) *StatusPayload {
	cfg := config.DashboardIssueBandsConfig{}
	if s.deps != nil && s.deps.Config != nil {
		cfg = s.deps.Config.Dashboard.IssueBands
	}
	selfAuthorizationHoldActive := s.selfAuthorizationHoldActiveForRepo
	out := *status
	out.OverviewBands = &OverviewBands{Issues: IssueBandSpecs(cfg), PRs: PRBandSpecs(cfg)}
	out.Repos = append([]FrontendRepo(nil), status.Repos...)
	issues := func(items []any, held bool, repoName string) []any {
		if items == nil {
			return nil
		}
		result := make([]any, 0, len(items))
		for _, raw := range items {
			issue, ok := frontendIssue(raw)
			if !ok {
				result = append(result, raw)
				continue
			}
			issue.Repo = nonEmpty(issue.Repo, repoName)
			info := IssueBand(issue, held, cfg, now, selfAuthorizationHoldActive)
			if held {
				info.HoldReason = heldReason(issue.Labels, false, status.HiveID)
			}
			for i := range out.OverviewBands.Issues {
				if out.OverviewBands.Issues[i].Key == info.Band {
					out.OverviewBands.Issues[i].Count++
				}
			}
			switch v := raw.(type) {
			case github.HoldItem:
				result = append(result, overviewStatusHold{v, info})
			case *github.HoldItem:
				result = append(result, overviewStatusHold{*v, info})
			default:
				result = append(result, overviewStatusIssue{issue, info})
			}
		}
		return result
	}
	prs := func(items []any, held bool) []any {
		if items == nil {
			return nil
		}
		result := make([]any, 0, len(items))
		for _, raw := range items {
			entry, ok := frontendPullRequest(raw)
			if !ok {
				result = append(result, raw)
				continue
			}
			info := prBand(entry.pr, entry.verdict, held, cfg, now, s.autoMergeLabel(), status.HiveID)
			for i := range out.OverviewBands.PRs {
				if out.OverviewBands.PRs[i].Key == info.Band {
					out.OverviewBands.PRs[i].Count++
				}
			}
			result = append(result, overviewStatusPR{FrontendPR{entry.pr, entry.verdict}, info})
		}
		return result
	}
	for i := range out.Repos {
		repo := &out.Repos[i]
		repoName := overviewRepoName(*repo)
		repo.ActionableIssues = issues(repo.ActionableIssues, false, repoName)
		repo.HeldIssues = issues(repo.HeldIssues, true, repoName)
		repo.OpenPrs = prs(repo.OpenPrs, false)
		repo.HeldPrs = prs(repo.HeldPrs, true)
	}
	out.ActionableNow = overviewActionableNow(&out)
	return &out
}

func overviewActionableNow(status *StatusPayload) FrontendActionableNow {
	var counts FrontendActionableNow
	if status == nil {
		return counts
	}
	for _, repo := range status.Repos {
		for _, raw := range repo.ActionableIssues {
			if !overviewKPIExcludedBand(overviewStatusIssueBand(raw)) {
				counts.Issues++
			}
		}
		for _, raw := range repo.OpenPrs {
			if !overviewKPIExcludedBand(overviewStatusPRBand(raw)) {
				counts.PRs++
			}
		}
	}
	counts.Total = counts.Issues + counts.PRs
	return counts
}

func overviewStatusIssueBand(raw any) string {
	switch v := raw.(type) {
	case overviewStatusIssue:
		return v.Band
	case *overviewStatusIssue:
		if v != nil {
			return v.Band
		}
	case overviewStatusHold:
		return v.Band
	case *overviewStatusHold:
		if v != nil {
			return v.Band
		}
	}
	return ""
}

func overviewStatusPRBand(raw any) string {
	switch v := raw.(type) {
	case overviewStatusPR:
		return v.Band
	case *overviewStatusPR:
		if v != nil {
			return v.Band
		}
	}
	return ""
}
