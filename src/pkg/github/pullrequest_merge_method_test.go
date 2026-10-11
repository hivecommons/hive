package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// The hive-merge relay reaches MergePR with the request's raw method, so the
// forward-merge guard (#9958) must live there, not only in the sweep (#10265).
func TestMergePR_ForwardMergeGuard(t *testing.T) {
	tests := []struct {
		name   string
		head   string
		title  string
		method string
		want   string
	}{
		{"forward-merge head, empty method", "sync/v5-to-v6-3", "sync", "", "merge"},
		{"forward-merge head, squash", "scanner/sync-v5-to-v6-9919", "sync", "squash", "merge"},
		{"forward-merge title", "feature/x", "sync: Forward-Merge v5 into v6", "", "merge"},
		{"ordinary PR defaults to squash", "feature/x", "fix: thing", "", "squash"},
		{"ordinary PR explicit squash", "feature/x", "fix: thing", "squash", "squash"},
		{"explicit non-squash preserved", "feature/x", "fix: thing", "rebase", "rebase"},
		{"explicit merge on forward-merge", "sync/v5-to-v6", "sync", "merge", "merge"},
		{"dotted-version forward-merge head", "sync/v1.7.11-to-v1.9.1", "sync", "", "merge"},
		{"upstream-sync title", "scanner/fix-86", "chore: sync upstream v1.7.12-v1.9.1", "squash", "merge"},
		{"plain scanner PR squashes", "scanner/fix-88", "fix: handle nil config", "", "squash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotMethod string
			mux := http.NewServeMux()
			mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": 7,
					"title":  tt.title,
					"head":   map[string]any{"ref": tt.head},
				})
			})
			mux.HandleFunc("PUT /repos/o/r/pulls/7/merge", func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				gotMethod, _ = body["merge_method"].(string)
				_ = json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": "abc", "message": "ok"})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
			if _, err := c.MergePR(context.Background(), "o/r", 7, tt.method, ""); err != nil {
				t.Fatalf("MergePR: %v", err)
			}
			if gotMethod != tt.want {
				t.Fatalf("merge_method = %q, want %q", gotMethod, tt.want)
			}
		})
	}
}

func TestIsMergeCommitPR(t *testing.T) {
	pr := func(head, title string, labels ...string) *gh.PullRequest {
		p := &gh.PullRequest{Title: gh.String(title), Head: &gh.PullRequestBranch{Ref: gh.String(head)}}
		for _, l := range labels {
			p.Labels = append(p.Labels, &gh.Label{Name: gh.String(l)})
		}
		return p
	}
	tests := []struct {
		name         string
		pr           *gh.PullRequest
		upstreamSync bool
		mergeCommit  bool
	}{
		{"nil", nil, false, false},
		{"dotted forward-merge head", pr("sync/v1.7.11-to-v1.9.1", "x"), false, true},
		{"scanner dotted forward-merge head", pr("scanner/sync-v1.7-to-v1.9-12", "x"), false, true},
		{"sync upstream title", pr("scanner/fix-86", "chore: sync upstream v1.7.12-v1.9.1"), true, true},
		{"sync upstream title, mixed case", pr("scanner/fix-86", "Chore: Sync Upstream main"), true, true},
		{"upstream-sync label", pr("scanner/fix-87", "chore: catch up", "bug", UpstreamSyncLabel), true, true},
		{"upstream-sync head prefix", pr("upstream-sync/v1.9.1", "chore: catch up"), true, true},
		{"unrelated label", pr("scanner/fix-88", "fix: thing", "bug"), false, false},
		{"resync in a word is not a sync", pr("scanner/fix-89", "fix: resyncupstream cache"), false, false},
		{"plain scanner PR", pr("scanner/fix-90", "fix: handle nil config"), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsUpstreamSyncPR(tt.pr); got != tt.upstreamSync {
				t.Fatalf("IsUpstreamSyncPR = %v, want %v", got, tt.upstreamSync)
			}
			if got := IsMergeCommitPR(tt.pr); got != tt.mergeCommit {
				t.Fatalf("IsMergeCommitPR = %v, want %v", got, tt.mergeCommit)
			}
		})
	}
}
