package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetPRMergeStateForkWorkflowPushFence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fork       bool
		state      string
		prFile     string
		baseFile   string
		previous   string
		compareErr bool
		wantBlock  bool
		wantErr    bool
	}{
		{name: "PR workflow", fork: true, state: "dirty", prFile: ".github/workflows/ci.yml", wantBlock: true},
		{name: "incoming base workflow", fork: true, state: "behind", baseFile: ".github/workflows/ci.yml", wantBlock: true},
		{name: "renamed out of workflows", fork: true, state: "dirty", prFile: "ci.yml", previous: ".github/workflows/ci.yml", wantBlock: true},
		{name: "ordinary fork rebase", fork: true, state: "dirty", prFile: "src/main.go", baseFile: "README.md"},
		{name: "same repo unaffected", state: "dirty", prFile: ".github/workflows/ci.yml"},
		{name: "clean fork can still merge", fork: true, state: "clean", prFile: ".github/workflows/ci.yml"},
		{name: "compare failure fails closed", fork: true, state: "dirty", compareErr: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compares := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls/8"):
					headRepo := "org/repo"
					if tc.fork {
						headRepo = "person/repo"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"state": "open", "mergeable_state": tc.state, "maintainer_can_modify": true,
						"head": map[string]any{"sha": "head", "repo": map[string]string{"full_name": headRepo}},
						"base": map[string]any{"ref": "v5", "repo": map[string]string{"full_name": "org/repo"}}})
				case strings.Contains(r.URL.Path, "/compare/"):
					compares++
					if tc.compareErr {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					file, previous := tc.prFile, tc.previous
					if strings.HasSuffix(r.URL.Path, "head...v5") {
						file, previous = tc.baseFile, ""
					}
					// Put the file on page two: a workflow can occur after a
					// full page of ordinary paths in a real comparison.
					if r.URL.Query().Get("page") == "" {
						w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
						_, _ = io.WriteString(w, `{"files":[{"filename":"README.md"}]}`)
						return
					}
					files := []map[string]string{}
					if file != "" {
						files = append(files, map[string]string{"filename": file, "previous_filename": previous})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
				case strings.HasSuffix(r.URL.Path, "/status"):
					_, _ = io.WriteString(w, `{"state":"success","statuses":[]}`)
				case strings.HasSuffix(r.URL.Path, "/check-runs"):
					_, _ = io.WriteString(w, `{"check_runs":[]}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			c := NewClientForTest(srv.URL, "org", []string{"repo"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			st, err := c.GetPRMergeState(context.Background(), "org/repo", 8)
			if (err != nil) != tc.wantErr {
				t.Fatalf("state=%+v err=%v", st, err)
			}
			if tc.wantErr {
				return
			}
			if st.PushReachable() == tc.wantBlock || (len(st.WorkflowPushPaths) > 0) != tc.wantBlock {
				t.Fatalf("push reachability or workflow evidence incorrect: %+v", st)
			}
			if tc.fork && st.NeedsRebase() {
				if compares != 4 {
					t.Fatalf("compares=%d, want both pages of both directions", compares)
				}
			} else if compares != 0 {
				t.Fatalf("unnecessary compares=%d", compares)
			}
		})
	}
}
