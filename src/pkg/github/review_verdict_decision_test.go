package github

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
)

func withDispositions(t *testing.T, raw string, decided bool) string {
	t.Helper()
	var report map[string]any
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatal(err)
	}
	report["dispositions"] = []any{map[string]any{"head": "cr-sol", "title": "nil deref", "disposition": "confirmed", "reason": "a.go:3 dereferences it", "severity": "medium"}}
	if decided {
		report["decided"] = true
	}
	out, _ := json.Marshal(report)
	return string(out)
}

// The decision step's reconciled report is stored apart from the heads' raw
// reports, and only a verdict bound to the decision dispatch is decided
// (hivecommons/hive#13542, part 2). A head that claims "decided" in its own
// JSON is recorded as a plain head report with its dispositions dropped.
func TestRecordReviewVerdictStoresDecidedReportsApart(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	head := pendingVerdict("o/r", 7, "correctness", "cr-sol-1")
	head.Head = "cr-sol"
	decision := pendingVerdict("o/r", 7, "correctness", "cr")
	decision.Decision = true
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{head, decision}})

	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 7, Agent: "cr-sol-1",
		Report: withDispositions(t, validVerdictJSON(t, "o/r", 7, "correctness", "changes_requested"), true)}, dir)
	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 7, Agent: "cr",
		Report: withDispositions(t, validVerdictJSON(t, "o/r", 7, "correctness", "approve"), false)}, dir)

	matches, _ := filepath.Glob(filepath.Join(dir, review.ReviewReportFilePrefix+"*"+review.ReviewReportFileSuffix))
	if len(matches) != 2 {
		t.Fatalf("want a head file and a decided file, got %v", matches)
	}
	var sawDecided, sawHead bool
	for _, m := range matches {
		b, _ := os.ReadFile(m)
		var r review.PerspectiveReport
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		switch {
		case r.Decided:
			sawDecided = true
			if !strings.HasSuffix(m, "-decided"+review.ReviewReportFileSuffix) || r.Head != "" || len(r.Dispositions) != 1 {
				t.Fatalf("decided report %s = %+v", m, r)
			}
		default:
			sawHead = true
			if r.Head != "cr-sol" || len(r.Dispositions) != 0 {
				t.Fatalf("head report kept a claimed decision: %+v", r)
			}
		}
	}
	if !sawDecided || !sawHead {
		t.Fatalf("decided=%v head=%v", sawDecided, sawHead)
	}

	artifact, err := review.Collect(dir, review.AggregateOptions{ExpectedHeads: []string{"cr-sol"}})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(artifact.Items) != 1 || !artifact.Items[0].Decided || artifact.Items[0].Perspectives[review.PerspectiveCorrectness] != review.VerdictApprove {
		t.Fatalf("aggregate = %+v, want the decided approve", artifact.Items)
	}
}
