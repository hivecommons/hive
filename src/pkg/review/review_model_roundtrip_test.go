package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewVerdictCarriesReviewModel(t *testing.T) {
	dir := t.TempDir()
	report := baseReport(PerspectiveCorrectness, VerdictApprove)
	report.AuthorModel = "gpt-5.6-terra"
	report.ReviewModel = "gemini-3.7-flash"
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ReviewReportFilePrefix+"model"+ReviewReportFileSuffix), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact, err := CollectAndWrite(dir, filepath.Join(dir, "review-verdicts.json"), AggregateOptions{})
	if err != nil {
		t.Fatalf("CollectAndWrite: %v", err)
	}
	if len(artifact.Items) != 1 || artifact.Items[0].ReviewModel != "gemini-3.7-flash" || artifact.Items[0].AuthorModel != "gpt-5.6-terra" {
		t.Fatalf("artifact item = %#v", artifact.Items)
	}
	loaded, err := LoadArtifact(filepath.Join(dir, "review-verdicts.json"))
	if err != nil {
		t.Fatalf("LoadArtifact: %v", err)
	}
	if loaded.Items[0].ReviewModel != "gemini-3.7-flash" {
		t.Fatalf("review_model did not round trip: %#v", loaded.Items[0])
	}
}
