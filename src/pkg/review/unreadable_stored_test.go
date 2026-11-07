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

// Report files written before the relay learned to skip rate-limited
// "could not read" reports (#16994) are still on disk on a live hive. Read as
// verdicts they keep the head parked: projectbluefin/testsuite#1116 at head
// ae22434 still read requires_human → 085 hours after the upgrade. The collect
// treats them as missing, and the merge withdraws the requires_human they
// alone produced, so the head is re-asked through the normal pending path.

const unreadableStoredRepo = "projectbluefin/testsuite"
const unreadableStoredNumber = 1116
const unreadableStoredHead = "ae22434cfe05ce9c119f2e508634d599201ab327"

func writeStoredReport(t *testing.T, dir string, r PerspectiveReport) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	name := ReportFilePrefix(r.Repo, r.Number) + string(r.Perspective) + ReviewReportFileSuffix
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func storedReport(p Perspective, v Verdict, summary string, findings ...outputschema.Finding) PerspectiveReport {
	r := baseReport(p, v, findings...)
	r.Repo, r.Number, r.HeadSHA = unreadableStoredRepo, unreadableStoredNumber, unreadableStoredHead
	r.Summary = summary
	return r
}

// seedPreUpgradeVerdict writes the reports and builds the artifact the way the
// hive did before #16994: every report is aggregated, so the stored verdict
// for the head is requires_human. The pre-upgrade artifact is reproduced by
// aggregating the reports directly, since the current collect skips them.
func seedPreUpgradeVerdict(t *testing.T, reportDir, artifactPath string, reports []PerspectiveReport, at time.Time) {
	t.Helper()
	for _, r := range reports {
		writeStoredReport(t, reportDir, r)
	}
	agg := AggregateReports(reports, AggregateOptions{})
	if agg.Verdict != VerdictRequiresHuman {
		t.Fatalf("seed: pre-upgrade aggregate = %s, want requires_human (%v)", agg.Verdict, agg.Reasons)
	}
	agg.RecordedAt, agg.FirstRecordedAt = at, at
	if err := WriteArtifact(artifactPath, Artifact{GeneratedAt: at, Items: []Aggregate{agg}}); err != nil {
		t.Fatal(err)
	}
}

func testsuite1116Reports() []PerspectiveReport {
	var reports []PerspectiveReport
	for _, p := range DefaultPerspectives {
		reports = append(reports, storedReport(p, VerdictRequiresHuman, testsuite1116Summaries[p]))
	}
	return reports
}

func storedAggregateFor(a Artifact) (Aggregate, bool) {
	return a.AggregateFor(unreadableStoredRepo, unreadableStoredNumber, unreadableStoredHead)
}

func unreadableStoredPR() PullRequest {
	return PullRequest{Repo: unreadableStoredRepo, Number: unreadableStoredNumber, HeadSHA: unreadableStoredHead}
}

func unreadableStoredDispatchOpts(now time.Time) DispatchOptions {
	return DispatchOptions{
		RequireApproval: true, FanOut: true, CombinedPerspectives: true, Now: now,
		Agents: []AgentCapability{reviewer("reviewer")},
	}
}

func TestStoredRateLimitedRequiresHumanIsWithdrawnAndReasked(t *testing.T) {
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)
	recorded := time.Date(2026, 11, 7, 10, 59, 0, 0, time.UTC)
	now := recorded.Add(8 * time.Hour)
	seedPreUpgradeVerdict(t, reportDir, artifactPath, testsuite1116Reports(), recorded)

	artifact, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Collection.UnreadableSkipped != len(DefaultPerspectives) {
		t.Errorf("UnreadableSkipped = %d, want %d", artifact.Collection.UnreadableSkipped, len(DefaultPerspectives))
	}
	if agg, ok := storedAggregateFor(artifact); ok {
		t.Fatalf("stored rate-limited requires_human still stands for the head: %s %v", agg.Verdict, agg.Reasons)
	}
	if hold, held := artifact.VerdictHoldFor(unreadableStoredRepo, unreadableStoredNumber); held {
		t.Fatalf("head still held by the stored verdict: %+v", hold)
	}
	onDisk, err := LoadArtifact(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := storedAggregateFor(onDisk); ok {
		t.Fatal("withdrawn verdict was written back to review-verdicts.json")
	}

	// The planner holds nothing for a person and asks every perspective again.
	plan := PlanDispatch([]PullRequest{unreadableStoredPR()}, artifact, DispatchState{}, unreadableStoredDispatchOpts(now))
	if len(plan.NewHuman) != 0 || len(plan.State.Human) != 0 {
		t.Fatalf("planner parked the head for a person: %+v", plan.State.Human)
	}
	if len(plan.ReviewKicks) != 1 || len(plan.ReviewKicks[0].Perspectives) != len(DefaultPerspectives) {
		t.Fatalf("want one combined re-review of every perspective, got %+v", plan.ReviewKicks)
	}

	// No re-dispatch loop: the next sweep, still inside pending_ttl, kicks
	// nothing; once the TTL passes the head is re-asked once and counted.
	again := PlanDispatch([]PullRequest{unreadableStoredPR()}, artifact, plan.State, unreadableStoredDispatchOpts(now.Add(time.Minute)))
	if len(again.ReviewKicks) != 0 {
		t.Fatalf("head re-kicked inside pending_ttl: %+v", again.ReviewKicks)
	}
	artifact, err = CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, now.Add(DefaultPendingReviewTTL+time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	expired := PlanDispatch([]PullRequest{unreadableStoredPR()}, artifact, again.State, unreadableStoredDispatchOpts(now.Add(DefaultPendingReviewTTL+time.Minute)))
	if len(expired.ReviewKicks) != 1 {
		t.Fatalf("want one re-ask after pending_ttl, got %+v", expired.ReviewKicks)
	}
	if got := expired.State.RedispatchCountFor(unreadableStoredRepo, unreadableStoredNumber, unreadableStoredHead); got != 1 {
		t.Fatalf("redispatch count = %d, want 1 (the max_redispatch budget applies)", got)
	}
}

func TestStoredPartlyUnreadableHeadReadsIncomplete(t *testing.T) {
	// Some perspectives read the PR and approved; the others hit the cap.
	// The head is under-asked, not a human decision.
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)
	recorded := time.Date(2026, 11, 7, 10, 59, 0, 0, time.UTC)
	now := recorded.Add(time.Hour)
	reports := []PerspectiveReport{
		storedReport(PerspectiveCorrectness, VerdictApprove, "Reads correctly."),
		storedReport(PerspectiveStyle, VerdictApprove, "Fine."),
		storedReport(PerspectiveSecurity, VerdictRequiresHuman, testsuite1116Summaries[PerspectiveSecurity]),
		storedReport(PerspectiveIntentAlignment, VerdictRequiresHuman, testsuite1116Summaries[PerspectiveIntentAlignment]),
		storedReport(PerspectiveDocsCurrency, VerdictRequiresHuman, testsuite1116Summaries[PerspectiveDocsCurrency]),
	}
	seedPreUpgradeVerdict(t, reportDir, artifactPath, reports, recorded)

	artifact, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	agg, ok := storedAggregateFor(artifact)
	if !ok || agg.Verdict != VerdictIncomplete {
		t.Fatalf("want the head incomplete, got ok=%v %s %v", ok, agg.Verdict, agg.Reasons)
	}
	plan := PlanDispatch([]PullRequest{unreadableStoredPR()}, artifact, DispatchState{}, unreadableStoredDispatchOpts(now))
	if len(plan.State.Human) != 0 {
		t.Fatalf("planner parked the head for a person: %+v", plan.State.Human)
	}
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("want one re-review kick, got %+v", plan.ReviewKicks)
	}
	got := map[Perspective]bool{}
	for _, p := range plan.ReviewKicks[0].Perspectives {
		got[p] = true
	}
	if len(got) != 3 || !got[PerspectiveSecurity] || !got[PerspectiveIntentAlignment] || !got[PerspectiveDocsCurrency] {
		t.Fatalf("want only the unread perspectives re-asked, got %v", plan.ReviewKicks[0].Perspectives)
	}
}

var leakFinding = outputschema.Finding{Title: "secret leaks into logs", Summary: "The token is logged at info level.",
	Severity: outputschema.SeverityHigh, File: "a.go", Line: 3}

func TestStoredRealRequiresHumanStillParks(t *testing.T) {
	unread := testsuite1116Summaries[PerspectiveSecurity]
	cases := []struct {
		name   string
		report PerspectiveReport
	}{
		{"security review: diff too large", storedReport(PerspectiveSecurity, VerdictRequiresHuman,
			"Could not read the diff: it is too large (12k lines). A person needs to review it.")},
		{"hive-authored PR: diff too large", func() PerspectiveReport {
			r := storedReport(PerspectiveCorrectness, VerdictRequiresHuman,
				"Could not read the diff: it is too large (12k lines). A person needs to review it.")
			r.HiveAuthored = true
			return r
		}()},
		{"security finding", storedReport(PerspectiveSecurity, VerdictRequiresHuman,
			"Token is written to the log; a person must decide.", leakFinding)},
		{"unread text but an in-scope finding", storedReport(PerspectiveSecurity, VerdictRequiresHuman, unread, leakFinding)},
		{"unread text but a decision question", func() PerspectiveReport {
			r := storedReport(PerspectiveSecurity, VerdictRequiresHuman, unread)
			r.DecisionRequested, r.DecisionQuestion = true, "Should the retry policy back off on HTTP 429?"
			return r
		}()},
		{"unread text but the high-risk marker", func() PerspectiveReport {
			r := storedReport(PerspectiveSecurity, VerdictRequiresHuman, unread)
			r.DecisionRiskHigh = true
			return r
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			reportDir := filepath.Join(root, "reports")
			artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)
			recorded := time.Date(2026, 11, 7, 10, 59, 0, 0, time.UTC)
			now := recorded.Add(8 * time.Hour)
			// The other perspectives hit the rate limit; the one under test
			// is a real hold and must keep the head parked.
			reports := []PerspectiveReport{tc.report}
			for _, p := range DefaultPerspectives {
				if p != tc.report.Perspective {
					r := storedReport(p, VerdictRequiresHuman, testsuite1116Summaries[p])
					r.HiveAuthored = tc.report.HiveAuthored
					reports = append(reports, r)
				}
			}
			seedPreUpgradeVerdict(t, reportDir, artifactPath, reports, recorded)

			artifact, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, now)
			if err != nil {
				t.Fatal(err)
			}
			agg, ok := storedAggregateFor(artifact)
			if !ok || agg.Verdict != VerdictRequiresHuman {
				t.Fatalf("real hold lost: ok=%v verdict=%s reasons=%v", ok, agg.Verdict, agg.Reasons)
			}
			plan := PlanDispatch([]PullRequest{unreadableStoredPR()}, artifact, DispatchState{}, unreadableStoredDispatchOpts(now))
			if len(plan.State.Human) == 0 {
				t.Fatal("real requires_human no longer parks the head for a person")
			}
			if len(plan.ReviewKicks) != 0 {
				t.Fatalf("settled head re-asked: %+v", plan.ReviewKicks)
			}
		})
	}
}

func TestStoredRequiresHumanFromLostReportIsKept(t *testing.T) {
	// A stored requires_human whose own report is gone from disk is not
	// withdrawn just because an unreadable report for another perspective
	// of the same head is still there: the unreadable one cannot account
	// for it.
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)
	recorded := time.Date(2026, 11, 7, 10, 59, 0, 0, time.UTC)
	real := storedReport(PerspectiveCorrectness, VerdictRequiresHuman, "Could not read the diff: it is too large (12k lines).")
	unread := storedReport(PerspectiveSecurity, VerdictRequiresHuman, testsuite1116Summaries[PerspectiveSecurity])
	seedPreUpgradeVerdict(t, reportDir, artifactPath, []PerspectiveReport{real, unread}, recorded)
	name := ReportFilePrefix(real.Repo, real.Number) + string(real.Perspective) + ReviewReportFileSuffix
	if err := os.Remove(filepath.Join(reportDir, name)); err != nil {
		t.Fatal(err)
	}

	artifact, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, recorded.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	agg, ok := storedAggregateFor(artifact)
	if !ok || agg.Verdict != VerdictRequiresHuman {
		t.Fatalf("stored hold from a lost report withdrawn: ok=%v %s", ok, agg.Verdict)
	}
	if !strings.Contains(strings.Join(agg.HumanSummaries, "\n"), "too large") {
		t.Fatalf("kept the wrong verdict: %v", agg.HumanSummaries)
	}
}

func TestStoredUnreadSecurityDoesNotLetContributorHoldResolveToApprove(t *testing.T) {
	// The contributor rule resolves a requires_human with no security
	// finding behind it to approve. With the security review the one set
	// aside as unread, that must not mark the head merge-eligible: the
	// security perspective is owed, so the head waits for the re-ask.
	// (With a stored requires_human from before the upgrade on the head, the
	// stored hold stands: the correctness report is a real one the unread
	// reports do not account for. This collect has no stored verdict.)
	root := t.TempDir()
	reportDir := filepath.Join(root, "reports")
	artifactPath := filepath.Join(root, "data", ReviewVerdictsFile)
	recorded := time.Date(2026, 11, 7, 10, 59, 0, 0, time.UTC)
	now := recorded.Add(time.Hour)
	reports := []PerspectiveReport{
		storedReport(PerspectiveCorrectness, VerdictRequiresHuman, "Could not read the diff: it is too large (12k lines)."),
		storedReport(PerspectiveStyle, VerdictApprove, "Fine."),
		storedReport(PerspectiveIntentAlignment, VerdictApprove, "Matches the issue."),
		storedReport(PerspectiveDocsCurrency, VerdictApprove, "Docs current."),
		storedReport(PerspectiveSecurity, VerdictRequiresHuman, testsuite1116Summaries[PerspectiveSecurity]),
	}
	for _, r := range reports {
		writeStoredReport(t, reportDir, r)
	}

	artifact, err := CollectAndMerge(reportDir, artifactPath, AggregateOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.HasAggregateApproval(unreadableStoredRepo, unreadableStoredNumber, unreadableStoredHead) {
		t.Fatal("head with an unread security review marked merge-eligible")
	}
	agg, ok := storedAggregateFor(artifact)
	if !ok || agg.Verdict != VerdictIncomplete {
		t.Fatalf("want incomplete while security is owed, got ok=%v %s %v", ok, agg.Verdict, agg.Reasons)
	}
	plan := PlanDispatch([]PullRequest{unreadableStoredPR()}, artifact, DispatchState{}, unreadableStoredDispatchOpts(now))
	if len(plan.ReviewKicks) != 1 || len(plan.ReviewKicks[0].Perspectives) != 1 || plan.ReviewKicks[0].Perspectives[0] != PerspectiveSecurity {
		t.Fatalf("want the security perspective re-asked, got %+v", plan.ReviewKicks)
	}
}
