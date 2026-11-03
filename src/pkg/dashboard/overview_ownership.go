package dashboard

import (
	"fmt"
	"sort"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The Overview reads the server's your-turn decision (hivecommons/hive#11507,
// part D1): the Actionable-now equation partitions every open item into
// exactly one term, and every chart shape plots every item in exactly one
// slice. Both are derived here from the stored decision, never from labels or
// bands in the browser.

const (
	// Chart slice keys that come from the ownership decision. The remaining
	// slice keys are band keys.
	overviewSliceYourTurn = "your-turn"
	overviewSliceToClose  = "to-close"
	overviewSliceWaiting  = "waiting-elsewhere"

	// overviewBandOutside is the export band of a non-actionable issue row
	// that carries no band of its own.
	overviewBandOutside = "outside"
)

// OverviewSlices is the ordered slice taxonomy of the Overview charts. Counts
// cover every repository in the snapshot; the browser re-counts the rows'
// chart_slice for the repositories selected in the Overview.
type OverviewSlices struct {
	Issues []BandSpec `json:"issues"`
	PRs    []BandSpec `json:"prs"`
}

// overviewOwnershipSlice returns the ownership slice of a row: Your turn
// (every verb but CLOSE), To close, or Waiting on someone else. Rows that are
// none of these return "" and fall to their band.
func overviewOwnershipSlice(o *Ownership) string {
	switch {
	case o == nil:
		return ""
	case o.YourTurn && o.Verb == OwnershipVerbClose:
		return overviewSliceToClose
	case o.YourTurn:
		return overviewSliceYourTurn
	case o.WaitingElsewhere:
		return overviewSliceWaiting
	}
	return ""
}

func overviewOwnershipSliceSpecs() []BandSpec {
	return []BandSpec{
		{Key: overviewSliceYourTurn, Label: "Your turn", Short: "your turn", Rule: "Hive is stuck on a person: no agent lane will move the item until you act. Items to close are counted separately."},
		{Key: overviewSliceToClose, Label: "To close", Short: "to close", Rule: "Hive verified the work as done but could not close it, or a close is waiting on you — verify and close the item."},
		{Key: overviewSliceWaiting, Label: "Waiting on someone else", Short: "waiting", Rule: "Gated on someone other than you: a hold a person or the dashboard pause placed, a Hive pull request still within the adjudicator's wait, a pending reporter confirmation, an issue an open PR covers, or a blocked issue with an open GitHub blocked-by link."},
	}
}

// OverviewSliceSpecs lists the chart slices in plot order: the three
// ownership slices, then the band slices an item gets with the waiting,
// agent-filed and done rules skipped.
func OverviewSliceSpecs(cfg config.DashboardIssueBandsConfig) OverviewSlices {
	issues := overviewOwnershipSliceSpecs()
	for _, key := range []string{"ready", "in-progress"} {
		issues = append(issues, issueBandSpec(key, cfg))
	}
	prs := overviewOwnershipSliceSpecs()
	for _, key := range prBandOrder {
		if key != "waiting" {
			prs = append(prs, prBandSpec(key, cfg))
		}
	}
	return OverviewSlices{Issues: issues, PRs: prs}
}

// overviewIssueChartSlice is the chart slice of an issue row: its ownership
// slice, otherwise the band it gets with the waiting, agent-filed and done
// rules skipped.
func overviewIssueChartSlice(issue github.Issue, o *Ownership) string {
	if slice := overviewOwnershipSlice(o); slice != "" {
		return slice
	}
	return issueChartBand(issue)
}

// overviewPRChartSlice is the chart slice of a PR row.
func overviewPRChartSlice(pr github.PullRequest, verdict *github.MergeVerdict, o *Ownership, autoMergeLabel string) string {
	if slice := overviewOwnershipSlice(o); slice != "" {
		return slice
	}
	return prChartBand(pr, verdict, autoMergeLabel)
}

// overviewNonActionableChartSlice is the chart slice of a non-actionable
// issue row: only rows that are your turn or waiting on someone else are
// plotted.
func overviewNonActionableChartSlice(o *Ownership) string {
	return overviewOwnershipSlice(o)
}

// nonActionableIssueBand is the band of a non-actionable issue row: parked
// rows (the four hard-suppress buckets) carry band waiting, every other row
// has none.
func nonActionableIssueBand(item github.RepoWorkIssue) string {
	if repoWorkIssueParkingBucket(item) != "" {
		return "waiting"
	}
	return ""
}

// nonActionableIssueInfo is the band facts of a non-actionable issue row, the
// same as any other issue row (chip, signals, stale flag, agent role). The
// hard-suppress bucket decides the band, not the configurable waiting-label
// list; a parked row with no chip of its own takes its reason's label chip.
func nonActionableIssueInfo(item github.RepoWorkIssue, repoName string, cfg config.DashboardIssueBandsConfig, now time.Time, selfAuthorizationHoldActive func(string) bool) IssueBandInfo {
	info := IssueBand(github.Issue{
		Repo: nonEmpty(item.Repo, repoName), Number: item.Number, Title: item.Title, Author: item.Author,
		Labels: item.Labels, Assignees: item.Assignees, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, URL: item.URL,
		HumanAcknowledged: item.HumanAcknowledged,
	}, false, cfg, now, selfAuthorizationHoldActive)
	info.Band = nonActionableIssueBand(item)
	if info.Band != "" && info.Chip == "" {
		info.Chip = issueLabelChip(labelSet([]string{item.Reason}))
	}
	return info
}

// Equation terms. A row's term is the first one it matches, in this order.
const (
	overviewTermYourTurn   = "your_turn"
	overviewTermWaiting    = "waiting_elsewhere"
	overviewTermHeld       = "held"
	overviewTermBlocked    = "blocked"
	overviewTermOutside    = "outside"
	overviewTermActionable = "actionable"
)

func overviewOwnershipTerm(o *Ownership) string {
	switch {
	case o != nil && o.YourTurn:
		return overviewTermYourTurn
	case o != nil && o.WaitingElsewhere:
		return overviewTermWaiting
	}
	return ""
}

// overviewIssueTerm is the equation term of an issue on the actionable or held
// list.
func overviewIssueTerm(o *Ownership, held bool) string {
	if term := overviewOwnershipTerm(o); term != "" {
		return term
	}
	if held {
		return overviewTermHeld
	}
	return overviewTermActionable
}

// overviewNonActionableTerm is the equation term of a non-actionable issue
// row: Your turn or Waiting on someone else when the decision says so,
// otherwise it stays in Outside with the other items the scanner filtered.
func overviewNonActionableTerm(o *Ownership) string {
	if term := overviewOwnershipTerm(o); term != "" {
		return term
	}
	return overviewTermOutside
}

// overviewPRTerm is the equation term of a PR on the open or held list. After
// the ownership terms and held, a PR is blocked when it has a blocked verdict,
// merge conflicts or failing CI, outside when it is a draft, and actionable
// otherwise. It reads the facts, not the merge-state band, so a draft with a
// GitHub review decision or an auto-merge-labelled conflicting PR is
// classified by what it is.
func overviewPRTerm(o *Ownership, held bool, pr github.PullRequest, verdict *github.MergeVerdict, autoMergeLabel string) string {
	if term := overviewOwnershipTerm(o); term != "" {
		return term
	}
	if held {
		return overviewTermHeld
	}
	if _, blocked, _ := prBandFlags(pr, verdict, labelSet(pr.Labels), autoMergeLabel, prGitHubReview(pr)); blocked {
		return overviewTermBlocked
	}
	if pr.Draft {
		return overviewTermOutside
	}
	return overviewTermActionable
}

// Keys of the Outside breakdown rows (overviewPartitionDetails) that a
// non-actionable row can leave when the decision moves it into another term.
const (
	outsideKeyNeedsHuman     = "needs_human"
	outsideKeyNeedsDirection = "needs_direction"
	outsideKeyNeedsDecision  = "needs_decision"
	outsideKeyNeedsSpec      = "needs_spec"
	outsideKeyReporter       = "reporter_triage"
	outsideKeyConfirmation   = "reporter_confirmation"
	outsideKeyOther          = "other"
)

// nonActionableOutsideKey names the totals breakdown bucket a non-actionable
// issue row is counted in.
func nonActionableOutsideKey(item github.RepoWorkIssue) string {
	switch repoWorkIssueParkingBucket(item) {
	case config.DefaultNeedsHumanLabel:
		return outsideKeyNeedsHuman
	case config.DefaultNeedsDirectionLabel:
		return outsideKeyNeedsDirection
	case config.DefaultNeedsDecisionLabel:
		return outsideKeyNeedsDecision
	case config.DefaultNeedsSpecLabel:
		return outsideKeyNeedsSpec
	}
	if item.Bucket == repoWorkBucketReporterTriage {
		return outsideKeyReporter
	}
	if item.Bucket == repoWorkBucketFiltered && item.Reason == ownershipLabelReporterConfirmation {
		return outsideKeyConfirmation
	}
	return outsideKeyOther
}

// overviewKindPartition counts one kind's open items per equation term. The
// first matching term decides: Your turn, Waiting on someone else, held,
// blocked (PRs), outside (draft PRs here; the scanner's filtered items are
// the rest of Outside), actionable.
type overviewKindPartition struct {
	yourTurn   int
	waiting    int
	held       int
	blocked    int
	draft      int
	actionable int
	// tracked is the rows on the actionable and held lists.
	tracked int
	// nonActionable is the your-turn and waiting rows taken from the
	// non-actionable list; they leave Outside for their own term.
	nonActionable int
	// yourTurnByCode and waitingByCause break the two ownership terms down.
	yourTurnByCode map[string]int
	verbByCode     map[string]string
	waitingByCause map[string]int
	// leftOutside counts the non-actionable rows moved out of Outside by
	// totals breakdown bucket.
	leftOutside map[string]int
}

type overviewPartition struct {
	issues overviewKindPartition
	prs    overviewKindPartition
}

func newOverviewKindPartition() overviewKindPartition {
	return overviewKindPartition{
		yourTurnByCode: map[string]int{},
		verbByCode:     map[string]string{},
		waitingByCause: map[string]int{},
		leftOutside:    map[string]int{},
	}
}

const (
	waitingCauseHold        = "hold"
	waitingCauseReporter    = "reporter"
	waitingCauseCovered     = "covered"
	waitingCauseAdjudicator = "adjudicator"
)

var overviewWaitingCauseLabels = map[string]string{
	waitingCauseHold:        "held by a person or paused",
	waitingCauseReporter:    "awaiting reporter confirmation",
	waitingCauseCovered:     "covered by an open PR",
	waitingCauseAdjudicator: "within the adjudicator wait",
}

var overviewWaitingCauseOrder = []string{waitingCauseHold, waitingCauseAdjudicator, waitingCauseReporter, waitingCauseCovered}

// overviewIssueWaitingCause names why a not-your-turn issue waits on someone
// else: a hold, an open PR that covers it, or the reporter's confirmation.
func overviewIssueWaitingCause(held bool, bucket string) string {
	switch {
	case held:
		return waitingCauseHold
	case bucket == repoWorkBucketClaimedByPR:
		return waitingCauseCovered
	}
	return waitingCauseReporter
}

// overviewPartitionOf classifies every issue and PR row of the snapshot into
// its one equation term, reading the stored ownership decision.
func overviewPartitionOf(status *StatusPayload, autoMergeLabel string) overviewPartition {
	part := overviewPartition{issues: newOverviewKindPartition(), prs: newOverviewKindPartition()}
	if status == nil {
		return part
	}
	for _, repo := range status.Repos {
		issueRows := func(items []any, held bool) {
			for _, raw := range items {
				issue, ok := frontendIssue(raw)
				if !ok {
					continue
				}
				part.issues.tracked++
				o := ownershipFor(repo.Ownership, ownershipKindIssue, issue.Number)
				var sources []string
				switch v := raw.(type) {
				case github.HoldItem:
					sources = v.HoldSources
				case *github.HoldItem:
					sources = v.HoldSources
				}
				// The cause follows the facts the ownership decision used:
				// rule 1 (a hiding hold), then rule 3 (open linked PR).
				bucket := ""
				if !(held && holdSourcesHide(sources)) && issueHasOpenLinkedPR(issue) {
					bucket = repoWorkBucketClaimedByPR
				}
				part.issues.add(overviewIssueTerm(o, held), o, overviewIssueWaitingCause(held && holdSourcesHide(sources), bucket))
			}
		}
		issueRows(repo.ActionableIssues, false)
		issueRows(repo.HeldIssues, true)
		for _, raw := range repo.NonActionableIssues {
			item, ok := repoWorkIssueRow(raw)
			if !ok {
				continue
			}
			o := ownershipFor(repo.Ownership, ownershipKindIssue, item.Number)
			term := overviewNonActionableTerm(o)
			if term == overviewTermOutside {
				continue
			}
			part.issues.add(term, o, overviewIssueWaitingCause(false, item.Bucket))
			part.issues.nonActionable++
			part.issues.leftOutside[nonActionableOutsideKey(item)]++
		}
		prRows := func(items []any, held bool) {
			for _, raw := range items {
				entry, ok := frontendPullRequest(raw)
				if !ok {
					continue
				}
				part.prs.tracked++
				o := ownershipFor(repo.Ownership, ownershipKindPR, entry.pr.Number)
				cause := waitingCauseAdjudicator
				if held {
					cause = waitingCauseHold
				}
				isHeld := held || len(holdLabels(entry.pr.Labels, status.HiveID)) > 0
				part.prs.add(overviewPRTerm(o, isHeld, entry.pr, entry.verdict, autoMergeLabel), o, cause)
			}
		}
		prRows(repo.OpenPrs, false)
		prRows(repo.HeldPrs, true)
	}
	return part
}

// add counts one row in its term.
func (p *overviewKindPartition) add(term string, o *Ownership, waitingCause string) {
	switch term {
	case overviewTermYourTurn:
		p.yourTurn++
		p.yourTurnByCode[o.ReasonCode]++
		p.verbByCode[o.ReasonCode] = o.Verb
	case overviewTermWaiting:
		p.waiting++
		p.waitingByCause[waitingCause]++
	case overviewTermHeld:
		p.held++
	case overviewTermBlocked:
		p.blocked++
	case overviewTermOutside:
		p.draft++
	default:
		p.actionable++
	}
}

// breakdown rows for the your-turn and waiting terms.
func (p overviewKindPartition) yourTurnBreakdown() []FrontendActionableBreakdownTerm {
	codes := make([]string, 0, len(p.yourTurnByCode))
	for code := range p.yourTurnByCode {
		codes = append(codes, code)
	}
	sort.Slice(codes, func(i, j int) bool {
		vi, vj := bandRank(ownershipVerbOrder, p.verbByCode[codes[i]]), bandRank(ownershipVerbOrder, p.verbByCode[codes[j]])
		if vi != vj {
			return vi < vj
		}
		return codes[i] < codes[j]
	})
	rows := make([]FrontendActionableBreakdownTerm, 0, len(codes))
	for _, code := range codes {
		verb := p.verbByCode[code]
		rows = append(rows, overviewBreakdownTerm("your-turn-"+code, fmt.Sprintf("%s · %s", verb, code), p.yourTurnByCode[code],
			"Hive will not move these items until a person acts ("+verb+").", "ownership decision", code, "Act on the item from the Your turn list, or follow the reason shown on its marker.", "", "/docs/dashboard.md#repository-card-legend-issue-bands-and-pr-bands"))
	}
	return rows
}

func (p overviewKindPartition) waitingBreakdown(reporterSearchHref string) []FrontendActionableBreakdownTerm {
	var rows []FrontendActionableBreakdownTerm
	for _, cause := range overviewWaitingCauseOrder {
		n := p.waitingByCause[cause]
		if n <= 0 {
			continue
		}
		if cause == waitingCauseReporter {
			rows = append(rows, overviewBreakdownTerm("waiting-on-reporter", "waiting on reporter", n,
				"Issue carries needs-reporter-confirmation, so Hive is waiting for the reporter's confirmation before closing.", "label:needs-reporter-confirmation", "is:open label:needs-reporter-confirmation", "Use the linked GitHub search, then reply /fixed or add new evidence.", "", reporterSearchHref))
			continue
		}
		rows = append(rows, overviewBreakdownTerm("waiting-"+cause, overviewWaitingCauseLabels[cause], n,
			"Gated on someone other than you, so it is not your turn.", "ownership decision", cause, "It moves when the other party acts or the gating condition changes.", "", "/docs/dashboard.md#repository-card-legend-issue-bands-and-pr-bands"))
	}
	return rows
}
