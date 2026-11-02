package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeHandoffReport(t *testing.T, dir, name string, report PerspectiveReport) {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestScopedCollectionKeepsOtherVerdictsAndSkipsUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ReviewVerdictsFile)
	now := time.Now().UTC()
	report := baseReport(PerspectiveSecurity, VerdictApprove)
	report.Repo, report.Number, report.HeadSHA, report.BodyDigest = "acme/hive", 7, "head", "body"
	writeHandoffReport(t, dir, ReportFilePrefix(report.Repo, report.Number)+"security.json", report)
	if err := os.WriteFile(filepath.Join(dir, ReportFilePrefix("acme/other", 8)+"broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	prior := Aggregate{Repo: "acme/other", Number: 8, HeadSHA: "other-head", RecordedAt: now,
		Verdict: VerdictReject, Perspectives: map[Perspective]Verdict{PerspectiveSecurity: VerdictReject}}
	if err := WriteArtifact(path, Artifact{Items: []Aggregate{prior}}); err != nil {
		t.Fatal(err)
	}
	artifact, err := CollectAndMergeForPRs(dir, path, []PullRequest{{Repo: report.Repo, Number: report.Number}}, AggregateOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Items) != 2 {
		t.Fatalf("unrelated durable verdict was lost: %+v", artifact)
	}
	if got, ok := artifact.AggregateForBody(report.Repo, report.Number, report.HeadSHA, report.BodyDigest); !ok ||
		got.Perspectives[PerspectiveSecurity] != VerdictApprove || got.Verdict != VerdictIncomplete {
		t.Fatalf("valid target output was not collected without manufacturing approval: %+v", got)
	}
	if artifact.Collection.FilesRead != 1 || artifact.Collection.NewReports != 1 {
		t.Fatalf("unrelated files counted as current collection work: %+v", artifact.Collection)
	}
}

func TestCollectBeforeDispatchSchedulesOnlyMissingCurrentWorkOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ReviewVerdictsFile)
	now := time.Now().UTC()
	report := baseReport(PerspectiveSecurity, VerdictApprove)
	report.Repo, report.Number, report.HeadSHA, report.BodyDigest = "acme/hive", 7, "head", "body"
	writeHandoffReport(t, dir, ReportFilePrefix(report.Repo, report.Number)+"security.json", report)
	pr := dispatchPR(report.HeadSHA)
	pr.Repo, pr.Number, pr.BodyDigest = report.Repo, report.Number, report.BodyDigest
	pending := PendingReview{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, BodyDigest: pr.BodyDigest,
		Perspective: PerspectiveSecurity, Agent: "r1", Dispatched: now.Add(-time.Minute)}
	artifact, err := CollectAndMergeForPRs(dir, path, []PullRequest{pr}, AggregateOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	opts := DispatchOptions{RequireApproval: true, FanOut: true, Now: now, MaxParallelReviews: 1,
		Agents: []AgentCapability{reviewer("r1")}}
	plan := PlanDispatch([]PullRequest{pr}, artifact, DispatchState{Pending: []PendingReview{pending}}, opts)
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Perspective != PerspectiveCorrectness || plan.SettledReviews != 1 {
		t.Fatalf("completed current security was re-asked or blocked its missing peer: %+v", plan)
	}
	again := PlanDispatch([]PullRequest{pr}, artifact, plan.State, opts)
	if len(again.ReviewKicks) != 0 || again.SettledReviews != 0 {
		t.Fatalf("a repeated evaluation duplicated paid work or released its new owner: %+v", again)
	}
	refreshed, err := CollectAndMergeForPRs(dir, path, []PullRequest{pr}, AggregateOptions{}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Collection.FilesRead != 1 || refreshed.Collection.NewReports != 0 {
		t.Fatalf("unchanged receipt was reported as newly collected: %+v", refreshed.Collection)
	}
}

func TestRecollectingOldReportDoesNotReleaseNewDispatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ReviewVerdictsFile)
	now := time.Now().UTC()
	report := baseReport(PerspectiveSecurity, VerdictApprove)
	report.Repo, report.Number, report.HeadSHA, report.BodyDigest = "acme/hive", 7, "head", "body"
	name := ReportFilePrefix(report.Repo, report.Number) + "security.json"
	writeHandoffReport(t, dir, name, report)
	received := now.Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, name), received, received); err != nil {
		t.Fatal(err)
	}
	artifact, err := CollectAndMergeForPRs(dir, path, []PullRequest{{Repo: report.Repo, Number: report.Number}}, AggregateOptions{}, now)
	if err != nil {
		t.Fatal(err)
	}
	pending := PendingReview{Repo: report.Repo, Number: report.Number, HeadSHA: report.HeadSHA, BodyDigest: report.BodyDigest,
		Perspective: report.Perspective, Agent: "new-owner", Dispatched: now.Add(-time.Minute)}
	if got := removeCompletedReviews([]PendingReview{pending}, artifact); len(got) != 1 || got[0].Agent != pending.Agent {
		t.Fatalf("fresh collection timestamp laundered an old report into completion: %+v", got)
	}
	if artifact.Collection.MaxReportLag < time.Hour {
		t.Fatalf("report arrival-to-collection lag was hidden: %+v", artifact.Collection)
	}
}

func TestOlderCollectionCannotOverwriteNewerResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ReviewVerdictsFile)
	now := time.Now().UTC()
	newer := Aggregate{Repo: "acme/hive", Number: 7, HeadSHA: "head", BodyDigest: "body",
		Verdict: VerdictReject, RecordedAt: now,
		Perspectives: map[Perspective]Verdict{PerspectiveSecurity: VerdictReject}}
	if err := WriteArtifact(path, Artifact{GeneratedAt: now, Items: []Aggregate{newer}}); err != nil {
		t.Fatal(err)
	}

	older := newer
	older.Verdict = VerdictApprove
	older.Perspectives = map[Perspective]Verdict{}
	for _, perspective := range DefaultPerspectives {
		older.Perspectives[perspective] = VerdictApprove
	}
	artifact, err := mergeCollected(Artifact{Items: []Aggregate{older}}, dir, path, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if artifact.GeneratedAt.Before(now) || len(artifact.Items) != 1 || artifact.Items[0].Verdict != VerdictReject ||
		!artifact.Items[0].RecordedAt.Equal(now) {
		t.Fatalf("older collection rolled state back: %+v", artifact)
	}
}

func TestLateRestampCannotOverwriteNewerCollectedVerdict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ReviewVerdictsFile)
	now := time.Now().UTC()
	newer := Aggregate{Repo: "acme/hive", Number: 7, HeadSHA: "head", BodyDigest: "body",
		Verdict: VerdictReject, RecordedAt: now,
		Perspectives: map[Perspective]Verdict{PerspectiveSecurity: VerdictReject}}
	if err := WriteArtifact(path, Artifact{GeneratedAt: now, Items: []Aggregate{newer}}); err != nil {
		t.Fatal(err)
	}
	older := newer
	older.RecordedAt, older.Verdict = now.Add(-time.Minute), VerdictApprove
	older.ReportReceipts = []ReportReceipt{{Perspective: PerspectiveSecurity, ReceivedAt: now}}
	restamped := restampAggregate(older, PullRequest{HeadSHA: newer.HeadSHA, BodyDigest: newer.BodyDigest}, DeltaContext{}, older.RecordedAt)
	if len(restamped.ReportReceipts) != 0 {
		t.Fatal("an old head's report receipt was rebound to the new head")
	}
	if count, err := RecordDeltaRestamps(path, []DeltaRestamp{{Aggregate: restamped}}, now.Add(time.Minute)); err != nil || count != 0 {
		t.Fatalf("late stale completion overwrote the new verdict: count=%d, err=%v", count, err)
	}
	got, err := LoadArtifact(path)
	if err != nil || len(got.Items) != 1 || got.Items[0].Verdict != VerdictReject || !got.GeneratedAt.Equal(now) {
		t.Fatalf("late restamp changed newer durable evidence: %+v, %v", got, err)
	}
}

func TestEmptyScopedCollectionDoesNotExpandToHistoricalReports(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ReviewVerdictsFile)
	if err := os.WriteFile(filepath.Join(dir, ReviewReportFilePrefix+"broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, prs := range [][]PullRequest{nil, {}} {
		got, err := CollectAndMergeForPRs(dir, path, prs, AggregateOptions{}, time.Now().UTC())
		if err != nil || len(got.Items) != 0 {
			t.Fatalf("empty snapshot expanded collection scope: %+v, %v", got, err)
		}
	}
}

func TestReportFilePrefixMatchesExistingRelaySlugs(t *testing.T) {
	if got := ReportFilePrefix(" owner.name/repo-name ", 228); got != "review-report-owner-name-repo-name-228-" {
		t.Fatalf("legacy report filenames became invisible: %q", got)
	}
}
