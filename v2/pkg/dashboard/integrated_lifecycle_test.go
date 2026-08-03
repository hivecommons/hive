package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doIntegratedPost(server *Server, path string, body any) *httptest.ResponseRecorder {
	data, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(data)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Hive-Role", "owner")
	request.Header.Set("X-Hive-User", "alice")
	server.mux.ServeHTTP(recorder, request)
	return recorder
}

func configureIntegratedLifecycleTestOwner(t *testing.T, server *Server, deps *Dependencies) {
	t.Helper()
	server.authToken = "dashboard-test-token"
	deps.Config.Project.Org = "owner"
	deps.Config.Project.PrimaryRepo = "repository"
	deps.IntegratedSetupTokenFunc = func() (string, error) { return "saved-owner-token", nil }
	deps.IntegratedSetupAuthorizerFunc = func(token string) (string, error) {
		if token != "saved-owner-token" {
			t.Fatalf("unexpected lifecycle token")
		}
		return "alice", nil
	}
}

func TestIntegratedLifecycleStatusUsesServerRepositoryAndSavedOwner(t *testing.T) {
	server, deps := apiServer(t)
	configureIntegratedLifecycleTestOwner(t, server, deps)
	called := false
	deps.IntegratedLifecycleFunc = func(_ context.Context, request IntegratedLifecycleRequest, token string) (map[string]any, error) {
		called = true
		if token != "saved-owner-token" || request.Repository != "owner/repository" || request.Operation != "status" {
			t.Fatalf("unexpected lifecycle request: token=%q request=%+v", token, request)
		}
		return map[string]any{"schema_version": "test.status.v1"}, nil
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/integrated/status", nil)
	request.Header.Set("X-Hive-Role", "owner")
	request.Header.Set("X-Hive-User", "Alice")
	server.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || !called || !strings.Contains(recorder.Body.String(), "test.status.v1") {
		t.Fatalf("status=%d called=%t body=%s", recorder.Code, called, recorder.Body.String())
	}
}

func TestIntegratedLifecycleRejectsViewerAndDifferentOwner(t *testing.T) {
	server, deps := apiServer(t)
	configureIntegratedLifecycleTestOwner(t, server, deps)
	deps.IntegratedLifecycleFunc = func(context.Context, IntegratedLifecycleRequest, string) (map[string]any, error) {
		t.Fatal("unauthorized lifecycle callback ran")
		return nil, nil
	}

	viewer := httptest.NewRecorder()
	viewerRequest := httptest.NewRequest(http.MethodGet, "/api/integrated/doctor", nil)
	viewerRequest.Header.Set("X-Hive-Role", "read")
	server.mux.ServeHTTP(viewer, viewerRequest)
	if viewer.Code != http.StatusForbidden {
		t.Fatalf("viewer status=%d body=%s", viewer.Code, viewer.Body.String())
	}

	mismatch := httptest.NewRecorder()
	mismatchRequest := httptest.NewRequest(http.MethodGet, "/api/integrated/doctor", nil)
	mismatchRequest.Header.Set("X-Hive-Role", "owner")
	mismatchRequest.Header.Set("X-Hive-User", "bob")
	server.mux.ServeHTTP(mismatch, mismatchRequest)
	if mismatch.Code != http.StatusForbidden || !strings.Contains(mismatch.Body.String(), "does not match") {
		t.Fatalf("mismatch status=%d body=%s", mismatch.Code, mismatch.Body.String())
	}
}

func TestIntegratedLifecycleRejectsUnauthenticatedOpenDashboard(t *testing.T) {
	server, deps := apiServer(t)
	deps.Config.Project.PrimaryRepo = "owner/repository"
	deps.IntegratedSetupTokenFunc = func() (string, error) { return "saved-owner-token", nil }
	deps.IntegratedSetupAuthorizerFunc = func(string) (string, error) { return "alice", nil }
	deps.IntegratedLifecycleFunc = func(context.Context, IntegratedLifecycleRequest, string) (map[string]any, error) {
		t.Fatal("unauthenticated lifecycle callback ran")
		return nil, nil
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/integrated/status", nil)
	request.Header.Set("X-Hive-Role", "owner")
	request.Header.Set("X-Hive-User", "alice")
	server.mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || !strings.Contains(recorder.Body.String(), "authenticated dashboard") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestIntegratedControlPlanAndApplyUseClosedTypedContract(t *testing.T) {
	server, deps := apiServer(t)
	configureIntegratedLifecycleTestOwner(t, server, deps)
	var requests []IntegratedLifecycleRequest
	deps.IntegratedLifecycleFunc = func(_ context.Context, request IntegratedLifecycleRequest, token string) (map[string]any, error) {
		requests = append(requests, request)
		return map[string]any{"schema_version": "test.control.v1"}, nil
	}

	plan := doIntegratedPost(server, "/api/integrated/control/plan", map[string]any{
		"operation":  "trigger",
		"request_id": "proof-cycle-a-001",
	})
	if plan.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", plan.Code, plan.Body.String())
	}
	apply := doIntegratedPost(server, "/api/integrated/control/apply", map[string]any{
		"operation":            "trigger",
		"request_id":           "proof-cycle-a-001",
		"expected_plan_sha256": strings.Repeat("a", 64),
	})
	if apply.Code != http.StatusOK || len(requests) != 2 ||
		requests[0].Operation != "control-plan" || requests[0].Action != "trigger" ||
		requests[1].Operation != "control-apply" || requests[1].ExpectedPlanSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("apply status=%d requests=%+v body=%s", apply.Code, requests, apply.Body.String())
	}
	retire := doIntegratedPost(server, "/api/integrated/control/plan", map[string]any{
		"operation":  "repair-retire",
		"request_id": "proof-cycle-a-retire-001",
	})
	if retire.Code != http.StatusOK || len(requests) != 3 || requests[2].Action != "repair-retire" {
		t.Fatalf("repair-retire status=%d requests=%+v body=%s", retire.Code, requests, retire.Body.String())
	}

	unknown := doIntegratedPost(server, "/api/integrated/control/plan", map[string]any{
		"operation":  "shell",
		"request_id": "proof-cycle-a-002",
		"command":    "rm -rf /",
	})
	if unknown.Code != http.StatusBadRequest || !strings.Contains(unknown.Body.String(), "unknown field") {
		t.Fatalf("unknown status=%d body=%s", unknown.Code, unknown.Body.String())
	}
}

func TestIntegratedBaselineApprovalRequiresEveryExactBinding(t *testing.T) {
	server, deps := apiServer(t)
	configureIntegratedLifecycleTestOwner(t, server, deps)
	called := false
	deps.IntegratedLifecycleFunc = func(_ context.Context, request IntegratedLifecycleRequest, _ string) (map[string]any, error) {
		called = true
		if request.Operation != "baseline-approve" || request.Baseline == nil || request.Baseline.PRNumber != 17 {
			t.Fatalf("unexpected baseline request: %+v", request)
		}
		return map[string]any{"schema_version": "test.baseline.v1"}, nil
	}
	valid := map[string]any{
		"request_id":       "baseline-cycle-a-001",
		"repository_id":    "123",
		"capture_run_id":   41,
		"artifact_id":      42,
		"pr_number":        17,
		"head_sha":         strings.Repeat("a", 40),
		"base_sha":         strings.Repeat("b", 40),
		"diff_digest":      strings.Repeat("c", 64),
		"candidate_digest": strings.Repeat("d", 64),
		"actor_id":         99,
		"plan_digest":      strings.Repeat("e", 64),
		"reason":           "Reviewed every exact candidate PNG at original resolution.",
	}
	recorder := doIntegratedPost(server, "/api/integrated/baseline/approve", valid)
	if recorder.Code != http.StatusOK || !called {
		t.Fatalf("approve status=%d called=%t body=%s", recorder.Code, called, recorder.Body.String())
	}
	delete(valid, "candidate_digest")
	missing := doIntegratedPost(server, "/api/integrated/baseline/approve", valid)
	if missing.Code != http.StatusBadRequest || !strings.Contains(missing.Body.String(), "candidate_digest") {
		t.Fatalf("missing status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestIntegratedLifecycleScrubsSavedTokenFromErrors(t *testing.T) {
	server, deps := apiServer(t)
	configureIntegratedLifecycleTestOwner(t, server, deps)
	deps.IntegratedLifecycleFunc = func(context.Context, IntegratedLifecycleRequest, string) (map[string]any, error) {
		return nil, &testIntegratedLifecycleError{message: "provider rejected token saved-owner-token and sk-proj-secretvalue"}
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/integrated/status", nil)
	request.Header.Set("X-Hive-Role", "owner")
	request.Header.Set("X-Hive-User", "alice")
	server.mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict || strings.Contains(recorder.Body.String(), "saved-owner-token") || strings.Contains(recorder.Body.String(), "secretvalue") {
		t.Fatalf("scrub status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

type testIntegratedLifecycleError struct{ message string }

func (err *testIntegratedLifecycleError) Error() string { return err.message }
