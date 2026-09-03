package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"time"

	"github.com/kubestellar/hive/pkg/agent"
	"github.com/kubestellar/hive/pkg/config"
	ghpkg "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/governor"
)

// --- api_packs.go ---

func TestHandlePacksList(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/packs", nil)
	w := httptest.NewRecorder()
	srv.handlePacksList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
	var result []map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	if len(result) == 0 {
		t.Error("expected at least one pack")
	}
}

func TestHandlePackApply_InvalidLevel(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/packs/abc/apply", nil)
	req.SetPathValue("level", "abc")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackApply(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestHandlePackSetLevel_InvalidBody(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestHandlePackSetLevel_OutOfRange(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":99}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestHandlePackSetLevel_Valid(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/packs/level", strings.NewReader(`{"level":1}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handlePackSetLevel(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
}

func TestDetectCurrentLevel_WithLevel(t *testing.T) {
	level := 3
	cfg := &config.Config{ACMMLevel: &level}
	got := detectACMMLevel(cfg)
	if got != 3 {
		t.Errorf("got %d, want 3", got)
	}
}

func TestDetectCurrentLevel_NoLevel(t *testing.T) {
	cfg := &config.Config{}
	got := detectACMMLevel(cfg)
	if got != 1 {
		t.Errorf("got %d, want 1 (default)", got)
	}
}

func TestApplyPack_Level1(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()

	result, err := srv.ApplyPack(1)
	if err != nil {
		t.Fatalf("ApplyPack: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Name == "" {
		t.Error("expected non-empty pack name")
	}
}

func TestApplyPack_InvalidLevel(t *testing.T) {
	srv := newFullServer(t)
	_, err := srv.ApplyPack(999)
	if err == nil {
		t.Error("expected error for invalid level")
	}
}

func TestApplyPack_NoAgentsDir(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = ""
	_, err := srv.ApplyPack(1)
	if err == nil {
		t.Error("expected error when agents_dir not configured")
	}
}

func TestSyncAgentVisibility(t *testing.T) {
	srv := newFullServer(t)
	paused, resumed := srv.syncAgentVisibility(1)
	// With only scanner agent, it may be paused or not depending on pack
	_ = paused
	_ = resumed
}

// --- ContributorSummary ---

func TestContributorSummary(t *testing.T) {
	srv := newFullServer(t)
	reg, active := srv.ContributorSummary()
	// No contributors registered in test setup
	_ = reg
	if active != 0 {
		t.Errorf("active = %d, want 0 (no contribute hub)", active)
	}
}

// --- handleContributeReissueToken ---

func TestHandleContributeReissueToken_NoAuth(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/contribute/reissue-token", nil)
	w := httptest.NewRecorder()
	srv.handleContributeReissueToken(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", w.Code)
	}
}

func TestHandleContributeReissueToken_InvalidToken(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/contribute/reissue-token", nil)
	req.Header.Set("Authorization", "Bearer fake-invalid-token")
	w := httptest.NewRecorder()
	srv.handleContributeReissueToken(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", w.Code)
	}
}

// --- handleContributeStatus ---

func TestHandleContributeStatus(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/contribute/status", nil)
	w := httptest.NewRecorder()
	srv.handleContributeStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- handleContributeActivity ---

func TestHandleContributeActivity_NoHub(t *testing.T) {
	srv := newFullServer(t)
	srv.contributeHub = nil
	req := httptest.NewRequest("GET", "/api/contribute/activity", nil)
	w := httptest.NewRecorder()
	srv.handleContributeActivity(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- handleContributorsList ---

func TestHandleContributorsList_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/contribute/list", nil)
	w := httptest.NewRecorder()
	srv.handleContributorsList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- audit.go loadFromDisk ---

func TestAuditLog_LoadFromDisk_WithData(t *testing.T) {
	a := &AuditLog{ring: make([]AuditEntry, 0, auditRingCap)}

	// The loadFromDisk function reads from hardcoded auditLogPath (/data/audit.jsonl).
	// We can't override that, but we can test the logic by loading manually
	// and verifying the ring buffer works.
	for i := 0; i < 10; i++ {
		a.Log("user", "action", "detail", "agent")
	}
	recent := a.Recent(5)
	if len(recent) != 5 {
		t.Errorf("recent = %d, want 5", len(recent))
	}
}

func TestAuditLog_RecentZero(t *testing.T) {
	a := &AuditLog{ring: make([]AuditEntry, 0, auditRingCap)}
	a.Log("u", "a", "", "")
	recent := a.Recent(0)
	if len(recent) != 1 {
		t.Errorf("Recent(0) should return all, got %d", len(recent))
	}
}

func TestAuditLog_RecentOverflow(t *testing.T) {
	a := &AuditLog{ring: make([]AuditEntry, 0, auditRingCap)}
	a.Log("u", "a", "", "")
	recent := a.Recent(999)
	if len(recent) != 1 {
		t.Errorf("Recent(999) should return all (1), got %d", len(recent))
	}
}

// --- handleAgentConfigRestrictions with data ---

func TestHandleAgentConfigRestrictions_ValidAgent(t *testing.T) {
	srv := newFullServer(t)
	body := `{"restrictions":["no-force-push"]}`
	req := httptest.NewRequest("PUT", "/api/agents/scanner/restrictions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "scanner")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleAgentConfigRestrictions(w, req)

	// Should not be 404
	if w.Code == http.StatusNotFound {
		t.Error("expected agent to be found")
	}
}

// --- handleGovernorLogging ---

func TestHandleGovernorLogging_SetLevel(t *testing.T) {
	srv := newFullServer(t)
	body := `{"level":"debug"}`
	req := httptest.NewRequest("PUT", "/api/config/governor/logging", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorLogging(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestHandleGovernorLogging_SetSize(t *testing.T) {
	srv := newFullServer(t)
	body := `{"maxSizeMB":10,"maxAgeDays":30,"maxBackups":5,"compress":true}`
	req := httptest.NewRequest("PUT", "/api/config/governor/logging", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorLogging(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, body=%s", w.Code, w.Body.String())
	}
}

// --- handleGovernorAddAgent ---

func TestHandleGovernorAddAgent_ValidAgent(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Data.AgentsDir = t.TempDir()
	body := `{"name":"new-agent","backend":"claude","model":"sonnet","role":"worker"}`
	req := httptest.NewRequest("POST", "/api/governor/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorAddAgent(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200, body = %s", w.Code, w.Body.String())
	}
}

// --- handleGovernorRemoveAgent ---

func TestHandleGovernorRemoveAgent_NotFound_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("DELETE", "/api/governor/agents/nonexistent", nil)
	req.SetPathValue("name", "nonexistent")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRemoveAgent(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

// --- handleChat ---

func TestHandleChat_NilNous(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Nous = nil
	body := `{"message":"hello"}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	// Should handle nil nous gracefully
	if w.Code == 0 {
		t.Error("unexpected zero status code")
	}
}

// --- handleNousGatePending ---

func TestHandleNousGatePending_NoNous(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Nous = nil
	req := httptest.NewRequest("GET", "/api/nous/gate/pending", nil)
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleNousGatePending(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- handleNousGateRespond ---

func TestHandleNousGateRespond_Valid(t *testing.T) {
	srv := newFullServer(t)
	body := `{"approved":true}`
	req := httptest.NewRequest("POST", "/api/nous/gate/respond", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleNousGateRespond(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

func TestHandleNousGateRespond_InvalidBody_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/nous/gate/respond", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleNousGateRespond(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

// --- handleNousGateResponse ---

func TestHandleNousGateResponse_NoNous(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Nous = nil
	req := httptest.NewRequest("GET", "/api/nous/gate/response", nil)
	w := httptest.NewRecorder()
	srv.handleNousGateResponse(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- handleKick with valid agent ---

func TestHandleKick_ValidAgent(t *testing.T) {
	srv := newFullServer(t)
	body := `{"message":"test kick"}`
	req := httptest.NewRequest("POST", "/api/kick/scanner", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("agent", "scanner")
	w := httptest.NewRecorder()
	srv.handleKick(w, req)

	if w.Code == 0 {
		t.Error("unexpected zero status code")
	}
}

func TestHandleKick_EmptyBody(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/kick/scanner", strings.NewReader(""))
	req.SetPathValue("agent", "scanner")
	w := httptest.NewRecorder()
	srv.handleKick(w, req)

	// Should still work with auto-generated message
	if w.Code == 0 {
		t.Error("unexpected zero status code")
	}
}

// --- handleRestart edge cases ---

func TestHandleRestart_ScannerAgent(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/restart/scanner", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("agent", "scanner")
	w := httptest.NewRecorder()
	srv.handleRestart(w, req)

	if w.Code >= 500 {
		t.Errorf("unexpected 5xx: %d", w.Code)
	}
}

// --- handleGHUserAuthStart with client ID ---

func TestHandleGHUserAuthStart_WithClientID(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.GitHub.OAuthClientID = "test-client-id"
	req := httptest.NewRequest("POST", "/api/gh-user-auth/start", nil)
	w := httptest.NewRecorder()
	srv.handleGHUserAuthStart(w, req)

	// Will fail because the device flow endpoint won't respond, but
	// should exercise the code past the client ID check
	if w.Code == http.StatusBadRequest {
		t.Error("should not get 400 when client ID is set")
	}
}

// --- handleGHUserAuthPoll with active flow ---

func TestHandleGHUserAuthPoll_ActiveFlow(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.GitHub.OAuthClientID = "test-client-id"
	// Simulate an active flow
	srv.deviceFlowMu.Lock()
	srv.deviceFlowState = &ghpkg.DeviceFlowState{
		DeviceCode: "test-device-code",
	}
	srv.deviceFlowMu.Unlock()

	req := httptest.NewRequest("POST", "/api/gh-user-auth/poll", nil)
	w := httptest.NewRecorder()
	srv.handleGHUserAuthPoll(w, req)

	// Will fail on the poll since no real GitHub endpoint, but exercises the code
	if w.Code == http.StatusBadRequest {
		t.Error("should not get 400 when flow is active")
	}
}

// --- loadSidebarFromDisk / saveSidebarToDisk ---

func TestSaveSidebarToDisk(t *testing.T) {
	srv := newFullServer(t)
	// saveSidebarToDisk writes to hardcoded path, just verify no panic
	srv.saveSidebarToDisk(map[string]any{"test": true})
}

// --- ContributeWSHub checkModelAllowed ---

func TestCheckModelAllowed_UnknownModelAllowed(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Hub.ContributeAllowModels = []string{"sonnet*"}
	srv.deps.Config.Hub.ContributeRejectUnknownModels = false
	hub := &ContributeWSHub{server: srv, logger: slog.Default()}
	ok, _ := hub.checkModelAllowed("unknown-model")
	if !ok {
		t.Error("unknown model should be allowed when reject disabled")
	}
}

// --- handleObsidianSync ---

func TestHandleObsidianSync_NoBody(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/obsidian/sync", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleObsidianSync(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

// --- securityHeaders ---

func TestSecurityHeaders(t *testing.T) {
	srv := newFullServer(t)
	handler := srv.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("missing X-Frame-Options")
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options")
	}
}

// --- securityHeaders with auth token ---

func TestSecurityHeaders_WithAuthToken(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := NewServer(0, logger)
	srv.authToken = "secret-token"

	// Authentication now lives in the authenticate middleware (moved out of
	// securityHeaders so it runs before roleEnforcement); test it there.
	handler := srv.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// No auth header - should be rejected
	req := httptest.NewRequest("GET", "/api/kick/scanner", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no auth: code = %d, want 401", w.Code)
	}

	// Valid auth header
	req = httptest.NewRequest("GET", "/api/kick/scanner", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("valid auth: code = %d, want 200", w.Code)
	}

	// Health endpoint should be exempt
	req = httptest.NewRequest("GET", "/api/health", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("health: code = %d, want 200", w.Code)
	}

	// X-Hive-Internal header
	req = httptest.NewRequest("GET", "/api/kick/scanner", nil)
	req.Header.Set("X-Hive-Internal", "secret-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("internal token: code = %d, want 200", w.Code)
	}

	// Hub auth headers. F7: the fail-closed default requires the hub's proof
	// header (X-Hive-Proxy-Auth) alongside the identity headers; the real hub
	// injects it. With a valid proof the request is trusted.
	req = httptest.NewRequest("GET", "/api/kick/scanner", nil)
	req.Header.Set("X-Hive-User", "testuser")
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(proxyAuthHeader, "secret-token")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("hub auth: code = %d, want 200", w.Code)
	}
}

// --- handleHealth ---

func TestHandleHealth_Ready(t *testing.T) {
	srv := newFullServer(t)
	srv.MarkReady()
	srv.UpdateStatus(minimalPayload())

	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	srv.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", w.Code)
	}
}

func TestHandleHealth_NotReady(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/health", nil)
	w := httptest.NewRecorder()
	srv.handleHealth(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", w.Code)
	}
}

// --- BuildContributorPoolStatus ---

func TestBuildContributorPoolStatus(t *testing.T) {
	srv := newFullServer(t)
	status := srv.BuildContributorPoolStatus()
	_ = status // Just verify it doesn't panic
}

// --- handleAgentConfigGeneral with valid body ---

func TestHandleAgentConfigGeneral_SetEnabled(t *testing.T) {
	srv := newFullServer(t)
	body := `{"enabled":false}`
	req := httptest.NewRequest("PUT", "/api/agents/scanner/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "scanner")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleAgentConfigGeneral(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d, want 200, body=%s", w.Code, w.Body.String())
	}
}

// --- handleAgentPause / handleAgentResume ---

func TestHandlePause_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/pause/scanner", strings.NewReader(`{"reason":"testing"}`))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	req.SetPathValue("agent", "scanner")
	w := httptest.NewRecorder()
	srv.handlePause(w, req)

	// May succeed or fail depending on agent state
	if w.Code >= 500 {
		t.Errorf("server error: %d", w.Code)
	}
}

// --- handleAgentImport ---

func TestHandleAgentImport_InvalidBody_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/agents/import", strings.NewReader("not yaml"))
	req.Header.Set("Content-Type", "application/json")
	// Import is owner-only (audit F16). This test asserts body validation, not
	// authorization, so give it credentials and keep asserting 400.
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleAgentImport(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

// --- buildInferenceBackends ---

func TestBuildInferenceBackends(t *testing.T) {
	srv := newFullServer(t)
	t.Setenv("HIVE_VLLM_MODELS", "")
	t.Setenv("HIVE_LLMD_MODELS", "")
	backends := srv.buildInferenceBackends()
	if len(backends) != 2 {
		t.Errorf("expected 2 backends, got %d", len(backends))
	}
}

// --- handleGitSourcesList ---

func TestHandleGitSourcesList(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/git-sources", nil)
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- handleGitSourcesConnect ---

func TestHandleGitSourcesConnect_InvalidBody(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/git-sources/connect", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesConnect(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

// --- handleGitSourcesDisconnect ---

func TestHandleGitSourcesDisconnect_NoKnowledge(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Knowledge = nil
	req := httptest.NewRequest("POST", "/api/git-sources/disconnect", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGitSourcesDisconnect(w, req)

	// Without knowledge, should fail with service unavailable or bad request
	if w.Code == 0 {
		t.Error("unexpected zero code")
	}
}

// --- handleVaultsConnect ---

func TestHandleVaultsConnect_InvalidBody_Boost(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Knowledge = nil
	req := httptest.NewRequest("POST", "/api/vaults/connect", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleVaultsConnect(w, req)

	// Without knowledge, handler checks knowledge first
	if w.Code == 0 {
		t.Error("unexpected zero code")
	}
}

// --- handleKnowledgeFact ---

func TestHandleKnowledgeFact_NotFound(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("GET", "/api/knowledge/facts/nonexistent", nil)
	req.SetPathValue("slug", "nonexistent")
	w := httptest.NewRecorder()
	srv.handleKnowledgeFact(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

// --- handleKnowledgePromote ---

func TestHandleKnowledgePromote_InvalidBody_Boost(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("POST", "/api/knowledge/promote", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleKnowledgePromote(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

// --- handleAgentConfigRestrictions with valid data ---

func TestHandleAgentConfigRestrictions_AddRestriction(t *testing.T) {
	srv := newFullServer(t)
	body := `{"action":"add","pattern":"no-force-push","reason":"safety"}`
	req := httptest.NewRequest("PUT", "/api/agents/scanner/restrictions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("name", "scanner")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleAgentConfigRestrictions(w, req)

	if w.Code == http.StatusNotFound {
		t.Error("should find scanner agent")
	}
}

// --- Agent-related helpers ---

func newFullServerWithAgents(t *testing.T) *Server {
	t.Helper()
	orig := privateURLResolver
	privateURLResolver = func(_ context.Context, _ string) ([]string, error) {
		return []string{"93.184.216.34"}, nil
	}
	t.Cleanup(func() { privateURLResolver = orig })

	level := 2
	cfg := &config.Config{
		ACMMLevel: &level,
		Project: config.ProjectConfig{
			Org: "testorg", Name: "test", PrimaryRepo: "testrepo",
			Repos: []string{"testrepo"},
		},
		Data: config.DataConfig{AgentsDir: t.TempDir()},
		Agents: map[string]config.AgentConfig{
			"scanner":    {ID: "scan-001", Role: "scanner", Backend: "claude", Model: "sonnet", DisplayName: "Scanner", Enabled: true},
			"reviewer":   {ID: "rev-001", Role: "reviewer", Backend: "claude", Model: "sonnet", DisplayName: "Reviewer", Enabled: true},
			"supervisor": {ID: "sup-001", Role: "supervisor", Backend: "claude", Model: "opus", DisplayName: "Supervisor", Enabled: true, BeadRole: "supervisor"},
		},
		GitHub:     config.GitHubConfig{Token: "ghp_test123456789"},
		SourcePath: t.TempDir() + "/hive.yaml",
		Governor: config.GovernorConfig{
			EvalIntervalS: 300,
			Modes: map[string]config.ModeConfig{
				"idle":  {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "15m", "reviewer": "15m"}},
				"quiet": {Threshold: 2, Cadences: map[string]config.Cadence{"scanner": "10m", "reviewer": "10m"}},
				"busy":  {Threshold: 10, Cadences: map[string]config.Cadence{"scanner": "5m", "reviewer": "5m"}},
				"surge": {Threshold: 20, Cadences: map[string]config.Cadence{"scanner": "2m", "reviewer": "2m"}},
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	gov := governor.New(cfg.Governor, cfg.Agents, logger)
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{
		Org: "testorg", Repos: []string{"testrepo"}, ACMMLevel: *cfg.ACMMLevel, PRsAllowed: true,
	})

	srv := NewServer(0, logger)
	srv.deps = &Dependencies{
		Config:         cfg,
		AgentMgr:       mgr,
		Governor:       gov,
		Logger:         logger,
		Ctx:            context.Background(),
		RefreshFunc:    func() {},
		PersistFunc:    func() {},
		SkipReloadFunc: func() {},
	}
	srv.RegisterAPI(srv.deps)
	return srv
}

// --- handleHealthDeep with contribute hub ---

func TestHandleHealthDeep_WithContributeHub(t *testing.T) {
	srv := newFullServerWithAgents(t)
	srv.MarkReady()
	srv.UpdateStatus(minimalPayload())
	srv.contributeHub = &ContributeWSHub{
		connections:    make(map[string]*ContributorConnection),
		completedTasks: make(map[string]time.Time),
		logger:         slog.Default(),
	}

	req := httptest.NewRequest("GET", "/api/health/deep", nil)
	w := httptest.NewRecorder()
	srv.handleHealthDeep(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
	var result map[string]any
	json.NewDecoder(w.Body).Decode(&result)
	checks := result["checks"].(map[string]any)
	if checks["contribute"] == nil {
		t.Error("expected contribute check")
	}
}

// --- HealthSummary with more state ---

func TestHealthSummary_WithAgents(t *testing.T) {
	srv := newFullServerWithAgents(t)
	srv.MarkReady()
	srv.UpdateStatus(minimalPayload())

	result := srv.HealthSummary()
	if result == nil {
		t.Fatal("expected non-nil")
	}
}

// --- handleGovernorRepos with PrimaryRepo URL ---

func TestHandleGovernorRepos_PrimaryRepoURL(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Org = "oldorg"
	body := `{"repos":["otherrepo"],"primaryRepo":"https://github.com/otherorg/otherrepo"}`
	req := httptest.NewRequest("PUT", "/api/governor/repos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	markOwnerRequest(req)
	srv.handleGovernorRepos(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", w.Code, w.Body.String())
	}
	if srv.deps.Config.Project.Org != "otherorg" {
		t.Fatalf("Project.Org = %q, want otherorg", srv.deps.Config.Project.Org)
	}
	if srv.deps.Config.Project.PrimaryRepo != "otherrepo" {
		t.Fatalf("primaryRepo = %q, want otherrepo", srv.deps.Config.Project.PrimaryRepo)
	}
}

// --- handleGHUserAuthLogout ---

func TestHandleGHUserAuthLogout_Boost(t *testing.T) {
	srv := newFullServer(t)
	sid := srv.createUserSession("owner", config.RoleOwner)
	req := httptest.NewRequest("POST", "/api/gh-user-auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	w := httptest.NewRecorder()
	srv.handleGHUserAuthLogout(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

// --- handleContributorTrust ---

func TestHandleContributorTrust_InvalidBody(t *testing.T) {
	srv := newFullServer(t)
	req := httptest.NewRequest("PUT", "/api/contribute/trust", strings.NewReader("invalid"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleContributorTrust(w, req)

	// decodeBody fails but handler may have different first check
	if w.Code == http.StatusOK {
		t.Error("expected non-200 for invalid body")
	}
}
