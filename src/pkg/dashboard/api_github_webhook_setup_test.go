package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func webhookSetupGet(t *testing.T, s *Server, owner bool, host string) (*httptest.ResponseRecorder, webhookSetupResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config/github/webhooks", nil)
	if host != "" {
		req.Host = host
	}
	if owner {
		markOwnerRequest(req)
	}
	s.mux.ServeHTTP(rec, req)
	var body webhookSetupResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rec, body
}

func TestWebhookSetup_OwnerOnly(t *testing.T) {
	srv := newFullServer(t)
	rec, _ := webhookSetupGet(t, srv, false, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner GET: %d, want 403", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/github/webhooks/secret", nil)
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner generate: %d, want 403", rec.Code)
	}
}

// The GET describes the setup without ever carrying the secret; the generate
// call returns it once and flips the verification mode to github-signed.
func TestWebhookSetup_PayloadURLSecretLifecycle(t *testing.T) {
	t.Setenv(ghpkg.WebhookSecretEnvVar, "")
	restore := ghpkg.SetWebhookSecretPathForTest(filepath.Join(t.TempDir(), "secret.key"))
	defer restore()
	restoreRelay := ghpkg.SetWebhookRelayKeyPathForTest(filepath.Join(t.TempDir(), "relay.key"))
	defer restoreRelay()
	srv := newFullServer(t)
	srv.deps.Config.Dashboard.PublicURL = "https://hive.example.org/"

	rec, body := webhookSetupGet(t, srv, true, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	if body.PayloadURL != "https://hive.example.org"+githubWebhookPath {
		t.Fatalf("payload_url = %q", body.PayloadURL)
	}
	if body.PayloadURLSource != payloadURLSourceConfig {
		t.Fatalf("payload_url_source = %q", body.PayloadURLSource)
	}
	if body.SecretConfigured || body.SecretSource != "" || !body.CanGenerateSecret {
		t.Fatalf("fresh hive secret state: configured=%v source=%q canGenerate=%v", body.SecretConfigured, body.SecretSource, body.CanGenerateSecret)
	}
	if body.Verification != ghpkg.WebhookVerificationNone || body.Hosted {
		t.Fatalf("verification=%q hosted=%v, want none/false", body.Verification, body.Hosted)
	}
	if body.CreateAvailable {
		t.Fatal("create must not be offered without a secret")
	}
	if len(body.Events) == 0 || body.Events[0] != "check_run" || body.ContentType != "application/json" {
		t.Fatalf("events/content type: %v %q", body.Events, body.ContentType)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/github/webhooks/secret", nil)
	markOwnerRequest(req)
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate: %d %s", rec.Code, rec.Body.String())
	}
	var gen map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &gen)
	secret, _ := gen["secret"].(string)
	if len(secret) != 64 || secret != ghpkg.WebhookSecret() {
		t.Fatalf("generated secret not returned/stored: %q", secret)
	}
	if gen["verification"] != ghpkg.WebhookVerificationGitHub {
		t.Fatalf("verification after generate = %v", gen["verification"])
	}

	rec, body = webhookSetupGet(t, srv, true, "")
	if !body.SecretConfigured || body.SecretSource != ghpkg.WebhookSecretSourceFile || body.Verification != ghpkg.WebhookVerificationGitHub {
		t.Fatalf("after generate: configured=%v source=%q verification=%q", body.SecretConfigured, body.SecretSource, body.Verification)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("GET must never echo the secret")
	}
}

func TestWebhookSetup_EnvSecretBlocksGenerate(t *testing.T) {
	t.Setenv(ghpkg.WebhookSecretEnvVar, "mounted")
	srv := newFullServer(t)
	_, body := webhookSetupGet(t, srv, true, "")
	if body.SecretSource != ghpkg.WebhookSecretSourceEnv || body.CanGenerateSecret {
		t.Fatalf("env secret: source=%q canGenerate=%v", body.SecretSource, body.CanGenerateSecret)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/github/webhooks/secret", nil)
	markOwnerRequest(req)
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("generate with env secret: %d, want 409", rec.Code)
	}
}

func TestWebhookSetup_PayloadURLFallsBackToRequestHost(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Dashboard.PublicURL = ""
	srv.deps.Config.Hub.DashboardURL = ""
	_, body := webhookSetupGet(t, srv, true, "hive.dan.example")
	if body.PayloadURL != "https://hive.dan.example"+githubWebhookPath || body.PayloadURLSource != payloadURLSourceRequest {
		t.Fatalf("payload_url=%q source=%q", body.PayloadURL, body.PayloadURLSource)
	}
	_, body = webhookSetupGet(t, srv, true, "localhost:8080")
	if body.PayloadURL != "" || body.PayloadURLSource != payloadURLSourceNone {
		t.Fatalf("loopback host must not be offered: %q %q", body.PayloadURL, body.PayloadURLSource)
	}
}

// Create is offered only when the App holds a hooks write permission, and
// the create endpoint refuses scopes the installation cannot manage.
func TestWebhookSetup_CreateGatedOnAppPermission(t *testing.T) {
	t.Setenv(ghpkg.WebhookSecretEnvVar, "mounted")
	srv := newFullServer(t)
	srv.deps.Config.Dashboard.PublicURL = "https://hive.example.org"
	srv.deps.Config.Project.Org = "projectbluefin"
	srv.deps.Config.Project.Repos = []string{"bluefin", "projectbluefin/common"}
	srv.deps.GHAppAuth = &ghpkg.AppAuth{}
	orig := webhookPermissionProbe
	defer func() { webhookPermissionProbe = orig }()
	webhookPermissionProbe = func(context.Context, *ghpkg.AppAuth) (*ghpkg.InstallationInfo, error) {
		return &ghpkg.InstallationInfo{Permissions: map[string]string{ghpkg.PermissionRepositoryHooks: "write"}}, nil
	}

	_, body := webhookSetupGet(t, srv, true, "")
	if body.AppOrgHooks || !body.AppRepoHooks || !body.CreateAvailable {
		t.Fatalf("org=%v repo=%v create=%v", body.AppOrgHooks, body.AppRepoHooks, body.CreateAvailable)
	}
	if len(body.Repos) != 2 || body.Repos[1] != "common" {
		t.Fatalf("repos = %v, want bare names", body.Repos)
	}

	post := func(payload string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/config/github/webhooks/create", strings.NewReader(payload))
		markOwnerRequest(req)
		srv.mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(`{"scope":"organization"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("org scope without organization_hooks: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"scope":"repository","repo":"not-ours"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown repo: %d %s", rec.Code, rec.Body.String())
	}

	webhookPermissionProbe = func(context.Context, *ghpkg.AppAuth) (*ghpkg.InstallationInfo, error) {
		return &ghpkg.InstallationInfo{Permissions: map[string]string{}}, nil
	}
	_, body = webhookSetupGet(t, srv, true, "")
	if body.CreateAvailable {
		t.Fatal("create offered without any hooks permission")
	}
}
