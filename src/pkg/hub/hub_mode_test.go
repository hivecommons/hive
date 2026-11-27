package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests for HIVE_HUB_MODE (#20196). Every test sets the variable explicitly
// with t.Setenv, including to "", so an ambient HIVE_HUB_MODE on the machine
// running the suite cannot change the result.

// TestHubModeNormalization pins the parse: only "self-hosted" (case and space
// ignored) selects self-hosted, everything else stays hosted.
func TestHubModeNormalization(t *testing.T) {
	cases := map[string]string{
		"":               hubModeHosted,
		"hosted":         hubModeHosted,
		"selfhosted":     hubModeHosted,
		"self-hosted":    hubModeSelfHosted,
		" Self-Hosted  ": hubModeSelfHosted,
	}
	for raw, want := range cases {
		t.Setenv(hubModeEnv, raw)
		if got := hubMode(); got != want {
			t.Errorf("HIVE_HUB_MODE=%q: hubMode() = %q, want %q", raw, got, want)
		}
	}
}

// TestSelfHostedMissingRegistryIsEmpty is the regression: a self-hosted hub
// with no registry file must not report the synthesized "OKE (default)"
// cluster.
func TestSelfHostedMissingRegistryIsEmpty(t *testing.T) {
	t.Setenv(hubModeEnv, "self-hosted")
	pointClustersConfigAt(t, "")
	fatal := captureClustersFatal(t)

	clusters, outcome := loadClustersChecked(slog.Default())

	if outcome != clustersAbsent {
		t.Fatalf("outcome = %v, want clustersAbsent", outcome)
	}
	if len(clusters) != 0 {
		t.Fatalf("self-hosted hub with no registry file got %d clusters (%v), want none", len(clusters), clusters)
	}
	if clusters == nil {
		t.Fatal("self-hosted registry must be an empty map, not nil")
	}
	if got := loadClusters(slog.Default()); len(got) != 0 {
		t.Fatalf("loadClusters returned %d clusters, want none", len(got))
	}
	if fatal.fired {
		t.Fatal("a missing registry file must not make a self-hosted hub refuse to start")
	}
}

// TestHostedMissingRegistryKeepsDefault is the positive control: without the
// variable the synthesized default is unchanged.
func TestHostedMissingRegistryKeepsDefault(t *testing.T) {
	t.Setenv(hubModeEnv, "")
	pointClustersConfigAt(t, "")

	clusters, outcome := loadClustersChecked(slog.Default())

	if outcome != clustersAbsent {
		t.Fatalf("outcome = %v, want clustersAbsent", outcome)
	}
	if len(clusters) != 1 {
		t.Fatalf("hosted hub with no registry file got %d clusters, want the one synthesized default", len(clusters))
	}
	c, ok := clusters[defaultClusterID]
	if !ok {
		t.Fatalf("hosted hub with no registry file has no %q entry", defaultClusterID)
	}
	if c.Name != "OKE (default)" {
		t.Errorf("default cluster name = %q, want %q", c.Name, "OKE (default)")
	}
}

// TestSelfHostedModeDoesNotOverrideRegistryFile: a registry file that exists is
// still loaded as-is in self-hosted mode; the mode only changes the absent case.
func TestSelfHostedModeDoesNotOverrideRegistryFile(t *testing.T) {
	t.Setenv(hubModeEnv, "self-hosted")
	pointClustersConfigAt(t, liveShapedRegistry)
	captureClustersFatal(t)

	clusters, outcome := loadClustersChecked(slog.Default())

	if outcome != clustersLoaded {
		t.Fatalf("outcome = %v, want clustersLoaded", outcome)
	}
	if len(clusters) == 0 {
		t.Fatal("self-hosted mode dropped clusters from a registry file that exists")
	}
}

// authUserHubMode performs an authenticated GET /api/auth/user and returns the
// reported hub_mode.
func authUserHubMode(t *testing.T) any {
	t.Helper()
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	mkUser(t, "octocat")

	req := httptest.NewRequest(http.MethodGet, "https://hive.hivecommons.dev/api/auth/user", nil)
	req.AddCookie(testAuthCookie("octocat"))
	rec := httptest.NewRecorder()
	s.handleAuthUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/auth/user = %d, want 200", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding auth payload: %v (body %q)", err, rec.Body.String())
	}
	if payload["authenticated"] != true {
		t.Fatalf("expected an authenticated payload, got %v", payload)
	}
	return payload["hub_mode"]
}

// TestAuthUserReportsHubMode pins the dashboard surface: the auth payload the
// dashboard fetches on every load carries hub_mode.
func TestAuthUserReportsHubMode(t *testing.T) {
	t.Run("self-hosted", func(t *testing.T) {
		t.Setenv(hubModeEnv, "self-hosted")
		if got := authUserHubMode(t); got != hubModeSelfHosted {
			t.Errorf("hub_mode = %v, want %q", got, hubModeSelfHosted)
		}
	})
	t.Run("unset", func(t *testing.T) {
		t.Setenv(hubModeEnv, "")
		if got := authUserHubMode(t); got != hubModeHosted {
			t.Errorf("hub_mode = %v, want %q", got, hubModeHosted)
		}
	})
}
