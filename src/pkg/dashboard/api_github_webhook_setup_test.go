package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	if rec := post(`{"scope":"organization"}`); rec.Code != http.StatusPreconditionFailed {
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

// webhookGitHubStub serves the installation-token mint plus the org/repo
// hooks list+create endpoints, remembering created hooks per path.
type webhookGitHubStub struct {
	mu    sync.Mutex
	hooks map[string][]map[string]any
	posts map[string]int
}

func (g *webhookGitHubStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case strings.HasSuffix(path, "/access_tokens"):
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_webhook_stub","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	case strings.HasSuffix(path, "/hooks") && r.Method == http.MethodGet:
		list := g.hooks[path]
		if list == nil {
			list = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(list)
	case strings.HasSuffix(path, "/hooks") && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.posts[path]++
		hook := map[string]any{"id": 4200 + len(g.hooks[path]) + g.posts[path], "events": body["events"], "config": body["config"], "active": true}
		g.hooks[path] = append(g.hooks[path], hook)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(hook)
	default:
		http.NotFound(w, r)
	}
}

func (g *webhookGitHubStub) postCount(path string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.posts[path]
}

func (g *webhookGitHubStub) totalPosts() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.posts {
		n += c
	}
	return n
}

func newWebhookCreateServer(t *testing.T, perms map[string]string) (*Server, *webhookGitHubStub) {
	t.Helper()
	stub := &webhookGitHubStub{hooks: map[string][]map[string]any{}, posts: map[string]int{}}
	api := httptest.NewServer(stub)
	t.Cleanup(api.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyFile := filepath.Join(t.TempDir(), "app-key.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	auth, err := ghpkg.NewAppAuthWithCache(1, 2, keyFile, filepath.Join(t.TempDir(), "token.cache"),
		slog.New(slog.NewTextHandler(io.Discard, nil)), api.URL)
	if err != nil {
		t.Fatalf("NewAppAuthWithCache: %v", err)
	}

	srv := newFullServer(t)
	srv.deps.Config.Dashboard.PublicURL = "https://hive.example.org"
	srv.deps.Config.Project.Org = "projectbluefin"
	srv.deps.Config.Project.Repos = []string{"bluefin", "projectbluefin/common"}
	srv.deps.GHAppAuth = auth
	orig := webhookPermissionProbe
	t.Cleanup(func() { webhookPermissionProbe = orig })
	webhookPermissionProbe = func(context.Context, *ghpkg.AppAuth) (*ghpkg.InstallationInfo, error) {
		return &ghpkg.InstallationInfo{Permissions: perms}, nil
	}
	return srv, stub
}

func postWebhookCreate(srv *Server, payload string) (*httptest.ResponseRecorder, ghpkg.WebhookCreateResult) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config/github/webhooks/create", strings.NewReader(payload))
	markOwnerRequest(req)
	srv.mux.ServeHTTP(rec, req)
	var res ghpkg.WebhookCreateResult
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &res)
	}
	return rec, res
}

func TestWebhookCreate_NoSecretIs409(t *testing.T) {
	t.Setenv(ghpkg.WebhookSecretEnvVar, "")
	restore := ghpkg.SetWebhookSecretPathForTest(filepath.Join(t.TempDir(), "absent.key"))
	defer restore()
	srv, stub := newWebhookCreateServer(t, map[string]string{ghpkg.PermissionOrganizationHooks: "write"})
	if rec, _ := postWebhookCreate(srv, `{"scope":"org"}`); rec.Code != http.StatusConflict {
		t.Fatalf("no secret: %d %s, want 409", rec.Code, rec.Body.String())
	}
	if stub.totalPosts() != 0 {
		t.Fatal("GitHub hook created without a secret")
	}
}

func TestWebhookCreate_MissingPermissionIs412(t *testing.T) {
	t.Setenv(ghpkg.WebhookSecretEnvVar, "mounted")
	srv, stub := newWebhookCreateServer(t, map[string]string{ghpkg.PermissionOrganizationHooks: "read"})
	rec, _ := postWebhookCreate(srv, `{"scope":"org"}`)
	if rec.Code != http.StatusPreconditionFailed || !strings.Contains(rec.Body.String(), "organization_hooks") {
		t.Fatalf("org without write: %d %s, want 412 naming organization_hooks", rec.Code, rec.Body.String())
	}
	rec, _ = postWebhookCreate(srv, `{"scope":"repo","repo":"projectbluefin/bluefin"}`)
	if rec.Code != http.StatusPreconditionFailed || !strings.Contains(rec.Body.String(), "repository_hooks") {
		t.Fatalf("repo without write: %d %s, want 412 naming repository_hooks", rec.Code, rec.Body.String())
	}
	if stub.totalPosts() != 0 {
		t.Fatal("GitHub hook created without the permission")
	}
}

// First call creates the org hook (audited); the second returns it instead
// of creating a duplicate.
func TestWebhookCreate_OrgCreatesThenReturnsExisting(t *testing.T) {
	t.Setenv(ghpkg.WebhookSecretEnvVar, "mounted")
	srv, stub := newWebhookCreateServer(t, map[string]string{ghpkg.PermissionOrganizationHooks: "write"})

	rec, first := postWebhookCreate(srv, `{"scope":"org"}`)
	if rec.Code != http.StatusOK || !first.Created || first.ID == 0 || len(first.Events) != len(ghpkg.WebhookEvents) {
		t.Fatalf("first create: %d %s", rec.Code, rec.Body.String())
	}
	if first.PayloadURL != "https://hive.example.org"+githubWebhookPath {
		t.Fatalf("payload url = %q", first.PayloadURL)
	}
	rec, second := postWebhookCreate(srv, `{"scope":"org"}`)
	if rec.Code != http.StatusOK || second.Created || second.ID != first.ID {
		t.Fatalf("second create: %d %s, want existing hook %d", rec.Code, rec.Body.String(), first.ID)
	}
	if n := stub.postCount("/orgs/projectbluefin/hooks"); n != 1 {
		t.Fatalf("org POSTs = %d, want 1", n)
	}
	var audited int
	for _, e := range srv.audit.Recent(50) {
		if e.Action == auditWebhookCreated {
			audited++
		}
	}
	if audited != 1 {
		t.Fatalf("audit entries = %d, want 1 (only the real creation)", audited)
	}
}

func TestWebhookCreate_RepoScopeHitsRepoEndpoint(t *testing.T) {
	t.Setenv(ghpkg.WebhookSecretEnvVar, "mounted")
	srv, stub := newWebhookCreateServer(t, map[string]string{ghpkg.PermissionRepositoryHooks: "write"})
	rec, res := postWebhookCreate(srv, `{"scope":"repo","repo":"projectbluefin/common"}`)
	if rec.Code != http.StatusOK || !res.Created || res.Target != "projectbluefin/common" {
		t.Fatalf("repo create: %d %s", rec.Code, rec.Body.String())
	}
	if stub.postCount("/repos/projectbluefin/common/hooks") != 1 || stub.postCount("/orgs/projectbluefin/hooks") != 0 {
		t.Fatal("want exactly one POST, to the repo endpoint")
	}
}
