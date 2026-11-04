package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestStageTrackerWorkflowApprovalCaption(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name       string
		pending    int
		permission bool
		caption    string
	}{
		{"one run", 1, false, "CI waiting · 1 workflow run needs approval on GitHub"},
		{"several runs", 3, false, "CI waiting · 3 workflow runs need approval on GitHub"},
		{"refused", 3, true, "CI waiting · needs workflow approval on GitHub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{
				Head: "head", Source: "github-check-runs", ObservedAt: &now,
				WorkflowApproval: &github.PRWorkflowApproval{Pending: tc.pending, PermissionDenied: tc.permission, ObservedAt: &now},
			}}
			got := stageTrackerCIActivity(pr, now)
			if got.Caption != tc.caption || got.State != "missing" || got.Active {
				t.Fatalf("activity=%+v", got)
			}
			if tc.permission && !strings.Contains(got.Detail, "actions: write") {
				t.Fatalf("permission requirement missing: %s", got.Detail)
			}
			pr.CIActivity.Head = "oldhead"
			if got := stageTrackerCIActivity(pr, now); got.State != "unknown" || strings.Contains(got.Caption, "workflow approval") {
				t.Fatalf("stale refusal leaked: %+v", got)
			}
		})
	}
}

func TestStageTrackerWorkflowApprovalDoesNotHideRequiredFailure(t *testing.T) {
	now := time.Now().UTC()
	pr := github.PullRequest{HeadSHA: "head", CIActivity: &github.PRCIActivity{
		Head: "head", Source: "github-check-runs", ObservedAt: &now,
		RequiredKnown: true, RequiredSource: "protection", Required: []string{"build", "test"},
		RequiredChecks: []github.PRRequiredCheck{{Name: "build", AppKnown: true}, {Name: "test", AppKnown: true}},
		StatusesKnown:  true, Checks: []github.PRActivityCheck{{Name: "build", State: "completed", Conclusion: "failure"}},
		WorkflowApproval: &github.PRWorkflowApproval{Pending: 2, PermissionDenied: true},
	}}
	if got := stageTrackerCIActivity(pr, now); got.State != "failing" || got.ChecksFailed != 1 {
		t.Fatalf("workflow approval hid a red required check: %+v", got)
	}
}

func TestStatusWorkflowApprovals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/repo/actions/runs":
			_, _ = w.Write([]byte(`{"workflow_runs":[{"id":11,"head_sha":"head","event":"pull_request","status":"action_required","head_repository":{"full_name":"org/repo"}}]}`))
		case "/repos/org/repo/pulls/7":
			_, _ = w.Write([]byte(`{"number":7,"state":"open","user":{"login":"dependabot[bot]"},"head":{"sha":"head","repo":{"full_name":"org/repo"}}}`))
		case "/repos/org/repo/actions/runs/11/approve":
			if r.Method != http.MethodPost {
				t.Errorf("approval method=%s", r.Method)
			}
			http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := newTestServer()
	c := github.NewClient("fake", "org", []string{"repo"}, s.logger, "")
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	c.GoGitHub().BaseURL = base
	c.SweepWorkflowApprovals(context.Background(), []github.PullRequest{{Repo: "org/repo", Number: 7, HeadSHA: "head", CIStatus: "pending"}}, config.GovernorCIConfig{})
	s.deps = &Dependencies{GHClient: c}
	s.UpdateStatus(minimalPayload())
	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status?fields=workflow_approvals", nil))
	var payload struct {
		Approvals github.WorkflowApprovalsStatus `json:"workflow_approvals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Approvals.PermissionErrors != 1 || payload.Approvals.Last == nil || payload.Approvals.Last.RunID != 11 {
		t.Fatalf("status=%s", rec.Body.String())
	}
}
