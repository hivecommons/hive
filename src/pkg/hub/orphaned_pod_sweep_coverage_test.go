package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Coverage for the orphaned Terminating-pod sweep (orphaned_pod_reaper.go).
// The pure predicate/parse/select helpers are covered by
// orphaned_pod_reaper_test.go; these tests drive reapOrphanedPods and
// reapOrphanedPodsIfDue end to end over the registry and a scripted kubectl,
// mirroring the sibling netadmin_sweep_coverage_test.go setup.

// orphanSweepServer builds a HubServer whose saasHivesDir points at a per-test
// TempDir and whose only cluster is an in-cluster default, so reapOrphanedPods
// exercises its real registry-walk and kubectl-shelling paths against fixtures.
func orphanSweepServer(t *testing.T) *HubServer {
	t.Helper()
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })
	return &HubServer{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		clusters: map[string]ClusterConfig{
			defaultClusterID: {ID: defaultClusterID, InCluster: true},
		},
		clusterUnreachableUntil: map[string]time.Time{},
	}
}

// installOrphanKubectl writes a fake kubectl that logs every invocation and
// scripts the two calls the sweep makes: `get pods` cats podsFile and exits
// getExit, `delete pod` exits deleteExit. Returns the invocation log path.
func installOrphanKubectl(t *testing.T, podsFile string, getExit, deleteExit int) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kubectl.log")
	script := `#!/bin/sh
echo "$*" >> "` + logPath + `"
case " $* " in
*" get pods "*)
	cat "` + podsFile + `"
	exit ` + strconv.Itoa(getExit) + `
	;;
*" delete pod "*)
	exit ` + strconv.Itoa(deleteExit) + `
	;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// writeOrphanPodList writes a `kubectl get pods -o json` fixture containing
// nOrphans pods matching the orphaned-Terminating signature plus one healthy
// Running pod, and returns the file path.
func writeOrphanPodList(t *testing.T, ns string, nOrphans int) string {
	t.Helper()
	type item = map[string]any
	items := []item{
		{
			"metadata": map[string]any{"name": "live-spoke", "namespace": ns},
			"status":   map[string]any{"phase": "Running"},
		},
	}
	deleted := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	for i := 0; i < nOrphans; i++ {
		items = append(items, item{
			"metadata": map[string]any{
				"name":              fmt.Sprintf("orphan-%d", i),
				"namespace":         ns,
				"deletionTimestamp": deleted,
				"finalizers":        []string{},
			},
			"status": map[string]any{"phase": "Failed"},
		})
	}
	raw, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pods.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFixtureFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pods.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReapOrphanedPodsDeletesOrphan is the happy path: one orphaned pod in a
// registry hive's hosted namespace is force-deleted with the exact safety
// flags, the healthy Running pod is untouched, and the successful kubectl
// round-trip clears any unreachable suppression for the cluster.
func TestReapOrphanedPodsDeletesOrphan(t *testing.T) {
	s := orphanSweepServer(t)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-orph-a1b2", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	ns := hiveHostedNamespacePrefix + "hosted-orph-a1b2"
	logPath := installOrphanKubectl(t, writeOrphanPodList(t, ns, 1), 0, 0)
	// Pre-arm an expired suppression entry so the success path visibly clears it.
	s.clusterUnreachableUntil[defaultClusterID] = time.Now().Add(-time.Minute)

	s.reapOrphanedPods()

	logged := readKubectlLog(t, logPath)
	if !strings.Contains(logged, "get pods -n "+ns+" -o json") {
		t.Errorf("expected a namespace-scoped pod list for %s, got:\n%s", ns, logged)
	}
	if !strings.Contains(logged, "delete pod orphan-0 -n "+ns+" --force --grace-period=0 --ignore-not-found") {
		t.Errorf("expected a guarded force-delete of orphan-0, got:\n%s", logged)
	}
	if strings.Contains(logged, "delete pod live-spoke") {
		t.Errorf("the Running pod must never be deleted, got:\n%s", logged)
	}
	if _, suppressed := s.clusterUnreachableUntil[defaultClusterID]; suppressed {
		t.Error("a successful pod list should mark the cluster reachable again")
	}
}

// TestReapOrphanedPodsDeleteFailureIsNonFatal: a failing force-delete is
// counted and logged but does not abort the sweep or panic — the orphan is
// simply retried next interval.
func TestReapOrphanedPodsDeleteFailureIsNonFatal(t *testing.T) {
	s := orphanSweepServer(t)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-orph-c3d4", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	ns := hiveHostedNamespacePrefix + "hosted-orph-c3d4"
	logPath := installOrphanKubectl(t, writeOrphanPodList(t, ns, 2), 0, 1)

	s.reapOrphanedPods()

	logged := readKubectlLog(t, logPath)
	// Both orphans are attempted: a per-pod failure must not short-circuit the loop.
	if !strings.Contains(logged, "delete pod orphan-0") || !strings.Contains(logged, "delete pod orphan-1") {
		t.Errorf("every orphan should be attempted despite delete failures, got:\n%s", logged)
	}
}

// TestReapOrphanedPodsListFailureSkipsNamespace: a failing `kubectl get pods`
// (namespace missing, transient error) skips the hive without any delete.
func TestReapOrphanedPodsListFailureSkipsNamespace(t *testing.T) {
	s := orphanSweepServer(t)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-orph-e5f6", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	ns := hiveHostedNamespacePrefix + "hosted-orph-e5f6"
	logPath := installOrphanKubectl(t, writeOrphanPodList(t, ns, 1), 1, 0)

	s.reapOrphanedPods()

	if logged := readKubectlLog(t, logPath); strings.Contains(logged, "delete pod") {
		t.Errorf("a failed pod list must not lead to any delete, got:\n%s", logged)
	}
}

// TestReapOrphanedPodsGarbageListSkipsNamespace: unparseable kubectl output is
// a warn-and-skip, never a delete — an unreadable list can never select a pod.
func TestReapOrphanedPodsGarbageListSkipsNamespace(t *testing.T) {
	s := orphanSweepServer(t)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-orph-g7h8", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	logPath := installOrphanKubectl(t, writeFixtureFile(t, "this is not json"), 0, 0)

	s.reapOrphanedPods()

	if logged := readKubectlLog(t, logPath); strings.Contains(logged, "delete pod") {
		t.Errorf("garbage pod list must not lead to any delete, got:\n%s", logged)
	}
}

// TestReapOrphanedPodsSkipsPullOnlyCluster: a pull-only cluster has no kubectl
// path from the hub — its hives are skipped with zero kubectl invocations.
func TestReapOrphanedPodsSkipsPullOnlyCluster(t *testing.T) {
	s := orphanSweepServer(t)
	s.clusters = map[string]ClusterConfig{
		defaultClusterID: {ID: defaultClusterID, PullOnly: true},
	}
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-orph-i9j0", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	logPath := installOrphanKubectl(t, writeFixtureFile(t, `{"items":[]}`), 0, 0)

	s.reapOrphanedPods()

	if logged := readKubectlLog(t, logPath); logged != "" {
		t.Errorf("pull-only cluster must see no kubectl calls, got:\n%s", logged)
	}
}

// TestReapOrphanedPodsSkipsSuppressedCluster: hives on a cluster inside its
// unreachable TTL are skipped entirely — no kubectl invocations at all.
func TestReapOrphanedPodsSkipsSuppressedCluster(t *testing.T) {
	s := orphanSweepServer(t)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-orph-k1l2", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	logPath := installOrphanKubectl(t, writeFixtureFile(t, `{"items":[]}`), 0, 0)
	s.clusterUnreachableUntil[defaultClusterID] = time.Now().Add(time.Hour)

	s.reapOrphanedPods()

	if logged := readKubectlLog(t, logPath); logged != "" {
		t.Errorf("suppressed cluster must see no kubectl calls, got:\n%s", logged)
	}
}

// TestReapOrphanedPodsSkipsUnresolvableCluster: a hive whose cluster_id
// resolves to no configured cluster (and no default fallback) is skipped
// before any kubectl call.
func TestReapOrphanedPodsSkipsUnresolvableCluster(t *testing.T) {
	s := orphanSweepServer(t)
	s.clusters = map[string]ClusterConfig{} // no default either
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-orph-m3n4", Status: "available", ClusterID: "ghost"}); err != nil {
		t.Fatal(err)
	}
	logPath := installOrphanKubectl(t, writeFixtureFile(t, `{"items":[]}`), 0, 0)

	s.reapOrphanedPods()

	if logged := readKubectlLog(t, logPath); logged != "" {
		t.Errorf("unresolvable cluster must see no kubectl calls, got:\n%s", logged)
	}
}

// TestReapOrphanedPodsCapsDeletesPerCycle: a namespace holding more orphans
// than orphanedPodMaxDeletesPerCycle stops at the cap and defers the rest —
// exactly the "predicate went wrong, stop and be noticed" guard.
func TestReapOrphanedPodsCapsDeletesPerCycle(t *testing.T) {
	s := orphanSweepServer(t)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-orph-o5p6", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	ns := hiveHostedNamespacePrefix + "hosted-orph-o5p6"
	logPath := installOrphanKubectl(t, writeOrphanPodList(t, ns, orphanedPodMaxDeletesPerCycle+3), 0, 0)

	s.reapOrphanedPods()

	deletes := strings.Count(readKubectlLog(t, logPath), "delete pod ")
	if deletes != orphanedPodMaxDeletesPerCycle {
		t.Errorf("expected exactly %d deletes (the per-cycle cap), got %d",
			orphanedPodMaxDeletesPerCycle, deletes)
	}
}

// TestReapOrphanedPodsHealthyNamespaceIsNoOp: a namespace with only a Running
// pod is listed but nothing is deleted — the no-orphans debug branch.
func TestReapOrphanedPodsHealthyNamespaceIsNoOp(t *testing.T) {
	s := orphanSweepServer(t)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-orph-q7r8", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	ns := hiveHostedNamespacePrefix + "hosted-orph-q7r8"
	logPath := installOrphanKubectl(t, writeOrphanPodList(t, ns, 0), 0, 0)

	s.reapOrphanedPods()

	logged := readKubectlLog(t, logPath)
	if !strings.Contains(logged, "get pods -n "+ns) {
		t.Errorf("healthy namespace should still be listed, got:\n%s", logged)
	}
	if strings.Contains(logged, "delete pod") {
		t.Errorf("healthy namespace must see no deletes, got:\n%s", logged)
	}
}

// TestReapOrphanedPodsEmptyRegistryIsQuietNoOp: with no hives at all the sweep
// terminates without kubectl calls and without tripping the scanned-nothing
// warn (that guard requires hives to exist).
func TestReapOrphanedPodsEmptyRegistryIsQuietNoOp(t *testing.T) {
	s := orphanSweepServer(t)
	logPath := installOrphanKubectl(t, writeFixtureFile(t, `{"items":[]}`), 0, 0)

	s.reapOrphanedPods()

	if logged := readKubectlLog(t, logPath); logged != "" {
		t.Errorf("empty registry must see no kubectl calls, got:\n%s", logged)
	}
}

// TestReapOrphanedPodsIfDueThrottles: the first call sweeps (lastOrphanedPodReap
// is zero), an immediate second call is inside orphanedPodReapInterval and must
// not sweep again.
func TestReapOrphanedPodsIfDueThrottles(t *testing.T) {
	s := orphanSweepServer(t)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-orph-s9t0", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	ns := hiveHostedNamespacePrefix + "hosted-orph-s9t0"
	logPath := installOrphanKubectl(t, writeOrphanPodList(t, ns, 0), 0, 0)

	s.reapOrphanedPodsIfDue()
	first := strings.Count(readKubectlLog(t, logPath), "get pods")
	if first != 1 {
		t.Fatalf("first IfDue call should sweep exactly once, got %d pod lists", first)
	}

	s.reapOrphanedPodsIfDue()
	if again := strings.Count(readKubectlLog(t, logPath), "get pods"); again != first {
		t.Errorf("second IfDue call inside the interval must not sweep, got %d pod lists", again)
	}
}

// TestReapOrphanedPodsIfDueRunsAfterInterval: a lastOrphanedPodReap older than
// the interval makes the sweep due again.
func TestReapOrphanedPodsIfDueRunsAfterInterval(t *testing.T) {
	s := orphanSweepServer(t)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-orph-u1v2", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	ns := hiveHostedNamespacePrefix + "hosted-orph-u1v2"
	logPath := installOrphanKubectl(t, writeOrphanPodList(t, ns, 0), 0, 0)
	s.lastOrphanedPodReap = time.Now().Add(-orphanedPodReapInterval - time.Minute)

	s.reapOrphanedPodsIfDue()

	if got := strings.Count(readKubectlLog(t, logPath), "get pods"); got != 1 {
		t.Errorf("a stale lastOrphanedPodReap must make the sweep due, got %d pod lists", got)
	}
}
