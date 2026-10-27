package automerge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/effects"
)

// mergeQueueRequiredTestMessage is the 405 body GitHub returns for a direct
// merge into a branch whose ruleset requires a merge queue
// (hivecommons/hive#14018).
const mergeQueueRequiredTestMessage = "Repository rule violations found\n\nChanges must be made through the merge queue"

func TestMergeQueueRequiredMergeError(t *testing.T) {
	resp := func(status int, msg string) error {
		return &gh.ErrorResponse{Response: &http.Response{StatusCode: status}, Message: msg}
	}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "plain error", err: errors.New("Changes must be made through the merge queue"), want: false},
		{name: "merge queue 405", err: resp(http.StatusMethodNotAllowed, mergeQueueRequiredTestMessage), want: true},
		{name: "merge queue 405 wrapped not-applied", err: effects.NotApplied(resp(http.StatusMethodNotAllowed, mergeQueueRequiredTestMessage)), want: true},
		{name: "merge queue 405 wrapped", err: fmt.Errorf("merge: %w", resp(http.StatusMethodNotAllowed, mergeQueueRequiredTestMessage)), want: true},
		{name: "other 405", err: resp(http.StatusMethodNotAllowed, "2 of 3 required status checks are expected."), want: false},
		{name: "merge queue text on 409", err: resp(http.StatusConflict, mergeQueueRequiredTestMessage), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeQueueRequiredMergeError(tc.err); got != tc.want {
				t.Fatalf("mergeQueueRequiredMergeError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestSweepSelfAuthoredMergeQueueRequiredBase covers hivecommons/hive#14068:
// a merge-queue-only base is a terminal skip logged once, and later passes
// stop re-issuing the direct merge until the recheck window elapses.
func TestSweepSelfAuthoredMergeQueueRequiredBase(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mergeMessage string
		advance      time.Duration
		wantReason   string
		wantAttempts int
		wantHolds    int
		wantWarns    int
	}{
		{name: "merge queue 405 short-circuits later passes", mergeMessage: mergeQueueRequiredTestMessage, wantReason: autoMergeReasonMergeQueueRequired, wantAttempts: 1, wantHolds: 1},
		{name: "merge queue base rechecked after window", mergeMessage: mergeQueueRequiredTestMessage, advance: mergeQueueBaseRecheck, wantReason: autoMergeReasonMergeQueueRequired, wantAttempts: 2, wantHolds: 1},
		{name: "other 405 stays merge-failed and warns once", mergeMessage: "Pull Request is not mergeable", wantReason: "merge-failed", wantAttempts: 2, wantWarns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var merged []int
			var attempts int
			api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{{
				number: 90, author: testHiveAppBotLogin, mergeableState: "clean",
				statusState: "success", checkStatus: "completed", checkConclusion: "success",
				mergeStatus: http.StatusMethodNotAllowed, mergeMessage: tc.mergeMessage, mergeAttempts: &attempts,
			}}, &merged)
			defer api.Close()

			var logs bytes.Buffer
			c := newAutoMergeSweepClient(api.URL)
			c.logger = slog.New(slog.NewTextHandler(&logs, nil))
			clock := time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC)
			c.now = func() time.Time { return clock }

			for pass := 0; pass < 2; pass++ {
				if pass == 1 {
					clock = clock.Add(tc.advance)
				}
				result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
				if err != nil {
					t.Fatalf("pass %d: SweepSelfAuthoredAutoMerges returned error: %v", pass, err)
				}
				if len(result.Merged) != 0 || result.Skipped != 1 {
					t.Fatalf("pass %d: result=%+v, want one skip and no merge", pass, result)
				}
			}
			gotLogs := logs.String()
			if attempts != tc.wantAttempts {
				t.Fatalf("merge attempts = %d, want %d", attempts, tc.wantAttempts)
			}
			if !strings.Contains(gotLogs, "reason="+tc.wantReason) {
				t.Fatalf("logs = %q, want reason=%s", gotLogs, tc.wantReason)
			}
			if got := strings.Count(gotLogs, "msg=\"automerge hold\""); got != tc.wantHolds {
				t.Fatalf("automerge hold logs = %d, want %d; logs = %q", got, tc.wantHolds, gotLogs)
			}
			if got := strings.Count(gotLogs, "level=WARN msg=\"self-authored automerge sweep skipped PR\""); got != tc.wantWarns {
				t.Fatalf("WARN logs = %d, want %d; logs = %q", got, tc.wantWarns, gotLogs)
			}
		})
	}
}
