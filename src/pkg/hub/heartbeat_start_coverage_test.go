package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ============================================================
// heartbeat.go — StartHeartbeat / StartTaskStatusPush / waitForReady
// ============================================================

func TestStartHeartbeatSendsThenStops(t *testing.T) {
	// Reset loop flag for a clean assertion.
	heartbeatLoopStarted.Store(false)

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(HeartbeatResponse{})
	}))
	defer hub.Close()

	// A cancelled context makes waitForReady return immediately; the first
	// sendHeartbeat then no-ops on the cancelled ctx and the loop exits at once.
	// This exercises the callback demux + waitForReady + processResponse plumbing
	// without blocking on the 5s readiness sleep.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	collect := func() *HeartbeatPayload { return &HeartbeatPayload{HiveID: "h1"} }
	StartHeartbeat(ctx, hub.URL, collect, time.Hour, slog.Default(),
		UpgradeCallback(func(string) {}),
		VisibilityCallback(func(bool) {}),
	)

	if !HeartbeatEnabled() {
		t.Error("StartHeartbeat should mark the loop as started")
	}
	heartbeatLoopStarted.Store(false)
}

func TestStartTaskStatusPush(t *testing.T) {
	// Disabled path.
	StartTaskStatusPush(context.Background(), "", func() *TaskStatusPayload { return nil }, slog.Default())

	// Cancelled context path (returns on ctx.Done before any tick).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	StartTaskStatusPush(ctx, "http://127.0.0.1:1", func() *TaskStatusPayload { return nil }, slog.Default())
}

// ============================================================
// saas_provision.go — loadClusters
// ============================================================

func TestLoadClustersFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clusters.json")
	cfg := `[
		{"id":"c1","domain":"c1.example.com","in_cluster":true},
		{"id":"","domain":"x"},
		{"id":"c2","domain":"","in_cluster":true},
		{"id":"remote","domain":"r.example.com","in_cluster":false}
	]`
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	old := clustersConfigPath
	clustersConfigPath = path
	defer func() { clustersConfigPath = old }()

	clusters := loadClusters(slog.Default())
	// c1 valid; empty-ID skipped; c2 no-domain skipped; remote no-kubeconfig skipped.
	if _, ok := clusters["c1"]; !ok {
		t.Errorf("c1 should be loaded, got %+v", clusters)
	}
	if _, ok := clusters["c2"]; ok {
		t.Error("c2 (no domain) should be skipped")
	}
	if _, ok := clusters["remote"]; ok {
		t.Error("remote (no kubeconfig) should be skipped")
	}
}

// TestLoadClustersBadJSON pins that an unparseable registry makes the hub
// REFUSE, and is deliberately the inversion of what it used to assert.
//
// AUDIT 8 / §6 ITEM 11. The original body asserted "bad JSON should yield empty
// map" — it encoded the vulnerability as the expected behaviour. An empty
// registry is not a safe degraded state: clusterForHive returns nil for every
// hive, which silently turns off the hub's writes to hive-oke while the hub
// reports healthy. The correct assertion is that the hub does not come up at
// all, so this test now checks the refusal fired.
func TestLoadClustersBadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clusters.json")
	os.WriteFile(path, []byte(`{not an array`), 0o644)
	old := clustersConfigPath
	clustersConfigPath = path
	defer func() { clustersConfigPath = old }()

	fatal := captureClustersFatal(t)

	_ = loadClusters(slog.Default())

	if !fatal.fired {
		t.Fatal("unparseable clusters.json did not make the hub refuse to start; " +
			"an empty registry nils out clusterForHive for every hive and silently disables writes to hive-oke")
	}
}

// ============================================================
// server.go — contribute proxy (no-hive path)
// ============================================================

func TestHandleContributeProxyNoHive(t *testing.T) {
	s := &HubServer{logger: slog.Default()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/contribute/register", nil)
	s.handleContributeProxy(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no-hive proxy status = %d, want 503", rec.Code)
	}
}
