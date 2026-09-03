package dashboard

// Regression tests for "Upgrade failed: not authenticated" on hosted spokes
// fronted by the Node gateway auth proxy (:3001). The gateway authenticates the
// operator with the shared dashboard token, strips X-Hive-User/X-Hive-Role, and
// injects X-Hive-Internal — so the Go backend's authenticate middleware grants
// a verified owner role WITHOUT any per-user identity. Every owner-gated
// endpoint works on that shape, but handleSelfUpgrade relays the request to the
// hub, whose spoke-upgrade lane rejected an empty X-Hive-User outright. These
// tests pin the spoke half of the fix: the relay must carry the dashboard-token
// proof (which the hub now accepts as the operator credential, attributing the
// upgrade to the hive's registered owner), and a spoke with no proof at all
// must fail fast with an honest, actionable error instead of a doomed round
// trip that surfaces as "not authenticated".

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestSelfUpgradeGatewayOwnerNoUserIdentityRelaysProof drives the REAL
// middleware chain (authenticate → handleSelfUpgrade) with the exact request
// shape the gateway produces: X-Hive-Internal shared token, no session cookie,
// no identity headers. The relayed hub request must carry the owner role and
// the dashboard-token proof so the hub can verify the spoke even though no
// X-Hive-User exists on this topology.
func TestSelfUpgradeGatewayOwnerNoUserIdentityRelaysProof(t *testing.T) {
	const token = "spoke-dashboard-token"

	var sawUser, sawRole, sawProof string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUser = r.Header.Get("X-Hive-User")
		sawRole = r.Header.Get("X-Hive-Role")
		sawProof = r.Header.Get(proxyAuthHeader)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"upgrading","mode":"heartbeat"}`))
	}))
	defer hub.Close()

	srv := NewServerWithAuth(0, token, slog.Default())
	srv.deps = &Dependencies{
		Config: &config.Config{
			Project: config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
			Agents:  map[string]config.AgentConfig{},
			Hub:     config.HubConfig{URL: hub.URL},
			HiveID:  "hosted-test-hive",
		},
		Logger: slog.Default(),
	}

	handler := srv.authenticate(http.HandlerFunc(srv.handleSelfUpgrade))
	req := httptest.NewRequest(http.MethodPost, "/api/self-upgrade", nil)
	req.Header.Set("X-Hive-Internal", token) // what the gateway injects
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if sawUser != "" {
		t.Errorf("relayed X-Hive-User = %q, want empty (gateway topology has no per-user identity)", sawUser)
	}
	if sawRole != "owner" {
		t.Errorf("relayed X-Hive-Role = %q, want owner", sawRole)
	}
	if sawProof != token {
		t.Errorf("relayed %s = %q, want the spoke's dashboard token", proxyAuthHeader, sawProof)
	}
}

// TestSelfUpgradeWithoutProofOrHubCookieFailsHonestly pins the honest-error
// half: a spoke with no dashboard token and no hub session cookie cannot
// possibly authenticate the relay, so it must say which credential is missing
// and how to configure it — not forward a doomed request whose 401 surfaces
// as a bare "not authenticated" toast.
func TestSelfUpgradeWithoutProofOrHubCookieFailsHonestly(t *testing.T) {
	hubCalled := false
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubCalled = true
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"not authenticated"}`))
	}))
	defer hub.Close()

	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{
		Config: &config.Config{
			Project: config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
			Agents:  map[string]config.AgentConfig{},
			Hub:     config.HubConfig{URL: hub.URL},
			HiveID:  "hosted-test-hive",
		},
		Logger: slog.Default(),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/self-upgrade", nil)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleSelfUpgrade(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "DASHBOARD_AUTH_TOKEN") {
		t.Errorf("error must name the missing credential and its config knob; got %q", body)
	}
	if strings.Contains(body, "not authenticated") {
		t.Errorf("error must not be the misleading 'not authenticated'; got %q", body)
	}
	if hubCalled {
		t.Error("spoke forwarded a credential-less request to the hub instead of failing fast")
	}
}

// TestSelfUpgradeNoProofButHubCookieStillRelays: a hub session cookie is a
// valid credential on its own (hub-domain deployments), so its presence must
// NOT trip the fail-fast even when the spoke has no dashboard token.
func TestSelfUpgradeNoProofButHubCookieStillRelays(t *testing.T) {
	sawCookie := false
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("hive_hub_user"); err == nil {
			sawCookie = true
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"upgrading"}`))
	}))
	defer hub.Close()

	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{
		Config: &config.Config{
			Project: config.ProjectConfig{Org: "testorg", Name: "test", PrimaryRepo: "testrepo"},
			Agents:  map[string]config.AgentConfig{},
			Hub:     config.HubConfig{URL: hub.URL},
			HiveID:  "hosted-test-hive",
		},
		Logger: slog.Default(),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/self-upgrade", nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "session-id"})
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.handleSelfUpgrade(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if !sawCookie {
		t.Error("hub session cookie was not relayed")
	}
}
