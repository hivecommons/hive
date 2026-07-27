package hub

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ============================================================
// webhook.go
// ============================================================

func newWebhookHub() *HubServer {
	return &HubServer{
		logger:                  slog.Default(),
		pendingWebhooks:         make(map[string]*pendingWebhookEntry),
		pendingGitHubAppConfigs: make(map[string]*HeartbeatGitHubAppConfig),
		clusters:                make(map[string]ClusterConfig),
	}
}

func signWebhook(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyWebhookSignature(t *testing.T) {
	payload := []byte(`{"hello":"world"}`)
	secret := "s3cr3t"
	good := signWebhook(secret, payload)

	if !verifyWebhookSignature(payload, good, secret) {
		t.Error("valid signature should verify")
	}
	if verifyWebhookSignature(payload, good, "wrong-secret") {
		t.Error("wrong secret should fail")
	}
	if verifyWebhookSignature(payload, "md5=abc", secret) {
		t.Error("non-sha256 prefix should fail")
	}
	if verifyWebhookSignature(payload, "sha256=zzznothex", secret) {
		t.Error("non-hex signature should fail")
	}
}

func TestLoadWebhookSecretEnv(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "env-secret")
	if got := loadWebhookSecret(); got != "env-secret" {
		t.Errorf("loadWebhookSecret = %q, want env-secret", got)
	}
}

func TestLoadWebhookSecretMissing(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "")
	// File path /data/saas/... doesn't exist in test env -> "".
	if got := loadWebhookSecret(); got != "" {
		t.Errorf("loadWebhookSecret = %q, want empty", got)
	}
}

func TestHandleGitHubWebhookMissingEvent(t *testing.T) {
	s := newWebhookHub()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/github/webhook", nil)
	s.handleGitHubWebhook(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleGitHubWebhookBadSignature(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "s3cr3t")
	s := newWebhookHub()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/github/webhook", nil)
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	s.handleGitHubWebhook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHandleGitHubWebhookIgnoredEvent(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "")
	s := newWebhookHub()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/github/webhook", nil)
	req.Header.Set("X-GitHub-Event", "push")
	s.handleGitHubWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandleGitHubWebhookInstallationNoMatch(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "")
	s := newWebhookHub()
	evt := installationEvent{Action: "created"}
	evt.Installation.ID = 42
	evt.Installation.Account.Login = "acme"
	body, _ := json.Marshal(evt)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "installation")
	s.handleGitHubWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	// No matching hive -> webhook stored for deferred matching.
	s.pendingWebhooksMu.Lock()
	_, ok := s.pendingWebhooks["acme"]
	s.pendingWebhooksMu.Unlock()
	if !ok {
		t.Error("expected pending webhook stored for org acme")
	}
}

func TestHandleInstallationEventBadJSON(t *testing.T) {
	s := newWebhookHub()
	s.handleInstallationEvent([]byte(`{not json`)) // parse warn, no panic
}

func TestHandleInstallationEventIgnoredAction(t *testing.T) {
	s := newWebhookHub()
	evt := installationEvent{Action: "deleted"}
	body, _ := json.Marshal(evt)
	s.handleInstallationEvent(body) // ignored, no pending stored
	if len(s.pendingWebhooks) != 0 {
		t.Error("deleted action should not store a pending webhook")
	}
}

func TestFindHiveByOrgRepos(t *testing.T) {
	s := newWebhookHub()
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", Org: "acme", PrimaryRepo: "widgets", Repos: []string{"widgets"}},
		{ID: "h2", Org: "acme", PrimaryRepo: "gadgets"},
	}

	// Exact repo match.
	if h := s.findHiveByOrgRepos("acme", []string{"widgets"}); h == nil || h.ID != "h1" {
		t.Errorf("expected h1 for widgets, got %+v", h)
	}
	// PrimaryRepo match.
	if h := s.findHiveByOrgRepos("acme", []string{"gadgets"}); h == nil || h.ID != "h2" {
		t.Errorf("expected h2 for gadgets, got %+v", h)
	}
	// Org-only fallback (empty repos).
	if h := s.findHiveByOrgRepos("acme", nil); h == nil {
		t.Error("expected org-only fallback match")
	}
	// No match at all (unknown repo, non-empty list).
	if h := s.findHiveByOrgRepos("acme", []string{"unknown"}); h != nil {
		t.Errorf("expected nil for unknown repo, got %+v", h)
	}
	// Unknown org.
	if h := s.findHiveByOrgRepos("nobody", []string{"x"}); h != nil {
		t.Errorf("expected nil for unknown org, got %+v", h)
	}
}

func TestFindHiveByPendingInstall(t *testing.T) {
	s := newWebhookHub()
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", Org: "acme", PendingGitHubAppInstall: false},
		{ID: "h2", Org: "beta", PendingGitHubAppInstall: true},
	}
	if h := s.findHiveByPendingInstall("beta"); h == nil || h.ID != "h2" {
		t.Errorf("expected h2, got %+v", h)
	}
	if h := s.findHiveByPendingInstall("acme"); h != nil {
		t.Errorf("acme is not pending, expected nil, got %+v", h)
	}
	if h := s.findHiveByPendingInstall("nobody"); h != nil {
		t.Errorf("expected nil for unknown org")
	}
}

func TestStorePendingWebhook(t *testing.T) {
	s := newWebhookHub()
	s.storePendingWebhook("Acme", 100, 200)
	s.pendingWebhooksMu.Lock()
	pw := s.pendingWebhooks["acme"]
	s.pendingWebhooksMu.Unlock()
	if pw == nil || pw.AppID != 100 || pw.InstallationID != 200 {
		t.Errorf("unexpected pending webhook: %+v", pw)
	}
}

func TestCheckPendingWebhookMatch(t *testing.T) {
	s := newWebhookHub()
	// No pending webhook -> early return, no panic.
	s.checkPendingWebhookMatch("acme", "h1")

	// Store then match a real hive; loadAppPrivateKey hits fake kubectl (error)
	// and returns "" but the pending config is still stored.
	s.registry.Hives = []RegistryEntry{{ID: "h1", Org: "acme", ClusterID: "hive-oke"}}
	s.clusters["hive-oke"] = ClusterConfig{ID: "hive-oke"}
	s.storePendingWebhook("acme", 100, 200)
	s.checkPendingWebhookMatch("acme", "h1")

	// Pending webhook should be consumed.
	s.pendingWebhooksMu.Lock()
	_, ok := s.pendingWebhooks["acme"]
	s.pendingWebhooksMu.Unlock()
	if ok {
		t.Error("pending webhook should be consumed on match")
	}
}

func TestCheckPendingWebhookMatchExpired(t *testing.T) {
	s := newWebhookHub()
	s.pendingWebhooks["acme"] = &pendingWebhookEntry{
		Org:        "acme",
		ReceivedAt: time.Now().Add(-2 * pendingWebhookExpiry),
	}
	s.checkPendingWebhookMatch("acme", "h1") // expired -> return, entry left/removed
}

func TestCheckPendingWebhookMatchHiveNotFound(t *testing.T) {
	s := newWebhookHub()
	s.storePendingWebhook("acme", 1, 2)
	// Matching org but no such hive ID in registry.
	s.checkPendingWebhookMatch("acme", "does-not-exist")
}

func TestLoadAppPrivateKeyNoCluster(t *testing.T) {
	s := newWebhookHub()
	// ClusterID resolves to default "hive-oke" which isn't registered -> "".
	if got := s.loadAppPrivateKey(&RegistryEntry{ID: "h1"}); got != "" {
		t.Errorf("expected empty key without cluster, got %q", got)
	}
}

func TestLoadAppPrivateKeyKubectlFails(t *testing.T) {
	s := newWebhookHub()
	s.clusters["hive-oke"] = ClusterConfig{ID: "hive-oke"}
	// Fake kubectl (exit 1) -> both secret lookups fail -> "".
	if got := s.loadAppPrivateKey(&RegistryEntry{ID: "h1", ClusterID: "hive-oke"}); got != "" {
		t.Errorf("expected empty key on kubectl failure, got %q", got)
	}
}

func TestLoadSpokeAuthToken(t *testing.T) {
	s := newWebhookHub()
	// No cluster -> "".
	if got := s.loadSpokeAuthToken(&RegistryEntry{ID: "h1"}); got != "" {
		t.Errorf("expected empty token without cluster, got %q", got)
	}
	// Cluster present, kubectl fails -> "".
	s.clusters["hive-oke"] = ClusterConfig{ID: "hive-oke"}
	if got := s.loadSpokeAuthToken(&RegistryEntry{ID: "h1", ClusterID: "hive-oke"}); got != "" {
		t.Errorf("expected empty token on kubectl failure, got %q", got)
	}
}

func TestPushGitHubConfigToSpoke(t *testing.T) {
	// Spoke returns 200 on first attempt -> success path (single iteration).
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newWebhookHub()
	s.clusters["hive-oke"] = ClusterConfig{ID: "hive-oke"}
	hive := &RegistryEntry{ID: "h1", ClusterID: "hive-oke", DashboardURL: srv.URL}
	s.pushGitHubConfigToSpoke(hive, 100, 200)
	if hits != 1 {
		t.Errorf("expected 1 spoke hit, got %d", hits)
	}
}

func TestPushGitHubConfigToSpokeRejected(t *testing.T) {
	// Spoke returns 400 -> "rejected" path, returns without retry.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	s := newWebhookHub()
	s.clusters["hive-oke"] = ClusterConfig{ID: "hive-oke"}
	hive := &RegistryEntry{ID: "h1", ClusterID: "hive-oke", DashboardURL: srv.URL}
	s.pushGitHubConfigToSpoke(hive, 100, 200)
}
