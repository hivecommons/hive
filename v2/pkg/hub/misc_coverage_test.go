package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ============================================================
// heartbeat.go — liveness accessors
// ============================================================

func TestHeartbeatAccessors(t *testing.T) {
	// Reset state.
	lastHeartbeatSuccessUnix.Store(0)
	heartbeatLoopStarted.Store(false)

	if _, ok := LastHeartbeatSuccess(); ok {
		t.Error("no heartbeat yet -> ok should be false")
	}
	if HeartbeatEnabled() {
		t.Error("loop not started -> HeartbeatEnabled false")
	}

	recordHeartbeatSuccess()
	tm, ok := LastHeartbeatSuccess()
	if !ok || tm.IsZero() {
		t.Errorf("after record: ok=%v tm=%v", ok, tm)
	}

	heartbeatLoopStarted.Store(true)
	if !HeartbeatEnabled() {
		t.Error("loop started -> HeartbeatEnabled true")
	}

	// Cleanup for other tests.
	lastHeartbeatSuccessUnix.Store(0)
	heartbeatLoopStarted.Store(false)
}

// ============================================================
// saas.go — placeholder + project-config helpers
// ============================================================

func TestPoolClusterForAuthMethod(t *testing.T) {
	if got := poolClusterForAuthMethod(authMethodPrivate); got != gpuClusterID {
		t.Errorf("private -> %q, want %q", got, gpuClusterID)
	}
	if got := poolClusterForAuthMethod("public"); got != defaultClusterID {
		t.Errorf("public -> %q, want %q", got, defaultClusterID)
	}
}

func TestClusterIDForHive(t *testing.T) {
	if got := clusterIDForHive(&SaaSHive{}); got != defaultClusterID {
		t.Errorf("empty cluster -> %q, want %q", got, defaultClusterID)
	}
	if got := clusterIDForHive(&SaaSHive{ClusterID: "gpu-cluster"}); got != "gpu-cluster" {
		t.Errorf("explicit cluster -> %q", got)
	}
}

func TestSameStringSliceFold(t *testing.T) {
	if !sameStringSliceFold([]string{"A", "b"}, []string{"a", "B"}) {
		t.Error("case-insensitive equal slices should match")
	}
	if sameStringSliceFold([]string{"a"}, []string{"a", "b"}) {
		t.Error("different lengths should not match")
	}
	if sameStringSliceFold([]string{"a"}, []string{"c"}) {
		t.Error("different contents should not match")
	}
}

func TestFindAndListPlaceholders(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// An available placeholder owned by admin on the default cluster.
	saveSaaSHive(&SaaSHive{ID: "ph1", Owner: hubAdminUsername, Status: statusAvailable, ProjectName: "P1"})
	// A claimed hive (not a placeholder).
	saveSaaSHive(&SaaSHive{ID: "claimed", Owner: "alice", Status: "running"})

	if got := findAvailablePlaceholder(defaultClusterID); got != "ph1" {
		t.Errorf("findAvailablePlaceholder = %q, want ph1", got)
	}
	if got := findAvailablePlaceholder("no-such-cluster"); got != "" {
		t.Errorf("expected no placeholder on other cluster, got %q", got)
	}

	all := listAvailablePlaceholders("")
	if len(all) != 1 || all[0].ID != "ph1" {
		t.Errorf("listAvailablePlaceholders = %+v", all)
	}
	// Pool filter that excludes ph1.
	if got := listAvailablePlaceholders("no-such-cluster"); len(got) != 0 {
		t.Errorf("expected empty for other pool, got %+v", got)
	}
}

func TestHandleAvailablePlaceholders(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	saveSaaSHive(&SaaSHive{ID: "ph1", Owner: hubAdminUsername, Status: statusAvailable, ProjectName: "P1"})

	s := &HubServer{logger: slog.Default()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/saas/placeholders", nil)
	s.handleAvailablePlaceholders(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp map[string][]AvailablePlaceholder
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp["placeholders"]) != 1 {
		t.Errorf("expected 1 placeholder, got %+v", resp)
	}

	// Empty result -> non-nil empty array.
	saveSaaSHive(&SaaSHive{ID: "ph1", Owner: hubAdminUsername, Status: "running"}) // no longer available
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/saas/placeholders?pool=x", nil)
	s.handleAvailablePlaceholders(rec, req)
	var empty map[string][]AvailablePlaceholder
	json.Unmarshal(rec.Body.Bytes(), &empty)
	if empty["placeholders"] == nil {
		t.Error("placeholders should be [] not null")
	}
}

func TestProjectConfigForHiveID(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// No such hive -> nil.
	if got := projectConfigForHiveID("missing", "", nil, "", 0, ""); got != nil {
		t.Errorf("missing hive -> %+v, want nil", got)
	}

	// Available placeholder -> nil (not yet claimed).
	saveSaaSHive(&SaaSHive{ID: "ph", Status: statusAvailable})
	if got := projectConfigForHiveID("ph", "", nil, "", 0, ""); got != nil {
		t.Errorf("placeholder -> %+v, want nil", got)
	}

	// Incomplete claim (no ACMM) -> nil.
	saveSaaSHive(&SaaSHive{ID: "inc", Status: "running", Org: "acme", Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 0})
	if got := projectConfigForHiveID("inc", "", nil, "", 0, ""); got != nil {
		t.Errorf("incomplete claim -> %+v, want nil", got)
	}

	// Complete claim, spoke not yet matching -> returns config.
	saveSaaSHive(&SaaSHive{ID: "full", Status: "running", Org: "acme", Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 3})
	got := projectConfigForHiveID("full", "", nil, "", 0, "")
	if got == nil || got.Org != "acme" || got.ACMMLevel != 3 {
		t.Errorf("complete claim -> %+v, want org=acme acmm=3", got)
	}

	// Spoke already matches -> nil (stop sending).
	if got := projectConfigForHiveID("full", "acme", []string{"r"}, "r", 3, ""); got != nil {
		t.Errorf("matched spoke -> %+v, want nil", got)
	}

	// Vanity URL set but spoke hasn't adopted it -> keep sending (with the URL),
	// even though org/repos/acmm already match.
	saveSaaSHive(&SaaSHive{ID: "van", Status: "running", Org: "acme", Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 3, VanityURL: "https://vanity.example/"})
	got = projectConfigForHiveID("van", "acme", []string{"r"}, "r", 3, "")
	if got == nil || got.DashboardURL != "https://vanity.example/" {
		t.Errorf("vanity not yet adopted -> %+v, want DashboardURL set", got)
	}
	// Spoke has now adopted the vanity URL -> stop sending.
	if got := projectConfigForHiveID("van", "acme", []string{"r"}, "r", 3, "https://vanity.example/"); got != nil {
		t.Errorf("vanity adopted -> %+v, want nil", got)
	}
}

// ============================================================
// saas.go — cluster health handler (kubectl unavailable path)
// ============================================================

func TestHandleClusterHealth(t *testing.T) {
	// Reset the package cache so buildClusterHealth runs.
	clusterHealthCacheMu.Lock()
	clusterHealthCache = nil
	clusterHealthCacheTime = time.Time{}
	clusterHealthCacheMu.Unlock()

	s := &HubServer{
		logger:          slog.Default(),
		clusters:        map[string]ClusterConfig{"hive-oke": {ID: "hive-oke"}},
		heartbeatHealth: make(map[string]*HeartbeatHealthEntry),
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/saas/cluster-health", nil)
	// kubectl fails (fake exits 1) -> per-cluster error is captured but the
	// overall response is still built and returned 200.
	s.handleClusterHealth(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	// Second call hits the fresh-cache branch.
	rec2 := httptest.NewRecorder()
	s.handleClusterHealth(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("cached status = %d", rec2.Code)
	}

	// Cleanup.
	clusterHealthCacheMu.Lock()
	clusterHealthCache = nil
	clusterHealthCacheTime = time.Time{}
	clusterHealthCacheMu.Unlock()
}
