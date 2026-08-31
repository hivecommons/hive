package hub

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ============================================================
// saas.go — buildSingleClusterHealth / buildClusterHealth
//
// Installs a scripted fake kubectl on PATH that returns canned `top nodes`,
// `get nodes -o json`, and `get pods` output, so the full parse/aggregate body
// of buildSingleClusterHealth runs without a real cluster.
// ============================================================

// installScriptedKubectl writes a fake kubectl to a temp dir and prepends it to
// PATH for the duration of the test. The script branches on its argument list.
func installScriptedKubectl(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
# Scripted fake kubectl for buildSingleClusterHealth tests.
case "$*" in
  *"top nodes"*)
    printf 'node-a 2000m 50%% 4096Mi 50%%\n'
    ;;
  *"get nodes"*)
    cat <<'JSON'
{"items":[{"metadata":{"name":"node-a"},"spec":{"unschedulable":false},"status":{"allocatable":{"cpu":"4","memory":"8Gi","pods":"110","nvidia.com/gpu":"1"},"capacity":{"cpu":"4","memory":"8Gi","pods":"110","nvidia.com/gpu":"1"},"conditions":[{"type":"Ready","status":"True"},{"type":"DiskPressure","status":"False"}]}}]}
JSON
    ;;
  *"get pods"*)
    cat <<'JSON'
{"items":[{"metadata":{"namespace":"hive-hosted-x"},"spec":{"nodeName":"node-a","containers":[{"resources":{"requests":{"cpu":"500m","memory":"512Mi"}}}]}}]}
JSON
    ;;
  *)
    echo "{}"
    ;;
esac
exit 0
`
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestBuildSingleClusterHealth(t *testing.T) {
	installScriptedKubectl(t)

	cluster := &ClusterConfig{ID: "hive-oke", InCluster: true}
	health, err := buildSingleClusterHealth(cluster, 2, slog.Default())
	if err != nil {
		t.Fatalf("buildSingleClusterHealth: %v", err)
	}
	if health.Summary.TotalNodes == 0 {
		t.Errorf("expected nodes, got summary %+v", health.Summary)
	}
	if health.HiveCount != 2 {
		t.Errorf("hiveCount = %d, want 2", health.HiveCount)
	}
}

func TestBuildClusterHealthWithScriptedKubectl(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	// Reset the response cache.
	clusterHealthCacheMu.Lock()
	clusterHealthCache = nil
	clusterHealthCacheMu.Unlock()

	s := &HubServer{
		hubSecret:       testHubSecret,
		logger:          slog.Default(),
		clusters:        map[string]ClusterConfig{"hive-oke": {ID: "hive-oke", InCluster: true, Name: "OKE"}},
		heartbeatHealth: make(map[string]*HeartbeatHealthEntry),
	}
	// Seed a SaaS hive so the per-cluster hive count path runs.
	saveSaaSHive(&SaaSHive{ID: "h1", ClusterID: "hive-oke", Status: "running"})

	resp, err := buildClusterHealth(s)
	if err != nil {
		t.Fatalf("buildClusterHealth: %v", err)
	}
	if resp == nil || len(resp.Clusters) == 0 {
		t.Errorf("expected per-cluster health, got %+v", resp)
	}

	clusterHealthCacheMu.Lock()
	clusterHealthCache = nil
	clusterHealthCacheMu.Unlock()
}

func TestHandleUpgradeHiveSuccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	mkUser(t, "alice")
	saveSaaSHive(&SaaSHive{ID: "h1", Owner: "alice", ClusterID: "hive-oke"})
	s := &HubServer{
		hubSecret: testHubSecret,
		logger:    slog.Default(),
		clusters:  map[string]ClusterConfig{"hive-oke": {ID: "hive-oke", InCluster: true}},
	}
	// LastHeartbeat: the manual path refuses a hive that cannot COLLECT the
	// upgrade (pull-only delivery, see pullonly_upgrade.go), so a success-path
	// test needs a hive that is heartbeating.
	s.registry.Hives = []RegistryEntry{{
		ID: "h1", GitBranch: "v2",
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
	}}

	rec := httptest.NewRecorder()
	req := setPathValue(reqWithUser("POST", "/up", "", "alice"), "id", "h1")
	s.handleUpgradeHive(rec, req)
	if rec.Code != 200 {
		t.Errorf("upgrade success status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}
