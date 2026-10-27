package github

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
)

type verdictHookCall struct {
	repo   string
	number int
}

func recordingVerdictHook(c *Client) *[]verdictHookCall {
	var calls []verdictHookCall
	c.SetReviewVerdictHook(func(repo string, number int) {
		calls = append(calls, verdictHookCall{repo, number})
	})
	return &calls
}

// A recorded dispatched verdict frees its head, so the hook fires: once per
// request, even when a combined review answered several perspectives
// (hivecommons/hive#14705).
func TestRecordReviewVerdictCallsHookOnceForDispatchedVerdicts(t *testing.T) {
	c := &Client{logger: testLogger()}
	calls := recordingVerdictHook(c)
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
		pendingVerdict("o/r", 7, "correctness", "reviewer"),
		pendingVerdict("o/r", 7, "security", "reviewer"),
	}})
	raw := "[" + strings.Join([]string{
		validVerdictJSON(t, "o/r", 7, "correctness", "approve"),
		validVerdictJSON(t, "o/r", 7, "security", "approve"),
	}, ",") + "]"

	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 7, Agent: "reviewer", Report: raw}, t.TempDir())

	if len(*calls) != 1 || (*calls)[0] != (verdictHookCall{"o/r", 7}) {
		t.Fatalf("hook calls = %+v, want one for o/r#7", *calls)
	}
}

// An advisory verdict binds no dispatch, so no head was freed and nothing
// wakes.
func TestRecordReviewVerdictDoesNotCallHookForAdvisoryVerdict(t *testing.T) {
	c := &Client{logger: testLogger()}
	calls := recordingVerdictHook(c)
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
		pendingVerdict("o/r", 2, review.PerspectiveSecurity, "reviewer"),
	}})
	raw := validVerdictJSONWithHead(t, "o/r", 1, "security", "approve", "abc123")

	got := c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 1, Agent: "reviewer", Report: raw}, t.TempDir())

	if len(got) != 1 {
		t.Fatalf("advisory verdict was not recorded: %+v", got)
	}
	if len(*calls) != 0 {
		t.Fatalf("advisory verdict called the hook: %+v", *calls)
	}
}

// A refused verdict records nothing and wakes nothing.
func TestRecordReviewVerdictDoesNotCallHookForRefusedVerdict(t *testing.T) {
	c := &Client{logger: testLogger()}
	calls := recordingVerdictHook(c)
	withVerdictDispatchState(t, review.DispatchState{})
	raw := validVerdictJSON(t, "o/r", 1, "correctness", "approve")

	got := c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 1, Agent: "reviewer", Report: raw}, t.TempDir())

	if len(got) != 0 {
		t.Fatalf("refused verdict was recorded: %+v", got)
	}
	if len(*calls) != 0 {
		t.Fatalf("refused verdict called the hook: %+v", *calls)
	}
}

func TestSetReviewVerdictHookNilClears(t *testing.T) {
	c := &Client{logger: testLogger()}
	calls := recordingVerdictHook(c)
	c.SetReviewVerdictHook(nil)
	c.notifyReviewVerdict("o/r", 1)
	if len(*calls) != 0 {
		t.Fatalf("cleared hook still called: %+v", *calls)
	}
	var nilClient *Client
	nilClient.SetReviewVerdictHook(func(string, int) {})
	nilClient.notifyReviewVerdict("o/r", 1)
}
