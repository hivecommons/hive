package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ============================================================
// server.go — handleHeartbeat validation + response branches
// ============================================================

func newHeartbeatHub() *HubServer {
	return &HubServer{
		logger:                  slog.Default(),
		saveCh:                  make(chan struct{}, 1),
		hubGitHash:              "abc1234",
		hubGitBranch:            "v2",
		clusters:                map[string]ClusterConfig{"hive-oke": {ID: "hive-oke"}},
		heartbeatHealth:         make(map[string]*HeartbeatHealthEntry),
		heartbeatUpgrade:        make(map[string]string),
		heartbeatSwitchTag:      make(map[string]string),
		pendingWebhooks:         make(map[string]*pendingWebhookEntry),
		pendingGitHubAppConfigs: make(map[string]*HeartbeatGitHubAppConfig),
		pendingGateways:         make(map[string]*HeartbeatGatewayConfig),
		hubBanners:              make(map[string]*HubBannerEntry),
	}
}

func postHeartbeat(t *testing.T, s *HubServer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/heartbeat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleHeartbeat(rec, req)
	return rec
}

func TestHandleHeartbeatValidation(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	cases := []struct {
		name, body string
		want       int
	}{
		{"bad json", `{`, http.StatusBadRequest},
		{"missing hive_id", `{"org":"x"}`, http.StatusBadRequest},
		{"invalid org", `{"hive_id":"h1","org":"bad org!"}`, http.StatusBadRequest},
		{"invalid primary repo", `{"hive_id":"h1","primary_repo":"bad repo!"}`, http.StatusBadRequest},
		{"invalid repo in list", `{"hive_id":"h1","repos":["good","bad repo!"]}`, http.StatusBadRequest},
		{"bad dashboard_url", `{"hive_id":"h1","dashboard_url":"ftp://x"}`, http.StatusBadRequest},
		{"bad snapshot_url", `{"hive_id":"h1","snapshot_url":"ftp://x"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postHeartbeat(t, s, tc.body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestHandleHeartbeatUnauthorized(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	s.hubSecret = "top-secret"

	req := httptest.NewRequest(http.MethodPost, "/api/heartbeat", strings.NewReader(`{"hive_id":"h1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleHeartbeat(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no-auth heartbeat status = %d, want 401", rec.Code)
	}

	// With the correct bearer token it should succeed.
	req = httptest.NewRequest(http.MethodPost, "/api/heartbeat", strings.NewReader(`{"hive_id":"h1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer top-secret")
	rec = httptest.NewRecorder()
	s.handleHeartbeat(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("authed heartbeat status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestHandleHeartbeatDeliversPending(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	hiveID := "kellyaa"
	// Queue a hub banner, a pending gateway, and a pending GitHub App config so
	// the response-construction branches that attach them run.
	s.hubBanners[hiveID] = &HubBannerEntry{ID: "b1", Message: "hi", Color: "green", SentAt: "now"}
	s.pendingGateways[hiveID] = &HeartbeatGatewayConfig{Name: "openrouter", Key: "k"}
	s.pendingGitHubAppConfigs[hiveID] = &HeartbeatGitHubAppConfig{AppID: 1, InstallationID: 2, PrivateKey: "pk"}

	body := `{
		"hive_id":"kellyaa",
		"org":"testorg",
		"primary_repo":"repo",
		"repos":["repo"],
		"dashboard_url":"https://kellyaa.hive.kubestellar.io",
		"git_hash":"deadbeef1234",
		"is_public":true,
		"tokens_24h":42
	}`
	rec := postHeartbeat(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	// The response body must carry the funded gateway.
	if !strings.Contains(rec.Body.String(), "openrouter") {
		t.Errorf("expected pending gateway in response, got %s", rec.Body.String())
	}
	// It must NOT be drained yet. This assertion is inverted from what it used to
	// be: draining on send made a paid-for gateway at-most-once, so a response
	// lost in flight — or one the spoke failed to apply — dropped it with nothing
	// left to retry from. The spoke's payload above reports no gateways, which is
	// UNKNOWN (an older spoke cannot report), never a confirmation.
	if !s.hasPendingGateway(hiveID) {
		t.Error("gateway was drained on send; it must survive until the spoke reports it back")
	}

	// Once the spoke reports the gateway, the hub stops offering it.
	confirmBody := `{
		"hive_id":"kellyaa",
		"org":"testorg",
		"primary_repo":"repo",
		"repos":["repo"],
		"dashboard_url":"https://kellyaa.hive.kubestellar.io",
		"git_hash":"deadbeef1234",
		"is_public":true,
		"tokens_24h":42,
		"gateway_names":["openrouter"]
	}`
	rec2 := postHeartbeat(t, s, confirmBody)
	if rec2.Code != http.StatusOK {
		t.Fatalf("confirm status = %d body=%s", rec2.Code, rec2.Body.String())
	}
	if s.hasPendingGateway(hiveID) {
		t.Error("gateway still pending after the spoke reported it back")
	}
}

func TestHandleHeartbeatPublicURLSelfCheckRoundTripAndOldSpokeCompatibility(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()

	body := `{
		"hive_id":"selfcheck",
		"org":"testorg",
		"primary_repo":"repo",
		"repos":["repo"],
		"dashboard_url":"https://selfcheck.hive.kubestellar.io",
		"public_url_self_check":{"status":"ok","checked_at":"2026-08-08T10:15:00Z","http_status":401},
		"route_exists":{"status":"found","checked_at":"2026-08-08T10:15:00Z","host":"selfcheck.hive.kubestellar.io","kind":"Ingress"}
	}`
	rec := postHeartbeat(t, s, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(s.registry.Hives) != 1 || s.registry.Hives[0].PublicURLSelfCheck == nil {
		t.Fatalf("self-check was not persisted: %+v", s.registry.Hives)
	}
	if got := s.registry.Hives[0].PublicURLSelfCheck; got.Status != PublicURLSelfCheckOK || got.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("self-check = %+v, want ok/401", got)
	}
	if got := s.registry.Hives[0].RouteExists; got == nil || got.Status != RouteExistenceFound || got.Kind != "Ingress" {
		t.Fatalf("route_exists = %+v, want found Ingress", got)
	}

	// Older spokes omit the new field. The heartbeat must still be accepted and
	// the registry must represent the signal as unknown, not as a failure.
	oldBody := `{
		"hive_id":"oldspoke",
		"org":"testorg",
		"primary_repo":"repo",
		"repos":["repo"],
		"dashboard_url":"https://oldspoke.hive.kubestellar.io"
	}`
	rec = postHeartbeat(t, s, oldBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("old-spoke status = %d body=%s", rec.Code, rec.Body.String())
	}
	var old *RegistryEntry
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == "oldspoke" {
			old = &s.registry.Hives[i]
		}
	}
	if old == nil {
		t.Fatal("old spoke heartbeat did not register")
	}
	if old.PublicURLSelfCheck != nil {
		t.Fatalf("old spoke should leave self-check unknown, got %+v", old.PublicURLSelfCheck)
	}
	if old.RouteExists != nil {
		t.Fatalf("old spoke should leave route_exists unknown, got %+v", old.RouteExists)
	}
}

func TestHandleHeartbeatSwitchTag(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	// A queued branch switch should be delivered as switch_to_tag.
	s.heartbeatSwitchTag["kellyaa"] = "dev-latest"

	rec := postHeartbeat(t, s, `{"hive_id":"kellyaa","primary_repo":"r"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "dev-latest") {
		t.Errorf("expected switch_to_tag in response, got %s", rec.Body.String())
	}
}
