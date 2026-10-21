package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
		{"topup head and title (#13404)", "topup/v5-into-v6-20261020", "🌱 top-up: merge v5 into v6 (51 commits)", "squash", "merge"},
		{"rebase on forward-merge upgraded", "sync/v5-to-v6", "sync", "rebase", "merge"},
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

func TestIsForwardMergePR(t *testing.T) {
	tests := []struct {
		head, title string
		want        bool
	}{
		{"topup/v5-into-v6-20261020", "🌱 top-up: merge v5 into v6 (51 commits)", true},
		{"topup/v5-into-v6-20261020", "merge v5 into v6", true},
		{"feature/x", "🌱 top-up: merge v5 into v6", true},
		{"feature/x", "sync: v5 into v6", true},
		{"sync/v5-to-v6", "chore", true},
		{"scanner/sync-v5-to-v6-9919", "chore", true},
		{"feature/topup-docs", "docs: explain the top-up workflow", false},
		{"feature/x", "🐛 fix: sync loop stalls", false},
		{"feature/x", "fix: thing", false},
	}
	for _, tt := range tests {
		pr := &gh.PullRequest{Title: gh.Ptr(tt.title), Head: &gh.PullRequestBranch{Ref: gh.Ptr(tt.head)}}
		if got := IsForwardMergePR(pr); got != tt.want {
			t.Errorf("IsForwardMergePR(head=%q, title=%q) = %v, want %v", tt.head, tt.title, got, tt.want)
		}
	}
}

// A top-up whose branch and title give no hint is still caught by its head
// history (#13404): the head contains v5's tip, or a v5 merge-base that v6 does
// not yet have.
func TestMergePR_ForwardMergeGuardByHeadHistory(t *testing.T) {
	tests := []struct {
		name string
		head string
		// compares maps "base...head" to the compare status and merge-base.
		compares map[string][2]string
		want     string
	}{
		{
			name:     "head contains v5 tip",
			head:     "h1",
			compares: map[string][2]string{"v5...h1": {"ahead", "v5tip"}},
			want:     "merge",
		},
		{
			name: "head merged an older v5 tip v6 does not have",
			head: "h2",
			compares: map[string][2]string{
				"v5...h2":    {"diverged", "v5old"},
				"v6...v5old": {"diverged", "v5fork"},
			},
			want: "merge",
		},
		{
			name: "ordinary v6 PR squashes",
			head: "h3",
			compares: map[string][2]string{
				"v5...h3":     {"diverged", "v5fork"},
				"v6...v5fork": {"behind", "v5fork"},
			},
			want: "squash",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotMethod string
			mux := http.NewServeMux()
			mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number": 7,
					"title":  "Update v6",
					"head":   map[string]any{"ref": "feature/x", "sha": tt.head},
					"base":   map[string]any{"ref": "v6"},
				})
			})
			mux.HandleFunc("GET /repos/o/r/branches", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("protected") != "true" {
					t.Errorf("branches query = %q, want protected=true", r.URL.RawQuery)
				}
				_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "main"}, {"name": "v5"}, {"name": "v6"}})
			})
			mux.HandleFunc("GET /repos/o/r/compare/", func(w http.ResponseWriter, r *http.Request) {
				key := strings.TrimPrefix(r.URL.Path, "/repos/o/r/compare/")
				c, ok := tt.compares[key]
				if !ok {
					t.Errorf("unexpected compare %s", key)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": c[0], "merge_base_commit": map[string]any{"sha": c[1]}})
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
			if _, err := c.MergePR(context.Background(), "o/r", 7, "squash", ""); err != nil {
				t.Fatalf("MergePR: %v", err)
			}
			if gotMethod != tt.want {
				t.Fatalf("merge_method = %q, want %q", gotMethod, tt.want)
			}
		})
	}
}
