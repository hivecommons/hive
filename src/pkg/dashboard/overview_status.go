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
	out := *status
	out.OverviewBands = &OverviewBands{Issues: IssueBandSpecs(cfg), PRs: PRBandSpecs(cfg)}
	out.Repos = append([]FrontendRepo(nil), status.Repos...)
	issues := func(items []any, held bool) []any {
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
			info := IssueBand(issue, held, cfg, now)
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
		repo.ActionableIssues = issues(repo.ActionableIssues, false)
		repo.HeldIssues = issues(repo.HeldIssues, true)
		repo.OpenPrs = prs(repo.OpenPrs, false)
		repo.HeldPrs = prs(repo.HeldPrs, true)
	}
	return &out
}
