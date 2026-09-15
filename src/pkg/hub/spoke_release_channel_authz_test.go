package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSpokeReleaseChannelSwitchCannotRetargetDifferentHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("POST /api/saas/hives/{id}/switch-branch", s.requireAuthOrSpokeSelfService(s.handleSwitchBranch))
	s.clusters = map[string]ClusterConfig{"c1": {ID: "c1", InCluster: true}}

	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{"hosted-a": "owner"}}); err != nil {
		t.Fatalf("save alice: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "bob", Hives: map[string]string{"hosted-b": "owner"}}); err != nil {
		t.Fatalf("save bob: %v", err)
	}
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-a", Owner: "alice", ClusterID: "c1", DashboardTokenHash: HashDashboardToken("token-a")}); err != nil {
		t.Fatalf("save hosted-a: %v", err)
	}
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-b", Owner: "bob", ClusterID: "c1", DashboardTokenHash: HashDashboardToken("token-b")}); err != nil {
		t.Fatalf("save hosted-b: %v", err)
	}

	oldImageExists := spokeImageExists
	spokeImageExists = func(tag string, _ *slog.Logger) bool { return true }
	t.Cleanup(func() { spokeImageExists = oldImageExists })

	req := httptest.NewRequest(http.MethodPost, "/api/saas/hives/hosted-b/switch-branch", strings.NewReader(`{"branch":"stable"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer spoke-self-service")
	req.Header.Set("X-Hive-Role", saasRoleOwner)
	req.Header.Set(proxyAuthHeader, "token-a")
	req.Header.Set("X-Hive-ID", "hosted-a")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("cross-hive switch status = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	if got := loadSaaSHive("hosted-b").TrackedChannel; got != "" {
		t.Fatalf("cross-hive switch persisted victim tracked_channel=%q", got)
	}
}

func TestSpokeReleaseChannelSwitchPersistsTrackedChannelIntent(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("POST /api/saas/hives/{id}/switch-branch", s.requireAuthOrSpokeSelfService(s.handleSwitchBranch))
	s.clusters = map[string]ClusterConfig{"c1": {ID: "c1", InCluster: true}}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{"hosted-a": "owner"}}); err != nil {
		t.Fatalf("save user: %v", err)
	}
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-a", Owner: "alice", ClusterID: "c1", DashboardTokenHash: HashDashboardToken("token-a")}); err != nil {
		t.Fatalf("save hive: %v", err)
	}
	oldImageExists := spokeImageExists
	spokeImageExists = func(tag string, _ *slog.Logger) bool { return tag == "candidate" }
	t.Cleanup(func() { spokeImageExists = oldImageExists })

	req := httptest.NewRequest(http.MethodPost, "/api/saas/hives/hosted-a/switch-branch", strings.NewReader(`{"branch":"candidate"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer spoke-self-service")
	req.Header.Set("X-Hive-Role", saasRoleOwner)
	req.Header.Set(proxyAuthHeader, "token-a")
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("switch status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := loadSaaSHive("hosted-a").TrackedChannel; got != "candidate" {
		t.Fatalf("tracked_channel = %q, want candidate", got)
	}
	s.mu.RLock()
	armed := s.heartbeatSwitchTag["hosted-a"]
	s.mu.RUnlock()
	if armed != "candidate" {
		t.Fatalf("heartbeat switch tag = %q, want candidate", armed)
	}
}
