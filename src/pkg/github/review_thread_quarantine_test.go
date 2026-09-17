package github

import (
	"context"
	"os"
	"testing"
	"time"
)

// Give-up horizon for the thread path (#7360): a resolve_thread request whose
// guard re-fetch keeps failing past requestRetryMaxAge must be quarantined as
// .failed by retryOrQuarantineReview instead of retrying every tick forever —
// the same contract TestReviewRequestWatcher_QuarantinesAfterMaxAge proves for
// the PR-review path.
func TestReviewRequestWatcher_ThreadFailureQuarantinesAfterMaxAge(t *testing.T) {
	mock, c, dir := threadWatcherFixture(t, testBots)
	mock.failWith = "Something went wrong while executing your query"
	reqPath, err := WriteReviewRequest(dir, ReviewRequest{Repo: "o/r", Number: 5, Event: "resolve_thread", ThreadID: "PRRT_bot", Agent: "scanner"})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	clock := func() time.Time { return now }

	// First failure lands in the retry branch: the request survives and the
	// result file records the transient error.
	c.processReviewRequests(context.Background(), clock)
	if _, err := os.Stat(reqPath); err != nil {
		t.Fatalf("request must survive the first transient failure: %v", err)
	}
	if resp := readReviewResult(t, reqPath); resp.OK {
		t.Fatalf("result must record the failure, got %+v", resp)
	}

	// Still failing past the give-up horizon: quarantined, not retried.
	now = now.Add(requestRetryMaxAge + time.Hour)
	c.processReviewRequests(context.Background(), clock)
	if _, err := os.Stat(reqPath + ".failed"); err != nil {
		t.Fatalf("expected .failed quarantine past the retry horizon: %v", err)
	}
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("original request should be renamed away")
	}

	// A quarantined request's tracker state is cleared: nothing left to allow
	// or suppress, and the next scan must not resurrect it.
	c.processReviewRequests(context.Background(), clock)
	if _, err := os.Stat(reqPath); !os.IsNotExist(err) {
		t.Errorf("quarantined request must stay quarantined")
	}
}
