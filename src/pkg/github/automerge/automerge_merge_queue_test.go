package automerge

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

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
