package review

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// Review-heads decision step (hivecommons/hive#13542, part 2): the heads'
// raw reports are evidence, and one decision session per PR reconciles them
// into a single decided report per perspective. This file holds the
// aggregation side: telling raw from decided reports, recognising when a PR
// head has settled, the no-findings skip rule, and the shapes the decision
// step records.

// Finding dispositions the decision step may give a head's finding.
const (
	DispositionConfirmed       = "confirmed"
	DispositionNotReproducible = "not_reproducible"
	DispositionOutOfScope      = "out_of_scope"
)

// FindingDisposition is the decision step's call on one finding a head
// raised: which head, which finding (by title and location), whether it
// stands, and why. Severity is the final severity for a confirmed finding.
type FindingDisposition struct {
	Perspective Perspective           `json:"perspective,omitempty"`
	Head        string                `json:"head"`
	Title       string                `json:"title"`
	File        string                `json:"file,omitempty"`
	Line        int                   `json:"line,omitempty"`
	Disposition string                `json:"disposition"`
	Reason      string                `json:"reason"`
	Severity    outputschema.Severity `json:"severity,omitempty"`
}

// HeadReport is one head's raw report for one perspective, as the decision
// step and reviewer accuracy read it.
type HeadReport struct {
	Perspective      Perspective            `json:"perspective"`
	Head             string                 `json:"head"`
	ReviewModel      string                 `json:"review_model,omitempty"`
	Verdict          Verdict                `json:"verdict"`
	NotApplicable    bool                   `json:"not_applicable,omitempty"`
	Summary          string                 `json:"summary,omitempty"`
	Findings         []outputschema.Finding `json:"findings,omitempty"`
	DecisionQuestion string                 `json:"decision_question,omitempty"`
}

func validDisposition(d string) bool {
	switch d {
	case DispositionConfirmed, DispositionNotReproducible, DispositionOutOfScope:
		return true
	}
	return false
}

// validateDispositions checks the decision step's dispositions. A report
// with none (every head report, every ordinary review) passes.
func validateDispositions(ds []FindingDisposition) error {
	for i, d := range ds {
		if strings.TrimSpace(d.Head) == "" || strings.TrimSpace(d.Title) == "" {
			return fmt.Errorf("dispositions[%d]: head and title are required", i)
		}
		if !validDisposition(strings.TrimSpace(d.Disposition)) {
			return fmt.Errorf("dispositions[%d]: disposition must be one of %s, %s, %s", i, DispositionConfirmed, DispositionNotReproducible, DispositionOutOfScope)
		}
		if strings.TrimSpace(d.Reason) == "" {
			return fmt.Errorf("dispositions[%d]: reason is required", i)
		}
		if d.Severity != "" && severityRank(d.Severity) == 0 {
			return fmt.Errorf("dispositions[%d]: severity %q is not a known severity", i, d.Severity)
		}
	}
	return nil
}

// usesReviewHeads reports whether a verdict group came from review heads.
func usesReviewHeads(reports []PerspectiveReport) bool {
	for _, r := range reports {
		if strings.TrimSpace(r.Head) != "" || r.Decided {
			return true
		}
	}
	return false
}

// aggregateWithHeads aggregates a PR head whose perspectives were judged by
// review heads.
//
//   - Decided reports present: they are the verdict. Routing (block_at,
//     backlog, fix kicks, human holds) runs on them exactly as on any
//     ordinary review; the heads' raw reports ride along as evidence.
//   - No decided reports, every head settled, every head clean (approve, no
//     findings, no decision question): the skip rule. The heads agree there
//     is nothing to reconcile, so their unanimous approval is the decision.
//   - Otherwise the head is not settled: incomplete, never routed on raw head
//     reports. AwaitingDecision marks the state the planner turns into the
//     decision kick.
//
// A report that carries neither a head nor a decision (a review recorded
// before heads were configured, or an advisory queue-mode verdict) is already
// one reviewer's final word on its perspective and counts as decided for it.
func aggregateWithHeads(reports []PerspectiveReport, opts AggregateOptions) Aggregate {
	var final, heads []PerspectiveReport
	for _, r := range reports {
		switch {
		case r.Decided:
			final = append(final, r)
		case strings.TrimSpace(r.Head) != "":
			heads = append(heads, r)
		default:
			final = append(final, r)
		}
	}
	missing := missingHeadsFor(reports, opts)
	if hasDecided(final) {
		agg := aggregatePlainReports(final, opts)
		agg.Decided = true
		attachHeadEvidence(&agg, heads, final, missing)
		return agg
	}

	// Historical reports remain evidence, but only configured perspectives can
	// receive new head dispatches or participate in the current decision.
	allHeads := heads
	configured := map[Perspective]bool{}
	for _, p := range opts.Perspectives.List() {
		configured[p] = true
	}
	heads = nil
	for _, r := range allHeads {
		if configured[r.Perspective] {
			heads = append(heads, r)
		}
	}
	settled, waiting := headsSettled(heads, final, opts.ExpectedHeads, missing)
	complete := len(settledPerspectives(heads, final)) >= requiredPerspectiveCount(opts.MaxPerspectivesPerPR, opts.Perspectives.Len())
	if settled && complete && headsClean(allHeads) && plainClean(final) {
		agg := aggregatePlainReports(append(append([]PerspectiveReport(nil), final...), heads...), opts)
		if agg.Verdict == VerdictApprove {
			agg.DecisionSkipped = true
			attachHeadEvidence(&agg, allHeads, final, missing)
			return agg
		}
	}

	agg := aggregatePlainReports(append(append([]PerspectiveReport(nil), final...), heads...), opts)
	agg.Verdict = VerdictIncomplete
	agg.MergeEligible = false
	agg.FixCycle = false
	agg.RequiresHuman = false
	agg.CloseRecommended = false
	agg.DecisionRequested = false
	agg.DecisionQuestion = ""
	agg.HumanSummaries = nil
	switch {
	case !settled:
		agg.Reasons = []string{"review coverage incomplete: waiting on " + strings.Join(waiting, ", ")}
	case !complete:
		agg.Reasons = []string{incompleteCoverageReason(agg.Perspectives, opts)}
	default:
		agg.AwaitingDecision = true
		agg.Reasons = []string{"awaiting the decision step to reconcile the review heads"}
	}
	attachHeadEvidence(&agg, allHeads, final, missing)
	return agg
}

func hasDecided(reports []PerspectiveReport) bool {
	for _, r := range reports {
		if r.Decided {
			return true
		}
	}
	return false
}

// missingHeadsFor returns the settled-missing "<perspective>/<head>" pairs
// recorded for this group's PR head.
func missingHeadsFor(reports []PerspectiveReport, opts AggregateOptions) []string {
	if len(reports) == 0 || len(opts.MissingHeads) == 0 {
		return nil
	}
	r := reports[0]
	out := append([]string(nil), opts.MissingHeads[reviewKey(r.Repo, r.Number, r.HeadSHA, "")]...)
	sort.Strings(out)
	return out
}

func missingHeadPair(p Perspective, head string) string {
	return string(p) + "/" + strings.TrimSpace(head)
}

// settledPerspectives are the perspectives with at least one report.
func settledPerspectives(heads, final []PerspectiveReport) map[Perspective]bool {
	out := map[Perspective]bool{}
	for _, r := range heads {
		out[r.Perspective] = true
	}
	for _, r := range final {
		out[r.Perspective] = true
	}
	return out
}

// headsSettled reports whether every expected head has answered (or run out
// of retries on) every perspective a head reported, and lists the
// "<perspective>/<head>" pairs still owed. A perspective with a final report
// needs nothing from the heads.
func headsSettled(heads, final []PerspectiveReport, expected, missing []string) (bool, []string) {
	have := map[string]bool{}
	for _, m := range missing {
		have[m] = true
	}
	perspectives := map[Perspective]bool{}
	for _, r := range heads {
		perspectives[r.Perspective] = true
		have[missingHeadPair(r.Perspective, r.Head)] = true
	}
	for _, r := range final {
		delete(perspectives, r.Perspective)
	}
	if len(expected) == 0 {
		for _, r := range heads {
			expected = appendUnique(expected, strings.TrimSpace(r.Head))
		}
	}
	names := make([]string, 0, len(perspectives))
	for p := range perspectives {
		names = append(names, string(p))
	}
	sort.Strings(names)
	var waiting []string
	for _, p := range names {
		for _, h := range expected {
			if pair := missingHeadPair(Perspective(p), h); !have[pair] {
				waiting = append(waiting, pair)
			}
		}
	}
	return len(waiting) == 0, waiting
}

func appendUnique(items []string, v string) []string {
	for _, it := range items {
		if it == v {
			return items
		}
	}
	return append(items, v)
}

// headsClean is the skip rule's test: every head approved (or found its
// perspective not applicable) with no findings and no decision question.
func headsClean(heads []PerspectiveReport) bool {
	for _, r := range heads {
		if !reportClean(r) {
			return false
		}
	}
	return true
}

func plainClean(reports []PerspectiveReport) bool {
	for _, r := range reports {
		if !reportClean(r) {
			return false
		}
	}
	return true
}

func reportClean(r PerspectiveReport) bool {
	if r.NotApplicable {
		return true
	}
	return r.Verdict == VerdictApprove && len(r.Findings) == 0 && !r.DecisionRequested
}

// attachHeadEvidence records the heads' raw reports, which heads answered
// each perspective, the decision step's dispositions, and the heads that
// never answered.
func attachHeadEvidence(agg *Aggregate, heads, final []PerspectiveReport, missing []string) {
	agg.HeadReports = headReportsOf(heads)
	agg.Heads = nil
	for _, r := range heads {
		recordAggregateHead(agg, r)
	}
	agg.Dispositions = nil
	for _, r := range final {
		for _, d := range r.Dispositions {
			if d.Perspective == "" {
				d.Perspective = r.Perspective
			}
			agg.Dispositions = append(agg.Dispositions, d)
		}
	}
	agg.MissingHeads = append([]string(nil), missing...)
}

func headReportsOf(heads []PerspectiveReport) []HeadReport {
	out := make([]HeadReport, 0, len(heads))
	for _, r := range heads {
		out = append(out, HeadReport{
			Perspective:      r.Perspective,
			Head:             strings.TrimSpace(r.Head),
			ReviewModel:      strings.TrimSpace(r.ReviewModel),
			Verdict:          r.Verdict,
			NotApplicable:    r.NotApplicable,
			Summary:          strings.TrimSpace(r.Summary),
			Findings:         append([]outputschema.Finding(nil), r.Findings...),
			DecisionQuestion: strings.TrimSpace(r.DecisionQuestion),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Perspective != out[j].Perspective {
			return out[i].Perspective < out[j].Perspective
		}
		return out[i].Head < out[j].Head
	})
	return out
}
