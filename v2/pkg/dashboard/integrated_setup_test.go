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
	deps.Config.Project.PrimaryRepo = "owner/repository"
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
			request.Coverage != "essential" || request.Automation != "repair-pr" ||
			request.Provider != "codex" || request.VisualHiveRef != strings.Repeat("a", 40) ||
			request.ExpectedPlanSHA256 != "" {
			t.Fatalf("unexpected setup request: token=%q request=%+v", token, request)
		}
		return map[string]any{"schema_version": "test.plan.v1"}, nil
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/integrated/setup/plan", strings.NewReader(`{
		"coverage":"essential",
		"automation":"repair-pr",
		"provider":"codex",
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
	deps.Config.Project.PrimaryRepo = "owner/repository"
	deps.IntegratedSetupTokenFunc = func() (string, error) { return "saved-token", nil }
	deps.IntegratedSetupAuthorizerFunc = func(string) (string, error) { return "alice", nil }
	deps.IntegratedSetupFunc = func(context.Context, IntegratedSetupRequest, string) (map[string]any, error) {
		t.Fatal("apply callback must not run without the exact plan digest")
		return nil, nil
	}

	recorder := doPost(server, "/api/integrated/setup/apply", map[string]any{
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
	deps.Config.Project.PrimaryRepo = "owner/repository"
	deps.IntegratedSetupTokenFunc = func() (string, error) { return "saved-token", nil }
	deps.IntegratedSetupAuthorizerFunc = func(string) (string, error) { return "alice", nil }
	deps.IntegratedSetupFunc = func(context.Context, IntegratedSetupRequest, string) (map[string]any, error) {
		t.Fatal("setup callback must not run for a different dashboard actor")
		return nil, nil
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/integrated/setup/plan", strings.NewReader(`{
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

	unknown := doPost(server, "/api/integrated/setup/plan", map[string]any{
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
