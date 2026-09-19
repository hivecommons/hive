package github

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
)

// A combined review hands the relay one array. The collector must then see
// exactly what it would have seen from five separate reviews: one file per
// perspective, each a standalone report, aggregating to one verdict. Nothing
// downstream learns that the perspectives arrived together.
func TestRecordReviewVerdictFansOutAnArray(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	raw := "[" + strings.Join([]string{
		validVerdictJSON(t, "o/r", 7, "correctness", "approve"),
		validVerdictJSON(t, "o/r", 7, "security", "changes_requested"),
		validVerdictJSON(t, "o/r", 7, "docs-currency", "approve"),
	}, ",") + "]"

	c.recordReviewVerdict(ReviewRequest{Repo: "o/r", Number: 7, Report: raw}, dir)

	matches, _ := filepath.Glob(filepath.Join(dir, review.ReviewReportFilePrefix+"*"+review.ReviewReportFileSuffix))
	if len(matches) != 3 {
		t.Fatalf("want one file per perspective, got %v", matches)
	}
	for _, m := range matches {
		b, _ := os.ReadFile(m)
		if !strings.HasPrefix(strings.TrimSpace(string(b)), "{") {
			t.Errorf("%s is not a standalone object: %s", filepath.Base(m), b)
		}
	}
	artifact, err := review.Collect(dir, review.AggregateOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(artifact.Items) != 1 || artifact.Items[0].Verdict != review.VerdictChangesRequested {
		t.Fatalf("aggregate = %+v, want one changes_requested item", artifact.Items)
	}
	if len(artifact.Items[0].Perspectives) != 3 {
		t.Fatalf("perspectives = %v", artifact.Items[0].Perspectives)
	}
}

// One bad element poisons the whole array — nothing is written. Half a
// combined review on disk would aggregate as if the missing perspectives had
// never been dispatched, and the PR would be re-reviewed for them from scratch.
func TestRecordReviewVerdictRejectsWholeArrayOnOneBadElement(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	raw := "[" + validVerdictJSON(t, "o/r", 7, "correctness", "approve") + "," +
		validVerdictJSON(t, "o/r", 8, "security", "approve") + "]" // different PR
	c.recordReviewVerdict(ReviewRequest{Repo: "o/r", Number: 7, Report: raw}, dir)
	if matches, _ := filepath.Glob(filepath.Join(dir, "*")); len(matches) != 0 {
		t.Fatalf("partial array was written: %v", matches)
	}
}

// The relay judges perspective names against the hive's own set: a perspective
// the hive defined is recorded, one it does not review with is refused.
func TestRecordReviewVerdictUsesTheHivesPerspectiveSet(t *testing.T) {
	set, err := review.NewPerspectiveSet([]string{"correctness", "api-compat"}, map[string]string{"api-compat": "API breakage"})
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{logger: testLogger()}
	c.SetPerspectives(set)

	dir := t.TempDir()
	// Built with the default set for the fixture check, but "api-compat" is not
	// a default — construct by hand.
	custom := strings.Replace(validVerdictJSON(t, "o/r", 7, "correctness", "approve"), `"correctness"`, `"api-compat"`, 1)
	c.recordReviewVerdict(ReviewRequest{Repo: "o/r", Number: 7, Report: custom}, dir)
	if matches, _ := filepath.Glob(filepath.Join(dir, "*api-compat*")); len(matches) != 1 {
		t.Fatalf("hive-defined perspective not recorded: %v", matches)
	}

	dir2 := t.TempDir()
	c.recordReviewVerdict(ReviewRequest{Repo: "o/r", Number: 7, Report: validVerdictJSON(t, "o/r", 7, "security", "approve")}, dir2)
	if matches, _ := filepath.Glob(filepath.Join(dir2, "*")); len(matches) != 0 {
		t.Fatalf("perspective outside the hive's set was recorded: %v", matches)
	}
}
