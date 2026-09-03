package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/openrouter"
)

// ============================================================
// openrouter.go
// ============================================================

func newORHub() *HubServer {
	return &HubServer{
		logger:          slog.Default(),
		hubSecret:       testHubSecret,
		pendingGateways: make(map[string]*HeartbeatGatewayConfig),
	}
}

// withCookieUser sets a signed hive_hub_user cookie for a user that exists on
// disk.
func withCookieUser(req *http.Request, username string) {
	req.AddCookie(testAuthCookie(username))
}

func TestPendingGatewayLifecycle(t *testing.T) {
	s := newORHub()
	if s.hasPendingGateway("h1") {
		t.Error("no gateway should be pending initially")
	}
	if got := s.pendingGatewayFor("h1", nil); got != nil {
		t.Error("fetch on empty should be nil")
	}

	gw := &HeartbeatGatewayConfig{Name: "openrouter", Key: "k"}
	s.queuePendingGateway("h1", gw)
	if !s.hasPendingGateway("h1") {
		t.Error("gateway should be pending after queue")
	}
	got := s.pendingGatewayFor("h1", nil)
	if got == nil || got.Key != "k" {
		t.Errorf("fetch = %+v, want key=k", got)
	}
	// This assertion is INVERTED from what it used to be. It previously required
	// the record to be gone after a single fetch, which is what made a funded
	// gateway at-most-once: one lost beat and the user's money bought nothing.
	// It must now survive until the spoke reports the gateway back.
	if !s.hasPendingGateway("h1") {
		t.Error("gateway was drained on send; it must persist until the spoke confirms it")
	}
	if got := s.pendingGatewayFor("h1", []string{"openrouter"}); got != nil {
		t.Error("still offered after the spoke confirmed it")
	}
	if s.hasPendingGateway("h1") {
		t.Error("gateway should be cleared once confirmed")
	}
}

func TestOwnsHiveForFunding(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// Admin always allowed.
	s := newORHub()
	if !s.ownsHiveForFunding(hubAdminUsername, "any-hive") {
		t.Error("admin should own any hive for funding")
	}

	// Owner via SaaSHive record.
	h := &SaaSHive{ID: "h1", Owner: "alice"}
	if err := saveSaaSHive(h); err != nil {
		t.Fatalf("saveSaaSHive: %v", err)
	}
	if !s.ownsHiveForFunding("alice", "h1") {
		t.Error("owner should own hive for funding")
	}

	// Grantee via SaaSUser.Hives role.
	u := &SaaSUser{GitHubUsername: "bob", Hives: map[string]string{"h1": "read-write"}}
	if err := saveSaaSUser(u); err != nil {
		t.Fatalf("saveSaaSUser: %v", err)
	}
	if !s.ownsHiveForFunding("bob", "h1") {
		t.Error("read-write grantee should own hive for funding")
	}

	// Unrelated user.
	if s.ownsHiveForFunding("carol", "h1") {
		t.Error("unrelated user should NOT own hive for funding")
	}
}

func TestHandleHubOpenRouterStartValidation(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newORHub()

	// Missing hive_id -> 400.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/openrouter/connect/start", nil)
	s.handleHubOpenRouterStart(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing hive_id status = %d, want 400", rec.Code)
	}

	// hive_id present but not owner -> 403 (no auth user).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/openrouter/connect/start?hive_id=h1", nil)
	s.handleHubOpenRouterStart(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-owner status = %d, want 403", rec.Code)
	}
}

func TestHandleHubOpenRouterStartSuccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newORHub()

	// Admin user exists on disk so getAuthUser resolves it.
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: hubAdminUsername, Hives: map[string]string{}}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/openrouter/connect/start?hive_id=h1&model=x/y", nil)
	withCookieUser(req, hubAdminUsername)
	s.handleHubOpenRouterStart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["authorize_url"] == nil || resp["state"] == nil {
		t.Errorf("expected authorize_url and state, got %+v", resp)
	}
}

func TestHandleHubOpenRouterQR(t *testing.T) {
	s := newORHub()

	// Non-OpenRouter data -> 400.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/openrouter/qr?data=https://evil.com", nil)
	s.handleHubOpenRouterQR(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("evil data status = %d, want 400", rec.Code)
	}

	// Valid OpenRouter authorize URL -> PNG.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/openrouter/qr?data="+openrouter.AuthURL+"?x=1", nil)
	s.handleHubOpenRouterQR(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid QR status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("content-type = %q, want image/png", ct)
	}
}

func TestHandleHubOpenRouterModels(t *testing.T) {
	s := newORHub()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/openrouter/models", nil)
	s.handleHubOpenRouterModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := resp["suggested"]; !ok {
		t.Error("expected suggested models in response")
	}
}

func TestHandleHubOpenRouterCredit(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newORHub()

	// Missing hive_id -> 400.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/openrouter/credit", nil)
	s.handleHubOpenRouterCredit(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing hive_id status = %d, want 400", rec.Code)
	}

	// Not owner -> 403.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/openrouter/credit?hive_id=h1", nil)
	s.handleHubOpenRouterCredit(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-owner status = %d, want 403", rec.Code)
	}

	// Admin owner, with a pending gateway -> pending_delivery true.
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: hubAdminUsername, Hives: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	s.queuePendingGateway("h1", &HeartbeatGatewayConfig{Name: "openrouter"})
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/openrouter/credit?hive_id=h1", nil)
	withCookieUser(req, hubAdminUsername)
	s.handleHubOpenRouterCredit(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner status = %d", rec.Code)
	}
	var resp map[string]bool
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp["pending_delivery"] {
		t.Error("expected pending_delivery true")
	}
}

func TestHandleHubOpenRouterCallbackErrors(t *testing.T) {
	s := newORHub()

	// Missing code/state -> redirect to error.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, hubOpenRouterCallbackPath, nil)
	s.handleHubOpenRouterCallback(rec, req)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "error") {
		t.Errorf("missing params: status=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}

	// Unknown state -> error redirect.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, hubOpenRouterCallbackPath+"?code=c&state=unknown", nil)
	s.handleHubOpenRouterCallback(rec, req)
	if !strings.Contains(rec.Header().Get("Location"), "error") {
		t.Errorf("unknown state should redirect to error, got %q", rec.Header().Get("Location"))
	}
}

func TestHandleHubOpenRouterCallbackExchangeFails(t *testing.T) {
	s := newORHub()

	// Point the key-exchange endpoint at a local server that returns 500 so the
	// exchange fails deterministically (no real network).
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fail.Close()
	oldURL := openrouter.KeyExchangeURL
	openrouter.KeyExchangeURL = fail.URL
	defer func() { openrouter.KeyExchangeURL = oldURL }()

	verifier, _, err := openrouter.GeneratePKCE()
	if err != nil {
		t.Fatalf("pkce: %v", err)
	}
	state, err := s.openRouterState().Create(verifier, "h1", "model-x")
	if err != nil {
		t.Fatalf("create state: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, hubOpenRouterCallbackPath+"?code=badcode&state="+state, nil)
	s.handleHubOpenRouterCallback(rec, req)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "error") {
		t.Errorf("failed exchange should redirect to error: status=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestHandleHubOpenRouterCallbackSuccess(t *testing.T) {
	s := newORHub()

	// Key-exchange endpoint returns a valid key -> gateway queued + connected.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"key":"sk-or-test-key"}`))
	}))
	defer ok.Close()
	oldURL := openrouter.KeyExchangeURL
	openrouter.KeyExchangeURL = ok.URL
	defer func() { openrouter.KeyExchangeURL = oldURL }()

	verifier, _, err := openrouter.GeneratePKCE()
	if err != nil {
		t.Fatalf("pkce: %v", err)
	}
	// Empty model on flow -> exercises the DefaultModel fallback branch.
	state, err := s.openRouterState().Create(verifier, "h1", "")
	if err != nil {
		t.Fatalf("create state: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, hubOpenRouterCallbackPath+"?code=goodcode&state="+state, nil)
	s.handleHubOpenRouterCallback(rec, req)

	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "connected") {
		t.Fatalf("success should redirect to connected: status=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	if !s.hasPendingGateway("h1") {
		t.Error("expected funded gateway queued for h1")
	}
}

func TestFetchOpenRouterModels(t *testing.T) {
	// Best-effort; returns a slice or nil, never panics.
	_ = fetchOpenRouterModels()
}

func TestRegisterOpenRouterRoutes(t *testing.T) {
	s := &HubServer{
		logger:          slog.Default(),
		mux:             http.NewServeMux(),
		pendingGateways: make(map[string]*HeartbeatGatewayConfig),
	}
	s.registerOpenRouterRoutes() // exercises route registration wiring
}
