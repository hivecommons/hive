package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
