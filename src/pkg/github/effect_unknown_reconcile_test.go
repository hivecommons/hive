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

// issueCommentServer serves issue 4 of o/r: the first POST comment answers
// 502 (an ambiguous failure the journal records Unknown) and later ones 201;
// GET comments answers with existing, or 500 when listFails.
func issueCommentServer(t *testing.T, existing []map[string]any, listFails bool, posts *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/issues/4/comments", func(w http.ResponseWriter, _ *http.Request) {
		if listFails {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(existing)
	})
	mux.HandleFunc("POST /repos/o/r/issues/4/comments", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if posts.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "bad gateway"})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 9, "html_url": "https://github.com/o/r/issues/4#issuecomment-9"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// An ambiguous comment POST leaves the issue_comment op Unknown; the next
// post must settle it from the issue's comments instead of refusing forever
// (hivecommons/hive#14011), without posting a duplicate when it landed.
func TestCreateIssueComment_ReconcilesUnknownByObservedComment(t *testing.T) {
	const body = "status update"
	tests := []struct {
		name      string
		existing  []map[string]any
		wantPosts int32
	}{
		{"comment landed: applied, no duplicate", []map[string]any{
			{"id": 1, "body": "unrelated"},
			{"id": 2, "body": body + "\r\n", "html_url": "https://github.com/o/r/issues/4#issuecomment-2"},
		}, 1},
		{"comment absent: not applied, post retries", []map[string]any{{"id": 1, "body": "unrelated"}}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var posts atomic.Int32
			srv := issueCommentServer(t, tt.existing, false, &posts)
			c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
			c.SetMutationBoundary(replayTestBoundary(t))

			if err := c.CreateIssueComment(context.Background(), "o/r", 4, body); err == nil {
				t.Fatal("first comment must fail on 502")
			}
			if err := c.CreateIssueComment(context.Background(), "o/r", 4, body); err != nil {
				t.Fatalf("second comment err = %v, want reconciled success", err)
			}
			if got := posts.Load(); got != tt.wantPosts {
				t.Fatalf("POST comment ran %d times, want %d", got, tt.wantPosts)
			}
		})
	}
}

// When the comments cannot be listed the Unknown op keeps refusing retries
// only until it is older than the TTL; past it the op expires and the post
// runs again.
func TestCreateIssueComment_ExpiredUnknownPermitsRetry(t *testing.T) {
	var posts atomic.Int32
	srv := issueCommentServer(t, nil, true, &posts)
	c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
	b := replayTestBoundary(t)
	now := time.Now()
	b.Executor.Now = func() time.Time { return now }
	c.SetMutationBoundary(b)

	if err := c.CreateIssueComment(context.Background(), "o/r", 4, "hello"); err == nil {
		t.Fatal("first comment must fail on 502")
	}
	now = now.Add(effects.DefaultUnresolvedTTL / 2)
	if err := c.CreateIssueComment(context.Background(), "o/r", 4, "hello"); !errors.Is(err, effects.ErrNeedsReconciliation) {
		t.Fatalf("inside TTL err = %v, want ErrNeedsReconciliation", err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("POST comment ran %d times inside TTL, want 1", got)
	}
	now = now.Add(effects.DefaultUnresolvedTTL)
	if err := c.CreateIssueComment(context.Background(), "o/r", 4, "hello"); err != nil {
		t.Fatalf("past TTL err = %v, want expired op to retry and succeed", err)
	}
	if got := posts.Load(); got != 2 {
		t.Fatalf("POST comment ran %d times, want 2", got)
	}
}

// An ambiguous PR create POST leaves the pull_request_create op Unknown; the
// next create must settle it from the open PR for the head instead of
// refusing forever (hivecommons/hive#14366), without opening a duplicate.
func TestCreatePR_ReconcilesUnknownByObservedHead(t *testing.T) {
	tests := []struct {
		name         string
		landedAtList int32
		wantPosts    int32
		wantExisted  bool
		wantPRNumber int
	}{
		{"PR landed: applied, no duplicate", 3, 1, true, 7},
		{"PR absent: not applied, create retries", 0, 2, false, 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var posts, headLists atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/org/repo/pulls":
					// Lists by head: 1 = first dedupe, 2 = second dedupe,
					// 3 = the reconciling observation.
					if r.URL.Query().Get("head") != "" && tt.landedAtList > 0 && headLists.Add(1) >= tt.landedAtList {
						_ = json.NewEncoder(w).Encode([]any{map[string]any{"number": 7, "html_url": "https://github.com/org/repo/pull/7"}})
						return
					}
					_ = json.NewEncoder(w).Encode([]any{})
				case r.Method == http.MethodPost && r.URL.Path == "/repos/org/repo/pulls":
					if posts.Add(1) == 1 {
						w.WriteHeader(http.StatusBadGateway)
						_ = json.NewEncoder(w).Encode(map[string]any{"message": "bad gateway"})
						return
					}
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 8, "html_url": "https://github.com/org/repo/pull/8"})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			c := NewClientForTest(srv.URL, "org", nil, prTestLogger())
			c.SetMutationBoundary(replayTestBoundary(t))

			if _, err := c.CreatePR(context.Background(), "repo", "feature", "main", "Add tests", "body"); err == nil {
				t.Fatal("first create must fail on 502")
			}
			got, err := c.CreatePR(context.Background(), "repo", "feature", "main", "Add tests", "body")
			if err != nil {
				t.Fatalf("second create err = %v, want reconciled success", err)
			}
			if errors.Is(err, effects.ErrNeedsReconciliation) {
				t.Fatal("second create must not be refused pending reconciliation")
			}
			if got.Number != tt.wantPRNumber || got.AlreadyExisted != tt.wantExisted {
				t.Fatalf("second create = %+v, want number %d AlreadyExisted=%v", got, tt.wantPRNumber, tt.wantExisted)
			}
			if n := posts.Load(); n != tt.wantPosts {
				t.Fatalf("POST /pulls ran %d times, want %d", n, tt.wantPosts)
			}
		})
	}
}
