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
	var fullCfg *config.Config
	if s.deps != nil {
		fullCfg = s.deps.Config
	}
	out.ActionableNow = overviewActionableNow(&out, overviewPartitionMetadataFromConfig(fullCfg, status.HiveID))
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

func overviewActionableNow(status *StatusPayload, meta overviewPartitionMeta) FrontendActionableNow {
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
	counts.Equation, counts.IssueEquation, counts.PREquation = overviewActionableEquations(totals, counts.Issues, counts.PRs, issueHeld, prHeld, issueBlockedHuman, prBlockedHuman, confirmClose, draft, meta)
	if counts.Equation != nil {
		if outside := overviewTerm(counts.Equation, "outside"); outside != nil {
			counts.Outside = &FrontendActionableOutside{Count: outside.Count, Breakdown: append([]FrontendActionableBreakdownTerm(nil), outside.Breakdown...)}
		}
	}
	return counts
}

func overviewActionableEquations(totals FrontendOverviewTotals, actionableIssues, actionablePRs, issueHeld, prHeld, issueBlockedHuman, prBlockedHuman, confirmClose, draft int, meta overviewPartitionMeta) (*FrontendActionableEquation, *FrontendActionableEquation, *FrontendActionableEquation) {
	openIssues := totals.Issues.Forge
	openPRs := totals.PRs.Forge
	outsideNeedsHuman := totals.Issues.Breakdown["needs_human"]
	issueBlockedHuman += outsideNeedsHuman
	issueOutside := max(0, totals.Issues.Outside-outsideNeedsHuman)
	prOutside := max(0, totals.PRs.Outside)
	details := overviewPartitionDetails(totals, issueOutside, prOutside, issueBlockedHuman, prBlockedHuman, meta)
	issueEq := overviewActionableKindEquation("issues", openIssues, actionableIssues, []FrontendActionableEquationTerm{
		{Key: "held", Label: "held", Count: issueHeld, Rule: "Issue has a hold label.", Breakdown: details.IssueHeld},
		{Key: "blocked_needs_human", Label: "blocked/needs-human", Count: issueBlockedHuman, Rule: "Issue is waiting on a person, confirmation, or dependency.", Breakdown: details.IssueBlocked},
		{Key: "confirm_close", Label: "confirm/close", Count: confirmClose},
		{Key: "outside", Label: "outside", Count: issueOutside, Rule: "Open issues filtered out before Overview bands by this hive's scanner settings.", Breakdown: details.IssueOutside},
	})
	prEq := overviewActionableKindEquation("PRs", openPRs, actionablePRs, []FrontendActionableEquationTerm{
		{Key: "held", Label: "held", Count: prHeld, Rule: "Pull request has a hold label.", Breakdown: details.PRHeld},
		{Key: "blocked_needs_human", Label: "blocked/needs-human", Count: prBlockedHuman, Rule: "Pull request is blocked, waiting, or needs human review.", Breakdown: details.PRBlocked},
		{Key: "draft", Label: "draft", Count: draft},
		{Key: "outside", Label: "outside", Count: prOutside, Rule: "Open PRs filtered out before Overview bands by this hive's scanner settings.", Breakdown: details.PROutside},
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
	heldBreakdown := combineOverviewBreakdown(overviewTermBreakdown(issueEq, "held"), overviewTermBreakdown(prEq, "held"))
	blockedBreakdown := combineOverviewBreakdown(overviewTermBreakdown(issueEq, "blocked_needs_human"), overviewTermBreakdown(prEq, "blocked_needs_human"))
	outsideBreakdown := combineOverviewBreakdown(overviewTermBreakdown(issueEq, "outside"), overviewTermBreakdown(prEq, "outside"))
	terms := []FrontendActionableEquationTerm{
		{Key: "actionable", Label: "actionable", Count: actionable},
		{Key: "held", Label: "held", Count: held, Rule: "Open issues and PRs paused by hold labels.", Breakdown: heldBreakdown},
		{Key: "blocked_needs_human", Label: "blocked/needs-human", Count: blockedHuman, Rule: "Open work waiting on a person, dependency, or mergeability.", Breakdown: blockedBreakdown},
	}
	if confirmClose > 0 {
		terms = append(terms, FrontendActionableEquationTerm{Key: "confirm_close", Label: "confirm/close", Count: confirmClose})
	}
	if draft > 0 {
		terms = append(terms, FrontendActionableEquationTerm{Key: "draft", Label: "draft", Count: draft})
	}
	if outside > 0 {
		terms = append(terms, FrontendActionableEquationTerm{Key: "outside", Label: "outside", Count: outside, Rule: "Open items excluded before they enter Overview bands by this hive's scanner settings.", Breakdown: outsideBreakdown})
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
	if term := overviewTerm(eq, key); term != nil {
		return term.Count
	}
	return 0
}

func overviewTerm(eq *FrontendActionableEquation, key string) *FrontendActionableEquationTerm {
	if eq == nil {
		return nil
	}
	for i := range eq.Terms {
		if eq.Terms[i].Key == key {
			return &eq.Terms[i]
		}
	}
	return nil
}

func overviewTermBreakdown(eq *FrontendActionableEquation, key string) []FrontendActionableBreakdownTerm {
	if term := overviewTerm(eq, key); term != nil {
		return term.Breakdown
	}
	return nil
}

func combineOverviewBreakdown(parts ...[]FrontendActionableBreakdownTerm) []FrontendActionableBreakdownTerm {
	var out []FrontendActionableBreakdownTerm
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
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

type overviewPartitionMeta struct {
	ExemptLabels                []string
	RequireLabels               []string
	ReporterTrustEnabled        bool
	ReporterTrustRequiredLabels []string
	ReporterTrustTrusted        []string
	ReporterTrustAwaitingLabel  string
	HoldLabels                  []string
	HardSuppressIssueLabels     []string
}

type overviewPartitionBreakdowns struct {
	IssueOutside []FrontendActionableBreakdownTerm
	PROutside    []FrontendActionableBreakdownTerm
	IssueHeld    []FrontendActionableBreakdownTerm
	PRHeld       []FrontendActionableBreakdownTerm
	IssueBlocked []FrontendActionableBreakdownTerm
	PRBlocked    []FrontendActionableBreakdownTerm
}

func overviewPartitionMetadataFromConfig(cfg *config.Config, hiveID string) overviewPartitionMeta {
	if cfg == nil {
		return overviewPartitionDefaults(hiveID)
	}
	rt := cfg.Project.IssueFilter.ReporterTrust
	meta := overviewPartitionDefaults(hiveID)
	meta.ExemptLabels = append(append([]string(nil), cfg.Governor.Labels.Exempt...), github.PermanentExemptLabels...)
	meta.RequireLabels = append([]string(nil), cfg.Project.IssueFilter.RequireLabels...)
	meta.ReporterTrustEnabled = rt.IsEnabled()
	meta.ReporterTrustRequiredLabels = rt.EffectiveUntrustedRequireLabels()
	meta.ReporterTrustTrusted = append([]string(nil), rt.EffectiveTrustedAssociations()...)
	meta.ReporterTrustAwaitingLabel = rt.EffectiveAwaitingLabel()
	return meta
}

func overviewPartitionDefaults(hiveID string) overviewPartitionMeta {
	holdLabels := append([]string(nil), github.HoldLabels...)
	if label := github.CanonicalHiveHoldLabel(hiveID); label != "" {
		holdLabels = append(holdLabels, label)
	}
	return overviewPartitionMeta{
		ExemptLabels:                append([]string(nil), github.PermanentExemptLabels...),
		ReporterTrustRequiredLabels: config.ReporterTrustConfig{}.EffectiveUntrustedRequireLabels(),
		ReporterTrustTrusted:        config.ReporterTrustConfig{}.EffectiveTrustedAssociations(),
		ReporterTrustAwaitingLabel:  config.ReporterTrustConfig{}.EffectiveAwaitingLabel(),
		HoldLabels:                  holdLabels,
		HardSuppressIssueLabels:     []string{"needs-human", "needs-direction", "needs-decision", "needs-spec"},
	}
}

func overviewPartitionDetails(totals FrontendOverviewTotals, issueOutside, prOutside, issueBlocked, prBlocked int, meta overviewPartitionMeta) overviewPartitionBreakdowns {
	issueFiltered := totals.Issues.Breakdown["filtered"]
	issueExempt := totals.Issues.Breakdown["exempt"]
	issueReporter := totals.Issues.Breakdown["reporter_triage"]
	issueAdvisory := totals.Issues.Breakdown["hive_advisory"]
	issueDependencyDashboard := totals.Issues.Breakdown["dependency_dashboard"]
	issueOther := totals.Issues.Breakdown["other"]
	issueRows := []FrontendActionableBreakdownTerm{
		overviewBreakdownTerm("needs-direction", "needs-direction", totals.Issues.Breakdown["needs_direction"], "Issue carries the hard-suppress label needs-direction, so Hive waits for maintainer direction before offering it to agents.", "hardSuppressIssueLabels", overviewValue(meta.HardSuppressIssueLabels), "Remove the label from the issue, or change the code-level escalation labels if your hive fork owns that policy.", "Labels", "/docs/labels-and-control-signals.md"),
		overviewBreakdownTerm("needs-decision", "needs-decision", totals.Issues.Breakdown["needs_decision"], "Issue carries the hard-suppress label needs-decision, so Hive waits for a maintainer decision.", "hardSuppressIssueLabels", overviewValue(meta.HardSuppressIssueLabels), "Remove the label from the issue after the decision is made.", "Labels", "/docs/labels-and-control-signals.md"),
		overviewBreakdownTerm("needs-spec", "needs-spec", totals.Issues.Breakdown["needs_spec"], "Issue carries the hard-suppress label needs-spec, so Hive waits for specification or acceptance criteria.", "hardSuppressIssueLabels", overviewValue(meta.HardSuppressIssueLabels), "Remove the label after adding enough spec for agents to act.", "Labels", "/docs/labels-and-control-signals.md"),
		overviewBreakdownTerm("exempt-labels", "exempt labels", issueExempt, "Issue matches governor.labels.exempt or a permanent exempt label such as do-not-merge.", "governor.labels.exempt", overviewValue(meta.ExemptLabels), "Open Settings → Labels to edit exempt labels, or edit hive.yaml governor.labels.exempt.", "Labels", "/docs/labels-and-control-signals.md"),
		overviewBreakdownTerm("reporter-triage", "reporter triage", issueReporter, "Reporter trust is enabled and the issue author is not trusted until a maintainer adds an allowed triage label.", "project.issue_filter.reporter_trust", overviewReporterValue(meta), "Open Settings → Labels → Reporter trust, or edit hive.yaml project.issue_filter.reporter_trust.", "Labels", "/docs/dashboard.md#overview"),
		overviewBreakdownTerm("project-issue-filter", "project issue filter", issueFiltered, "Issue lacks every label required by project.issue_filter.require_labels.", "project.issue_filter.require_labels", overviewValue(meta.RequireLabels), "Open Settings → Labels to change require labels, or edit hive.yaml project.issue_filter.require_labels.", "Labels", "/docs/dashboard.md#overview"),
		overviewBreakdownTerm("standing-meta-advisory", "standing meta/advisory", issueAdvisory, "Hive advisory reports are standing meta issues, not work to assign.", "standing meta title/label patterns", "hive advisory title or hive/advisory label", "Close/rename the advisory issue or remove the advisory label if it should become work.", "", "/docs/labels-and-control-signals.md"),
		overviewBreakdownTerm("dependency-dashboard", "dependency dashboard", issueDependencyDashboard, "Bot Dependency Dashboard issues are control panels, not actionable work.", "standing meta title/author patterns", "Dependency Dashboard from renovate[bot]/dependabot[bot]", "Rename/close the dashboard issue, or file concrete child work instead.", "", "/docs/dashboard.md#overview"),
		overviewBreakdownTerm("hold-adjacent-other-issues", "hold-adjacent/other issues", issueOther, "Open issue did not enter an explicit scanner bucket; this is the safety remainder.", "enumeration fallback", "unclassified open issue remainder", "Check labels and scanner rules; if this persists, add a dedicated classifier.", "", "/docs/dashboard.md#overview"),
	}
	issueRows = overviewExactOutsideRows(issueRows, issueOutside, "hold-adjacent-other-issues")

	prRows := []FrontendActionableBreakdownTerm{
		overviewBreakdownTerm("exempt-pr-labels", "exempt PR labels", totals.PRs.Breakdown["filtered"], "PR matches governor.labels.exempt or a permanent exempt label such as do-not-merge.", "governor.labels.exempt", overviewValue(meta.ExemptLabels), "Open Settings → Labels to edit exempt labels, or edit hive.yaml governor.labels.exempt.", "Labels", "/docs/labels-and-control-signals.md"),
		overviewBreakdownTerm("draft-prs", "draft PRs", totals.PRs.Breakdown["draft"], "Draft pull requests stay outside Actionable now until marked ready for review.", "GitHub draft PR state", "draft=true", "Use GitHub's Ready for review button when the PR should count.", "", "/docs/dashboard.md#overview"),
		overviewBreakdownTerm("hold-adjacent-other-prs", "hold-adjacent/other PRs", totals.PRs.Breakdown["other"], "Open PR did not enter an explicit scanner bucket; this is the safety remainder.", "enumeration fallback", "unclassified open PR remainder", "Check labels, draft state, and scanner rules; if this persists, add a dedicated classifier.", "", "/docs/dashboard.md#overview"),
	}
	prRows = overviewExactOutsideRows(prRows, prOutside, "hold-adjacent-other-prs")

	return overviewPartitionBreakdowns{
		IssueOutside: issueRows,
		PROutside:    prRows,
		IssueHeld: []FrontendActionableBreakdownTerm{
			overviewBreakdownTerm("issue-hold-labels", "held issues", totals.Issues.Held, "Issue carries a hold label and is intentionally parked for a maintainer.", "github.HoldLabels + hive scoped hold label", overviewValue(meta.HoldLabels), "Remove the hold label from the issue, or use the dashboard hold toggle.", "Labels", "/docs/labels-and-control-signals.md"),
		},
		PRHeld: []FrontendActionableBreakdownTerm{
			overviewBreakdownTerm("pr-hold-labels", "held PRs", totals.PRs.Held, "PR carries a hold label and is intentionally parked for a maintainer.", "github.HoldLabels + hive scoped hold label", overviewValue(meta.HoldLabels), "Remove the hold label from the PR, or use the dashboard hold toggle.", "Labels", "/docs/labels-and-control-signals.md"),
		},
		IssueBlocked: []FrontendActionableBreakdownTerm{
			overviewBreakdownTerm("needs-human", "needs-human", totals.Issues.Breakdown["needs_human"], "Issue carries the hard-suppress label needs-human, so Hive waits for a person.", "hardSuppressIssueLabels", overviewValue(meta.HardSuppressIssueLabels), "Remove needs-human after the person finishes the required action.", "Labels", "/docs/labels-and-control-signals.md"),
			overviewBreakdownTerm("issue-waiting-band", "waiting issue band", max(0, issueBlocked-totals.Issues.Breakdown["needs_human"]), "Overview waiting-band issues include open dependency links, claimed waiting states, or human gates.", "overview issue band specs", "server-provided overview_bands.issues", "Resolve the dependency or clear the waiting signal shown on the issue pill.", "", "/docs/dashboard.md#overview"),
		},
		PRBlocked: []FrontendActionableBreakdownTerm{
			overviewBreakdownTerm("pr-blocked-band", "blocked/waiting PR band", prBlocked, "Overview PR blocked/waiting bands cover merge conflicts, requested human review, needs-human, or other PR gates.", "overview PR band specs", "server-provided overview_bands.prs", "Resolve mergeability/review gates or clear the needs-human signal shown on the PR pill.", "", "/docs/dashboard.md#overview"),
		},
	}
}

func overviewBreakdownTerm(key, label string, count int, rule, settingPath, settingValue, how, tab, docs string) FrontendActionableBreakdownTerm {
	return FrontendActionableBreakdownTerm{Key: key, Label: label, Count: count, Rule: rule, SettingPath: settingPath, SettingValue: settingValue, HowToChange: how, SettingsTab: tab, DocsHref: docs}
}

func overviewExactOutsideRows(rows []FrontendActionableBreakdownTerm, want int, adjustKey string) []FrontendActionableBreakdownTerm {
	sum := 0
	for _, row := range rows {
		sum += row.Count
	}
	if delta := want - sum; delta != 0 {
		for i := range rows {
			if rows[i].Key == adjustKey {
				rows[i].Count += delta
				return rows
			}
		}
	}
	return rows
}

func overviewValue(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	return "[" + strings.Join(values, ", ") + "]"
}

func overviewReporterValue(meta overviewPartitionMeta) string {
	return fmt.Sprintf("enabled=%t; trusted_associations=%s; untrusted_require_labels=%s; awaiting_label=%s",
		meta.ReporterTrustEnabled, overviewValue(meta.ReporterTrustTrusted), overviewValue(meta.ReporterTrustRequiredLabels), meta.ReporterTrustAwaitingLabel)
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
