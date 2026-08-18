package dashboard

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/github"
)

func TestIntegratedPreflightUsesOwnerBoundRepository(t *testing.T) {
	server, deps := apiServer(t)
	server.authToken = "dashboard-test-token"
	deps.Config.Project.PrimaryRepo = "owner/repository"
	configureIntegratedOwnerIdentity(deps, "alice")
	called := false
	deps.IntegratedPreflightFunc = func(_ context.Context, request IntegratedPreflightRequest, operator github.AuthenticatedUserIdentity) (map[string]any, error) {
		called = true
		if operator.ID != 101 || !strings.EqualFold(operator.Login, "alice") || request.Repository != "owner/repository" || request.RequestID != "preflight-cycle-a-001" ||
			request.Provider != "codex" || request.VisualHiveRef != strings.Repeat("a", 40) || request.Coverage != "comprehensive" ||
			request.Automation != "repair-pr" || request.MaxActiveIssues == nil || *request.MaxActiveIssues != 3 {
			t.Fatalf("unexpected preflight request: operator=%+v request=%+v", operator, request)
		}
		return map[string]any{"schema_version": "test.preflight.v1", "ready": true}, nil
	}
	recorder := doIntegratedPost(server, "/api/integrated/preflight", map[string]any{
		"request_id": "preflight-cycle-a-001", "provider": "codex", "visual_hive_ref": strings.Repeat("a", 40),
		"coverage": "comprehensive", "automation": "repair-pr", "max_active_issues": 3,
	})
	if recorder.Code != http.StatusOK || !called || !strings.Contains(recorder.Body.String(), `"ready":true`) {
		t.Fatalf("preflight status=%d called=%t body=%s", recorder.Code, called, recorder.Body.String())
	}
}

func TestIntegratedPreflightRejectsViewerUnknownFieldsAndInvalidBinding(t *testing.T) {
	server, deps := apiServer(t)
	server.authToken = "dashboard-test-token"
	deps.Config.Project.PrimaryRepo = "owner/repository"
	configureIntegratedOwnerIdentity(deps, "alice")
	deps.IntegratedPreflightFunc = func(context.Context, IntegratedPreflightRequest, github.AuthenticatedUserIdentity) (map[string]any, error) {
		t.Fatal("invalid preflight must not reach the callback")
		return nil, nil
	}

	viewer := doIntegratedPost(server, "/api/integrated/preflight", map[string]any{})
	if viewer.Code != http.StatusBadRequest {
		t.Fatalf("empty preflight status=%d body=%s", viewer.Code, viewer.Body.String())
	}
	unknown := doIntegratedPost(server, "/api/integrated/preflight", map[string]any{
		"request_id": "preflight-cycle-a-002", "provider": "codex", "visual_hive_ref": strings.Repeat("a", 40),
		"coverage": "essential", "automation": "repair-pr", "repository": "foreign/repository",
	})
	if unknown.Code != http.StatusBadRequest || !strings.Contains(unknown.Body.String(), "unknown field") {
		t.Fatalf("unknown-field status=%d body=%s", unknown.Code, unknown.Body.String())
	}
	invalid := doIntegratedPost(server, "/api/integrated/preflight", map[string]any{
		"request_id": "preflight-cycle-a-003", "provider": "codex", "visual_hive_ref": "floating",
		"coverage": "essential", "automation": "repair-pr",
	})
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), "visual_hive_ref") {
		t.Fatalf("invalid-ref status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}
