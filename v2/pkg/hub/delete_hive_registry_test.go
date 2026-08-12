package hub

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests cover the "ghost hive" bug: deleting a hosted hive tore down the
// Kubernetes resources but left the entry in the hub registry, so the hive kept
// rendering in "My Hives" forever and could never be removed from the UI.

// helperDeleteHiveEnv points the SaaS hive dir and the registry file at temp
// paths and returns a mux wired to handleDeleteHive.
func helperDeleteHiveEnv(t *testing.T) (*HubServer, *http.ServeMux) {
	t.Helper()
	provisionWG.Wait()

	dir := t.TempDir()
	oldHives := saasHivesDir
	oldUsers := saasUsersDir
	oldRegistry := registryPath
	oldTimeline := timelineDir

	saasHivesDir = filepath.Join(dir, "hives")
	saasUsersDir = filepath.Join(dir, "users")
	registryPath = filepath.Join(dir, "hub-registry.json")
	timelineDir = filepath.Join(dir, "timeline")
	if err := os.MkdirAll(saasHivesDir, 0o755); err != nil {
		t.Fatalf("mkdir hives: %v", err)
	}
	if err := os.MkdirAll(saasUsersDir, 0o755); err != nil {
		t.Fatalf("mkdir users: %v", err)
	}
	if err := os.MkdirAll(timelineDir, 0o755); err != nil {
		t.Fatalf("mkdir timeline: %v", err)
	}

	t.Cleanup(func() {
		provisionWG.Wait()
		saasHivesDir = oldHives
		saasUsersDir = oldUsers
		registryPath = oldRegistry
		timelineDir = oldTimeline
	})

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /api/saas/hives/{id}", srv.handleDeleteHive)
	return srv, mux
}

// helperHiveRecordExists reports whether the on-disk SaaS hive record dir
// (hives/<id>/) still exists. This husk is what resurrected the hive: a delete
// that left it behind — with status:"available" — kept the hive in the
// unassigned/available listing across a hub restart.
func helperHiveRecordExists(id string) bool {
	if _, err := os.Stat(filepath.Join(saasHivesDir, id)); err == nil {
		return true
	}
	return false
}

// helperTimelineFileExists reports whether the hive's timeline file
// (timelineDir/<id>.json) still exists on disk.
func helperTimelineFileExists(id string) bool {
	tp := timelinePath(id)
	if tp == "" {
		return false
	}
	if _, err := os.Stat(tp); err == nil {
		return true
	}
	return false
}

// helperRegistryHasHive reports whether the in-memory registry holds an entry.
func helperRegistryHasHive(s *HubServer, id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.ID == id {
			return true
		}
	}
	return false
}

// helperPersistedRegistryHasHive reads the registry back off disk. This is the
// representation that survives a hub restart, so it is what actually determines
// whether a deleted hive stays deleted.
func helperPersistedRegistryHasHive(t *testing.T, id string) bool {
	t.Helper()
	data, err := os.ReadFile(registryPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatalf("read registry: %v", err)
	}
	var reg Registry
	if err := json.Unmarshal(data, &reg); err != nil {
		t.Fatalf("parse registry: %v", err)
	}
	for _, h := range reg.Hives {
		if h.ID == id {
			return true
		}
	}
	return false
}

// TestDeleteHiveRemovesRegistryEntryFromMemoryAndDisk is the core regression:
// after a successful delete the hive must be absent from both the in-memory
// registry (what the listing renders) and the on-disk registry (what a restart
// reloads). Removing it from only one leaves the hive reappearing.
func TestDeleteHiveRemovesRegistryEntryFromMemoryAndDisk(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_del_reg", "del-reg-owner")
	defer cleanup()

	srv, mux := helperDeleteHiveEnv(t)

	const hiveID = "hosted-del-reg-hive"
	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: "del-reg-owner", ClusterID: defaultClusterID}); err != nil {
		t.Fatalf("seed hive: %v", err)
	}
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: "del-reg-owner", Online: true}}
	srv.mu.Unlock()
	if err := srv.saveRegistryNow(); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	req := httptest.NewRequest("DELETE", "/api/saas/hives/"+hiveID, nil)
	req.Header.Set("Authorization", "Bearer ghp_del_reg")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if helperRegistryHasHive(srv, hiveID) {
		t.Error("hive still present in the in-memory registry after delete")
	}
	if helperPersistedRegistryHasHive(t, hiveID) {
		t.Error("hive still present in the persisted registry after delete (would resurrect on hub restart)")
	}
}

// TestDeleteHiveWithNoClusterConfigPurgesOnDiskRecord is the direct regression
// for the resurrection bug. When clusterForHive returns nil, deprovisionHive is
// skipped — and deprovisionHive was the ONLY code that removed the hives/<id>/
// dir. A slot record with status:"available" therefore survived on disk and the
// hive reappeared in the unassigned/available listing after a hub restart, still
// pointing at a namespace that no longer existed. After the fix the delete must
// purge the on-disk record (and the timeline file) even with no cluster config.
func TestDeleteHiveWithNoClusterConfigPurgesOnDiskRecord(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_del_purge", "del-purge-owner")
	defer cleanup()

	srv, mux := helperDeleteHiveEnv(t)
	// No cluster config → clusterForHive returns nil → deprovision is skipped.
	srv.clusters = map[string]ClusterConfig{}

	const hiveID = "hosted-del-purge-hive"
	// The exact husk that resurrects: an available-looking slot record on disk.
	if err := saveSaaSHive(&SaaSHive{
		ID:        hiveID,
		Owner:     "del-purge-owner",
		ClusterID: "cluster-that-was-removed",
		Status:    statusAvailable,
	}); err != nil {
		t.Fatalf("seed hive: %v", err)
	}
	// A timeline file that nothing else in the delete path ever removed.
	srv.recordTimeline(hiveID, TimelineOwnership, "seeded", "tester")

	if !helperHiveRecordExists(hiveID) {
		t.Fatal("precondition: seeded hive record dir should exist on disk")
	}

	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: "del-purge-owner"}}
	srv.mu.Unlock()

	req := httptest.NewRequest("DELETE", "/api/saas/hives/"+hiveID, nil)
	req.Header.Set("Authorization", "Bearer ghp_del_purge")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	if helperRegistryHasHive(srv, hiveID) || helperPersistedRegistryHasHive(t, hiveID) {
		t.Error("registry entry survived a no-cluster delete")
	}
	if helperHiveRecordExists(hiveID) {
		t.Error("resurrection bug: on-disk hive record dir survived delete with no cluster config")
	}
	if helperTimelineFileExists(hiveID) {
		t.Error("resurrection bug: timeline file survived delete")
	}
}

// TestDeleteHivePurgesTimelineFile pins that the timeline file is removed on a
// normal delete too. deprovisionHive removes the hive dir but never touched the
// timeline; removeHiveRecord closes that gap.
func TestDeleteHivePurgesTimelineFile(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_del_tl", "del-tl-owner")
	defer cleanup()

	srv, mux := helperDeleteHiveEnv(t)

	const hiveID = "hosted-del-tl-hive"
	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: "del-tl-owner", ClusterID: defaultClusterID}); err != nil {
		t.Fatalf("seed hive: %v", err)
	}
	srv.recordTimeline(hiveID, TimelineOwnership, "seeded", "tester")
	if !helperTimelineFileExists(hiveID) {
		t.Fatal("precondition: seeded timeline file should exist on disk")
	}
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: "del-tl-owner"}}
	srv.mu.Unlock()

	req := httptest.NewRequest("DELETE", "/api/saas/hives/"+hiveID, nil)
	req.Header.Set("Authorization", "Bearer ghp_del_tl")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if helperHiveRecordExists(hiveID) {
		t.Error("on-disk hive record dir survived delete")
	}
	if helperTimelineFileExists(hiveID) {
		t.Error("timeline file survived delete")
	}
}

// TestDeleteHiveWithMissingNamespaceStillRemovesRegistryEntry reproduces the
// exact stuck state the user hit: the namespace and pods were already gone, but
// the registry row survived and the UI kept showing the hive. Deleting a hive
// whose cluster-side resources no longer exist must still clear the row.
func TestDeleteHiveWithMissingNamespaceStillRemovesRegistryEntry(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_del_gone", "del-gone-owner")
	defer cleanup()

	srv, mux := helperDeleteHiveEnv(t)

	const hiveID = "hosted-del-gone-hive"
	// No namespace exists for this hive; kubectl delete --ignore-not-found is a
	// no-op / failure depending on cluster reachability. Either way the registry
	// entry must go.
	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: "del-gone-owner", ClusterID: defaultClusterID}); err != nil {
		t.Fatalf("seed hive: %v", err)
	}
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: "del-gone-owner", Online: false}}
	srv.mu.Unlock()

	req := httptest.NewRequest("DELETE", "/api/saas/hives/"+hiveID, nil)
	req.Header.Set("Authorization", "Bearer ghp_del_gone")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 even with the namespace already gone, got %d body=%s", w.Code, w.Body.String())
	}
	if helperRegistryHasHive(srv, hiveID) {
		t.Error("ghost entry: hive whose namespace was already gone remained in the registry")
	}
	if helperPersistedRegistryHasHive(t, hiveID) {
		t.Error("ghost entry persisted to disk for a hive whose namespace was already gone")
	}
}

// TestDeleteHiveWithNoClusterConfigStillRemovesEntryAndWarns covers the branch
// that caused the production ghost: clusterForHive returned nil, the handler
// returned 500 and never reached removeRegistryEntry, so the row was stranded
// permanently. Now the entry is removed and the partial outcome is reported.
func TestDeleteHiveWithNoClusterConfigStillRemovesEntryAndWarns(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_del_nocluster", "del-nocluster-owner")
	defer cleanup()

	srv, mux := helperDeleteHiveEnv(t)
	// Drop every cluster config so clusterForHive cannot resolve one.
	srv.clusters = map[string]ClusterConfig{}

	const hiveID = "hosted-del-nocluster-hive"
	if err := saveSaaSHive(&SaaSHive{ID: hiveID, Owner: "del-nocluster-owner", ClusterID: "cluster-that-was-removed"}); err != nil {
		t.Fatalf("seed hive: %v", err)
	}
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: "del-nocluster-owner", Online: false}}
	srv.mu.Unlock()

	req := httptest.NewRequest("DELETE", "/api/saas/hives/"+hiveID, nil)
	req.Header.Set("Authorization", "Bearer ghp_del_nocluster")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with a partial-delete body, got %d body=%s", w.Code, w.Body.String())
	}

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse response: %v (body=%s)", err, w.Body.String())
	}
	if body["status"] != deleteStatusPartial {
		t.Errorf("expected status %q so the user learns cleanup was incomplete, got %q", deleteStatusPartial, body["status"])
	}
	if body["warning"] == "" {
		t.Error("partial delete must carry a warning explaining what was left behind")
	}

	// The critical assertion: a teardown that could not run must NOT leave a ghost.
	if helperRegistryHasHive(srv, hiveID) {
		t.Error("ghost entry: registry row survived a delete whose teardown could not run")
	}
	if helperPersistedRegistryHasHive(t, hiveID) {
		t.Error("ghost entry persisted to disk after a delete whose teardown could not run")
	}
}

// TestDeleteHiveIsIdempotentWhenSaaSRecordAlreadyGone covers repeated Delete
// clicks and the case where the on-disk hive record was cleaned up but the
// registry row was not — the state the user had to fix by hand.
func TestDeleteHiveIsIdempotentWhenSaaSRecordAlreadyGone(t *testing.T) {
	cleanup := helperSetupAuthUser(t, "ghp_del_idem", "del-idem-owner")
	defer cleanup()

	srv, mux := helperDeleteHiveEnv(t)

	const hiveID = "hosted-del-idem-hive"
	// Deliberately no saveSaaSHive: the meta.json is already gone, only the
	// registry row remains.
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{ID: hiveID, Owner: "del-idem-owner", Online: false}}
	srv.mu.Unlock()

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("DELETE", "/api/saas/hives/"+hiveID, nil)
		req.Header.Set("Authorization", "Bearer ghp_del_idem")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("delete attempt %d: expected 200, got %d body=%s", i+1, w.Code, w.Body.String())
		}
	}

	if helperRegistryHasHive(srv, hiveID) {
		t.Error("orphaned registry row was not cleared when the SaaS record was already gone")
	}
	if helperPersistedRegistryHasHive(t, hiveID) {
		t.Error("orphaned registry row still on disk after delete")
	}
}

// TestRemoveRegistryEntryPersistsSynchronously guards the durability fix
// directly. requestSave defers the write by registrySaveDelay; a hub restart in
// that window reloaded the pre-delete registry and the hive came back. The
// removal must be on disk by the time removeRegistryEntry returns.
func TestRemoveRegistryEntryPersistsSynchronously(t *testing.T) {
	_, _ = helperDeleteHiveEnv(t)

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	const keepID = "keep-me-hive"
	const dropID = "drop-me-hive"
	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{ID: keepID}, {ID: dropID}}
	srv.mu.Unlock()
	if err := srv.saveRegistryNow(); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	srv.removeRegistryEntry(dropID, "tester")

	// No sleep: the write must already have happened.
	if helperPersistedRegistryHasHive(t, dropID) {
		t.Error("removeRegistryEntry did not flush the removal to disk before returning")
	}
	if !helperPersistedRegistryHasHive(t, keepID) {
		t.Error("removeRegistryEntry dropped an unrelated hive from the persisted registry")
	}

	// A second removal of an absent ID must be a harmless no-op.
	srv.removeRegistryEntry(dropID, "tester")
	if !helperPersistedRegistryHasHive(t, keepID) {
		t.Error("no-op removal corrupted the registry")
	}
}

// TestKubectlUnreachableHiveIsNotReaped pins the deliberate decision NOT to add
// a namespace-existence garbage collector.
//
// Hives on firewalled spokes are heartbeat-only and are NOT kubectl-reachable
// from the hub, so "the hub cannot see the namespace" does not imply "the hive
// is gone" — it is equally consistent with a transient API outage or a spoke
// the hub was never able to reach. Reaping on that signal would delete live
// hives. The hub's only automatic eviction stays heartbeat-based
// (staleRemoveAge), because a heartbeat is positive evidence of liveness that
// the hive itself supplies; its absence for a full day is a far safer signal
// than the hub's inability to run kubectl.
//
// This test asserts that a hive which is offline and whose namespace does not
// exist is still retained, as long as it is heartbeating within staleRemoveAge.
func TestKubectlUnreachableHiveIsNotReaped(t *testing.T) {
	_, _ = helperDeleteHiveEnv(t)

	srv := NewHubServer(0, slog.Default(), "test", "v2")
	const unreachableID = "hosted-unreachable-spoke-hive"
	// Beat recently enough to be well inside staleRemoveAge, but old enough to
	// be marked offline: exactly what an unreachable-but-alive hive looks like.
	recentButStale := time.Now().Add(-2 * maxHeartbeatAge).UTC().Format(time.RFC3339)

	srv.mu.Lock()
	srv.registry.Hives = []RegistryEntry{{
		ID:            unreachableID,
		Owner:         "spoke-owner",
		Online:        true,
		LastHeartbeat: recentButStale,
	}}
	srv.markStaleHives()
	srv.mu.Unlock()

	if !helperRegistryHasHive(srv, unreachableID) {
		t.Fatal("an unreachable hive was reaped from the registry without an explicit delete")
	}

	srv.mu.RLock()
	stillOnline := srv.registry.Hives[0].Online
	srv.mu.RUnlock()
	if stillOnline {
		t.Error("expected the hive to be marked offline (but retained), not left online")
	}
}
