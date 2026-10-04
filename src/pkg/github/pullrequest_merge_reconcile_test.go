package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/effects"
)

// mergeReconcileServer serves PR 7 of o/r: each PUT .../merge answers with the
// next status in mergeStatuses (the last repeats), and GET reports merged.
func mergeReconcileServer(t *testing.T, mergeStatuses []int, merged bool, puts *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		pr := map[string]any{"number": 7, "merged": merged, "head": map[string]any{"ref": "feature/x"}}
		if merged {
			pr["merge_commit_sha"] = "reconciledsha"
		}
		_ = json.NewEncoder(w).Encode(pr)
	})
	mux.HandleFunc("PUT /repos/o/r/pulls/7/merge", func(w http.ResponseWriter, _ *http.Request) {
		n := int(puts.Add(1)) - 1
		if n >= len(mergeStatuses) {
			n = len(mergeStatuses) - 1
		}
		status := mergeStatuses[n]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": "mergedsha", "message": "ok"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Pull Request is not mergeable"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestMergePR_DefiniteRefusalAllowsRetry is the #10536 reproduction: a
// definite GitHub refusal (405/409/422) must reach the requester as GitHub's
// own error and leave the same logical merge retryable, not journaled Unknown.
func TestMergePR_DefiniteRefusalAllowsRetry(t *testing.T) {
	for _, status := range []int{http.StatusMethodNotAllowed, http.StatusConflict, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var puts atomic.Int32
			srv := mergeReconcileServer(t, []int{status, http.StatusOK}, false, &puts)
			c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
			c.SetMutationBoundary(replayTestBoundary(t))

			_, err := c.MergePR(context.Background(), "o/r", 7, "squash", "headsha")
			if err == nil || !strings.Contains(err.Error(), "Pull Request is not mergeable") {
				t.Fatalf("first MergePR err = %v, want GitHub's refusal", err)
			}
			if errors.Is(err, effects.ErrNeedsReconciliation) {
				t.Fatalf("definite refusal surfaced the journal sentinel: %v", err)
			}
			res, err := c.MergePR(context.Background(), "o/r", 7, "squash", "headsha")
			if err != nil || !res.Merged || res.SHA != "mergedsha" {
				t.Fatalf("retry MergePR = (%+v, %v), want merged", res, err)
			}
			if got := puts.Load(); got != 2 {
				t.Fatalf("PUT merge ran %d times, want 2", got)
			}
		})
	}
}

// TestMergePR_ReconcilesUnknownAgainstGitHub: an ambiguous failure (5xx) is
// journaled Unknown; the next request reconciles from the PR's merged state
// instead of refusing with the journal sentinel forever.
func TestMergePR_ReconcilesUnknownAgainstGitHub(t *testing.T) {
	tests := []struct {
		name     string
		merged   bool
		statuses []int
		wantPuts int32
		wantSHA  string
		wantErr  string
	}{
		{"merged externally is idempotent success", true, []int{http.StatusBadGateway}, 1, "reconciledsha", ""},
		{"unmerged retries and surfaces GitHub's answer", false, []int{http.StatusBadGateway, http.StatusMethodNotAllowed}, 2, "", "Pull Request is not mergeable"},
		{"unmerged retries and merges", false, []int{http.StatusBadGateway, http.StatusOK}, 2, "mergedsha", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var puts atomic.Int32
			srv := mergeReconcileServer(t, tt.statuses, tt.merged, &puts)
			c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
			c.SetMutationBoundary(replayTestBoundary(t))

			if _, err := c.MergePR(context.Background(), "o/r", 7, "squash", "headsha"); err == nil {
				t.Fatal("first MergePR must fail on 502")
			}
			res, err := c.MergePR(context.Background(), "o/r", 7, "squash", "headsha")
			if got := puts.Load(); got != tt.wantPuts {
				t.Fatalf("PUT merge ran %d times, want %d", got, tt.wantPuts)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || errors.Is(err, effects.ErrNeedsReconciliation) {
					t.Fatalf("second MergePR err = %v, want GitHub's %q, not the journal sentinel", err, tt.wantErr)
				}
				return
			}
			if err != nil || !res.Merged || res.SHA != tt.wantSHA {
				t.Fatalf("second MergePR = (%+v, %v), want merged at %s", res, err, tt.wantSHA)
			}
		})
	}
}
