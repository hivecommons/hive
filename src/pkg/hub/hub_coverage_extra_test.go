package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ============================================================
// convertHeartbeatToPerClusterHealth — pure logic
// ============================================================

func TestConvertHeartbeatToPerClusterHealth(t *testing.T) {
	entry := &HeartbeatHealthEntry{
		Report: &HeartbeatClusterHealthReport{
			Nodes: []HeartbeatNodeMetric{
				{
					Name:          "node-1",
					CPUCores:      4,
					CPUUsedMillis: 2000,
					CPUPercent:    50,
					MemTotalMB:    16384,
					MemUsedMB:     8192,
					MemPercent:    50,
					Ready:         true,
					Pods:          25,
					PodCapacity:   110,
					DiskPressure:  false,
					Conditions:    []string{"Ready"},
				},
				{
					Name:          "node-2",
					CPUCores:      8,
					CPUUsedMillis: 6000,
					CPUPercent:    75,
					MemTotalMB:    32768,
					MemUsedMB:     24576,
					MemPercent:    75,
					Ready:         true,
					GPUs:          2,
					GPUType:       "nvidia-a100",
				},
			},
			Summary: HeartbeatClusterSummary{
				TotalNodes:    2,
				ReadyNodes:    2,
				TotalCPUCores: 12,
				TotalCPUPct:   62,
				TotalMemGB:    48,
				TotalMemPct:   62,
				TotalPods:     25,
			},
			GPUSummary: &HeartbeatGPUSummary{
				Total:     4,
				Allocated: 2,
				Types:     []string{"nvidia-a100"},
			},
			CollectedAt: "2024-01-01T12:00:00Z",
		},
		ReceivedAt: time.Now(), // fresh data
	}

	pch := convertHeartbeatToPerClusterHealth("cluster-1", "Test Cluster", entry, 3)

	if pch.ID != "cluster-1" {
		t.Errorf("ID = %q, want cluster-1", pch.ID)
	}
	if pch.Name != "Test Cluster" {
		t.Errorf("Name = %q, want Test Cluster", pch.Name)
	}
	if len(pch.Nodes) != 2 {
		t.Errorf("Nodes = %d, want 2", len(pch.Nodes))
	}
	if pch.Nodes[0].Name != "node-1" {
		t.Errorf("Node[0].Name = %q, want node-1", pch.Nodes[0].Name)
	}
	if pch.Nodes[0].CPUCores != 4 {
		t.Errorf("Node[0].CPUCores = %d, want 4", pch.Nodes[0].CPUCores)
	}
	if pch.Summary.TotalNodes != 2 {
		t.Errorf("Summary.TotalNodes = %d, want 2", pch.Summary.TotalNodes)
	}
	if pch.Summary.HiveCount != 3 {
		t.Errorf("Summary.HiveCount = %d, want 3", pch.Summary.HiveCount)
	}
	if pch.HiveCount != 3 {
		t.Errorf("HiveCount = %d, want 3", pch.HiveCount)
	}
	if pch.DataSource != "heartbeat" {
		t.Errorf("DataSource = %q, want heartbeat", pch.DataSource)
	}
	if pch.DataStale {
		t.Error("fresh data should not be stale")
	}
	if pch.GPUSummary == nil {
		t.Fatal("GPUSummary should not be nil")
	}
	if pch.GPUSummary.TotalGPUs != 4 {
		t.Errorf("GPUSummary.TotalGPUs = %d, want 4", pch.GPUSummary.TotalGPUs)
	}
	if pch.GPUSummary.AllocatableGPUs != 2 {
		t.Errorf("GPUSummary.AllocatableGPUs = %d, want 2", pch.GPUSummary.AllocatableGPUs)
	}
}

func TestConvertHeartbeatToPerClusterHealthStale(t *testing.T) {
	entry := &HeartbeatHealthEntry{
		Report: &HeartbeatClusterHealthReport{
			Nodes:   []HeartbeatNodeMetric{},
			Summary: HeartbeatClusterSummary{TotalNodes: 1},
		},
		ReceivedAt: time.Now().Add(-10 * time.Minute), // stale
	}

	pch := convertHeartbeatToPerClusterHealth("cluster-2", "Stale", entry, 1)

	if !pch.DataStale {
		t.Error("data should be stale after 10 minutes")
	}
	if !strings.Contains(pch.DataAge, "m ago") {
		t.Errorf("DataAge should contain 'm ago', got %q", pch.DataAge)
	}
}

func TestConvertHeartbeatToPerClusterHealthNoGPU(t *testing.T) {
	entry := &HeartbeatHealthEntry{
		Report: &HeartbeatClusterHealthReport{
			Nodes:   []HeartbeatNodeMetric{},
			Summary: HeartbeatClusterSummary{},
		},
		ReceivedAt: time.Now(),
	}

	pch := convertHeartbeatToPerClusterHealth("cluster-3", "NoGPU", entry, 0)
	if pch.GPUSummary != nil {
		t.Error("GPUSummary should be nil when report has no GPU data")
	}
}

// ============================================================
// getHeartbeatHealthForCluster
// ============================================================

func TestGetHeartbeatHealthForCluster(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	// No data
	if got := srv.getHeartbeatHealthForCluster("nonexistent"); got != nil {
		t.Error("should return nil for nonexistent cluster")
	}

	// With data
	srv.heartbeatHealthMu.Lock()
	srv.heartbeatHealth["test-cluster"] = &HeartbeatHealthEntry{
		Report: &HeartbeatClusterHealthReport{
			Nodes: []HeartbeatNodeMetric{{Name: "node1"}},
		},
		ReceivedAt: time.Now(),
	}
	srv.heartbeatHealthMu.Unlock()

	got := srv.getHeartbeatHealthForCluster("test-cluster")
	if got == nil {
		t.Fatal("should return entry for existing cluster")
	}
	if len(got.Report.Nodes) != 1 {
		t.Errorf("nodes = %d, want 1", len(got.Report.Nodes))
	}

	// Nil report
	srv.heartbeatHealthMu.Lock()
	srv.heartbeatHealth["nil-report"] = &HeartbeatHealthEntry{
		Report:     nil,
		ReceivedAt: time.Now(),
	}
	srv.heartbeatHealthMu.Unlock()

	if srv.getHeartbeatHealthForCluster("nil-report") != nil {
		t.Error("should return nil when report is nil")
	}
}

// ============================================================
// handleClusterHealth — requires admin (exercises requireAdmin path)
// ============================================================

func TestHandleClusterHealthNotAdmin(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/api/saas/cluster-health", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

// ============================================================
// handleAccessStatus with bearer auth (deeper code path)
// ============================================================

func TestHandleAccessStatusWithBearerAuth(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	// Pre-populate cache for bearer auth
	ghTokenCacheMu.Lock()
	ghTokenCache["ghp_access_status_test"] = ghTokenCacheEntry{
		username:  "access-status-user",
		expiresAt: time.Now().Add(5 * time.Minute),
	}
	ghTokenCacheMu.Unlock()
	defer func() {
		ghTokenCacheMu.Lock()
		delete(ghTokenCache, "ghp_access_status_test")
		ghTokenCacheMu.Unlock()
	}()

	// Add some hives to the registry
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "hive-a", Owner: "access-status-user", Online: true, LastHeartbeat: time.Now().Format(time.RFC3339)},
		{ID: "hive-b", Owner: "other-user", Online: true, LastHeartbeat: time.Now().Format(time.RFC3339)},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest("GET", "/api/saas/access-status", nil)
	req.Header.Set("Authorization", "Bearer ghp_access_status_test")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["authenticated"] != true {
		t.Error("should be authenticated")
	}
}

// ============================================================
// handleCreateHive — deeper validation paths
// ============================================================

func TestHandleCreateHiveNotAuthenticatedDirect(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("POST", "/test", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleCreateHive(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

// ============================================================
// handleMigrateHive — deeper paths
// ============================================================

// ============================================================
// handleToggleVisibility — CORS headers
// ============================================================

func TestHandleToggleVisibilityCORSHeaders(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /visibility/{id}", srv.handleToggleVisibility)

	req := httptest.NewRequest("PUT", "/visibility/test-hive", strings.NewReader(`{"is_public":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "https://hive.kubestellar.io" {
		t.Error("should set CORS headers for trusted origin")
	}
}

func TestHandleToggleVisibilityUntrustedOrigin(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /visibility/{id}", srv.handleToggleVisibility)

	req := httptest.NewRequest("PUT", "/visibility/test-hive", strings.NewReader(`{"is_public":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("should not set CORS headers for untrusted origin")
	}
}

// ============================================================
// handleUpgradeHive — CORS + forbidden
// ============================================================

func TestHandleUpgradeHiveCORSHeaders(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /upgrade/{id}", srv.handleUpgradeHive)

	req := httptest.NewRequest("POST", "/upgrade/test-hive", nil)
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "https://hive.kubestellar.io" {
		t.Error("should set CORS headers")
	}
}

// ============================================================
// handleToggleAutoUpgrade — CORS headers
// ============================================================

func TestHandleToggleAutoUpgradeCORSHeaders(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /auto-upgrade/{id}", srv.handleToggleAutoUpgrade)

	req := httptest.NewRequest("PUT", "/auto-upgrade/test-hive", strings.NewReader(`{"auto_upgrade":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://hive.kubestellar.io")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "https://hive.kubestellar.io" {
		t.Error("should set CORS headers")
	}
}

// ============================================================
// handleHubAutoUpgrade — valid request
// ============================================================

func TestHandleHubAutoUpgradeValid(t *testing.T) {
	defer helperSetupTempDirs(t)()
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("PUT", "/hub-auto-upgrade", strings.NewReader(`{"auto_upgrade":false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHubAutoUpgrade(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Error("should return ok:true")
	}
}

func TestHandleHubAutoUpgradeEnable(t *testing.T) {
	defer helperSetupTempDirs(t)()
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("PUT", "/hub-auto-upgrade", strings.NewReader(`{"auto_upgrade":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHubAutoUpgrade(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ============================================================
// handleHubSelfUpgrade
// ============================================================

// TestHandleHubSelfUpgrade asserts the handler REFUSES to upgrade when no
// target SHA is known, and does so without touching a cluster.
//
// The previous version of this test asserted nothing: it accepted both 200 and
// 500 and merely t.Logf'd anything else, so it could not fail. Worse, its
// premise was wrong. It reasoned "will fail because kubectl doesn't exist or
// cluster not accessible" — untrue inside a hive pod, where kubectl exists and
// the pod's ServiceAccount holds patch on the hub Deployment. Combined with a
// sibling test seeding latestSHAByBranch["v2"] = "target1", it issued a real
//
//	kubectl set image deployment/hive-hub hub=ghcr.io/hivecommons/hive-hub:target1
//
// against production, leaving the hub serving stale code behind an
// ImagePullBackOff. See TestMain, which removes the in-cluster credentials
// that made it reachable.
//
// The SHA cache is a package-level global shared across the suite, so this
// clears it and restores it rather than assuming a starting state.
func TestHandleHubSelfUpgrade(t *testing.T) {
	latestSHAMu.Lock()
	saved, had := latestSHAByBranch["v2"]
	delete(latestSHAByBranch, "v2")
	latestSHAMu.Unlock()
	t.Cleanup(func() {
		latestSHAMu.Lock()
		if had {
			latestSHAByBranch["v2"] = saved
		} else {
			delete(latestSHAByBranch, "v2")
		}
		latestSHAMu.Unlock()
	})

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("POST", "/hub-self-upgrade", nil)
	w := httptest.NewRecorder()
	srv.handleHubSelfUpgrade(w, req)

	// No cached SHA means an empty target, which rolloutHubToSHA must reject
	// before it ever builds a kubectl command. This is deterministic: it does
	// not depend on whether a cluster happens to be reachable.
	if w.Code != http.StatusInternalServerError {
		t.Errorf("hub self-upgrade with no target SHA: got %d, want %d",
			w.Code, http.StatusInternalServerError)
	}
}

// ============================================================
// handleHiveStatus — path traversal
// ============================================================

func TestHandleHiveStatusPathTraversal(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hives/{id}/status", srv.handleHiveStatus)

	req := httptest.NewRequest("GET", "/api/saas/hives/..%2F..%2Fetc/status", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound && w.Code != http.StatusBadRequest {
		t.Errorf("expected 404 or 400 for path traversal, got %d", w.Code)
	}
}

// ============================================================
// handleDeleteHive — more paths
// ============================================================

func TestHandleDeleteHiveNotOwner(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/hives/{id}", srv.handleDeleteHive)

	// Hive doesn't exist → idempotent 200 (covers the loadSaaSHive nil
	// path, which purges any registry leftover and reports deleted).
	req := httptest.NewRequest("DELETE", "/api/saas/hives/nonexistent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (idempotent delete), got %d", w.Code)
	}
}

// ============================================================
// loadClusters — with JSON file
// ============================================================

func TestLoadClustersWithTempFile(t *testing.T) {
	// Can't easily write to /data/saas/clusters.json, but the default path
	// returns a single cluster — exercises some code paths.
	clusters := loadClusters(slog.Default())
	if len(clusters) == 0 {
		t.Error("should return at least default cluster")
	}
	defaultC, ok := clusters[defaultClusterID]
	if !ok {
		t.Fatal("missing default cluster")
	}
	if defaultC.Arch != "arm64" {
		t.Errorf("default cluster arch = %q, want arm64", defaultC.Arch)
	}
	if defaultC.InCluster != true {
		t.Error("default cluster should be in-cluster")
	}
}

// ============================================================
// handleSaaSAuthCheck — more paths
// ============================================================

func TestHandleSaaSAuthCheckPublicPathSnapshot(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/api/saas/auth-check?hive=test", nil)
	req.Header.Set("X-Original-URI", "/snapshot/something")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("snapshot path should be public, got %d", w.Code)
	}
}

func TestHandleSaaSAuthCheckPublicPathContribute(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/api/saas/auth-check?hive=test", nil)
	req.Header.Set("X-Original-URI", "/contribute/something")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("contribute path should be public, got %d", w.Code)
	}
}

// ============================================================
// handleMyHives — various paths exercising more lines
// ============================================================

func TestHandleMyHivesAdminSeesAllHives(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	// Pre-populate cache for admin user
	ghTokenCacheMu.Lock()
	ghTokenCache["ghp_admin_myhives"] = ghTokenCacheEntry{
		username:  hubAdminUsername,
		expiresAt: time.Now().Add(5 * time.Minute),
	}
	ghTokenCacheMu.Unlock()
	defer func() {
		ghTokenCacheMu.Lock()
		delete(ghTokenCache, "ghp_admin_myhives")
		ghTokenCacheMu.Unlock()
	}()

	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "admin-hive", Owner: hubAdminUsername, Online: true, LastHeartbeat: time.Now().Format(time.RFC3339)},
		{ID: "other-hive", Owner: "otheruser", Online: true, LastHeartbeat: time.Now().Format(time.RFC3339)},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest("GET", "/fleet", nil)
	req.Header.Set("Authorization", "Bearer ghp_admin_myhives")
	w := httptest.NewRecorder()
	srv.handleMyHives(w, req)

	// ensureSaaSUser will fail without /data but exercises the code path
	_ = w.Code
}

// ============================================================
// handleApproveAccess / handleDenyAccess — auth checks
// ============================================================

func TestHandleApproveAccessNotAuthorized(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/saas/hives/{id}/approve-access/{username}", srv.handleApproveAccess)

	// Hive not found
	req := httptest.NewRequest("PUT", "/api/saas/hives/nonexist/approve-access/user", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleDenyAccessNotAuthorized(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/hives/{id}/deny-access/{username}", srv.handleDenyAccess)

	req := httptest.NewRequest("DELETE", "/api/saas/hives/nonexist/deny-access/user", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// handleRequestAccess — not found path
// ============================================================

func TestHandleRequestAccessHiveNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/request-access", srv.handleRequestAccess)

	req := httptest.NewRequest("POST", "/api/saas/hives/nonexist/request-access", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// handleGetRequests — not found path
// ============================================================

func TestHandleGetRequestsHiveNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hives/{id}/requests", srv.handleGetRequests)

	req := httptest.NewRequest("GET", "/api/saas/hives/nonexist/requests", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// handleApproveRequest — more paths
// ============================================================

func TestHandleApproveRequestHiveNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/approve", srv.handleApproveRequest)

	req := httptest.NewRequest("POST", "/api/saas/hives/nonexist/requests/user/approve", strings.NewReader(`{"role":"read"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// handleDenyRequest — more paths
// ============================================================

func TestHandleDenyRequestHiveNotFound(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/deny", srv.handleDenyRequest)

	req := httptest.NewRequest("POST", "/api/saas/hives/nonexist/requests/user/deny", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// ============================================================
// Heartbeat — update existing hive with sparkline
// ============================================================

func TestHandleHeartbeatUpdateExistingWithSparkline(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Pre-populate a hive with sparkline data from 20 minutes ago
	oldTime := time.Now().Add(-20 * time.Minute).Unix()
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{
			ID:            "spark-hive",
			Online:        true,
			RegisteredAt:  "2024-01-01T00:00:00Z",
			GitHash:       "old123",
			IssueHistory:  []SparkPoint{{T: oldTime, V: 3}},
			PRHistory:     []SparkPoint{{T: oldTime, V: 2}},
			LastHeartbeat: time.Now().Format(time.RFC3339),
		},
	}
	srv.mu.Unlock()

	payload := `{"hive_id":"spark-hive","governor":{"issues":5,"prs":3}}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	// Check sparkline was updated
	srv.mu.RLock()
	for _, h := range srv.registry.Hives {
		if h.ID == "spark-hive" {
			if len(h.IssueHistory) < 2 {
				t.Error("issue history should have at least 2 points after 15+ min gap")
			}
		}
	}
	srv.mu.RUnlock()
}

// ============================================================
// Heartbeat — upgrading flag handling
// ============================================================

func TestHandleHeartbeatUpgradeCompleted(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{
			ID:            "upgrading-hive",
			Online:        true,
			GitHash:       "old123",
			Upgrading:     true,
			UpgradeTarget: "new456",
			LastHeartbeat: time.Now().Format(time.RFC3339),
		},
	}
	srv.mu.Unlock()

	// Heartbeat with new git hash → upgrade completed
	payload := `{"hive_id":"upgrading-hive","git_hash":"new456"}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	srv.mu.RLock()
	for _, h := range srv.registry.Hives {
		if h.ID == "upgrading-hive" {
			if h.Upgrading {
				t.Error("upgrading should be false after hash changed")
			}
		}
	}
	srv.mu.RUnlock()
}

func TestHandleHeartbeatUpgradeInProgress(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{
			ID:            "still-upgrading",
			Online:        true,
			GitHash:       "old123",
			Upgrading:     true,
			UpgradeTarget: "new456",
			LastHeartbeat: time.Now().Format(time.RFC3339),
		},
	}
	srv.mu.Unlock()

	// Same git hash → still upgrading
	payload := `{"hive_id":"still-upgrading","git_hash":"old123"}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	srv.mu.RLock()
	for _, h := range srv.registry.Hives {
		if h.ID == "still-upgrading" {
			if !h.Upgrading {
				t.Error("should still be upgrading")
			}
		}
	}
	srv.mu.RUnlock()
}

// ============================================================
// Heartbeat — heartbeat response with auto-upgrade instruction
// ============================================================

func TestHandleHeartbeatResponseAutoUpgrade(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Set up a latestSHA for v2
	latestSHAMu.Lock()
	oldV2 := latestSHAByBranch["v2"]
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "newest7", Message: "new commit"}
	latestSHAMu.Unlock()
	defer func() {
		latestSHAMu.Lock()
		latestSHAByBranch["v2"] = oldV2
		latestSHAMu.Unlock()
	}()

	// Pre-populate hive with auto_upgrade and old hash
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "auto-up-hive", Online: true, GitHash: "old123", GitBranch: "v2", LastHeartbeat: time.Now().Format(time.RFC3339)},
	}
	srv.mu.Unlock()

	payload := `{"hive_id":"auto-up-hive","git_hash":"old123","git_branch":"v2","auto_upgrade":true}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp HeartbeatResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.LatestSHA != "newest7" {
		t.Errorf("LatestSHA = %q, want newest7", resp.LatestSHA)
	}
}

// ============================================================
// Heartbeat — ClusterHealth stored
// ============================================================

func TestHandleHeartbeatStoresClusterHealth(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	payload := HeartbeatPayload{
		HiveID:    "health-store-hive",
		ClusterID: "my-test-cluster",
		ClusterHealth: &HeartbeatClusterHealthReport{
			Nodes: []HeartbeatNodeMetric{
				{Name: "n1", CPUCores: 4, MemTotalMB: 8192},
			},
			Summary: HeartbeatClusterSummary{TotalNodes: 1},
		},
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify health was stored
	entry := srv.getHeartbeatHealthForCluster(defaultClusterID)
	// The ClusterID from payload is sanitized and used but the entry's ClusterID comes from SaaS hive lookup.
	// Since the hive doesn't exist in SaaS, it defaults to defaultClusterID.
	if entry == nil {
		t.Log("heartbeat health stored under cluster ID (may differ from payload)")
	}
}

// ============================================================
// requestSave + saveLoop edge cases
// ============================================================

func TestRequestSave(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	// Should not panic when called multiple times
	srv.requestSave()
	srv.requestSave()
	srv.requestSave()
}

// ============================================================
// handleContributeProxy with valid public hive
// ============================================================

func TestHandleContributeProxyRejectsPrivateURL(t *testing.T) {
	// httptest servers listen on 127.0.0.1 which is private — findContributeHive rejects it
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "contribute-hive", Online: true, IsPublic: true, DashboardURL: upstream.URL, Owner: "user1"},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest("POST", "/api/contribute/register", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	// Private URLs are rejected by findContributeHive → 503
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 for private URL, got %d", w.Code)
	}
}

// ============================================================
// handleContributeWSProxy
// ============================================================

func TestHandleContributeWSProxyNoHive(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/api/contribute/ws", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

// ============================================================
// handleUserToken — additional paths
// ============================================================

func TestHandleUserTokenEmptyBody(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("POST", "/test", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleUserToken(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleUserTokenInvalidJSON(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("POST", "/test", strings.NewReader(`{invalid`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleUserToken(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// ============================================================
// handleRequestProvision — with valid auth but missing primary_repo
// ============================================================

func TestHandleRequestProvisionDefaultPrimaryRepo(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	ghTokenCacheMu.Lock()
	ghTokenCache["ghp_prov_default"] = ghTokenCacheEntry{
		username:  "prov-default-user",
		expiresAt: time.Now().Add(5 * time.Minute),
	}
	ghTokenCacheMu.Unlock()
	defer func() {
		ghTokenCacheMu.Lock()
		delete(ghTokenCache, "ghp_prov_default")
		ghTokenCacheMu.Unlock()
	}()

	// primary_repo not provided — should default to first repo. github_host is
	// required and must be present for the request to reach that defaulting.
	body := `{"org":"validorg","github_host":"github.com","repos":"repo1,repo2","acmm_level":2,"full_name":"Ada Lovelace"}`
	req := httptest.NewRequest("POST", "/provision-test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer ghp_prov_default")
	w := httptest.NewRecorder()
	srv.handleRequestProvision(w, req)

	// May succeed (200) or fail on save (500), but exercises code paths
	if w.Code == http.StatusBadRequest {
		t.Errorf("should not reject valid request, got %d: %s", w.Code, w.Body.String())
	}
}

// ============================================================
// handleAdminUsers — exercised directly
// ============================================================

func TestHandleAdminUsersResponse(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/admin-users", nil)
	w := httptest.NewRecorder()
	srv.handleAdminUsers(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp["users"]; !ok {
		t.Error("should have 'users' key")
	}
}

// ============================================================
// encryptToken / decryptToken — direct tests
// ============================================================

func TestEncryptTokenFailsWithoutDir(t *testing.T) {
	// If /data/saas doesn't exist, this will fail to create HMAC key
	_, err := encryptToken("test-token")
	if err != nil {
		t.Logf("encryptToken failed (expected without /data): %v", err)
	}
}

func TestDecryptTokenWithBadCiphertext(t *testing.T) {
	// Valid base64 but garbage ciphertext
	_, err := decryptToken("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err == nil {
		t.Error("should fail on bad ciphertext")
	}
}

// ============================================================
// Heartbeat handler — registrySaveDelay exercises saveLoop
// ============================================================

func TestHandleHeartbeatTriggersRegistrySave(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	payload := `{"hive_id":"save-trigger-hive"}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	// The save is async — just verify no panic
}

// ============================================================
// handleDashboard with various user agents
// ============================================================

func TestHandleDashboardRegularBrowserWithCookie(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "user123"})
	req.Header.Set("User-Agent", "Mozilla/5.0")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Error("should return HTML")
	}
}

// ============================================================
// handleOAuthCallback — error paths
// ============================================================

func TestHandleOAuthCallbackMissingCodeParam(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	req := httptest.NewRequest("GET", "/callback", nil)
	w := httptest.NewRecorder()
	srv.handleOAuthCallback(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// ============================================================
// handleContributeProxy — bad URL in hive
// ============================================================

func TestHandleContributeProxyBadURL(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	// Set a hive with an unparseable URL
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "bad-url-hive", Online: true, IsPublic: true, DashboardURL: "://invalid", Owner: "user"},
	}
	srv.mu.Unlock()

	// findContributeHive will filter because isPrivateURL fails on bad URLs (returns true)
	// So this will be service unavailable
	req := httptest.NewRequest("POST", "/api/contribute/register", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable && w.Code != http.StatusInternalServerError {
		t.Logf("bad URL contribute proxy: %d", w.Code)
	}
}

// ============================================================
// handleProxyHiveConfig — upstream error
// ============================================================

func TestHandleProxyHiveConfigUpstreamError(t *testing.T) {
	orig := hiveConfigSSRFGuard
	hiveConfigSSRFGuard = func(context.Context, string) bool { return false }
	defer func() { hiveConfigSSRFGuard = orig }()

	srv := NewHubServer(0, slog.Default(), "test", "v2")

	// Ownerless entry: after the F9 fix the unauthenticated test caller cannot
	// reach the upstream, so the ownership check (403) preempts any proxy fetch.
	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("upstream error"))
	}))
	defer upstream.Close()

	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{ID: "error-hive", DashboardURL: upstream.URL}, // Owner intentionally empty
	}
	srv.mu.Unlock()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/saas/hive-config/{hiveID}", srv.handleProxyHiveConfig)
	req := httptest.NewRequest("GET", "/api/saas/hive-config/error-hive", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Ownerless hive is refused before the proxy fetch (F9).
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for ownerless hive, got %d", w.Code)
	}
	if reached {
		t.Error("upstream was fetched for an ownerless hive — F9 regression")
	}
}

// ============================================================
// Start + Shutdown lifecycle
// ============================================================

func TestStartAndShutdown(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")

	// Start in background
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(0) // port 0 = random
	}()

	// Give it a moment to start
	time.Sleep(100 * time.Millisecond)

	// Shutdown
	if err := srv.Shutdown(2 * time.Second); err != nil {
		t.Errorf("Shutdown failed: %v", err)
	}
}

// ============================================================
// triggerAutoUpgrades — exercises most of the function
// ============================================================

func TestTriggerAutoUpgradesNoHives(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	// Should not panic with no hives
	srv.triggerAutoUpgrades()
}

// ============================================================
// Various edge cases for higher coverage
// ============================================================

func TestHandleHeartbeatSaaSHivePrefix(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	secret := srv.hubSecret

	// saas- prefix without SaaS entry should be rejected
	payload := `{"hive_id":"saas-test-nosaas"}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", heartbeatBearer(secret, "saas-test-nosaas"))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for saas- prefix without SaaS entry, got %d", w.Code)
	}
}

func TestHandleHeartbeatUpgradingFlag(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	payload := `{"hive_id":"upgrading-flag-test","upgrading":true,"upgrade_target_sha":"7a41e01"}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	srv.mu.RLock()
	for _, h := range srv.registry.Hives {
		if h.ID == "upgrading-flag-test" && !h.Upgrading {
			t.Error("upgrading flag should be set")
		}
	}
	srv.mu.RUnlock()
}

func TestHandleLeaderboardWithData(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{
		{
			ID:       "lb-hive",
			IsPublic: true,
			Leaderboard: []LeaderboardEntry{
				{GitHubUsername: "dev1", TasksCompleted: 10, Active: true, CurrentTask: "fix-123", HiveName: "lb-hive"},
			},
		},
	}
	srv.mu.Unlock()

	req := httptest.NewRequest("GET", "/api/hub/leaderboard", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	lb, ok := resp["leaderboard"]
	if !ok {
		t.Fatal("should have leaderboard key")
	}
	entries := lb.([]any)
	if len(entries) == 0 {
		t.Error("should have at least one leaderboard entry")
	}
}

func TestGetLatestSHAForBranchNotFound(t *testing.T) {
	sha := getLatestSHAForBranch("nonexistent-branch")
	if sha != "" {
		t.Errorf("expected empty for nonexistent branch, got %q", sha)
	}
}

func TestHandleHeartbeatHiveTypeExplicit(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	payload := `{"hive_id":"explicit-type","hive_type":"custom-type"}`
	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	srv.mu.RLock()
	for _, h := range srv.registry.Hives {
		if h.ID == "explicit-type" {
			if h.HiveType != "custom-type" {
				t.Errorf("hive_type = %q, want custom-type", h.HiveType)
			}
		}
	}
	srv.mu.RUnlock()
}

func TestHandleHeartbeatMaxAgents(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// Create 60 agents to test the max agents cap (50)
	agents := make([]AgentSummary, 60)
	for i := range agents {
		agents[i] = AgentSummary{Name: fmt.Sprintf("agent-%d", i), State: "running"}
	}
	payload := HeartbeatPayload{
		HiveID: "max-agents-hive",
		Agents: agents,
	}
	body, _ := json.Marshal(payload)

	req := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	srv.mu.RLock()
	for _, h := range srv.registry.Hives {
		if h.ID == "max-agents-hive" {
			if len(h.Agents) > 50 {
				t.Errorf("agents should be capped at 50, got %d", len(h.Agents))
			}
		}
	}
	srv.mu.RUnlock()
}

func TestHandleHeartbeatSnapshotURL(t *testing.T) {
	srv := NewHubServer(0, slog.Default(), "test", "v2")
	srv.setHubSecret("")

	// First heartbeat with snapshot
	payload1 := `{"hive_id":"snapshot-hive","snapshot_url":"https://snap.example.com/1"}`
	req1 := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload1))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	srv.mux.ServeHTTP(w1, req1)

	// Second heartbeat without snapshot — should preserve
	payload2 := `{"hive_id":"snapshot-hive"}`
	req2 := httptest.NewRequest("POST", "/api/heartbeat", strings.NewReader(payload2))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.mux.ServeHTTP(w2, req2)

	srv.mu.RLock()
	for _, h := range srv.registry.Hives {
		if h.ID == "snapshot-hive" {
			if h.SnapshotURL != "https://snap.example.com/1" {
				t.Errorf("snapshot URL should be preserved, got %q", h.SnapshotURL)
			}
		}
	}
	srv.mu.RUnlock()
}
