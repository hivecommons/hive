package dashboard

// Regression tests for #2459: the dashboard "Set ID" flow (PUT
// /api/config/github) on hosted spokes whose GitHub App private key was
// hub-delivered to the per-app-id path with key_file deliberately left empty.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
