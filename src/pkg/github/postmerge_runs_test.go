package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/workflowmodel"
)

func postMergeTestModel() *workflowmodel.Model {
	pipe := func(path string, role workflowmodel.Role) workflowmodel.Pipeline {
		return workflowmodel.Pipeline{ID: workflowmodel.PipelineID(workflowmodel.SystemGitHubActions, path), Name: path, System: workflowmodel.SystemGitHubActions, Path: path, Role: role}
	}
	return workflowmodel.Bind(workflowmodel.Scope{Forge: "github", Repository: "o/r"},
		[]workflowmodel.Pipeline{pipe(".github/workflows/main.yml", workflowmodel.RolePushVerify), pipe(".github/workflows/release.yml", workflowmodel.RoleRelease)},
		nil, workflowmodel.Coverage{})
}

func TestObservePostMergeRunsFeedsPipelineStatus(t *testing.T) {
	commit := func(sha string, parents ...string) map[string]any {
		ps := []map[string]string{}
		for _, p := range parents {
			ps = append(ps, map[string]string{"sha": p})
		}
		return map[string]any{"sha": sha, "parents": ps}
	}
	runs := map[string][]map[string]any{
		"main.yml": {
			{"id": 11, "path": ".github/workflows/main.yml", "event": "push", "head_branch": "main", "head_sha": "c3", "status": "completed", "conclusion": "success", "created_at": "2026-11-01T00:03:00Z"},
		},
		"release.yml": {
			{"id": 22, "path": ".github/workflows/release.yml", "event": "release", "head_branch": "v1.1.0", "head_sha": "c2", "status": "completed", "conclusion": "failure", "created_at": "2026-11-01T00:04:00Z"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/o/r/commits":
			if r.URL.Query().Get("sha") != "main" {
				t.Errorf("history listed %q, want main", r.URL.Query().Get("sha"))
			}
			// c3 merges side commit s1 as its second parent; s1 is not
			// first-parent history.
			_ = json.NewEncoder(w).Encode([]map[string]any{commit("c3", "c2", "s1"), commit("s1", "c1"), commit("c2", "c1"), commit("c1", "c0"), commit("c0")})
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/actions/workflows/"):
			file := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/o/r/actions/workflows/"), "/runs")
			list := runs[file]
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(list), "workflow_runs": list})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := gh.NewClient(nil)
	client.BaseURL, _ = url.Parse(srv.URL + "/")

	got, err := ObservePostMergeRuns(context.Background(), client, PostMergeRequest{
		Owner: "o", Repo: "r", DefaultBranch: "main", Model: postMergeTestModel(), EnabledAt: "c0",
		PRs: []workflowmodel.MergedPR{{Number: 1, MergeCommit: "c1"}, {Number: 3, MergeCommit: "c3"}, {Number: 9, MergeCommit: "s1"}},
	})
	if err != nil {
		t.Fatalf("ObservePostMergeRuns: %v", err)
	}
	if pm := got[1]; pm == nil || pm.Status != "red" || pm.Column != workflowmodel.StateRelease {
		t.Fatalf("PR 1 (failed batched release): %+v", pm)
	}
	if pm := got[3]; pm == nil || pm.Status != "unknown" || pm.Column != workflowmodel.StateRelease || pm.Reason == "" {
		t.Fatalf("PR 3 (not yet released): %+v", pm)
	}
	if pm := got[9]; pm == nil || pm.Status != "unknown" || !strings.Contains(pm.Reason, "not on the observed default-branch history") {
		t.Fatalf("PR 9 (off first-parent history): %+v", pm)
	}
}

func TestObservePostMergeRunsRequiresRepo(t *testing.T) {
	if _, err := ObservePostMergeRuns(context.Background(), gh.NewClient(nil), PostMergeRequest{}); err == nil {
		t.Fatal("want an error without owner, repo and default branch")
	}
}
