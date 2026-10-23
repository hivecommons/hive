package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
)

// branchUpdateServer serves PR 5 of o/r: the first PUT update-branch answers
// 502 (an ambiguous failure the journal records Unknown) and later ones 202;
// GET answers with headSHA, or 500 when headSHA is empty.
func branchUpdateServer(t *testing.T, headSHA string, puts *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/5", func(w http.ResponseWriter, _ *http.Request) {
		if headSHA == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "head": map[string]any{"sha": headSHA}})
	})
	mux.HandleFunc("PUT /repos/o/r/pulls/5/update-branch", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if puts.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "bad gateway"})
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Updating pull request branch."})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// An ambiguous update-branch failure leaves the op Unknown; the next request
// must settle it from the PR's observed head instead of refusing forever.
func TestUpdateBranch_ReconcilesUnknownByObservedHead(t *testing.T) {
	tests := []struct {
		name     string
		head     string
		wantPuts int32
	}{
		{"head moved off expected: applied, no second update", "newhead", 1},
		{"head still at expected: not applied, update retries", "oldhead", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var puts atomic.Int32
			srv := branchUpdateServer(t, tt.head, &puts)
			c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
			c.SetMutationBoundary(replayTestBoundary(t))

			if err := c.UpdateBranchExpectedHead(context.Background(), "o/r", 5, "oldhead", "base1"); err == nil {
				t.Fatal("first update must fail on 502")
			}
			if err := c.UpdateBranchExpectedHead(context.Background(), "o/r", 5, "oldhead", "base1"); err != nil {
				t.Fatalf("second update err = %v, want reconciled success", err)
			}
			if got := puts.Load(); got != tt.wantPuts {
				t.Fatalf("PUT update-branch ran %d times, want %d", got, tt.wantPuts)
			}
		})
	}
}

// When the PR cannot be read the Unknown op keeps refusing retries only until
// it is older than the TTL; past it the op expires and the update runs again.
func TestUpdateBranch_ExpiredUnknownPermitsRetry(t *testing.T) {
	var puts atomic.Int32
	srv := branchUpdateServer(t, "", &puts)
	c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
	b := replayTestBoundary(t)
	now := time.Now()
	b.Executor.Now = func() time.Time { return now }
	c.SetMutationBoundary(b)

	if err := c.UpdateBranch(context.Background(), "o/r", 5); err == nil {
		t.Fatal("first update must fail on 502")
	}
	now = now.Add(effects.DefaultUnresolvedTTL / 2)
	if err := c.UpdateBranch(context.Background(), "o/r", 5); !errors.Is(err, effects.ErrNeedsReconciliation) {
		t.Fatalf("inside TTL err = %v, want ErrNeedsReconciliation", err)
	}
	if got := puts.Load(); got != 1 {
		t.Fatalf("PUT update-branch ran %d times inside TTL, want 1", got)
	}
	now = now.Add(effects.DefaultUnresolvedTTL)
	if err := c.UpdateBranch(context.Background(), "o/r", 5); err != nil {
		t.Fatalf("past TTL err = %v, want expired op to retry and succeed", err)
	}
	if got := puts.Load(); got != 2 {
		t.Fatalf("PUT update-branch ran %d times, want 2", got)
	}
}

// An ambiguous review POST leaves the review_submit op Unknown; the next
// request finds the review on the PR and replays as success without posting
// a duplicate approval.
func TestQueuePRAutoMerge_ReconcilesUnknownReviewByObservedReview(t *testing.T) {
	var posts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "head": map[string]any{"sha": "head7"}})
	})
	mux.HandleFunc("GET /repos/o/r/labels/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": AutoMergeQueuedLabel})
	})
	mux.HandleFunc("POST /repos/o/r/pulls/7/reviews", func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "bad gateway"})
	})
	mux.HandleFunc("GET /repos/o/r/pulls/7/reviews", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "state": "COMMENTED", "commit_id": "head7", "body": "unrelated"},
			{"id": 2, "state": "APPROVED", "commit_id": "head7", "body": "Approved by @alice for Hive auto-merge on green CI.\r\n"},
		})
	})
	mux.HandleFunc("POST /repos/o/r/statuses/head7", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "state": "success"})
	})
	mux.HandleFunc("POST /repos/o/r/issues/7/labels", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"name": AutoMergeQueuedLabel}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
	c.SetMutationBoundary(replayTestBoundary(t))

	if err := c.QueuePRAutoMerge(context.Background(), "o/r", 7, "alice"); err == nil {
		t.Fatal("first queue must fail on 502")
	}
	if err := c.QueuePRAutoMerge(context.Background(), "o/r", 7, "alice"); err != nil {
		t.Fatalf("second queue err = %v, want reconciled success", err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("POST review ran %d times, want 1 (no duplicate approval)", got)
	}
}
