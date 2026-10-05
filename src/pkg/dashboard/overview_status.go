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
	out.OverviewTotals = overviewTotals(&out)
	out.ActionableNow = overviewActionableNow(&out)
	return &out
}

func overviewTotals(status *StatusPayload) FrontendOverviewTotals {
	var totals FrontendOverviewTotals
	if status == nil {
		return totals
	}
	totals.Issues.Breakdown = map[string]int{}
	totals.PRs.Breakdown = map[string]int{}
	for _, repo := range status.Repos {
		totals.Issues.Tracked += len(repo.ActionableIssues) + len(repo.HeldIssues)
		totals.Issues.Held += len(repo.HeldIssues)
		totals.PRs.Tracked += len(repo.OpenPrs) + len(repo.HeldPrs)
		totals.PRs.Held += len(repo.HeldPrs)
		if repo.WorkBreakdown != nil {
			ib := repo.WorkBreakdown.Issues
			totals.Issues.Forge += ib.Total()
			addOverviewBreakdown(totals.Issues.Breakdown, "actionable", ib.Actionable)
			addOverviewBreakdown(totals.Issues.Breakdown, "hold", ib.Hold)
			addOverviewBreakdown(totals.Issues.Breakdown, "needs_human", ib.NeedsHuman)
			addOverviewBreakdown(totals.Issues.Breakdown, "needs_direction", ib.NeedsDirection)
			addOverviewBreakdown(totals.Issues.Breakdown, "needs_decision", ib.NeedsDecision)
			addOverviewBreakdown(totals.Issues.Breakdown, "needs_spec", ib.NeedsSpec)
			addOverviewBreakdown(totals.Issues.Breakdown, "exempt", ib.Exempt)
			addOverviewBreakdown(totals.Issues.Breakdown, "filtered", overviewGenericIssueFiltered(ib))
			addOverviewBreakdown(totals.Issues.Breakdown, "reporter_triage", ib.ReporterTriage)
			addOverviewBreakdown(totals.Issues.Breakdown, "hive_advisory", ib.HiveAdvisory)
			addOverviewBreakdown(totals.Issues.Breakdown, "dependency_dashboard", ib.DependencyDashboard)
			addOverviewBreakdown(totals.Issues.Breakdown, "other", ib.Other)

			pb := repo.WorkBreakdown.PRs
			totals.PRs.Forge += pb.Total()
			addOverviewBreakdown(totals.PRs.Breakdown, "actionable", pb.Actionable)
			addOverviewBreakdown(totals.PRs.Breakdown, "hold", pb.Hold)
			addOverviewBreakdown(totals.PRs.Breakdown, "draft", pb.Draft)
			addOverviewBreakdown(totals.PRs.Breakdown, "filtered", pb.Filtered)
			addOverviewBreakdown(totals.PRs.Breakdown, "other", pb.Other)
			continue
		}
		issueForge := repo.Issues
		if issueForge == 0 {
			issueForge = len(repo.ActionableIssues) + len(repo.HeldIssues)
		}
		prForge := repo.PRs
		if prForge == 0 {
			prForge = len(repo.OpenPrs) + len(repo.HeldPrs)
		}
		totals.Issues.Forge += issueForge
		totals.PRs.Forge += prForge
	}
	totals.Issues.Outside = max(0, totals.Issues.Forge-totals.Issues.Tracked)
	totals.PRs.Outside = max(0, totals.PRs.Forge-totals.PRs.Tracked)
	if len(totals.Issues.Breakdown) == 0 {
		totals.Issues.Breakdown = nil
	}
	if len(totals.PRs.Breakdown) == 0 {
		totals.PRs.Breakdown = nil
	}
	return totals
}

func addOverviewBreakdown(dst map[string]int, key string, count int) {
	if count > 0 {
		dst[key] += count
	}
}

func overviewGenericIssueFiltered(b github.RepoIssueBreakdown) int {
	named := b.NeedsHuman + b.NeedsDirection + b.NeedsDecision + b.NeedsSpec + b.Exempt
	return max(0, b.Filtered-named)
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
