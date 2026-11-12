package automerge

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// heldReviewSummaryAsk is a blocking reviewer's summary as the lane verdict
// carries it (review.Aggregate.BlockSummaryAsks).
const heldReviewSummaryAsk = "correctness: the early return is still present, ask @maintainer before changing it"

func heldReviewObservation(label, reason string) stageObservation {
	return stageObservation{
		Lane: stageLaneContributor, Display: "acme/widget", Owner: "acme", Repo: "widget", Number: 9,
		HeadSHA: "sha", Labels: []string{label}, Reason: reason, LaneReason: reason, AuthorKind: authorkind.Clanker,
	}
}

// A blocking verdict with no in-scope finding (the reviewer said "still
// present" in its summary only) never renders as the lead line alone: the
// asks list what the blocking reviewers said, mention-neutralised. Findings
// come first and stand alone when there are any. A verdict that does not
// block lists nothing past the lead line.
func TestSwarmChangesAsksListsTheBlockingSummariesWithoutFindings(t *testing.T) {
	summaryOnly := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested", SummaryAsks: []string{heldReviewSummaryAsk, "  "}}}
	want := []string{swarmChangesAskLead, "review summary: correctness: the early return is still present, ask `maintainer` before changing it"}
	if got := swarmChangesAsks(summaryOnly); !slices.Equal(got, want) {
		t.Fatalf("summary-only asks = %q, want %q", got, want)
	}
	for _, reason := range []string{contributorLaneReasonChangesRequested, stagelabels.LaneReasonChangesRequested} {
		if _, asks, _ := nudgeDiagnosis(reason, summaryOnly); !slices.Equal(asks, want) {
			t.Fatalf("%s: asks = %q, want %q", reason, asks, want)
		}
	}

	both := summaryOnly
	both.Verdict.FindingAsks = []string{heldReviewFindingAsk}
	if got := swarmChangesAsks(both); !slices.Equal(got, []string{swarmChangesAskLead, "review finding: " + heldReviewFindingAsk}) {
		t.Fatalf("asks with a finding = %q, want the finding and no summary line", got)
	}

	for name, v := range map[string]ContributorLaneVerdict{
		"approve":        {Configured: true, Approved: true, Verdict: "approve", FindingAsks: []string{heldReviewFindingAsk}, SummaryAsks: []string{heldReviewSummaryAsk}},
		"requires_human": {Configured: true, Verdict: "requires_human", DecisionRequested: true, DecisionQuestion: "Ship behind a flag?", FindingAsks: []string{heldReviewFindingAsk}, SummaryAsks: []string{heldReviewSummaryAsk}},
		"none":           {Configured: true, SummaryAsks: []string{heldReviewSummaryAsk}},
	} {
		d := stageDetail{VerdictKnown: true, Verdict: v}
		if got := swarmChangesAsks(d); !slices.Equal(got, []string{swarmChangesAskLead}) {
			t.Fatalf("%s verdict: asks = %q, want the lead line alone", name, got)
		}
		// Another lane reason beside a verdict that does not block says
		// nothing about the review at all.
		if _, asks, _ := nudgeDiagnosis("label:needs-rebase", d); len(asks) != 1 {
			t.Fatalf("%s verdict beside a rebase: asks = %q, want the rebase ask alone", name, asks)
		}
	}
}

// The human route carries what the review asked for only when it sends the
// PR back on a blocking verdict. The lane's detail answers first; the
// route fills in only when the detail has nothing to list.
func TestRoutedVerdictAsksComeFromTheRouteWhenTheDetailHasNoVerdict(t *testing.T) {
	routed := &stagelabels.Route{
		Stage:      stagelabels.ReturnedToContributor,
		Reasons:    []string{"verdict changes_requested @abc1234"},
		LaneReason: stagelabels.LaneReasonChangesRequested,
		ReviewAsks: []string{"review finding: " + heldReviewFindingAsk, " "},
	}
	want := []string{swarmChangesAskLead, "review finding: " + heldReviewFindingAsk}
	// The held PR: the lane stopped at the gate, the detail has no verdict.
	if _, asks, _ := nudgeDiagnosisRouted(stagelabels.LaneReasonChangesRequested, stageDetail{}, routed); !slices.Equal(asks, want) {
		t.Fatalf("held asks = %q, want %q", asks, want)
	}
	// The route's own reason is another one (a red check first): the
	// verdict's asks still follow the lead line once.
	_, asks, _ := nudgeDiagnosisRouted("check-failing:unit", stageDetail{}, routed)
	if !slices.Equal(asks, want) {
		t.Fatalf("asks beside a red check = %q, want %q", asks, want)
	}

	// The lane read the verdict itself: its detail is what the nudge
	// lists, exactly as before, whatever the route carries.
	lane := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested", FindingAsks: []string{"`src/other.go:7` the lane's own finding"}}}
	laneWant := []string{swarmChangesAskLead, "review finding: `src/other.go:7` the lane's own finding"}
	if _, asks, _ := nudgeDiagnosisRouted(contributorLaneReasonChangesRequested, lane, routed); !slices.Equal(asks, laneWant) {
		t.Fatalf("unheld asks = %q, want %q", asks, laneWant)
	}

	// A blocking detail with nothing to list is filled in from the route.
	bare := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}}
	if _, asks, _ := nudgeDiagnosisRouted(contributorLaneReasonChangesRequested, bare, routed); !slices.Equal(asks, want) {
		t.Fatalf("bare blocking detail asks = %q, want %q", asks, want)
	}

	// A route that was not decided on a changes_requested verdict lists
	// no review asks, even if it carried some.
	conflict := &stagelabels.Route{Stage: stagelabels.ReturnedToContributor, Reasons: []string{"merge conflict with the base branch"}, LaneReason: stagelabels.LaneReasonConflicting, ReviewAsks: routed.ReviewAsks}
	_, asks, _ = nudgeDiagnosisRouted(stagelabels.LaneReasonConflicting, stageDetail{}, conflict)
	for _, a := range asks {
		if strings.HasPrefix(a, "review ") || a == swarmChangesAskLead {
			t.Fatalf("a conflict route listed a review ask: %q", asks)
		}
	}
	// No route at all: the lead line, as before.
	if _, asks, _ := nudgeDiagnosisRouted(stagelabels.LaneReasonChangesRequested, stageDetail{}, nil); !slices.Equal(asks, want[:1]) {
		t.Fatalf("asks without a route = %q, want the lead line", asks)
	}
}

// More findings than the nudge lists: the limit the lane verdict was built
// with and its "and N more" line reach the nudge unchanged through the
// route.
func TestRoutedVerdictAsksKeepTheFindingLimit(t *testing.T) {
	const limit = 5
	var findings []string
	for i := 1; i <= limit; i++ {
		findings = append(findings, fmt.Sprintf("`src/widget.go:%d` finding %d", i, i))
	}
	findings = append(findings, "and 2 more finding(s) in the review")
	verdict := ContributorLaneVerdict{Configured: true, Verdict: "changes_requested", FindingAsks: findings, SummaryAsks: []string{heldReviewSummaryAsk}}

	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
	e, tracker, _ := heldReviewEngine(t, api.url, clock, verdict, nil)
	obs := heldReviewObservation("sentinel-alert", contributorLaneReasonSentinelBlocked)
	route, _ := e.routeHuman(context.Background(), tracker, stageKey("acme", "widget", 9), obs, stagelabels.ReadyForHumanMerge, clock.now())
	if route.Stage != stagelabels.ReturnedToContributor || route.LaneReason != stagelabels.LaneReasonChangesRequested {
		t.Fatalf("route = %+v, want 072 for the review's requested changes", route)
	}
	_, asks, _ := nudgeDiagnosisRouted(route.LaneReason, obs.Detail, &route)
	if len(asks) != limit+2 {
		t.Fatalf("asks = %q, want the lead line, %d findings and the counting line", asks, limit)
	}
	if asks[0] != swarmChangesAskLead || asks[1] != "review finding: `src/widget.go:1` finding 1" || asks[limit+1] != "review finding: and 2 more finding(s) in the review" {
		t.Fatalf("asks = %q", asks)
	}
	for _, a := range asks {
		if strings.HasPrefix(a, "review summary:") {
			t.Fatalf("a summary line was listed beside findings: %q", asks)
		}
	}
}

// The route as the stage pass builds it for a held contributor PR, per
// verdict: a blocking verdict rides its asks on the evidence and the fixing
// route (findings, else the blocking reviewers' summaries); a verdict that
// does not block carries none, and the route's stage and reasons are what
// they were.
func TestHeldPRRouteCarriesTheAsksOfABlockingVerdictOnly(t *testing.T) {
	cases := []struct {
		name    string
		label   string
		reason  string
		verdict ContributorLaneVerdict
		stage   stagelabels.Stage
		asks    []string
	}{
		{"sentinel, one finding", "sentinel-alert", contributorLaneReasonSentinelBlocked,
			ContributorLaneVerdict{Configured: true, Verdict: "changes_requested", FindingAsks: []string{heldReviewFindingAsk}},
			stagelabels.ReturnedToContributor, []string{"review finding: " + heldReviewFindingAsk}},
		{"hold label, one finding", "hold", "label:hold",
			ContributorLaneVerdict{Configured: true, Verdict: "changes_requested", FindingAsks: []string{heldReviewFindingAsk}},
			stagelabels.ReturnedToContributor, []string{"review finding: " + heldReviewFindingAsk}},
		{"sentinel, blocked in the summary only", "sentinel-alert", contributorLaneReasonSentinelBlocked,
			ContributorLaneVerdict{Configured: true, Verdict: "changes_requested", SummaryAsks: []string{heldReviewSummaryAsk}},
			stagelabels.ReturnedToContributor, []string{"review summary: correctness: the early return is still present, ask `maintainer` before changing it"}},
		{"hold label, blocked in the summary only", "hold", "label:hold",
			ContributorLaneVerdict{Configured: true, Verdict: "changes_requested", SummaryAsks: []string{heldReviewSummaryAsk}},
			stagelabels.ReturnedToContributor, []string{"review summary: correctness: the early return is still present, ask `maintainer` before changing it"}},
		{"sentinel, blocking with nothing to list", "sentinel-alert", contributorLaneReasonSentinelBlocked,
			ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"},
			stagelabels.ReturnedToContributor, nil},
		{"sentinel, approved", "sentinel-alert", contributorLaneReasonSentinelBlocked,
			ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve", FindingAsks: []string{heldReviewFindingAsk}, SummaryAsks: []string{heldReviewSummaryAsk}},
			stagelabels.ReadyForHumanMerge, nil},
		{"hold label, no verdict yet", "hold", "label:hold",
			ContributorLaneVerdict{Configured: true, SummaryAsks: []string{heldReviewSummaryAsk}},
			stagelabels.ReviewPending, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, closeAPI := seededStageAPI(t)
			defer closeAPI()
			clock := &contributorLaneClock{at: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)}
			e, tracker, _ := heldReviewEngine(t, api.url, clock, tc.verdict, nil)
			obs := heldReviewObservation(tc.label, tc.reason)
			route, ev := e.routeHuman(context.Background(), tracker, stageKey("acme", "widget", 9), obs, stagelabels.ReadyForHumanMerge, clock.now())
			if route.Stage != tc.stage {
				t.Fatalf("stage = %v, want %v (route %+v)", route.Stage, tc.stage, route)
			}
			if !slices.Equal(ev.ReviewAsks, tc.asks) || !slices.Equal(route.ReviewAsks, tc.asks) {
				t.Fatalf("review asks: evidence %q, route %q, want %q", ev.ReviewAsks, route.ReviewAsks, tc.asks)
			}
			blocking := tc.stage == stagelabels.ReturnedToContributor
			if got := slices.Contains(route.Reasons, "verdict changes_requested @sha"); got != blocking {
				t.Fatalf("route reasons = %q, verdict reason present = %v, want %v", route.Reasons, got, blocking)
			}
			for _, r := range route.Reasons {
				if strings.HasPrefix(r, "review finding:") || strings.HasPrefix(r, "review summary:") {
					t.Fatalf("a review ask became a route reason: %q", route.Reasons)
				}
			}
			// The observation is the lane's: its detail still has no
			// verdict, so nothing that classifies on it reads one.
			if obs.Detail.VerdictKnown || verdictBlocks(obs.Detail) {
				t.Fatalf("the route wrote a verdict onto the lane's detail: %+v", obs.Detail)
			}
			if !blocking {
				return
			}
			_, asks, _ := nudgeDiagnosisRouted(route.LaneReason, obs.Detail, &route)
			if want := append([]string{swarmChangesAskLead}, tc.asks...); !slices.Equal(asks, want) {
				t.Fatalf("nudge asks = %q, want %q", asks, want)
			}
		})
	}
}
