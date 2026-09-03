package dashboard

// Regression tests for #2459: the dashboard "Set ID" flow (PUT
// /api/config/github) on hosted spokes whose GitHub App private key was
// hub-delivered to the per-app-id path with key_file deliberately left empty.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Live values from the spoke the bug was observed on (hosted-available-
// vllmd-06): App 5686 on github.ibm.com, installation 43021. Any real IDs
// would do; these make the scenario recognizable.
const (
	testHubDeliveredAppID          = int64(5686)
	testHubDeliveredInstallationID = 43021
)

// TestConfigGitHub_SetIDReinitsWithHubDeliveredKey drives the fleet-standard
// hosted-spoke configuration through the production handler: key_file empty,
// key present only at the per-app-id delivered path. Setting the installation
// ID must trigger the GitHub client rebuild with the RESOLVED key — before
// #2459 the reinit was gated on the raw cfg.GitHub.KeyFile and silently
// skipped, leaving the banner up and Re-check dead on a nil client.
func TestConfigGitHub_SetIDReinitsWithHubDeliveredKey(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	deps.Config.GitHub.AppID = testHubDeliveredAppID
	deps.Config.GitHub.InstallationID = 0
	deps.Config.GitHub.KeyFile = "" // hub-delivered keys never persist key_file

	// The hub-delivered per-app-id key, at a path the config does not name.
	delivered := filepath.Join(t.TempDir(), "gh-app-key-5686.pem")
	if err := os.WriteFile(delivered, []byte("-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Mirrors resolveAppKeyFile's contract (cmd/hive): an explicit config
	// value wins, otherwise the per-app-id delivered file is preferred.
	deps.ResolveAppKeyFileFunc = func(configured string, appID int64) string {
		if strings.TrimSpace(configured) != "" {
			return configured
		}
		if appID == testHubDeliveredAppID {
			return delivered
		}
		return ""
	}

	var gotAppID, gotInstallationID int64
	var gotKeyFile string
	reinitCalls := 0
	deps.ReinitGitHubFunc = func(appID, installationID int64, keyFile string) error {
		reinitCalls++
		gotAppID, gotInstallationID, gotKeyFile = appID, installationID, keyFile
		return nil
	}

	rec := doPut(s, "/api/config/github", map[string]any{"installation_id": testHubDeliveredInstallationID})
	if rec.Code != http.StatusOK {
		t.Fatalf("set id: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["reinit"] != "ok" {
		t.Fatalf("reinit not reported ok: body = %v", body)
	}
	if reinitCalls != 1 {
		t.Fatalf("ReinitGitHubFunc calls = %d, want 1 (raw key_file gate skipped the rebuild?)", reinitCalls)
	}
	if gotKeyFile != delivered {
		t.Fatalf("reinit key file = %q, want the resolved per-app-id key %q", gotKeyFile, delivered)
	}
	if gotAppID != testHubDeliveredAppID || gotInstallationID != testHubDeliveredInstallationID {
		t.Fatalf("reinit ids = (%d, %d), want (%d, %d)", gotAppID, gotInstallationID, testHubDeliveredAppID, testHubDeliveredInstallationID)
	}
}

// TestConfigGitHub_ExplicitKeyFileStillWins keeps the pre-#2459 behavior
// intact: when the operator configured a key_file, the reinit signs with that
// exact path, resolver or not.
func TestConfigGitHub_ExplicitKeyFileStillWins(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	deps.Config.GitHub.AppID = testHubDeliveredAppID
	explicit := filepath.Join(t.TempDir(), "operator-key.pem")
	deps.Config.GitHub.KeyFile = explicit

	deps.ResolveAppKeyFileFunc = func(configured string, appID int64) string {
		if strings.TrimSpace(configured) != "" {
			return configured
		}
		return "/data/should-not-be-used.pem"
	}

	var gotKeyFile string
	deps.ReinitGitHubFunc = func(appID, installationID int64, keyFile string) error {
		gotKeyFile = keyFile
		return nil
	}

	rec := doPut(s, "/api/config/github", map[string]any{"installation_id": testHubDeliveredInstallationID})
	if rec.Code != http.StatusOK {
		t.Fatalf("set id: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if gotKeyFile != explicit {
		t.Fatalf("reinit key file = %q, want the explicit %q", gotKeyFile, explicit)
	}
}

// TestConfigGitHub_UnpersistableSaveIsAnError covers the secondary #2459 bug:
// with no config source path the save was a silent no-op, yet the handler
// reported "status":"updated" — the owner's installation_id lived only in
// memory and vanished on the next restart. The handler must refuse and name
// the cause instead.
func TestConfigGitHub_UnpersistableSaveIsAnError(t *testing.T) {
	s, deps := apiServer(t)
	if deps.Config.SourcePath != "" {
		t.Fatalf("precondition: test deps config should have no source path")
	}

	reinitCalls := 0
	deps.ReinitGitHubFunc = func(appID, installationID int64, keyFile string) error {
		reinitCalls++
		return nil
	}

	rec := doPut(s, "/api/config/github", map[string]any{"installation_id": testHubDeliveredInstallationID})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unpersistable save: want 500, got %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["status"] == "updated" {
		t.Fatalf("unpersistable save must never report \"updated\": body = %v", body)
	}
	errMsg, _ := body["error"].(string)
	if !strings.Contains(errMsg, "source path") {
		t.Fatalf("error must name the cause (no source path): body = %v", body)
	}
	if deps.Config.GitHub.InstallationID != 0 {
		t.Fatalf("installation_id mutated in memory despite refused save: %d", deps.Config.GitHub.InstallationID)
	}
	if reinitCalls != 0 {
		t.Fatalf("reinit attempted despite refused save: %d calls", reinitCalls)
	}
}

func TestAutoDiscoverGitHubInstallationIDPersistsLikeManualSetID(t *testing.T) {
	ghpkg.ResetInstallationDiscoveryCache()
	s, deps := apiServer(t)
	deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	deps.Config.Project.Org = "open-source"
	deps.Config.GitHub.AppID = testHubDeliveredAppID
	deps.Config.GitHub.InstallationID = 0
	deps.Config.GitHub.KeyFile = ""

	keyFile := filepath.Join(t.TempDir(), "gh-app-key.pem")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations" && r.URL.Path != "/orgs/open-source/installation" {
			t.Fatalf("unexpected discovery path %s", r.URL.Path)
		}
		if r.URL.Path == "/orgs/open-source/installation" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": testHubDeliveredInstallationID, "account": map[string]any{"login": "open-source"}},
		})
	}))
	defer api.Close()

	auth, err := ghpkg.NewAppAuth(testHubDeliveredAppID, 0, keyFile, deps.Logger, api.URL)
	if err != nil {
		t.Fatalf("NewAppAuth: %v", err)
	}
	deps.GHAppAuth = auth
	deps.ResolveAppKeyFileFunc = func(configured string, appID int64) string {
		return keyFile
	}
	var gotAppID, gotInstallationID int64
	var gotKeyFile string
	reinitCalls := 0
	deps.ReinitGitHubFunc = func(appID, installationID int64, keyFile string) error {
		reinitCalls++
		gotAppID, gotInstallationID, gotKeyFile = appID, installationID, keyFile
		return nil
	}

	id, err := s.AutoDiscoverGitHubInstallationID(context.Background(), false)
	if err != nil {
		t.Fatalf("AutoDiscoverGitHubInstallationID: %v", err)
	}
	if id != testHubDeliveredInstallationID {
		t.Fatalf("id = %d, want %d", id, testHubDeliveredInstallationID)
	}
	if deps.Config.GitHub.InstallationID != testHubDeliveredInstallationID {
		t.Fatalf("config installation_id = %d, want %d", deps.Config.GitHub.InstallationID, testHubDeliveredInstallationID)
	}
	if reinitCalls != 1 {
		t.Fatalf("ReinitGitHubFunc calls = %d, want 1", reinitCalls)
	}
	if gotAppID != testHubDeliveredAppID || gotInstallationID != testHubDeliveredInstallationID || gotKeyFile != keyFile {
		t.Fatalf("reinit args = (%d, %d, %q), want (%d, %d, %q)",
			gotAppID, gotInstallationID, gotKeyFile, testHubDeliveredAppID, testHubDeliveredInstallationID, keyFile)
	}
}

func TestAutoDiscoverGitHubInstallationIDDerivesOrgWhenConfigOrgIsForgeHost(t *testing.T) {
	ghpkg.ResetInstallationDiscoveryCache()
	s, deps := apiServer(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s.logger = logger
	deps.Logger = logger

	deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	deps.Config.Project.Org = "github.ibm.com"
	deps.Config.Project.PrimaryRepo = "lee-cooper/toolkit"
	deps.Config.GitHub.AppID = testHubDeliveredAppID
	deps.Config.GitHub.InstallationID = 0
	deps.Config.GitHub.KeyFile = ""
	deps.Config.GitHub.BaseURL = "https://github.ibm.com"

	var queriedOrg string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/lee-cooper/installation" {
			t.Fatalf("unexpected discovery path %s", r.URL.Path)
		}
		queriedOrg = "lee-cooper"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": testHubDeliveredInstallationID,
			"account": map[string]any{
				"login": "lee-cooper",
				"type":  "Organization",
			},
		})
	}))
	defer api.Close()
	deps.Config.GitHub.APIURL = api.URL

	keyFile := writeTestAppKey(t)
	auth, err := ghpkg.NewAppAuth(testHubDeliveredAppID, 0, keyFile, deps.Logger, api.URL)
	if err != nil {
		t.Fatalf("NewAppAuth: %v", err)
	}
	deps.GHAppAuth = auth
	deps.ResolveAppKeyFileFunc = func(configured string, appID int64) string { return keyFile }
	deps.ReinitGitHubFunc = func(appID, installationID int64, keyFile string) error { return nil }

	id, err := s.AutoDiscoverGitHubInstallationID(context.Background(), false)
	if err != nil {
		t.Fatalf("AutoDiscoverGitHubInstallationID: %v", err)
	}
	if id != testHubDeliveredInstallationID || queriedOrg != "lee-cooper" {
		t.Fatalf("discovery id/org = (%d, %q), want (%d, lee-cooper)", id, queriedOrg, testHubDeliveredInstallationID)
	}
	if got := logs.String(); !strings.Contains(got, "configured_org=github.ibm.com") || !strings.Contains(got, "derived_org=lee-cooper") {
		t.Fatalf("warning log = %q, want configured and derived orgs", got)
	}
}
func TestGitHubSetupCallback_ValidInstallConfigures(t *testing.T) {
	s, deps, api := setupCallbackServer(t, "myorg")
	defer api.Close()

	var gotInstallationID int64
	deps.ReinitGitHubFunc = func(appID, installationID int64, keyFile string) error {
		gotInstallationID = installationID
		return nil
	}

	rec := doGet(s, "/gh-setup?setup_action=install&installation_id=4242")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup callback: want 303, got %d (%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/?ghSetup=ok" {
		t.Fatalf("redirect = %q, want /?ghSetup=ok", loc)
	}
	if deps.Config.GitHub.InstallationID != 4242 {
		t.Fatalf("installation_id = %d, want 4242", deps.Config.GitHub.InstallationID)
	}
	if gotInstallationID != 4242 {
		t.Fatalf("reinit installation_id = %d, want 4242", gotInstallationID)
	}
	if entries := s.GetAudit().Recent(1); len(entries) != 1 || entries[0].Action != "config_github" {
		t.Fatalf("audit entry = %#v, want config_github", entries)
	}
}

func TestGitHubSetupCallbackDerivesOrgWhenConfigOrgIsForgeHost(t *testing.T) {
	s, deps, api := setupCallbackServer(t, "lee-cooper")
	defer api.Close()
	deps.Config.Project.Org = "github.ibm.com"
	deps.Config.Project.PrimaryRepo = "lee-cooper/toolkit"
	deps.Config.GitHub.BaseURL = "https://github.ibm.com"

	rec := doGet(s, "/gh-setup?setup_action=install&installation_id=4242")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup callback: want 303, got %d (%s)", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/?ghSetup=ok" {
		t.Fatalf("redirect = %q, want /?ghSetup=ok", loc)
	}
	if deps.Config.GitHub.InstallationID != 4242 {
		t.Fatalf("installation_id = %d, want 4242", deps.Config.GitHub.InstallationID)
	}
}

func TestGitHubSetupCallback_RequestDoesNotConfigure(t *testing.T) {
	s, deps, api := setupCallbackServer(t, "myorg")
	defer api.Close()

	rec := doGet(s, "/gh-setup?setup_action=request")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup request: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/?ghSetup=requested" {
		t.Fatalf("redirect = %q, want /?ghSetup=requested", loc)
	}
	if deps.Config.GitHub.InstallationID != 0 {
		t.Fatalf("installation_id = %d, want unchanged 0", deps.Config.GitHub.InstallationID)
	}
}

func TestGitHubSetupCallback_MismatchedOrgRejected(t *testing.T) {
	s, deps, api := setupCallbackServer(t, "other-org")
	defer api.Close()

	rec := doGet(s, "/gh-setup?setup_action=install&installation_id=4242")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup callback: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/?ghSetup=error" {
		t.Fatalf("redirect = %q, want /?ghSetup=error", loc)
	}
	if deps.Config.GitHub.InstallationID != 0 {
		t.Fatalf("installation_id = %d, want unchanged 0", deps.Config.GitHub.InstallationID)
	}
}

func TestGitHubSetupCallback_NonNumericIDRejected(t *testing.T) {
	s, deps, api := setupCallbackServer(t, "myorg")
	defer api.Close()

	rec := doGet(s, "/gh-setup?setup_action=install&installation_id=abc")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup callback: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/?ghSetup=error" {
		t.Fatalf("redirect = %q, want /?ghSetup=error", loc)
	}
	if deps.Config.GitHub.InstallationID != 0 {
		t.Fatalf("installation_id = %d, want unchanged 0", deps.Config.GitHub.InstallationID)
	}
}

func TestGitHubSetupCallback_AlreadyConfiguredUnauthenticatedRejected(t *testing.T) {
	s, deps, api := setupCallbackServer(t, "myorg")
	defer api.Close()
	deps.Config.GitHub.InstallationID = 1111

	rec := doGet(s, "/gh-setup?setup_action=install&installation_id=4242")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup callback: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/?ghSetup=error" {
		t.Fatalf("redirect = %q, want /?ghSetup=error", loc)
	}
	if deps.Config.GitHub.InstallationID != 1111 {
		t.Fatalf("installation_id = %d, want unchanged 1111", deps.Config.GitHub.InstallationID)
	}
}

func TestGitHubSetupCallback_UpdateForSameInstallationAllowed(t *testing.T) {
	s, deps, api := setupCallbackServer(t, "myorg")
	defer api.Close()
	deps.Config.GitHub.InstallationID = 4242

	rec := doGet(s, "/gh-setup?setup_action=update&installation_id=4242")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup callback: want 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/?ghSetup=ok" {
		t.Fatalf("redirect = %q, want /?ghSetup=ok", loc)
	}
	if deps.Config.GitHub.InstallationID != 4242 {
		t.Fatalf("installation_id = %d, want unchanged 4242", deps.Config.GitHub.InstallationID)
	}
}

func setupCallbackServer(t *testing.T, accountLogin string) (*Server, *Dependencies, *httptest.Server) {
	t.Helper()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/app/installations/4242" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 4242,
			"account": map[string]any{
				"login": accountLogin,
				"type":  "Organization",
			},
		})
	}))

	s, deps := apiServer(t)
	deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	deps.Config.Project.Org = "myorg"
	deps.Config.GitHub.AppID = testHubDeliveredAppID
	deps.Config.GitHub.InstallationID = 0
	deps.Config.GitHub.APIURL = api.URL
	deps.Config.GitHub.KeyFile = writeTestAppKey(t)
	return s, deps, api
}

func writeTestAppKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "gh-app-key.pem")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("creating key file: %v", err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		_ = f.Close()
		t.Fatalf("writing key PEM: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing key file: %v", err)
	}
	return path
}
