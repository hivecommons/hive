package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIntegratedSetupPlanUsesExactHiveRepositoryAndSavedOwner(t *testing.T) {
	server, deps := apiServer(t)
	server.authToken = "dashboard-test-token"
	deps.Config.Project.Org = "owner"
	deps.Config.Project.PrimaryRepo = "repository"
	deps.IntegratedSetupTokenFunc = func() (string, error) { return "saved-token", nil }
	deps.IntegratedSetupAuthorizerFunc = func(token string) (string, error) {
		if token != "saved-token" {
			t.Fatalf("setup callback received unexpected token")
		}
		return "alice", nil
	}
	called := false
	deps.IntegratedSetupFunc = func(_ context.Context, request IntegratedSetupRequest, token string) (map[string]any, error) {
		called = true
		if token != "saved-token" || request.Repository != "owner/repository" ||
			request.RequestID != "setup-cycle-a-001" ||
			request.Coverage != "essential" || request.Automation != "repair-pr" ||
			request.Provider != "codex" || request.VisualHiveRef != strings.Repeat("a", 40) ||
			request.MaxActiveIssues == nil || *request.MaxActiveIssues != 3 ||
			request.ExpectedPlanSHA256 != "" {
			t.Fatalf("unexpected setup request: token=%q request=%+v", token, request)
		}
		return map[string]any{"schema_version": "test.plan.v1"}, nil
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/integrated/setup/plan", strings.NewReader(`{
		"request_id":"setup-cycle-a-001",
		"coverage":"essential",
		"automation":"repair-pr",
		"provider":"codex",
		"max_active_issues":3,
		"visual_hive_ref":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Hive-Role", "owner")
	request.Header.Set("X-Hive-User", "Alice")
	server.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || !called {
		t.Fatalf("plan status=%d called=%t body=%s", recorder.Code, called, recorder.Body.String())
	}
}

func TestIntegratedSetupApplyRequiresExactPlanDigest(t *testing.T) {
	server, deps := apiServer(t)
	server.authToken = "dashboard-test-token"
	deps.Config.Project.PrimaryRepo = "owner/repository"
	deps.IntegratedSetupTokenFunc = func() (string, error) { return "saved-token", nil }
	deps.IntegratedSetupAuthorizerFunc = func(string) (string, error) { return "alice", nil }
	deps.IntegratedSetupFunc = func(context.Context, IntegratedSetupRequest, string) (map[string]any, error) {
		t.Fatal("apply callback must not run without the exact plan digest")
		return nil, nil
	}

	recorder := doIntegratedPost(server, "/api/integrated/setup/apply", map[string]any{
		"request_id":      "setup-cycle-a-002",
		"coverage":        "essential",
		"automation":      "repair-pr",
		"provider":        "codex",
		"visual_hive_ref": strings.Repeat("a", 40),
	})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "expected_plan_sha256") {
		t.Fatalf("apply status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestIntegratedSetupRejectsDifferentDashboardActor(t *testing.T) {
	server, deps := apiServer(t)
	server.authToken = "dashboard-test-token"
	deps.Config.Project.PrimaryRepo = "owner/repository"
	deps.IntegratedSetupTokenFunc = func() (string, error) { return "saved-token", nil }
	deps.IntegratedSetupAuthorizerFunc = func(string) (string, error) { return "alice", nil }
	deps.IntegratedSetupFunc = func(context.Context, IntegratedSetupRequest, string) (map[string]any, error) {
		t.Fatal("setup callback must not run for a different dashboard actor")
		return nil, nil
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/integrated/setup/plan", strings.NewReader(`{
		"request_id":"setup-cycle-a-003",
		"coverage":"essential",
		"automation":"repair-pr",
		"provider":"codex",
		"visual_hive_ref":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Hive-Role", "owner")
	request.Header.Set("X-Hive-User", "bob")
	server.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "does not match") {
		t.Fatalf("mismatch status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestIntegratedSetupRejectsReadOnlyViewerAndUnknownFields(t *testing.T) {
	server, deps := apiServer(t)
	server.authToken = "dashboard-test-token"
	deps.Config.Project.PrimaryRepo = "owner/repository"
	deps.IntegratedSetupTokenFunc = func() (string, error) { return "saved-token", nil }
	deps.IntegratedSetupAuthorizerFunc = func(string) (string, error) { return "alice", nil }
	deps.IntegratedSetupFunc = func(context.Context, IntegratedSetupRequest, string) (map[string]any, error) {
		t.Fatal("setup callback must not run")
		return nil, nil
	}

	viewer := httptest.NewRecorder()
	viewerRequest := httptest.NewRequest(http.MethodPost, "/api/integrated/setup/plan", strings.NewReader(`{}`))
	viewerRequest.Header.Set("X-Hive-Role", "read")
	server.mux.ServeHTTP(viewer, viewerRequest)
	if viewer.Code != http.StatusForbidden {
		t.Fatalf("viewer status=%d body=%s", viewer.Code, viewer.Body.String())
	}

	unknown := doIntegratedPost(server, "/api/integrated/setup/plan", map[string]any{
		"request_id":      "setup-cycle-a-004",
		"coverage":        "essential",
		"automation":      "repair-pr",
		"provider":        "codex",
		"visual_hive_ref": strings.Repeat("a", 40),
		"repository":      "foreign/repository",
	})
	if unknown.Code != http.StatusBadRequest || !strings.Contains(unknown.Body.String(), "unknown field") {
		t.Fatalf("unknown-field status=%d body=%s", unknown.Code, unknown.Body.String())
	}
}

func TestIntegratedSetupRejectsInvalidActiveIssueLimit(t *testing.T) {
	server, deps := apiServer(t)
	server.authToken = "dashboard-test-token"
	deps.Config.Project.PrimaryRepo = "owner/repository"
	deps.IntegratedSetupTokenFunc = func() (string, error) { return "saved-token", nil }
	deps.IntegratedSetupAuthorizerFunc = func(string) (string, error) { return "alice", nil }
	deps.IntegratedSetupFunc = func(context.Context, IntegratedSetupRequest, string) (map[string]any, error) {
		t.Fatal("setup callback must not run for an invalid active issue limit")
		return nil, nil
	}

	for _, limit := range []int{0, 101} {
		recorder := doIntegratedPost(server, "/api/integrated/setup/plan", map[string]any{
			"request_id":        "setup-cycle-a-006",
			"coverage":          "essential",
			"automation":        "repair-pr",
			"provider":          "codex",
			"visual_hive_ref":   strings.Repeat("a", 40),
			"max_active_issues": limit,
		})
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "max_active_issues") {
			t.Fatalf("limit=%d status=%d body=%s", limit, recorder.Code, recorder.Body.String())
		}
	}
}

func TestIntegratedSetupScrubsSavedTokenFromErrors(t *testing.T) {
	server, deps := apiServer(t)
	server.authToken = "dashboard-test-token"
	deps.Config.Project.PrimaryRepo = "owner/repository"
	deps.IntegratedSetupTokenFunc = func() (string, error) { return "saved-setup-token", nil }
	deps.IntegratedSetupAuthorizerFunc = func(string) (string, error) { return "alice", nil }
	deps.IntegratedSetupFunc = func(context.Context, IntegratedSetupRequest, string) (map[string]any, error) {
		return nil, &testIntegratedLifecycleError{message: "provider rejected token saved-setup-token and sk-proj-secretvalue"}
	}

	recorder := doIntegratedPost(server, "/api/integrated/setup/plan", map[string]any{
		"request_id":      "setup-cycle-a-005",
		"coverage":        "essential",
		"automation":      "repair-pr",
		"provider":        "codex",
		"visual_hive_ref": strings.Repeat("a", 40),
	})
	if recorder.Code != http.StatusConflict ||
		strings.Contains(recorder.Body.String(), "saved-setup-token") ||
		strings.Contains(recorder.Body.String(), "secretvalue") {
		t.Fatalf("scrub status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
