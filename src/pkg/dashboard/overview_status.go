package dashboard

import (
	"fmt"
	"net/url"
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
	Ownership    *Ownership `json:"ownership,omitempty"`
	ChartSlice   string     `json:"chart_slice,omitempty"`
	OverviewTerm string     `json:"overview_term,omitempty"`
}
type overviewStatusHold struct {
	github.HoldItem
	IssueBandInfo
	Ownership    *Ownership `json:"ownership,omitempty"`
	ChartSlice   string     `json:"chart_slice,omitempty"`
	OverviewTerm string     `json:"overview_term,omitempty"`
}
type overviewStatusPR struct {
	FrontendPR
	PRBandInfo
	Ownership    *Ownership `json:"ownership,omitempty"`
	ChartSlice   string     `json:"chart_slice,omitempty"`
	OverviewTerm string     `json:"overview_term,omitempty"`
}

// overviewStatusNonActionable is a non-actionable issue row with its
// your-turn decision. Parked rows (the four hard-suppress buckets) carry band
// waiting and the same band facts as any other issue row (chip, signals,
// stale flag, agent role) so the card renders them like every other issue;
// every other non-actionable row keeps no band and its bucket group.
// LinkedPRs on a parked row are the open and held pull requests of the same
// repository whose GitHub closing references name the issue, read from the
// snapshot's own pull-request rows. ChartSlice is set only for rows the
// charts plot: those that are your turn or waiting on someone else.
type overviewStatusNonActionable struct {
	github.RepoWorkIssue
	*IssueBandInfo
	LinkedPRs    []github.IssueLinkedPR `json:"linked_prs,omitempty"`
	Ownership    *Ownership             `json:"ownership,omitempty"`
	ChartSlice   string                 `json:"chart_slice,omitempty"`
	OverviewTerm string                 `json:"overview_term,omitempty"`
}

// parkedIssueLinkedPRs lists the open and held pull requests of repo whose
// closing references include the parked issue.
func parkedIssueLinkedPRs(repo FrontendRepo, repoName string, number int) []github.IssueLinkedPR {
	var linked []github.IssueLinkedPR
	for _, rows := range [][]any{repo.OpenPrs, repo.HeldPrs} {
		for _, raw := range rows {
			entry, ok := frontendPullRequest(raw)
			if !ok {
				continue
			}
			for _, closes := range entry.pr.LinkedIssues {
				if closes.Number != number || (closes.Repo != "" && !strings.EqualFold(closes.Repo, repoName)) {
					continue
				}
				linked = append(linked, github.IssueLinkedPR{Number: entry.pr.Number, Repo: nonEmpty(entry.pr.Repo, repoName), State: "open", URL: entry.pr.URL, Closing: true})
				break
			}
		}
	}
	return linked
}

// statusWithOverviewBands copies response containers, leaving the cached typed
// items untouched for concurrent readers and the export endpoints.
func (s *Server) statusWithOverviewBands(status *StatusPayload, now time.Time) *StatusPayload {
	cfg := config.DashboardIssueBandsConfig{}
	if s.deps != nil && s.deps.Config != nil {
		cfg = s.deps.Config.Dashboard.IssueBands
	}
	selfAuthorizationHoldActive := s.selfAuthorizationHoldActiveForRepo
	autoMergeLabel := s.autoMergeLabel()
	out := *status
	out.OverviewBands = &OverviewBands{Issues: IssueBandSpecs(cfg), PRs: PRBandSpecs(cfg)}
	slices := OverviewSliceSpecs(cfg)
	out.OverviewSlices = &slices
	out.Repos = append([]FrontendRepo(nil), status.Repos...)
	countSlice := func(specs []BandSpec, key string) {
		for i := range specs {
			if specs[i].Key == key {
				specs[i].Count++
			}
		}
	}
	issues := func(items []any, held bool, repoName string, decisions map[string]Ownership) []any {
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
			countSlice(out.OverviewBands.Issues, info.Band)
			ownership := ownershipFor(decisions, ownershipKindIssue, issue.Number)
			slice := overviewIssueChartSlice(issue, ownership)
			term := overviewIssueTerm(ownership, held)
			countSlice(slices.Issues, slice)
			switch v := raw.(type) {
			case github.HoldItem:
				result = append(result, overviewStatusHold{v, info, ownership, slice, term})
			case *github.HoldItem:
				result = append(result, overviewStatusHold{*v, info, ownership, slice, term})
			default:
				result = append(result, overviewStatusIssue{issue, info, ownership, slice, term})
			}
		}
		return result
	}
	nonActionable := func(items []any, repo FrontendRepo, repoName string) []any {
		if items == nil {
			return nil
		}
		result := make([]any, 0, len(items))
		for _, raw := range items {
			item, ok := repoWorkIssueRow(raw)
			if !ok {
				result = append(result, raw)
				continue
			}
			ownership := ownershipFor(repo.Ownership, ownershipKindIssue, item.Number)
			row := overviewStatusNonActionable{RepoWorkIssue: item, Ownership: ownership, ChartSlice: overviewNonActionableChartSlice(ownership), OverviewTerm: overviewNonActionableTerm(ownership)}
			if nonActionableIssueBand(item) != "" {
				info := nonActionableIssueInfo(item, repoName, cfg, now, selfAuthorizationHoldActive)
				row.IssueBandInfo = &info
				row.LinkedPRs = parkedIssueLinkedPRs(repo, repoName, item.Number)
				countSlice(out.OverviewBands.Issues, info.Band)
			}
			if row.ChartSlice != "" {
				countSlice(slices.Issues, row.ChartSlice)
			}
			result = append(result, row)
		}
		return result
	}
	prs := func(items []any, held bool, decisions map[string]Ownership) []any {
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
			info := prBand(entry.pr, entry.verdict, held, cfg, now, autoMergeLabel, status.HiveID)
			countSlice(out.OverviewBands.PRs, info.Band)
			ownership := ownershipFor(decisions, ownershipKindPR, entry.pr.Number)
			slice := overviewPRChartSlice(entry.pr, entry.verdict, ownership, autoMergeLabel)
			term := overviewPRTerm(ownership, held || len(holdLabels(entry.pr.Labels, status.HiveID)) > 0, entry.pr, entry.verdict, autoMergeLabel)
			countSlice(slices.PRs, slice)
			result = append(result, overviewStatusPR{FrontendPR{entry.pr, entry.verdict}, info, ownership, slice, term})
		}
		return result
	}
	for i := range out.Repos {
		repo := &out.Repos[i]
		repoName := overviewRepoName(*repo)
		repo.ActionableIssues = issues(repo.ActionableIssues, false, repoName, repo.Ownership)
		repo.HeldIssues = issues(repo.HeldIssues, true, repoName, repo.Ownership)
		repo.NonActionableIssues = nonActionable(repo.NonActionableIssues, *repo, repoName)
		repo.OpenPrs = prs(repo.OpenPrs, false, repo.Ownership)
		repo.HeldPrs = prs(repo.HeldPrs, true, repo.Ownership)
	}
	out.OverviewTotals = overviewTotals(status)
	var fullCfg *config.Config
	if s.deps != nil {
		fullCfg = s.deps.Config
	}
	meta := overviewPartitionMetadataFromConfig(fullCfg, status.HiveID)
	for _, repo := range out.Repos {
		if name := overviewRepoName(repo); strings.Contains(name, "/") {
			meta.Repos = append(meta.Repos, name)
		}
	}
	out.ActionableNow = overviewActionableNow(&out, meta, overviewPartitionOf(status, autoMergeLabel))
	out.LabelPresets = labelPresets(fullCfg, status.HiveID)
	s.attachSLA(&out, now)
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
		// Every listed row is an open item, so the forge count is at least the
		// rows the repository lists; the equation relies on it to stay a
		// partition.
		listedIssues := len(repo.ActionableIssues) + len(repo.HeldIssues) + len(repo.NonActionableIssues)
		if repo.WorkBreakdown != nil {
			ib := repo.WorkBreakdown.Issues
			totals.Issues.Forge += max(ib.Total(), listedIssues)
			addOverviewBreakdown(totals.Issues.Breakdown, "actionable", ib.Actionable)
			addOverviewBreakdown(totals.Issues.Breakdown, "hold", ib.Hold)
			addOverviewBreakdown(totals.Issues.Breakdown, "needs_human", ib.NeedsHuman)
			addOverviewBreakdown(totals.Issues.Breakdown, "needs_direction", ib.NeedsDirection)
			addOverviewBreakdown(totals.Issues.Breakdown, "needs_decision", ib.NeedsDecision)
			addOverviewBreakdown(totals.Issues.Breakdown, "needs_spec", ib.NeedsSpec)
			addOverviewBreakdown(totals.Issues.Breakdown, "reporter_confirmation", ib.ReporterConfirmation)
			addOverviewBreakdown(totals.Issues.Breakdown, "exempt", ib.Exempt)
			addOverviewBreakdown(totals.Issues.Breakdown, "filtered", overviewGenericIssueFiltered(ib))
			addOverviewBreakdown(totals.Issues.Breakdown, "reporter_triage", ib.ReporterTriage)
			addOverviewBreakdown(totals.Issues.Breakdown, "hive_advisory", ib.HiveAdvisory)
			addOverviewBreakdown(totals.Issues.Breakdown, "dependency_dashboard", ib.DependencyDashboard)
			addOverviewBreakdown(totals.Issues.Breakdown, "other", ib.Other)

			pb := repo.WorkBreakdown.PRs
			totals.PRs.Forge += max(pb.Total(), len(repo.OpenPrs)+len(repo.HeldPrs))
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
		totals.Issues.Forge += max(issueForge, listedIssues)
		totals.PRs.Forge += max(prForge, len(repo.OpenPrs)+len(repo.HeldPrs))
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
	named := b.NeedsHuman + b.NeedsDirection + b.NeedsDecision + b.NeedsSpec + b.ReporterConfirmation + b.Exempt
	return max(0, b.Filtered-named)
}

func overviewActionableNow(status *StatusPayload, meta overviewPartitionMeta, part overviewPartition) FrontendActionableNow {
	var counts FrontendActionableNow
	if status == nil {
		return counts
	}
	counts.Issues = part.issues.actionable
	counts.PRs = part.prs.actionable
	counts.Total = counts.Issues + counts.PRs
	totals := status.OverviewTotals
	if totals.Issues.Forge == 0 && totals.PRs.Forge == 0 && (len(status.Repos) > 0 || part.issues.tracked+part.prs.tracked > 0) {
		totals = overviewTotals(status)
	}
	counts.Equation, counts.IssueEquation, counts.PREquation = overviewActionableEquations(totals, part, meta)
	if counts.Equation != nil {
		if outside := overviewTerm(counts.Equation, "outside"); outside != nil {
			counts.Outside = &FrontendActionableOutside{Count: outside.Count, Breakdown: append([]FrontendActionableBreakdownTerm(nil), outside.Breakdown...)}
		}
	}
	return counts
}

// overviewActionableEquations builds the Actionable-now equation as an exact
// partition of the open items. Every open item counts in the first term it
// matches, in this order: Your turn, Waiting on someone else, held, blocked,
// outside (the scanner's filtered items and draft PRs), then actionable for
// the rest (hivecommons/hive#11507). Outside is what the scanner never put on
// a list, less the non-actionable rows that now count in Your turn or Waiting
// on someone else, plus the draft PRs on the lists.
func overviewActionableEquations(totals FrontendOverviewTotals, part overviewPartition, meta overviewPartitionMeta) (*FrontendActionableEquation, *FrontendActionableEquation, *FrontendActionableEquation) {
	issues, prs := part.issues, part.prs
	issueOutside := max(0, totals.Issues.Outside-issues.nonActionable)
	prOutside := max(0, totals.PRs.Outside) + prs.draft
	details := overviewPartitionDetails(totals, issueOutside, prOutside, part, meta)
	issueEq := overviewActionableKindEquation("issues", totals.Issues.Forge, issues.actionable, []FrontendActionableEquationTerm{
		{Key: "your_turn", Label: "Your turn", Count: issues.yourTurn, Rule: overviewYourTurnRule, Breakdown: details.IssueYourTurn},
		{Key: "waiting_elsewhere", Label: "Waiting on someone else", Count: issues.waiting, Rule: overviewWaitingRule, Breakdown: details.IssueWaiting},
		{Key: "held", Label: "held", Count: issues.held, Rule: "Issue has a hold label and no earlier term claimed it.", Breakdown: details.IssueHeld},
		{Key: "outside", Label: "outside", Count: issueOutside, Rule: "Open issues filtered out before Overview bands by this hive's scanner settings.", Breakdown: details.IssueOutside},
	})
	prEq := overviewActionableKindEquation("PRs", totals.PRs.Forge, prs.actionable, []FrontendActionableEquationTerm{
		{Key: "your_turn", Label: "Your turn", Count: prs.yourTurn, Rule: overviewYourTurnRule, Breakdown: details.PRYourTurn},
		{Key: "waiting_elsewhere", Label: "Waiting on someone else", Count: prs.waiting, Rule: overviewWaitingRule, Breakdown: details.PRWaiting},
		{Key: "held", Label: "held", Count: prs.held, Rule: "Pull request has a hold label and no earlier term claimed it.", Breakdown: details.PRHeld},
		{Key: "blocked", Label: "blocked", Count: prs.blocked, Rule: "Pull request has a blocked verdict, merge conflicts or failing CI.", Breakdown: details.PRBlocked},
		{Key: "outside", Label: "outside", Count: prOutside, Rule: "Open PRs filtered out before Overview bands by this hive's scanner settings, and draft PRs.", Breakdown: details.PROutside},
	})
	combined := overviewActionableCombinedEquation(totals, issueEq, prEq)
	return combined, issueEq, prEq
}

const (
	overviewYourTurnRule = "Items Hive is stuck on a person for: no agent lane will move them without a human action. Includes held, parked, outside and agent-filed items."
	overviewWaitingRule  = "Items gated on someone other than you: a hold a person or the dashboard pause placed, a Hive pull request within the adjudicator's wait, a pending reporter confirmation, or an issue an open PR covers."
)

// overviewActionableKindEquation lists the terms that subtract from the open
// count to leave the actionable items. A kind whose terms add up to more than
// the forge count (rows the totals did not count) is opened up to the sum so
// the equation stays a partition; a positive remainder shows as "other".
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
	} else if remainder < 0 {
		open = sum
	}
	eq := &FrontendActionableEquation{Kind: kind, Open: open, TotalOpen: open, Result: actionable, Terms: terms}
	eq.Text = overviewKindEquationText(eq)
	eq.Title = overviewKindEquationTitle(eq)
	return eq
}

// overviewActionableCombinedEquation adds the two kind equations term by
// term, in the order the first-match rule applies.
func overviewActionableCombinedEquation(totals FrontendOverviewTotals, issueEq, prEq *FrontendActionableEquation) *FrontendActionableEquation {
	openIssues := max(totals.Issues.Forge, issueEq.Open)
	openPRs := max(totals.PRs.Forge, prEq.Open)
	var terms []FrontendActionableEquationTerm
	for _, def := range []struct{ key, label, rule string }{
		{"your_turn", "Your turn", "Open issues and PRs waiting on a person: " + overviewYourTurnRule},
		{"waiting_elsewhere", "Waiting on someone else", "Open issues and PRs gated on someone other than you."},
		{"held", "held", "Open issues and PRs paused by hold labels that no earlier term claimed."},
		{"blocked", "blocked", "Open PRs with a blocked verdict, merge conflicts or failing CI."},
		{"outside", "outside", "Open items excluded before they enter Overview bands by this hive's scanner settings, and draft PRs."},
	} {
		count := overviewTermCount(issueEq, def.key) + overviewTermCount(prEq, def.key)
		if count == 0 {
			continue
		}
		terms = append(terms, FrontendActionableEquationTerm{Key: def.key, Label: def.label, Count: count, Rule: def.rule,
			Breakdown: combineOverviewBreakdown(overviewTermBreakdown(issueEq, def.key), overviewTermBreakdown(prEq, def.key))})
	}
	actionable := issueEq.Result + prEq.Result
	terms = append(terms, FrontendActionableEquationTerm{Key: "actionable", Label: "actionable", Count: actionable})
	if other := overviewTermCount(issueEq, "other") + overviewTermCount(prEq, "other"); other > 0 {
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
	return strings.ToUpper("actionable "+eq.Kind) + " — " + what + " that can move without waiting."
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

func overviewEquationTitle(eq *FrontendActionableEquation, _ FrontendOverviewTotals) string {
	if eq == nil {
		return ""
	}
	return "Actionable now — work that can move without waiting."
}

type overviewPartitionMeta struct {
	Repos                       []string
	ExemptLabels                []string
	RequireLabels               []string
	ReporterTrustEnabled        bool
	ReporterTrustRequiredLabels []string
	ReporterTrustTrusted        []string
	ReporterTrustAwaitingLabel  string
	HoldLabels                  []string
	HardSuppressNeedsHuman      []string
	HardSuppressNeedsDirection  []string
	HardSuppressNeedsDecision   []string
	HardSuppressNeedsSpec       []string
}

type overviewPartitionBreakdowns struct {
	IssueOutside  []FrontendActionableBreakdownTerm
	PROutside     []FrontendActionableBreakdownTerm
	IssueHeld     []FrontendActionableBreakdownTerm
	PRHeld        []FrontendActionableBreakdownTerm
	PRBlocked     []FrontendActionableBreakdownTerm
	IssueYourTurn []FrontendActionableBreakdownTerm
	PRYourTurn    []FrontendActionableBreakdownTerm
	IssueWaiting  []FrontendActionableBreakdownTerm
	PRWaiting     []FrontendActionableBreakdownTerm
}

func overviewPartitionMetadataFromConfig(cfg *config.Config, hiveID string) overviewPartitionMeta {
	if cfg == nil {
		return overviewPartitionDefaults(hiveID)
	}
	rt := cfg.Project.IssueFilter.ReporterTrust
	meta := overviewPartitionDefaults(hiveID)
	meta.ExemptLabels = append(append([]string(nil), cfg.Governor.Labels.Exempt...), github.PermanentExemptLabels...)
	meta.RequireLabels = append([]string(nil), cfg.Project.IssueFilter.RequireLabels...)
	hs := cfg.Project.IssueFilter.HardSuppressLabels
	meta.HardSuppressNeedsHuman = hs.EffectiveNeedsHuman()
	meta.HardSuppressNeedsDirection = hs.EffectiveNeedsDirection()
	meta.HardSuppressNeedsDecision = hs.EffectiveNeedsDecision()
	meta.HardSuppressNeedsSpec = hs.EffectiveNeedsSpec()
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
		HardSuppressNeedsHuman:      config.HardSuppressLabelsConfig{}.EffectiveNeedsHuman(),
		HardSuppressNeedsDirection:  config.HardSuppressLabelsConfig{}.EffectiveNeedsDirection(),
		HardSuppressNeedsDecision:   config.HardSuppressLabelsConfig{}.EffectiveNeedsDecision(),
		HardSuppressNeedsSpec:       config.HardSuppressLabelsConfig{}.EffectiveNeedsSpec(),
	}
}

// overviewOutsideBreakdown is the totals breakdown less the non-actionable
// rows the decision moved from Outside into Your turn or Waiting on someone
// else.
func overviewOutsideBreakdown(breakdown map[string]int, left map[string]int) map[string]int {
	out := make(map[string]int, len(breakdown))
	for key, n := range breakdown {
		out[key] = max(0, n-left[key])
	}
	return out
}

func overviewPartitionDetails(totals FrontendOverviewTotals, issueOutside, prOutside int, part overviewPartition, meta overviewPartitionMeta) overviewPartitionBreakdowns {
	issueBreakdown := overviewOutsideBreakdown(totals.Issues.Breakdown, part.issues.leftOutside)
	issueFiltered := issueBreakdown["filtered"]
	issueExempt := issueBreakdown["exempt"]
	issueReporter := issueBreakdown["reporter_triage"]
	issueAdvisory := issueBreakdown["hive_advisory"]
	issueDependencyDashboard := issueBreakdown["dependency_dashboard"]
	issueOther := issueBreakdown["other"]
	issueRows := []FrontendActionableBreakdownTerm{
		overviewBreakdownTerm("needs-direction", "needs-direction", issueBreakdown["needs_direction"], "Issue carries a needs-direction hard-suppress label, so Hive waits for maintainer direction before offering it to agents.", "project.issue_filter.hard_suppress_labels.needs_direction", overviewValue(meta.HardSuppressNeedsDirection), "Open Settings → Labels → Triage buckets to edit these labels, or remove the label from the issue.", "Labels", "/docs/dashboard-sections.md#overview"),
		overviewBreakdownTerm("needs-decision", "needs-decision", issueBreakdown["needs_decision"], "Issue carries a needs-decision hard-suppress label, so Hive waits for a maintainer decision.", "project.issue_filter.hard_suppress_labels.needs_decision", overviewValue(meta.HardSuppressNeedsDecision), "Open Settings → Labels → Triage buckets to edit these labels, or remove the label from the issue.", "Labels", "/docs/dashboard-sections.md#overview"),
		overviewBreakdownTerm("needs-spec", "needs-spec", issueBreakdown["needs_spec"], "Issue carries a needs-spec hard-suppress label, so Hive waits for specification or acceptance criteria.", "project.issue_filter.hard_suppress_labels.needs_spec", overviewValue(meta.HardSuppressNeedsSpec), "Open Settings → Labels → Triage buckets to edit these labels, or remove the label from the issue.", "Labels", "/docs/dashboard-sections.md#overview"),
		overviewBreakdownTerm("exempt-labels", "exempt labels", issueExempt, "Issue matches governor.labels.exempt or a permanent exempt label such as do-not-merge.", "governor.labels.exempt", overviewValue(meta.ExemptLabels), "Open Settings → Labels to edit exempt labels, or edit hive.yaml governor.labels.exempt.", "Labels", "/docs/dashboard-sections.md#overview"),
		overviewBreakdownTerm("reporter-triage", "reporter triage", issueReporter, "Reporter trust is enabled and the issue author is not trusted until a maintainer adds an allowed triage label.", "project.issue_filter.reporter_trust", overviewReporterValue(meta), "Open Settings → Labels → Reporter trust, or edit hive.yaml project.issue_filter.reporter_trust.", "Labels", "/docs/dashboard-sections.md#overview"),
		overviewBreakdownTerm("project-issue-filter", "project issue filter", issueFiltered, "Issue lacks every label required by project.issue_filter.require_labels.", "project.issue_filter.require_labels", overviewValue(meta.RequireLabels), "Open Settings → Labels to change require labels, or edit hive.yaml project.issue_filter.require_labels.", "Labels", "/docs/dashboard-sections.md#overview"),
		overviewBreakdownTerm("standing-meta-advisory", "standing meta/advisory", issueAdvisory, "Hive advisory reports are standing meta issues, not work to assign.", "standing meta title/label patterns", "hive advisory title or hive/advisory label", "Close/rename the advisory issue or remove the advisory label if it should become work.", "", "/docs/dashboard-sections.md#overview"),
		overviewBreakdownTerm("dependency-dashboard", "dependency dashboard", issueDependencyDashboard, "Bot Dependency Dashboard issues are control panels, not actionable work.", "standing meta title/author patterns", "Dependency Dashboard from renovate[bot]/dependabot[bot]", "Rename/close the dashboard issue, or file concrete child work instead.", "", "/docs/dashboard-sections.md#overview"),
		overviewBreakdownTerm("hold-adjacent-other-issues", "hold-adjacent/other issues", issueOther, "Open issue did not enter an explicit scanner bucket; this is the safety remainder.", "enumeration fallback", "unclassified open issue remainder", "Check labels and scanner rules; if this persists, add a dedicated classifier.", "", "/docs/dashboard-sections.md#overview"),
	}
	issueRows = overviewExactOutsideRows(issueRows, issueOutside, "hold-adjacent-other-issues")

	prRows := []FrontendActionableBreakdownTerm{
		overviewBreakdownTerm("exempt-pr-labels", "exempt PR labels", totals.PRs.Breakdown["filtered"], "PR matches governor.labels.exempt or a permanent exempt label such as do-not-merge.", "governor.labels.exempt", overviewValue(meta.ExemptLabels), "Open Settings → Labels to edit exempt labels, or edit hive.yaml governor.labels.exempt.", "Labels", "/docs/dashboard-sections.md#overview"),
		overviewBreakdownTerm("draft-prs", "draft PRs", totals.PRs.Breakdown["draft"], "Draft pull requests stay outside Actionable now until marked ready for review.", "GitHub draft PR state", "draft=true", "Use GitHub's Ready for review button when the PR should count.", "", "/docs/dashboard-sections.md#overview"),
		overviewBreakdownTerm("hold-adjacent-other-prs", "hold-adjacent/other PRs", totals.PRs.Breakdown["other"], "Open PR did not enter an explicit scanner bucket; this is the safety remainder.", "enumeration fallback", "unclassified open PR remainder", "Check labels, draft state, and scanner rules; if this persists, add a dedicated classifier.", "", "/docs/dashboard-sections.md#overview"),
	}
	prRows = overviewExactOutsideRows(prRows, prOutside, "hold-adjacent-other-prs")

	return overviewPartitionBreakdowns{
		IssueOutside: issueRows,
		PROutside:    prRows,
		IssueHeld: []FrontendActionableBreakdownTerm{
			overviewBreakdownTerm("issue-hold-labels", "held issues", part.issues.held, "Issue carries a hold label and is intentionally parked; a person's hold or the dashboard pause counts under Waiting on someone else.", "github.HoldLabels + hive scoped hold label", overviewValue(meta.HoldLabels), "Remove the hold label from the issue, or use the dashboard hold toggle.", "Labels", "/docs/dashboard-sections.md#overview"),
		},
		PRHeld: []FrontendActionableBreakdownTerm{
			overviewBreakdownTerm("pr-hold-labels", "held PRs", part.prs.held, "PR carries a hold label and is intentionally parked; a hold whose sources are Hive's own policy counts under Your turn.", "github.HoldLabels + hive scoped hold label", overviewValue(meta.HoldLabels), "Remove the hold label from the PR, or use the dashboard hold toggle.", "Labels", "/docs/dashboard-sections.md#overview"),
		},
		PRBlocked: []FrontendActionableBreakdownTerm{
			overviewBreakdownTerm("pr-blocked-band", "blocked PRs", part.prs.blocked, "Pull requests with a blocked merge verdict, merge conflicts or failing CI.", "overview PR band specs", "server-provided overview_bands.prs", "Resolve mergeability or the failing checks shown on the PR pill.", "", "/docs/dashboard-sections.md#overview"),
		},
		IssueYourTurn: part.issues.yourTurnBreakdown(),
		PRYourTurn:    part.prs.yourTurnBreakdown(),
		IssueWaiting:  part.issues.waitingBreakdown(overviewReporterSearchHref(meta.Repos)),
		PRWaiting:     part.prs.waitingBreakdown(overviewReporterSearchHref(meta.Repos)),
	}
}

// overviewReporterSearchHref searches the hive's watched repos; without any it
// points at the docs instead of another project's issues.
func overviewReporterSearchHref(repos []string) string {
	if len(repos) == 0 {
		return "/docs/dashboard.md#outside-buckets"
	}
	q := "is:open label:needs-reporter-confirmation"
	for _, r := range repos {
		q += " repo:" + r
	}
	return "https://github.com/issues?q=" + url.QueryEscape(q)
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
