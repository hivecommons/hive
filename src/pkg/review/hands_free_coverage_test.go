package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// The contributor hands-free rule (hivecommons/hive#15640) lets a bare
// non-security requires_human on a contributor PR resolve to approve. It is
// an approve like any other, so it needs the same coverage the all-approve
// path needs: a head the security perspective never reported on has had no
// security review, and must read incomplete, not merge-eligible. Since
// hivecommons/hive#16994 the relay drops a rate-limited "could not read"
// security report instead of recording it, so a missing security report is
// a real case, not only a dead reviewer kick.

func handsFreeReports(skip Perspective, ask func(*PerspectiveReport)) []PerspectiveReport {
	var out []PerspectiveReport
	for _, p := range DefaultPerspectives {
		if p == skip {
			continue
		}
		if p == PerspectiveCorrectness {
			r := baseReport(p, VerdictRequiresHuman)
			r.Summary = "Unclear whether the groundwork should land before its consumer."
			if ask != nil {
				ask(&r)
			}
			out = append(out, r)
			continue
		}
		out = append(out, baseReport(p, VerdictApprove))
	}
	return out
}

// withApprovingSiblings is r plus an approve from every other default
// perspective: a fully reviewed head.
func withApprovingSiblings(r PerspectiveReport) []PerspectiveReport {
	out := []PerspectiveReport{r}
	for _, p := range DefaultPerspectives {
		if p != r.Perspective {
			out = append(out, baseReport(p, VerdictApprove))
		}
	}
	return out
}

func askDecision(r *PerspectiveReport) {
	r.DecisionRequested = true
	r.DecisionQuestion = "@clubanderson should decide whether this groundwork lands before its consumer."
}

func TestContributorHandsFreeWithMissingPerspectiveIsIncomplete(t *testing.T) {
	cases := []struct {
		name string
		ask  func(*PerspectiveReport)
	}{
		{name: "bare requires_human"},
		{name: "decision question", ask: askDecision},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AggregateReports(handsFreeReports(PerspectiveSecurity, tc.ask), AggregateOptions{})
			if got.Verdict != VerdictIncomplete || got.MergeEligible || got.RequiresHuman || got.DecisionRequested || got.FixCycle {
				t.Fatalf("aggregate = %s (merge_eligible=%t requires_human=%t decision=%t fix=%t), want incomplete and not merge-eligible",
					got.Verdict, got.MergeEligible, got.RequiresHuman, got.DecisionRequested, got.FixCycle)
			}
			want := "review coverage incomplete: missing security"
			if len(got.Reasons) != 1 || got.Reasons[0] != want {
				t.Fatalf("Reasons = %q, want [%q] (the all-approve path's reason)", got.Reasons, want)
			}
			if _, ok := got.Perspectives[PerspectiveSecurity]; ok {
				t.Fatalf("perspectives = %v, the missing security perspective was counted", got.Perspectives)
			}
		})
	}
}

// The same report set with every perspective present still resolves hands-
// free to approve: the coverage check only adds the missing-perspective case.
func TestContributorHandsFreeWithFullCoverageStillApproves(t *testing.T) {
	cases := []struct {
		name string
		ask  func(*PerspectiveReport)
	}{
		{name: "bare requires_human"},
		{name: "decision question", ask: askDecision},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AggregateReports(handsFreeReports("", tc.ask), AggregateOptions{})
			if got.Verdict != VerdictApprove || !got.MergeEligible || got.RequiresHuman || got.DecisionRequested {
				t.Fatalf("aggregate = %+v, want hands-free advisory approve", got)
			}
			if got.Perspectives[PerspectiveCorrectness] != VerdictApprove {
				t.Fatalf("perspectives = %v, want the requires_human read as approve", got.Perspectives)
			}
			if len(got.Reasons) != 1 || !reasonsHave(got.Reasons, "no security finding") {
				t.Fatalf("Reasons = %v, want the hands-free advisory", got.Reasons)
			}
		})
	}
	// Under a per-PR cap the head is owed only that many perspectives.
	capped := AggregateReports(handsFreeReports(PerspectiveSecurity, nil), AggregateOptions{MaxPerspectivesPerPR: 3})
	if capped.Verdict != VerdictApprove || !capped.MergeEligible {
		t.Fatalf("capped aggregate = %+v, want approve: four of the three owed perspectives reported", capped)
	}
}

// The other hands-free outcomes do not wait on coverage: a fix the review
// asked for goes to the fixer now, and the fix-cycle cap still hands the
// head to a person.
func TestContributorHandsFreeFixOutcomesIgnoreCoverage(t *testing.T) {
	withFinding := func(r *PerspectiveReport) {
		r.Findings = append(r.Findings, outputschema.Finding{Title: "Get-before-Clear is redundant", Severity: outputschema.SeverityLow, ReviewScope: "in-scope"})
	}
	got := AggregateReports(handsFreeReports(PerspectiveSecurity, withFinding), AggregateOptions{})
	if got.Verdict != VerdictChangesRequested || !got.FixCycle || got.MergeEligible {
		t.Fatalf("aggregate = %+v, want the fixer lane", got)
	}
	got = AggregateReports(handsFreeReports(PerspectiveSecurity, withFinding), AggregateOptions{FixAttempts: 3, MaxFixAttempts: 3})
	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || got.MergeEligible {
		t.Fatalf("aggregate = %+v, want the fix-cycle cap to hold for a person", got)
	}
}

// The planner re-asks only the missing perspective, asks no person, and
// once the security report arrives the head approves: no loop.
func TestContributorHandsFreeIncompleteReasksOnlyMissingPerspective(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	agg := AggregateReports(handsFreeReports(PerspectiveSecurity, nil), AggregateOptions{})
	agg.RecordedAt = now
	pr := PullRequest{Repo: agg.Repo, Number: agg.Number, Title: "groundwork", Author: "contributor", HeadSHA: testSHA}
	for perspective := range agg.Perspectives {
		agg.ReportReceipts = append(agg.ReportReceipts, ReportReceipt{Perspective: perspective, Agent: "r1", ReceivedAt: now.Add(-time.Minute)})
	}
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

	done := AggregateReports(append(handsFreeReports(PerspectiveSecurity, nil), baseReport(PerspectiveSecurity, VerdictApprove)), AggregateOptions{})
	if done.Verdict != VerdictApprove || !done.MergeEligible {
		t.Fatalf("aggregate after the security report = %+v, want approve", done)
	}
}

// A verdict artifact written before this fix may hold a hands-free approve
// for a partly reviewed head. The fresh incomplete replaces it (approve is
// not a settled non-approve verdict that retainsOverIncomplete keeps).
func TestContributorHandsFreeIncompleteReplacesStoredPartialApprove(t *testing.T) {
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, r := range handsFreeReports(PerspectiveSecurity, nil) {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(reportDir, ReviewReportFilePrefix+string(r.Perspective)+ReviewReportFileSuffix), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stale := AggregateReports(handsFreeReports(PerspectiveSecurity, nil), AggregateOptions{})
	stale.Verdict, stale.MergeEligible, stale.Reasons = VerdictApprove, true, []string{"no security finding — the PR keeps moving"}
	stale.RecordedAt = now.Add(-time.Hour)
	if err := WriteArtifact(artifactPath, Artifact{GeneratedAt: stale.RecordedAt, Items: []Aggregate{stale}}); err != nil {
		t.Fatal(err)
	}
	merged, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Items) != 1 {
		t.Fatalf("items = %+v, want one", merged.Items)
	}
	if got := merged.Items[0]; got.Verdict != VerdictIncomplete || got.MergeEligible {
		t.Fatalf("merged = %s merge_eligible=%t, want the stored partial approve replaced by incomplete", got.Verdict, got.MergeEligible)
	}
}

func reasonsHave(reasons []string, sub string) bool {
	for _, r := range reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}
