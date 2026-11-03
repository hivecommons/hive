package github

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
)

const fullHead13205 = "98489e7ba141fc60c30c9aaf47774dc42f9b1c76"

// An abbreviated head_sha in the verdict still binds to the dispatch row that
// named the full head, and is stored under the full head so the lanes find it.
func TestRecordReviewVerdictWidensAbbreviatedHeadToDispatchHead(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	raw := validVerdictJSONWithHead(t, "o/r", 1, "correctness", "approve", "98489e7")
	pending := pendingVerdict("o/r", 1, review.PerspectiveCorrectness, "reviewer")
	pending.HeadSHA = fullHead13205
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{pending}})

	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 1, Agent: "reviewer", Report: raw}, dir)

	artifact, err := review.Collect(dir, review.AggregateOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(artifact.Items) != 1 {
		t.Fatalf("want 1 aggregated item, got %+v", artifact.Items)
	}
	if got := artifact.Items[0].HeadSHA; got != fullHead13205 {
		t.Fatalf("abbreviated head was stored as %q, want the dispatch head %q", got, fullHead13205)
	}
}

// Queue-mode reviews (no dispatch row, the hivecommons/hive#13205 case) widen
// the abbreviated head against the PR's current head with one GET; a short
// head that is not the current head is kept as written.
func TestRecordReviewVerdictWidensAbbreviatedHeadToPRHeadWithoutDispatch(t *testing.T) {
	withEvidenceRoot(t)
	f := &evidencePRFixture{head: fullHead13205}
	c := evidenceTestClient(t, f, nil)
	withVerdictDispatchState(t, review.DispatchState{})

	dir := filepath.Join(t.TempDir(), "reports")
	for _, p := range []string{"correctness", "security"} {
		raw := validVerdictJSONWithHead(t, "o/r", 7, p, "approve", "98489e7")
		c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 7, Agent: "reviewer", Report: raw}, dir)
	}
	artifact, err := review.Collect(dir, review.AggregateOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(artifact.Items) != 1 || artifact.Items[0].HeadSHA != fullHead13205 {
		t.Fatalf("advisory verdicts not keyed by the full PR head: %+v", artifact.Items)
	}
	if _, ok := artifact.AggregateForBody("o/r", 7, fullHead13205, ""); !ok {
		t.Fatal("full-head lookup must find the widened verdict")
	}
	if f.fetches() != 2 {
		t.Fatalf("PR head must be fetched once per relay pass, got %d GETs over 2 passes", f.fetches())
	}

	stale := validVerdictJSONWithHead(t, "o/r", 7, "style", "approve", "deadbee")
	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 7, Agent: "reviewer", Report: stale}, dir)
	artifact, err = review.Collect(dir, review.AggregateOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var sawStale bool
	for _, it := range artifact.Items {
		if it.HeadSHA == "deadbee" {
			sawStale = true
		}
		if it.HeadSHA == fullHead13205 && it.Perspectives[review.PerspectiveStyle] != "" {
			t.Fatalf("stale head verdict was re-attributed to the current head: %+v", it)
		}
	}
	if !sawStale {
		t.Fatalf("stale verdict must be recorded under the head it named: %+v", artifact.Items)
	}
}
