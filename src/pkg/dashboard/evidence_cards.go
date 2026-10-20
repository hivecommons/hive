package dashboard

import (
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

// Evidence card keys match the dashboard's data-ev-panel attributes.
const (
	evidenceCardOutcome         = "outcome-by-author"
	evidenceCardCalibration     = "calibration"
	evidenceCardAccuracy        = "accuracy-by-model"
	evidenceCardMaintainability = "maintainability"
	evidenceCardBoundary        = "boundary-enforcement"
	evidenceCardCIGate          = "claim-vs-verdict"
	evidenceCardWorkBalance     = "work-balance"
	evidenceCardShadow          = "shadow-gates"
)

// evidenceCards derives the per-card letter grade and plain-English summary
// (#13204) from a fully populated summary. It only reads fields the summary
// already exposes, so the card and the chart below it can never disagree.
func evidenceCards(resp *EvidenceSummary, win evidenceWindow, now time.Time) map[string]evidence.Card {
	cards := map[string]evidence.Card{}

	outcomes := make([]evidence.OutcomeStats, 0, len(resp.Outcomes))
	kinds := make([]string, 0, len(resp.Outcomes))
	for k := range resp.Outcomes {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		o := resp.Outcomes[k]
		s := evidence.OutcomeStats{Kind: k, N: o.N, Merged: o.Merged, Closed: o.Closed, Open: o.Open, Unknown: o.Unknown}
		if o.RevertedWithin14d != nil {
			s.Reverted = *o.RevertedWithin14d
		}
		outcomes = append(outcomes, s)
	}
	cards[evidenceCardOutcome] = evidence.Card{Grade: evidence.GradeOutcome(outcomes), PlainSummary: evidence.SummarizeOutcome(outcomes)}

	buckets := make([]evidence.CalibrationBucket, len(evidenceSummaryBuckets))
	for i, b := range evidenceSummaryBuckets {
		buckets[i].Bucket = b.name
	}
	for _, p := range resp.Calibration {
		for i, b := range p.Buckets {
			if i >= len(buckets) {
				break
			}
			buckets[i].Known += b.N - b.Unknown
			buckets[i].Merged += b.Merged
		}
	}
	if len(resp.Calibration) == 0 {
		buckets = nil
	}
	cards[evidenceCardCalibration] = evidence.Card{Grade: evidence.GradeCalibration(buckets), PlainSummary: evidence.SummarizeCalibration(buckets)}

	acc := evidence.AccuracyStats{}
	for _, m := range resp.ReviewerModels {
		acc.BundleVerdicts += m.N
	}
	if ra := resp.ReviewerAccuracy; ra != nil {
		acc.Available = true
		acc.Samples = ra.Samples
		var approvals, falseApprovals, blocks, falseBlocks int
		for _, g := range ra.ReviewerModels {
			approvals += g.Approvals
			falseApprovals += g.FalseApprovals
			blocks += g.Blocks
			falseBlocks += g.FalseBlocks
		}
		if approvals > 0 {
			acc.FalseApproveRate = float64(falseApprovals) / float64(approvals)
		}
		if blocks > 0 {
			acc.FalseBlockRate = float64(falseBlocks) / float64(blocks)
		}
	}
	cards[evidenceCardAccuracy] = evidence.Card{Grade: evidence.GradeAccuracy(acc), PlainSummary: evidence.SummarizeAccuracy(acc)}

	trends, reposWithSnapshots := evidenceComplexityTrends(resp.ComplexityByRepo, resp.Repo, win, now)
	cards[evidenceCardMaintainability] = evidence.Card{Grade: evidence.GradeMaintainability(trends), PlainSummary: evidence.SummarizeMaintainability(trends, reposWithSnapshots)}

	boundary := evidence.BoundaryStats{TotalWrites: resp.WriteActivity.TotalWrites, Blocked: resp.WriteActivity.Blocked}
	for _, wk := range resp.SentinelPerWeek.Weeks {
		boundary.SentinelHits += wk.Hits
	}
	cards[evidenceCardBoundary] = evidence.Card{Grade: evidence.GradeBoundary(boundary), PlainSummary: evidence.SummarizeBoundary(boundary)}

	gate := resp.CIRecovery.Gate
	conv := resp.CIRecovery.Convergence
	ci := evidence.CIGateStats{MergedN: gate.MergedN, WithChecks: gate.WithChecks, Breaches: gate.Breaches, AgentSettled: conv.Settled, MedianPasses: conv.MedianPasses}
	cards[evidenceCardCIGate] = evidence.Card{Grade: evidence.GradeCIGate(ci), PlainSummary: evidence.SummarizeCIGate(ci)}

	wb := evidence.WorkBalanceStats{}
	for _, w := range resp.WorkBalance {
		for _, v := range w.Opened {
			wb.IssuesOpened += v
		}
		for _, v := range w.PRsOpened {
			wb.PRsOpened += v
		}
		wb.IssuesClosed += w.Closed
		wb.PRsMerged += w.PRsMerged
		if w.QueueIssues != nil {
			wb.QueueIssues = w.QueueIssues
		}
		if w.QueuePRs != nil {
			wb.QueuePRs = w.QueuePRs
		}
	}
	cards[evidenceCardWorkBalance] = evidence.Card{Grade: evidence.GradeWorkBalance(wb), PlainSummary: evidence.SummarizeWorkBalance(wb)}

	sg := resp.ShadowGate
	shadow := evidence.ShadowStats{N: sg.N}
	for _, side := range []EvidenceShadowGateSide{sg.ReviewGate, sg.Intent} {
		shadow.Held += side.Held.N
		shadow.HeldMerged += side.Held.Merged
		shadow.HeldReverted += side.Held.Reverted
		shadow.PassedReverted += side.Passed.Reverted
	}
	cards[evidenceCardShadow] = evidence.Card{Grade: evidence.GradeShadow(shadow), PlainSummary: evidence.SummarizeShadow(shadow)}

	return cards
}

// evidenceComplexityTrends applies the dashboard's month window to the
// complexity series (mirrors evMaintainabilityRows in index.html: a custom
// range uses from/to with to exclusive, a preset uses hours back from now,
// all-time keeps every month) and returns first-vs-latest trends for repos
// with at least two snapshots plus the number of repos with any snapshot.
func evidenceComplexityTrends(series []EvidenceComplexityRepo, repo string, win evidenceWindow, now time.Time) ([]evidence.ComplexityTrend, int) {
	month := func(t time.Time) string { return t.UTC().Format("2006-01") }
	start, end := "", month(now)
	switch {
	case win.Custom && win.To.After(win.From):
		start, end = month(win.From), month(win.To.Add(-time.Millisecond))
	case win.Hours > 0:
		start = month(now.Add(-time.Duration(win.Hours) * time.Hour))
	}
	var trends []evidence.ComplexityTrend
	withSnapshots := 0
	for _, r := range series {
		if repo != "" && !strings.EqualFold(r.Repo, repo) {
			continue
		}
		months := make([]EvidenceComplexityMonth, 0, len(r.Months))
		for _, m := range r.Months {
			if len(m.Month) == 7 && m.Month >= start && m.Month <= end {
				months = append(months, m)
			}
		}
		if len(months) == 0 {
			continue
		}
		withSnapshots++
		if len(months) < 2 {
			continue
		}
		sort.Slice(months, func(i, j int) bool { return months[i].Month < months[j].Month })
		first, last := months[0], months[len(months)-1]
		trends = append(trends, evidence.ComplexityTrend{
			Repo:        r.Repo,
			FirstMedian: first.MedianCyclomatic, LastMedian: last.MedianCyclomatic,
			FirstOver: first.FunctionsOverThreshold, LastOver: last.FunctionsOverThreshold,
		})
	}
	return trends, withSnapshots
}
