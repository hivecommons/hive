package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// /api/status carries the workflow-approval counters (hivecommons/hive#16603).
func TestStatusWorkflowApprovals(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{GHClient: github.NewClient("fake", "org", []string{"repo"}, s.logger, "")}
	s.UpdateStatus(minimalPayload())
	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status?fields=workflow_approvals", nil))
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	raw, ok := payload["workflow_approvals"]
	if !ok {
		t.Fatalf("workflow_approvals missing: %s", rec.Body.String())
	}
	var got github.WorkflowApprovalsStatus
	if err := json.Unmarshal(raw, &got); err != nil || got.Approved != 0 || got.Denied != 0 || got.PermissionErrors != 0 || got.Last != nil {
		t.Fatalf("workflow_approvals=%s err=%v", raw, err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"approved", "denied", "permission_errors"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("workflow_approvals lacks %q: %s", key, raw)
		}
	}
}
