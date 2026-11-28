package github

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
)

// fakeReviewedPRReader serves one PR and counts the reads, standing in for a
// non-GitHub forge (hivecommons/hive#20289).
type fakeReviewedPRReader struct {
	pr    ReviewedPR
	reads int
}

func (f *fakeReviewedPRReader) ReviewedPR(_ context.Context, repo string, number int) (ReviewedPR, error) {
	f.reads++
	return f.pr, nil
}

// fakeReaderClient is a Client with no GitHub REST client: every PR read the
// verdict writer makes must go through reader.
func fakeReaderClient(reader ReviewedPRReader) *Client {
	c := &Client{logger: testLogger(), reviewerAgent: func(agent string) bool { return agent == "reviewer" }}
	c.SetReviewedPRReader(reader)
	return c
}

func recordedReports(t *testing.T, reportDir string) []review.PerspectiveReport {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(reportDir, review.ReviewReportFilePrefix+"*"))
	var out []review.PerspectiveReport
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		var r review.PerspectiveReport
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("unmarshal %s: %v", m, err)
		}
		out = append(out, r)
	}
	return out
}

// An advisory verdict with an abbreviated head is widened to the head the PR
// reader reports, and stamped with the digest of the title and body it reads.
func TestRecordReviewVerdictAdvisoryReadsPRThroughForgeNeutralReader(t *testing.T) {
	withVerdictDispatchState(t, review.DispatchState{})
	reportDir := filepath.Join(t.TempDir(), "reports")
	reader := &fakeReviewedPRReader{pr: ReviewedPR{HeadSHA: headMoveH2, Title: "t", Body: "b", Author: "someone", Base: "v5", Labels: []string{"lgtm"}}}
	c := fakeReaderClient(reader)
	raw := validVerdictJSONWithHead(t, "o/r", 1, "correctness", "approve", headMoveH2[:7])

	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 1, Agent: "reviewer", Report: raw}, reportDir)

	got := recordedReports(t, reportDir)
	if len(got) != 1 {
		t.Fatalf("recorded %d reports, want 1", len(got))
	}
	if got[0].HeadSHA != headMoveH2 {
		t.Errorf("HeadSHA = %q, want %q", got[0].HeadSHA, headMoveH2)
	}
	if want := review.PullRequestBodyDigest("t", "b"); got[0].BodyDigest != want {
		t.Errorf("BodyDigest = %q, want %q", got[0].BodyDigest, want)
	}
	if reader.reads != 1 {
		t.Errorf("reader reads = %d, want 1 (memoized)", reader.reads)
	}
}

// The dispatch binding resolves a head move from the PR reader's head: a
// verdict for the PR's current head binds to the row dispatched at the
// earlier head.
func TestRecordReviewVerdictDispatchBindingUsesForgeNeutralReader(t *testing.T) {
	row := pendingVerdict("o/r", 1, review.PerspectiveCorrectness, "reviewer")
	row.HeadSHA = headMoveH1
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{row}})
	reportDir := filepath.Join(t.TempDir(), "reports")
	c := fakeReaderClient(&fakeReviewedPRReader{pr: ReviewedPR{HeadSHA: headMoveH2, Title: "t", Body: "b", Base: "v5"}})
	raw := validVerdictJSONWithHead(t, "o/r", 1, "correctness", "approve", headMoveH2)

	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 1, Agent: "reviewer", Report: raw}, reportDir)

	got := recordedReports(t, reportDir)
	if len(got) != 1 || got[0].HeadSHA != headMoveH2 {
		t.Fatalf("reports = %+v, want one bound under %s", got, headMoveH2)
	}
}

// The advisory rule is unchanged behind the reader: a lane that is not a
// reviewer records nothing without a dispatch.
func TestRecordReviewVerdictAdvisoryRuleHoldsWithForgeNeutralReader(t *testing.T) {
	withVerdictDispatchState(t, review.DispatchState{})
	reportDir := filepath.Join(t.TempDir(), "reports")
	c := fakeReaderClient(&fakeReviewedPRReader{pr: ReviewedPR{HeadSHA: headMoveH2, Title: "t", Body: "b"}})
	raw := validVerdictJSONWithHead(t, "o/r", 1, "correctness", "approve", headMoveH2)

	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 1, Agent: "docs-writer", Report: raw}, reportDir)

	if got := recordedReports(t, reportDir); len(got) != 0 {
		t.Fatalf("non-reviewer advisory verdict was recorded: %+v", got)
	}
}
