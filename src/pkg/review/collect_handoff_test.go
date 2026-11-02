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
	older.Verdict = VerdictIncomplete
	older.Perspectives = map[Perspective]Verdict{PerspectiveCorrectness: VerdictApprove}
	artifact, err := mergeCollected(Artifact{Items: []Aggregate{older}}, dir, path, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if artifact.GeneratedAt.Before(now) || len(artifact.Items) != 1 || artifact.Items[0].Verdict != VerdictReject ||
		!artifact.Items[0].RecordedAt.Equal(now) {
		t.Fatalf("older collection rolled state back: %+v", artifact)
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
