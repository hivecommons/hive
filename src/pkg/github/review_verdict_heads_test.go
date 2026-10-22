package github

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
)

// Two review heads judging the same perspective must each keep their verdict
// (hivecommons/hive#13542). Before heads, the report file was keyed by
// perspective alone, so the second head's verdict overwrote the first. The
// head comes from the dispatch record, not the reviewer's own JSON.
func TestRecordReviewVerdictKeepsOneFilePerReviewHead(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	sonnet := pendingVerdict("o/r", 7, "correctness", "cr-sonnet-1")
	sonnet.Head = "cr-sonnet"
	sol := pendingVerdict("o/r", 7, "correctness", "cr-sol-1")
	sol.Head = "cr-sol"
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{sonnet, sol}})

	var spoofed map[string]any
	if err := json.Unmarshal([]byte(validVerdictJSON(t, "o/r", 7, "correctness", "changes_requested")), &spoofed); err != nil {
		t.Fatal(err)
	}
	spoofed["head"] = "cr-sonnet" // a head claiming to be another head is ignored
	raw, _ := json.Marshal(spoofed)

	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 7, Agent: "cr-sonnet-1", Report: validVerdictJSON(t, "o/r", 7, "correctness", "approve")}, dir)
	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 7, Agent: "cr-sol-1", Report: string(raw)}, dir)

	matches, _ := filepath.Glob(filepath.Join(dir, review.ReviewReportFilePrefix+"*"+review.ReviewReportFileSuffix))
	if len(matches) != 2 {
		t.Fatalf("want one file per head, got %v", matches)
	}
	heads := map[string]string{}
	for _, m := range matches {
		b, _ := os.ReadFile(m)
		var r review.PerspectiveReport
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		heads[r.Head] = string(r.Verdict)
	}
	if heads["cr-sonnet"] != "approve" || heads["cr-sol"] != "changes_requested" {
		t.Fatalf("verdicts by head = %v", heads)
	}

	artifact, err := review.Collect(dir, review.AggregateOptions{ExpectedHeads: []string{"cr-sol", "cr-sonnet"}})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(artifact.Items) != 1 || artifact.Items[0].Perspectives[review.PerspectiveCorrectness] != review.VerdictChangesRequested {
		t.Fatalf("aggregate = %+v, want the stricter head's verdict", artifact.Items)
	}
}
