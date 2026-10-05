package dashboard

import (
	"fmt"
	"strings"
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
	var issueHeld, prHeld, issueBlockedHuman, prBlockedHuman, confirmClose, draft int
	for _, repo := range status.Repos {
		for _, raw := range repo.ActionableIssues {
			switch overviewStatusIssueBand(raw) {
			case "waiting":
				issueBlockedHuman++
			case "done":
				confirmClose++
			default:
				counts.Issues++
			}
		}
		issueHeld += len(repo.HeldIssues)
		for _, raw := range repo.OpenPrs {
			switch overviewStatusPRBand(raw) {
			case "waiting", "blocked":
				prBlockedHuman++
			case "draft":
				draft++
			default:
				counts.PRs++
			}
		}
		prHeld += len(repo.HeldPrs)
	}
	counts.Total = counts.Issues + counts.PRs
	totals := status.OverviewTotals
	if totals.Issues.Forge == 0 && totals.PRs.Forge == 0 && (len(status.Repos) > 0 || counts.Total > 0 || issueHeld > 0 || prHeld > 0 || issueBlockedHuman > 0 || prBlockedHuman > 0 || confirmClose > 0 || draft > 0) {
		totals = overviewTotals(status)
	}
	counts.Equation, counts.IssueEquation, counts.PREquation = overviewActionableEquations(totals, counts.Issues, counts.PRs, issueHeld, prHeld, issueBlockedHuman, prBlockedHuman, confirmClose, draft)
	return counts
}

func overviewActionableEquations(totals FrontendOverviewTotals, actionableIssues, actionablePRs, issueHeld, prHeld, issueBlockedHuman, prBlockedHuman, confirmClose, draft int) (*FrontendActionableEquation, *FrontendActionableEquation, *FrontendActionableEquation) {
	openIssues := totals.Issues.Forge
	openPRs := totals.PRs.Forge
	outsideNeedsHuman := totals.Issues.Breakdown["needs_human"]
	issueBlockedHuman += outsideNeedsHuman
	issueOutside := max(0, totals.Issues.Outside-outsideNeedsHuman)
	prOutside := max(0, totals.PRs.Outside)
	issueEq := overviewActionableKindEquation("issues", openIssues, actionableIssues, []FrontendActionableEquationTerm{
		{Key: "held", Label: "held", Count: issueHeld},
		{Key: "blocked_needs_human", Label: "blocked/needs-human", Count: issueBlockedHuman},
		{Key: "confirm_close", Label: "confirm/close", Count: confirmClose},
		{Key: "outside", Label: "outside", Count: issueOutside},
	})
	prEq := overviewActionableKindEquation("PRs", openPRs, actionablePRs, []FrontendActionableEquationTerm{
		{Key: "held", Label: "held", Count: prHeld},
		{Key: "blocked_needs_human", Label: "blocked/needs-human", Count: prBlockedHuman},
		{Key: "draft", Label: "draft", Count: draft},
		{Key: "outside", Label: "outside", Count: prOutside},
	})
	combined := overviewActionableCombinedEquation(totals, actionableIssues+actionablePRs, issueHeld+prHeld, issueBlockedHuman+prBlockedHuman, confirmClose, draft, issueEq, prEq)
	return combined, issueEq, prEq
}

func overviewActionableKindEquation(kind string, open, actionable int, subtract []FrontendActionableEquationTerm) *FrontendActionableEquation {
	terms := make([]FrontendActionableEquationTerm, 0, len(subtract)+1)
	sum := actionable
	for _, term := range subtract {
		if term.Count <= 0 {
			continue
		}
		terms = append(terms, term)
		sum += term.Count
	}
	if remainder := open - sum; remainder > 0 {
		terms = append(terms, FrontendActionableEquationTerm{Key: "other", Label: "other", Count: remainder})
	}
	eq := &FrontendActionableEquation{Kind: kind, Open: open, TotalOpen: open, Result: actionable, Terms: terms}
	eq.Text = overviewKindEquationText(eq)
	eq.Title = overviewKindEquationTitle(eq)
	return eq
}

func overviewActionableCombinedEquation(totals FrontendOverviewTotals, actionable, held, blockedHuman, confirmClose, draft int, issueEq, prEq *FrontendActionableEquation) *FrontendActionableEquation {
	openIssues := totals.Issues.Forge
	openPRs := totals.PRs.Forge
	outside := overviewTermCount(issueEq, "outside") + overviewTermCount(prEq, "outside")
	other := overviewTermCount(issueEq, "other") + overviewTermCount(prEq, "other")
	terms := []FrontendActionableEquationTerm{
		{Key: "actionable", Label: "actionable", Count: actionable},
		{Key: "held", Label: "held", Count: held},
		{Key: "blocked_needs_human", Label: "blocked/needs-human", Count: blockedHuman},
	}
	if confirmClose > 0 {
		terms = append(terms, FrontendActionableEquationTerm{Key: "confirm_close", Label: "confirm/close", Count: confirmClose})
	}
	if draft > 0 {
		terms = append(terms, FrontendActionableEquationTerm{Key: "draft", Label: "draft", Count: draft})
	}
	if outside > 0 {
		terms = append(terms, FrontendActionableEquationTerm{Key: "outside", Label: "outside", Count: outside})
	}
	if other > 0 {
		terms = append(terms, FrontendActionableEquationTerm{Key: "other", Label: "other", Count: other})
	}
	eq := &FrontendActionableEquation{OpenIssues: openIssues, OpenPRs: openPRs, TotalOpen: openIssues + openPRs, Result: actionable, Terms: terms}
	eq.Text = overviewEquationText(eq)
	eq.Title = overviewEquationTitle(eq, totals)
	return eq
}

func overviewTermCount(eq *FrontendActionableEquation, key string) int {
	if eq == nil {
		return 0
	}
	for _, term := range eq.Terms {
		if term.Key == key {
			return term.Count
		}
	}
	return 0
}

func overviewKindEquationText(eq *FrontendActionableEquation) string {
	if eq == nil {
		return ""
	}
	parts := make([]string, 0, len(eq.Terms)+1)
	parts = append(parts, fmt.Sprintf("%d open", eq.Open))
	for _, term := range eq.Terms {
		if term.Count > 0 {
			parts = append(parts, fmt.Sprintf("− %d %s", term.Count, term.Label))
		}
	}
	parts = append(parts, fmt.Sprintf("= %d", eq.Result))
	return strings.Join(parts, " ")
}

func overviewKindEquationTitle(eq *FrontendActionableEquation) string {
	if eq == nil {
		return ""
	}
	what := "issue work"
	if strings.EqualFold(eq.Kind, "PRs") {
		what = "pull requests"
	}
	return strings.ToUpper("actionable "+eq.Kind) + " — " + what + " that can move without waiting.\n\n" +
		"WHAT: The " + eq.Kind + " split of the shared Overview Actionable now set.\n\n" +
		"HOW: " + eq.Text + ". Held, blocked/needs-human, confirm/close, draft, outside, and other terms use the same server-side partition as Overview. Claimed/in-progress work remains actionable unless it is in one of those excluded terms."
}

func overviewEquationText(eq *FrontendActionableEquation) string {
	if eq == nil {
		return ""
	}
	parts := make([]string, 0, len(eq.Terms))
	for _, term := range eq.Terms {
		if term.Count == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %s", term.Count, term.Label))
	}
	if len(parts) == 0 {
		parts = append(parts, "0 actionable")
	}
	return fmt.Sprintf("%d issues + %d PRs = %s", eq.OpenIssues, eq.OpenPRs, strings.Join(parts, " + "))
}

func overviewEquationTitle(eq *FrontendActionableEquation, totals FrontendOverviewTotals) string {
	if eq == nil {
		return ""
	}
	lines := []string{"Open issue/PR partition: " + eq.Text}
	if detail := overviewOutsideDetail("outside issues", totals.Issues.Breakdown); detail != "" {
		lines = append(lines, detail)
	}
	if detail := overviewOutsideDetail("outside PRs", totals.PRs.Breakdown); detail != "" {
		lines = append(lines, detail)
	}
	return strings.Join(lines, "\n")
}

func overviewOutsideDetail(prefix string, breakdown map[string]int) string {
	if len(breakdown) == 0 {
		return ""
	}
	labels := map[string]string{
		"needs_direction":      "needs-direction",
		"needs_decision":       "needs-decision",
		"needs_spec":           "needs-spec",
		"exempt":               "exempt",
		"filtered":             "filtered",
		"reporter_triage":      "reporter triage",
		"hive_advisory":        "hive advisory",
		"dependency_dashboard": "dependency dashboard",
		"other":                "other",
	}
	order := []string{"needs_direction", "needs_decision", "needs_spec", "exempt", "filtered", "reporter_triage", "hive_advisory", "dependency_dashboard", "other"}
	parts := make([]string, 0, len(breakdown))
	for _, key := range order {
		count := breakdown[key]
		if count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, labels[key]))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return prefix + ": " + strings.Join(parts, " · ")
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
