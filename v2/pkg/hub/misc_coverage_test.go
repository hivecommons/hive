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
	if got := projectConfigForHiveID("missing", "", nil, "", 0, "", ""); got != nil {
		t.Errorf("missing hive -> %+v, want nil", got)
	}

	// Available placeholder -> nil (not yet claimed).
	saveSaaSHive(&SaaSHive{ID: "ph", Status: statusAvailable})
	if got := projectConfigForHiveID("ph", "", nil, "", 0, "", ""); got != nil {
		t.Errorf("placeholder -> %+v, want nil", got)
	}

	// Incomplete claim (no ACMM) -> nil.
	saveSaaSHive(&SaaSHive{ID: "inc", Status: "running", Org: "acme", Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 0})
	if got := projectConfigForHiveID("inc", "", nil, "", 0, "", ""); got != nil {
		t.Errorf("incomplete claim -> %+v, want nil", got)
	}

	// Freshly-claimed hive (ClaimDelivered=false), spoke still reporting a stale
	// placeholder project -> hub PUSHES the real claim until the spoke reports it.
	saveSaaSHive(&SaaSHive{ID: "fresh", Status: "running", Org: "acme", Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 3})
	if got := projectConfigForHiveID("fresh", "placeholder", nil, "old", 0, "", ""); got == nil || got.Org != "acme" {
		t.Errorf("undelivered claim, stale spoke -> %+v, want claim pushed (org=acme)", got)
	}

	// Delivered claim (ClaimDelivered=true): org/repos/ACMM are now operator-owned
	// and adopted by the caller, not pushed here — so projectConfigForHiveID
	// returns nil regardless of what the spoke reports (nothing to push).
	saveSaaSHive(&SaaSHive{ID: "full", Status: "running", Org: "acme", Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 3, ClaimDelivered: true})
	if got := projectConfigForHiveID("full", "acme", []string{"r"}, "r", 3, "", ""); got != nil {
		t.Errorf("delivered claim -> %+v, want nil (org/repos/ACMM are adopted, not pushed)", got)
	}
	// Even a spoke reporting a different level -> still nil (no push; adopt path handles it).
	if got := projectConfigForHiveID("full", "acme", []string{"r"}, "r", 2, "", ""); got != nil {
		t.Errorf("differing ACMM -> %+v, want nil (adopted, not pushed)", got)
	}

	// Vanity URL set but spoke hasn't adopted it -> keep sending (with the URL),
	// even though the claim is delivered and org/repos/acmm already match.
	saveSaaSHive(&SaaSHive{ID: "van", Status: "running", Org: "acme", Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 3, ClaimDelivered: true, VanityURL: "https://vanity.example/"})
	got := projectConfigForHiveID("van", "acme", []string{"r"}, "r", 3, "", "")
	if got == nil || got.DashboardURL != "https://vanity.example/" {
		t.Errorf("vanity not yet adopted -> %+v, want DashboardURL set", got)
	}
	// Spoke has now adopted the vanity URL -> stop sending.
	if got := projectConfigForHiveID("van", "acme", []string{"r"}, "r", 3, "https://vanity.example/", ""); got != nil {
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

// TestAdoptSpokeProjectConfig verifies the hub's adopt path:
//   - ACMM level follows the SAME ClaimDelivered handshake as org/repos: before
//     delivery the assigned level is authoritative and a stale spoke report is
//     ignored (an approved L3 request was silently downgraded to the level the
//     placeholder was minted at); after delivery a dashboard level change is
//     adopted and never reverted (the Joe Runde / spyre revert bug);
//   - org/repos/primary follow the ClaimDelivered handshake: before the spoke
//     first reports the claimed project, hub-pushed values are authoritative and
//     spoke reports do NOT adopt (a stale placeholder must not win); the first
//     matching report flips ClaimDelivered, and only afterwards do dashboard
//     edits to org/repos get adopted.
//
// It also leaves unclaimed placeholders and empty/no-op reports alone.
func TestAdoptSpokeProjectConfig(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: slog.Default()}

	// Freshly-claimed hive (ClaimDelivered=false), assigned L3. Nothing reported
	// before delivery is adopted — including the level. A report that matches the
	// claim IN FULL (org/repos AND level) flips ClaimDelivered.
	saveSaaSHive(&SaaSHive{ID: "claimed", Status: "running", Org: "o", Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 3})

	// Pre-delivery: the spoke is still running its placeholder project at the
	// level it was MINTED at (2), not the assigned 3. This is the production
	// bug: adopting that report rewrote meta from the requested L3 down to L2,
	// and since the hub then had nothing higher to push, the hive stayed L2
	// forever. The assigned level must survive.
	s.adoptSpokeProjectConfig("claimed", "stale", []string{"old"}, "old", 2)
	if h := loadSaaSHive("claimed"); h == nil || h.ACMMLevel != 3 || h.Org != "o" || len(h.Repos) != 1 || h.ClaimDelivered {
		t.Errorf("pre-delivery stale report: meta = %+v, want acmm=3 (assigned level held) org=o repos=[r] ClaimDelivered=false", h)
	}

	// A report matching org/repos but still carrying the OLD level is not a
	// complete delivery: the claim includes the level, so the flag must stay
	// down and the hub must keep pushing until the level lands too.
	s.adoptSpokeProjectConfig("claimed", "o", []string{"r"}, "r", 2)
	if h := loadSaaSHive("claimed"); h == nil || h.ClaimDelivered || h.ACMMLevel != 3 {
		t.Errorf("partial delivery (level not yet applied) must not flip ClaimDelivered: %+v", h)
	}

	// Spoke now reports the claim IN FULL, level included -> flips ClaimDelivered.
	s.adoptSpokeProjectConfig("claimed", "o", []string{"r"}, "r", 3)
	if h := loadSaaSHive("claimed"); h == nil || !h.ClaimDelivered {
		t.Errorf("matching report should mark ClaimDelivered: %+v", h)
	}

	// Post-delivery: operator changes repos on the dashboard -> adopted.
	s.adoptSpokeProjectConfig("claimed", "o", []string{"r", "r2"}, "r2", 4)
	if h := loadSaaSHive("claimed"); h == nil || h.ACMMLevel != 4 || len(h.Repos) != 2 || h.PrimaryRepo != "r2" {
		t.Errorf("post-delivery edit: meta = %+v, want acmm=4 repos=[r r2] primary=r2 (adopted)", h)
	}

	// EMPTY/zero reported values must NOT wipe or downgrade meta (mid-boot spoke).
	s.adoptSpokeProjectConfig("claimed", "", nil, "", 0)
	if h := loadSaaSHive("claimed"); h == nil || h.ACMMLevel != 4 || len(h.Repos) != 2 || h.Org != "o" {
		t.Errorf("empty report clobbered meta: %+v, want unchanged", h)
	}

	// Unclaimed placeholder: assign owns it -> NOT adopted.
	saveSaaSHive(&SaaSHive{ID: "ph", Status: statusAvailable, ACMMLevel: 2, Org: "orig"})
	s.adoptSpokeProjectConfig("ph", "hijack", []string{"x"}, "x", 5)
	if h := loadSaaSHive("ph"); h == nil || h.ACMMLevel != 2 || h.Org != "orig" {
		t.Errorf("placeholder: meta = %+v, want unchanged (assign owns it)", h)
	}

	// No record -> no panic, no-op.
	s.adoptSpokeProjectConfig("missing", "o", []string{"r"}, "r", 4)
}

// TestProjectConfigForHiveID_PushesAssignedACMMUntilDelivered locks the push
// half of the assigned-ACMM fix.
//
// #2061 removed the ACMM push entirely to stop a dashboard level change being
// reverted every beat. That fixed the revert but meant a freshly-assigned
// placeholder was never TOLD the level its owner requested: it kept running the
// level it was minted at and reported it back. Bounding the push by
// ClaimDelivered delivers the requested level exactly once, then stops forever.
func TestProjectConfigForHiveID_PushesAssignedACMMUntilDelivered(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// Assigned at L3; the spoke still reports the minted L2 with matching
	// org/repos. Before the fix every non-ACMM field matched, so the reconcile
	// returned "nothing left to push" and the level never travelled.
	saveSaaSHive(&SaaSHive{
		ID: "h", Status: "running", Org: "o", Repos: []string{"r"},
		PrimaryRepo: "r", ACMMLevel: 3, ClaimDelivered: false,
	})
	got := projectConfigForHiveID("h", "o", []string{"r"}, "r", 2, "", "")
	if got == nil {
		t.Fatal("no push for an undelivered claim whose level has not landed; the hive stays at the wrong level forever")
	}
	if got.ACMMLevel != 3 {
		t.Errorf("pushed ACMMLevel = %d, want the assigned 3", got.ACMMLevel)
	}

	// Once delivered, ACMM is the spoke's to own: a dashboard change to L5 must
	// NOT be pushed back down (the spyre revert #2061 fixed).
	saveSaaSHive(&SaaSHive{
		ID: "d", Status: "running", Org: "o", Repos: []string{"r"},
		PrimaryRepo: "r", ACMMLevel: 3, ClaimDelivered: true,
	})
	if got := projectConfigForHiveID("d", "o", []string{"r"}, "r", 5, "", ""); got != nil {
		t.Errorf("delivered claim must never push ACMM back to the spoke, got %+v", got)
	}
}
