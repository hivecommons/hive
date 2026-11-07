package review

import (
	"strings"
	"testing"
	"time"
)

// Two aggregate paths approved heads they should not have:
//
//   - On a contributor PR the hands-free rule (hivecommons/hive#15640) made a
//     non-security decision question advisory, and with it a sibling
//     perspective's reject: reject + question resolved to approve and
//     merge-eligible.
//   - The hive-authored advisory approves (a non-risk decision question,
//     hivecommons/hive#12834, or a bare non-risk requires_human,
//     hivecommons/hive#12888) never checked coverage. A head the security
//     perspective never reported on (a dead kick, or since
//     hivecommons/hive#16994 a rate-limited review the relay does not record)
//     was merge-eligible with no security review.

const p33Question = "@clubanderson should decide whether this groundwork lands before its consumer."

// alongsideApprovals is reports plus an approve, with a neutral summary, from
// every default perspective none of them covers: a fully reviewed head.
// skip leaves those perspectives out, as a review that never arrived.
func alongsideApprovals(reports []PerspectiveReport, skip ...Perspective) []PerspectiveReport {
	have := map[Perspective]bool{}
	for _, p := range skip {
		have[p] = true
	}
	hive := false
	for _, r := range reports {
		have[r.Perspective] = true
		hive = hive || r.HiveAuthored
	}
	out := append([]PerspectiveReport(nil), reports...)
	for _, p := range DefaultPerspectives {
		if have[p] {
			continue
		}
		r := baseReport(p, VerdictApprove)
		r.Summary = "Read the diff; fine."
		r.HiveAuthored = hive
		out = append(out, r)
	}
	return out
}

func p33DecisionQuestion(p Perspective, hive bool) PerspectiveReport {
	r := baseReport(p, VerdictRequiresHuman)
	r.Summary = "Read the diff; the sequencing is a product call."
	r.DecisionRequested = true
	r.DecisionQuestion = p33Question
	r.HiveAuthored = hive
	return r
}

func p33BareRequiresHuman(p Perspective, hive bool) PerspectiveReport {
	r := baseReport(p, VerdictRequiresHuman)
	r.Summary = "Unclear whether the groundwork should land before its consumer."
	r.HiveAuthored = hive
	return r
}

func TestContributorRejectWithDecisionQuestionStaysReject(t *testing.T) {
	reject := baseReport(PerspectiveIntentAlignment, VerdictReject)
	reject.Summary = "This duplicates an open PR; it should not land."
	changes := baseReport(PerspectiveStyle, VerdictChangesRequested)
	changes.Summary = "Rename the helper."
	cases := []struct {
		name string
		reps []PerspectiveReport
	}{
		{name: "reject + decision question", reps: []PerspectiveReport{p33DecisionQuestion(PerspectiveCorrectness, false), reject}},
		// Without the reject check the fixer lane took it: the reject was
		// dropped just the same.
		{name: "reject + changes_requested + decision question", reps: []PerspectiveReport{p33DecisionQuestion(PerspectiveCorrectness, false), reject, changes}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reps := alongsideApprovals(tc.reps)
			if len(reps) != len(DefaultPerspectives) {
				t.Fatalf("fixture has %d perspectives, want all %d", len(reps), len(DefaultPerspectives))
			}
			got := AggregateReports(reps, AggregateOptions{})
			if got.Verdict != VerdictReject || got.MergeEligible || got.FixCycle || !got.CloseRecommended {
				t.Fatalf("aggregate = %s (merge_eligible=%t fix=%t close=%t), want reject: a reject is never resolved to approve",
					got.Verdict, got.MergeEligible, got.FixCycle, got.CloseRecommended)
			}
			// Same outcome as the reject without the question.
			var noQuestion []PerspectiveReport
			for _, r := range reps {
				if r.DecisionRequested {
					r = baseReport(r.Perspective, VerdictApprove)
					r.Summary = "Read the diff; fine."
				}
				noQuestion = append(noQuestion, r)
			}
			if want := AggregateReports(noQuestion, AggregateOptions{}); want.Verdict != got.Verdict || want.CloseRecommended != got.CloseRecommended {
				t.Fatalf("with the question: %s close=%t; without it: %s close=%t, want the same", got.Verdict, got.CloseRecommended, want.Verdict, want.CloseRecommended)
			}
			if got.DecisionRequested || got.RequiresHuman {
				t.Fatalf("aggregate = %+v, the contributor question must stay advisory", got)
			}
			if !reasonsInclude(got.Reasons, p33Question) {
				t.Fatalf("Reasons = %q, want the decision question recorded", got.Reasons)
			}
			if BlocksSelfMerge(got) != true {
				t.Fatalf("BlocksSelfMerge = false for %+v, want the reject to hold the PR", got)
			}
		})
	}
}

// The bare requires_human and changes_requested paths already keep a reject
// and a change request on a contributor PR (the reject check and the fix
// cycle run before the hands-free branch); these pin it.
func TestContributorBareRequiresHumanKeepsRejectAndChanges(t *testing.T) {
	reject := baseReport(PerspectiveIntentAlignment, VerdictReject)
	reject.Summary = "This duplicates an open PR; it should not land."
	changes := baseReport(PerspectiveStyle, VerdictChangesRequested)
	changes.Summary = "Rename the helper."
	got := AggregateReports(alongsideApprovals([]PerspectiveReport{p33BareRequiresHuman(PerspectiveCorrectness, false), reject}), AggregateOptions{})
	if got.Verdict != VerdictReject || got.MergeEligible {
		t.Fatalf("reject + bare requires_human = %+v, want reject", got)
	}
	got = AggregateReports(alongsideApprovals([]PerspectiveReport{p33BareRequiresHuman(PerspectiveCorrectness, false), changes}), AggregateOptions{})
	if got.Verdict != VerdictChangesRequested || !got.FixCycle || got.MergeEligible {
		t.Fatalf("changes_requested + bare requires_human = %+v, want the fixer lane", got)
	}
}

// Control: the decision question alone, the other four perspectives
// approving, resolves hands-free to approve as before.
func TestContributorDecisionQuestionAloneStillApprovesHandsFree(t *testing.T) {
	got := AggregateReports(alongsideApprovals([]PerspectiveReport{p33DecisionQuestion(PerspectiveCorrectness, false)}), AggregateOptions{})
	if got.Verdict != VerdictApprove || !got.MergeEligible || got.RequiresHuman || got.DecisionRequested {
		t.Fatalf("aggregate = %+v, want the hands-free advisory approve", got)
	}
	if len(got.Reasons) != 1 || !strings.HasPrefix(got.Reasons[0], "no security finding") || !strings.Contains(got.Reasons[0], p33Question) {
		t.Fatalf("Reasons = %q, want the contributor advisory with the question", got.Reasons)
	}
}

func TestSelfAuthoredAdvisoryApproveNeedsFullCoverage(t *testing.T) {
	cases := []struct {
		name       string
		rep        PerspectiveReport
		wantReason string
	}{
		{name: "bare non-risk requires_human", rep: p33BareRequiresHuman(PerspectiveCorrectness, true), wantReason: "self-authored non-risk requires_human recorded as advisory"},
		{name: "non-risk decision question", rep: p33DecisionQuestion(PerspectiveCorrectness, true), wantReason: "decision resolved by policy:"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/security missing", func(t *testing.T) {
			got := AggregateReports(alongsideApprovals([]PerspectiveReport{tc.rep}, PerspectiveSecurity), AggregateOptions{})
			if got.Verdict != VerdictIncomplete || got.MergeEligible || got.RequiresHuman || got.DecisionRequested || got.FixCycle {
				t.Fatalf("aggregate = %s (merge_eligible=%t requires_human=%t decision=%t fix=%t), want incomplete: the head had no security review",
					got.Verdict, got.MergeEligible, got.RequiresHuman, got.DecisionRequested, got.FixCycle)
			}
			if want := "review coverage incomplete: missing security"; len(got.Reasons) != 1 || got.Reasons[0] != want {
				t.Fatalf("Reasons = %q, want [%q]", got.Reasons, want)
			}
			if len(got.HumanSummaries) != 0 {
				t.Fatalf("HumanSummaries = %q, an incomplete head asks nobody", got.HumanSummaries)
			}
			if BlocksSelfMerge(got) {
				t.Fatalf("incomplete head blocks self-merge as a person's hold: %+v", got)
			}
		})
		// Control: the same set with every perspective present stays the
		// advisory approve the self-authored rule is for.
		t.Run(tc.name+"/full coverage", func(t *testing.T) {
			got := AggregateReports(alongsideApprovals([]PerspectiveReport{tc.rep}), AggregateOptions{})
			if got.Verdict != VerdictApprove || !got.MergeEligible || got.RequiresHuman || got.DecisionRequested {
				t.Fatalf("aggregate = %+v, want advisory approve", got)
			}
			if !reasonsInclude(got.Reasons, tc.wantReason) {
				t.Fatalf("Reasons = %q, want %q", got.Reasons, tc.wantReason)
			}
		})
	}
	// Under a per-PR cap the head is owed only that many perspectives.
	capped := AggregateReports(alongsideApprovals([]PerspectiveReport{p33BareRequiresHuman(PerspectiveCorrectness, true)}, PerspectiveSecurity), AggregateOptions{MaxPerspectivesPerPR: 3})
	if capped.Verdict != VerdictApprove || !capped.MergeEligible {
		t.Fatalf("capped aggregate = %+v, want approve: four reported, three owed", capped)
	}
}

// The planner re-asks only the missing perspective and asks no person.
func TestSelfAuthoredIncompleteReasksOnlyMissingPerspective(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	agg := AggregateReports(alongsideApprovals([]PerspectiveReport{p33BareRequiresHuman(PerspectiveCorrectness, true)}, PerspectiveSecurity), AggregateOptions{})
	agg.RecordedAt = now
	for perspective := range agg.Perspectives {
		agg.ReportReceipts = append(agg.ReportReceipts, ReportReceipt{Perspective: perspective, Agent: "r1", ReceivedAt: now.Add(-time.Minute)})
	}
	pr := PullRequest{Repo: agg.Repo, Number: agg.Number, Title: "groundwork", Author: "hive-bot", HeadSHA: testSHA}
	opts := DispatchOptions{RequireApproval: true, FanOut: true, ProjectOrg: "hivecommons", Now: now,
		MaxParallelReviews: 5, Agents: []AgentCapability{reviewer("r1"), reviewer("r2")}}
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, DispatchState{}, opts)
	if len(plan.NewHuman) != 0 || len(plan.State.Human) != 0 {
		t.Fatalf("an incomplete head asked a person: %+v", plan.NewHuman)
	}
	var asked []Perspective
	for _, k := range plan.ReviewKicks {
		asked = append(asked, k.Perspective)
	}
	if len(asked) != 1 || asked[0] != PerspectiveSecurity {
		t.Fatalf("re-asked %v, want only the missing security perspective", asked)
	}
}

func reasonsInclude(reasons []string, sub string) bool {
	for _, r := range reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}
