package dashboard

// Branch coverage for AutoDiscoverGitHubInstallationID (pkg/dashboard/api.go):
// the guard returns, the force re-discovery path, the discovery-error path,
// and both rollback paths (persist failure, reinit failure). The happy path
// and the forge-host org derivation are covered in api_config_github_test.go.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// autoDiscoverServer wires an apiServer with App auth pointed at handler.
// Every test resets the package-level discovery cache first so memoised
// results from other tests cannot leak in.
func autoDiscoverServer(t *testing.T, handler http.HandlerFunc) (*Server, *Dependencies, *httptest.Server) {
	t.Helper()
	ghpkg.ResetInstallationDiscoveryCache()
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)

	s, deps := apiServer(t)
	deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	deps.Config.Project.Org = "open-source"
	deps.Config.GitHub.AppID = testHubDeliveredAppID
	deps.Config.GitHub.InstallationID = 0
	deps.Config.GitHub.KeyFile = ""

	keyFile := writeTestAppKey(t)
	auth, err := ghpkg.NewAppAuth(testHubDeliveredAppID, 0, keyFile, deps.Logger, api.URL)
	if err != nil {
		t.Fatalf("NewAppAuth: %v", err)
	}
	deps.GHAppAuth = auth
	deps.ResolveAppKeyFileFunc = func(configured string, appID int64) string { return keyFile }
	deps.ReinitGitHubFunc = func(appID, installationID int64, keyFile string) error { return nil }
	return s, deps, api
}

// discoveredInstallationHandler answers both discovery endpoints with a
// single installation owned by "open-source".
func discoveredInstallationHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/open-source/installation":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id": 43021, "account": {"login": "open-source", "type": "Organization"}}`))
		case "/app/installations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id": 43021, "account": {"login": "open-source"}}]`))
		default:
			t.Errorf("unexpected discovery path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}
}

// TestAutoDiscoverSkipsWhenInstallationIDAlreadySet: a configured
// installation_id means there is nothing to discover — the function must
// return (0, nil) without a single API call.
func TestAutoDiscoverSkipsWhenInstallationIDAlreadySet(t *testing.T) {
	var calls atomic.Int64
	s, deps, _ := autoDiscoverServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	})
	deps.Config.GitHub.InstallationID = 1111

	id, err := s.AutoDiscoverGitHubInstallationID(context.Background(), false)
	if id != 0 || err != nil {
		t.Fatalf("already-configured guard: got (%d, %v), want (0, nil)", id, err)
	}
	if deps.Config.GitHub.InstallationID != 1111 {
		t.Fatalf("installation_id = %d, want untouched 1111", deps.Config.GitHub.InstallationID)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("discovery API calls = %d, want 0", n)
	}
}

// TestAutoDiscoverSkipsWhenNoOrgConfigured: with no project org there is no
// account to discover against; the function returns (0, nil) silently.
func TestAutoDiscoverSkipsWhenNoOrgConfigured(t *testing.T) {
	var calls atomic.Int64
	s, deps, _ := autoDiscoverServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	})
	deps.Config.Project.Org = ""
	deps.Config.Project.PrimaryRepo = ""
	deps.Config.Project.Repos = nil

	id, err := s.AutoDiscoverGitHubInstallationID(context.Background(), false)
	if id != 0 || err != nil {
		t.Fatalf("empty-org guard: got (%d, %v), want (0, nil)", id, err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("discovery API calls = %d, want 0", n)
	}
}

// TestAutoDiscoverNilContextDefaultsToBackground: callers with no request
// context pass nil; the function must substitute context.Background() and
// complete discovery rather than panic or fail.
func TestAutoDiscoverNilContextDefaultsToBackground(t *testing.T) {
	s, deps, _ := autoDiscoverServer(t, discoveredInstallationHandler(t))

	//lint:ignore SA1012 the nil-context branch is exactly what is under test
	id, err := s.AutoDiscoverGitHubInstallationID(nil, false) //nolint:staticcheck
	if err != nil {
		t.Fatalf("AutoDiscoverGitHubInstallationID(nil ctx): %v", err)
	}
	if id != testHubDeliveredInstallationID {
		t.Fatalf("id = %d, want %d", id, testHubDeliveredInstallationID)
	}
	if deps.Config.GitHub.InstallationID != testHubDeliveredInstallationID {
		t.Fatalf("config installation_id = %d, want %d", deps.Config.GitHub.InstallationID, testHubDeliveredInstallationID)
	}
}

// TestAutoDiscoverErrorIsSoftAndLeavesConfigUntouched: a failing discovery
// (App not installed anywhere) returns the error, warns, and must not mutate
// the config — the manual installation-ID flow stays available.
// A follow-up call with force=false must be answered from the negative cache,
// and force=true must forget that cache entry and re-discover.
func TestAutoDiscoverErrorIsSoftAndForceRetriesAfterNegativeCache(t *testing.T) {
	var installed atomic.Bool
	var calls atomic.Int64
	success := discoveredInstallationHandler(t)
	s, deps, _ := autoDiscoverServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if installed.Load() {
			success(w, r)
			return
		}
		// App not installed on the org: 404 on the direct lookup, empty
		// installation listing on the fallback walk.
		if r.URL.Path == "/app/installations" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.NotFound(w, r)
	})
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s.logger = logger

	id, err := s.AutoDiscoverGitHubInstallationID(context.Background(), false)
	if err == nil {
		t.Fatal("discovery against an uninstalled App must return an error")
	}
	if id != 0 {
		t.Fatalf("id = %d, want 0 on discovery failure", id)
	}
	if deps.Config.GitHub.InstallationID != 0 {
		t.Fatalf("installation_id = %d, want untouched 0 after soft failure", deps.Config.GitHub.InstallationID)
	}
	if !strings.Contains(logs.String(), "auto-discovery failed") {
		t.Fatalf("warn log = %q, want the auto-discovery failure warning", logs.String())
	}

	// The org admin installs the App now — but the negative result is cached,
	// so a non-forced retry must NOT hit the API again.
	installed.Store(true)
	before := calls.Load()
	if _, err := s.AutoDiscoverGitHubInstallationID(context.Background(), false); err == nil {
		t.Fatal("non-forced retry must be served the cached negative result")
	}
	if calls.Load() != before {
		t.Fatalf("non-forced retry hit the API (%d calls, want %d)", calls.Load(), before)
	}

	// force=true forgets the cached negative entry and re-discovers.
	id, err = s.AutoDiscoverGitHubInstallationID(context.Background(), true)
	if err != nil {
		t.Fatalf("forced retry: %v", err)
	}
	if id != testHubDeliveredInstallationID {
		t.Fatalf("forced retry id = %d, want %d", id, testHubDeliveredInstallationID)
	}
	if deps.Config.GitHub.InstallationID != testHubDeliveredInstallationID {
		t.Fatalf("config installation_id = %d, want %d", deps.Config.GitHub.InstallationID, testHubDeliveredInstallationID)
	}
}

// TestAutoDiscoverRollsBackWhenPersistFails: a discovered ID that cannot be
// persisted (no config source path) must be rolled back in memory — a value
// that survives only until restart is worse than the manual flow.
func TestAutoDiscoverRollsBackWhenPersistFails(t *testing.T) {
	s, deps, _ := autoDiscoverServer(t, discoveredInstallationHandler(t))
	deps.Config.SourcePath = ""

	id, err := s.AutoDiscoverGitHubInstallationID(context.Background(), false)
	if err == nil {
		t.Fatal("unpersistable discovery must return an error")
	}
	if id != 0 {
		t.Fatalf("id = %d, want 0 when persist fails", id)
	}
	if deps.Config.GitHub.InstallationID != 0 {
		t.Fatalf("installation_id = %d, want rolled back to 0", deps.Config.GitHub.InstallationID)
	}
}

// TestAutoDiscoverRollsBackWhenReinitFails: when the GitHub client rebuild
// fails after the discovered ID was saved, the ID must be rolled back both in
// memory and on disk, and the error must name the reinit as the cause.
func TestAutoDiscoverRollsBackWhenReinitFails(t *testing.T) {
	s, deps, _ := autoDiscoverServer(t, discoveredInstallationHandler(t))
	deps.ReinitGitHubFunc = func(appID, installationID int64, keyFile string) error {
		return context.DeadlineExceeded
	}

	id, err := s.AutoDiscoverGitHubInstallationID(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "reinitializing github client") {
		t.Fatalf("err = %v, want a reinitializing-github-client error", err)
	}
	if id != 0 {
		t.Fatalf("id = %d, want 0 when reinit fails", id)
	}
	if deps.Config.GitHub.InstallationID != 0 {
		t.Fatalf("installation_id = %d, want rolled back to 0", deps.Config.GitHub.InstallationID)
	}
	// The rollback must be persisted, not memory-only.
	data, readErr := os.ReadFile(deps.Config.SourcePath)
	if readErr != nil {
		t.Fatalf("reading persisted config: %v", readErr)
	}
	if strings.Contains(string(data), "43021") {
		t.Fatalf("persisted config still holds the rolled-back installation id:\n%s", data)
	}
}

// TestAutoDiscoverReinitFailureWithUnpersistableRollbackStillErrors: the
// worst case — reinit fails AND the rollback save fails — must still return
// the reinit error and warn that the rollback could not be persisted, never
// panic or report success.
func TestAutoDiscoverReinitFailureWithUnpersistableRollbackStillErrors(t *testing.T) {
	s, deps, _ := autoDiscoverServer(t, discoveredInstallationHandler(t))
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s.logger = logger

	deps.ReinitGitHubFunc = func(appID, installationID int64, keyFile string) error {
		// Sabotage the rollback save: pointing SourcePath at a directory makes
		// the post-rollback Config.Save fail deterministically.
		deps.Config.SourcePath = t.TempDir()
		return context.DeadlineExceeded
	}

	id, err := s.AutoDiscoverGitHubInstallationID(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "reinitializing github client") {
		t.Fatalf("err = %v, want a reinitializing-github-client error", err)
	}
	if id != 0 {
		t.Fatalf("id = %d, want 0", id)
	}
	if deps.Config.GitHub.InstallationID != 0 {
		t.Fatalf("installation_id = %d, want rolled back to 0 in memory", deps.Config.GitHub.InstallationID)
	}
	if !strings.Contains(logs.String(), "rollback could not be persisted") {
		t.Fatalf("warn log = %q, want the unpersistable-rollback warning", logs.String())
	}
}
