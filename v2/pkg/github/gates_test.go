package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInspectPullRequestGateAndMergeExactSHA(t *testing.T) {
	mergeSHASeen := ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7":
			_, _ = io.WriteString(writer, `{"number":7,"state":"open","draft":false,"html_url":"https://example.test/pull/7","mergeable":true,"mergeable_state":"clean","head":{"sha":"abc"},"base":{"ref":"main"},"labels":[]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls/7/files":
			_, _ = io.WriteString(writer, `[{"filename":"tests/widget.test.ts"}]`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":["Visual Hive PR","unit"]},"required_pull_request_reviews":{"required_approving_review_count":0}}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/commits/abc/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":2,"check_runs":[{"name":"Visual Hive PR","head_sha":"abc","status":"completed","conclusion":"success","html_url":"https://example.test/run/1"},{"name":"unit","head_sha":"abc","status":"completed","conclusion":"success"}]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/commits/abc/status":
			_, _ = io.WriteString(writer, `{"state":"success","statuses":[]}`)
		case request.Method == http.MethodPut && request.URL.Path == "/repos/owner/repo/pulls/7/merge":
			var body struct {
				SHA string `json:"sha"`
			}
			_ = json.NewDecoder(request.Body).Decode(&body)
			mergeSHASeen = body.SHA
			_, _ = io.WriteString(writer, `{"merged":true,"sha":"merge-sha","message":"merged"}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if gate.HeadSHA != "abc" || !gate.VisualHiveVerdictGreen || !gate.BranchProtectionEnabled || !gate.Mergeable {
		t.Fatalf("unexpected gate: %+v", gate)
	}
	if len(gate.RequiredCheckStates) != 2 || gate.RequiredCheckStates[0] != "success" || gate.RequiredCheckStates[1] != "success" {
		t.Fatalf("required checks = %v", gate.RequiredCheckStates)
	}
	mergeSHA, err := client.MergePullRequestExact(context.Background(), "owner/repo", 7, gate.HeadSHA)
	if err != nil || mergeSHA != "merge-sha" || mergeSHASeen != "abc" {
		t.Fatalf("merge = %q expected=%q err=%v", mergeSHA, mergeSHASeen, err)
	}
}

func TestInspectPullRequestGateKeepsUnsafeAndPendingSignals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo/pulls/8":
			_, _ = io.WriteString(writer, `{"number":8,"state":"open","draft":true,"mergeable":false,"head":{"sha":"def"},"base":{"ref":"main"},"labels":[{"name":"hold"}]}`)
		case "/repos/owner/repo/pulls/8/files":
			_, _ = io.WriteString(writer, `[{"filename":".github/workflows/release.yml"},{"filename":"tests/__screenshots__/home.png"},{"filename":"src/auth/session.ts"},{"filename":"deploy/app.yaml"}]`)
		case "/repos/owner/repo/branches/main/protection":
			_, _ = io.WriteString(writer, `{"required_status_checks":{"strict":true,"contexts":["Visual Hive PR"]}}`)
		case "/repos/owner/repo/commits/def/check-runs":
			_, _ = io.WriteString(writer, `{"total_count":1,"check_runs":[{"name":"Visual Hive PR","head_sha":"def","status":"in_progress"}]}`)
		case "/repos/owner/repo/commits/def/status":
			_, _ = io.WriteString(writer, `{"state":"pending","statuses":[]}`)
		default:
			http.Error(writer, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	gate, err := client.InspectPullRequestGate(context.Background(), "owner/repo", 8)
	if err != nil {
		t.Fatal(err)
	}
	if !gate.Hold || gate.VisualHiveVerdictGreen || gate.RequiredCheckStates[0] != "pending" || !gate.WorkflowChanged || !gate.BaselineChanged || !gate.SecuritySensitive || !gate.DeploymentChanged {
		t.Fatalf("unsafe signals were lost: %+v", gate)
	}
}
