package hub

import (
	"bytes"
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

// These tests cover the reapOrphanedPods sweep itself — namespace selection,
// the kubectl get/delete round-trip, failure accounting, the per-cycle delete
// cap, and the reapOrphanedPodsIfDue throttle — using the same fixture shape
// as the NET_ADMIN sweep tests: a per-test saasHivesDir, an in-cluster default
// cluster, and a fake kubectl on PATH that logs every invocation. The pure
// predicate/parse/select units are covered in orphaned_pod_reaper_test.go.

// reaperSweepServer builds a HubServer whose saasHivesDir points at a per-test
// TempDir and whose only cluster is the given one, so reapOrphanedPods
// exercises its real hive-selection and kubectl-shelling paths against
// fixtures instead of live hub state.
func reaperSweepServer(t *testing.T, cluster ClusterConfig, logSink io.Writer) *HubServer {
	t.Helper()
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })
	if logSink == nil {
		logSink = io.Discard
	}
	return &HubServer{
		logger: slog.New(slog.NewTextHandler(logSink, &slog.HandlerOptions{Level: slog.LevelDebug})),
		clusters: map[string]ClusterConfig{
			cluster.ID: cluster,
		},
		clusterUnreachableUntil: map[string]time.Time{},
	}
}

// installReaperKubectl writes a fake kubectl that logs every invocation and
// scripts the two calls the sweep makes: `get pods` cats getJSONPath and exits
// getExit, `delete pod` exits deleteExit. Returns the invocation log path.
func installReaperKubectl(t *testing.T, podListJSON string, getExit, deleteExit int) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kubectl.log")
	jsonPath := filepath.Join(dir, "pods.json")
	if err := os.WriteFile(jsonPath, []byte(podListJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
echo "$*" >> "` + logPath + `"
case " $* " in
*" get pods "*)
	cat "` + jsonPath + `"
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

func readReaperKubectlLog(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("reading kubectl log: %v", err)
	}
	return string(data)
}

// reaperPodListJSON builds a `kubectl get pods -o json` document from the
// candidate shape the parser consumes, so fixtures stay in one place.
func reaperPodListJSON(t *testing.T, pods []orphanedPodCandidate) string {
	t.Helper()
	type meta struct {
		Name              string   `json:"name"`
		Namespace         string   `json:"namespace"`
		DeletionTimestamp string   `json:"deletionTimestamp,omitempty"`
		Finalizers        []string `json:"finalizers,omitempty"`
	}
	type item struct {
		Metadata meta `json:"metadata"`
		Status   struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	var items []item
	for _, p := range pods {
		it := item{Metadata: meta{
			Name:       p.Name,
			Namespace:  p.Namespace,
			Finalizers: p.Finalizers,
		}}
		if !p.DeletionTimestamp.IsZero() {
			it.Metadata.DeletionTimestamp = p.DeletionTimestamp.Format(time.RFC3339)
		}
		it.Status.Phase = p.Phase
		items = append(items, it)
	}
	doc := map[string]any{"items": items}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestReapOrphanedPodsDeletesOrphanAndSparesOthers is the happy sweep path:
// one hive whose pod list holds an orphaned-Terminating pod, a Running pod,
// and a Terminating pod protected by a finalizer. Exactly the orphan is
// force-deleted, in the hive's hosted namespace, and the successful kubectl
// round-trip clears any unreachable suppression for the cluster.
func TestReapOrphanedPodsDeletesOrphanAndSparesOthers(t *testing.T) {
	var logBuf bytes.Buffer
	s := reaperSweepServer(t, ClusterConfig{ID: defaultClusterID, InCluster: true}, &logBuf)
	podList := reaperPodListJSON(t, []orphanedPodCandidate{
		{Name: "hive-orphan", DeletionTimestamp: time.Now().Add(-2 * time.Hour), Phase: "Failed"},
		{Name: "hive-live", Phase: podPhaseRunning},
		{Name: "hive-finalized", DeletionTimestamp: time.Now().Add(-2 * time.Hour),
			Finalizers: []string{"example.io/cleanup"}, Phase: "Failed"},
	})
	logPath := installReaperKubectl(t, podList, 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-reap-a1b2", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	// Pre-arm an expired suppression entry so the success path visibly clears it.
	s.clusterUnreachableUntil[defaultClusterID] = time.Now().Add(-time.Minute)

	s.reapOrphanedPods()

	logged := readReaperKubectlLog(t, logPath)
	ns := hiveHostedNamespacePrefix + "hosted-reap-a1b2"
	if !strings.Contains(logged, "get pods -n "+ns) {
		t.Errorf("expected a pod list for %s, got:\n%s", ns, logged)
	}
	if !strings.Contains(logged, "delete pod hive-orphan -n "+ns+" --force --grace-period=0 --ignore-not-found") {
		t.Errorf("expected a force-delete of hive-orphan in %s, got:\n%s", ns, logged)
	}
	if strings.Contains(logged, "delete pod hive-live") {
		t.Errorf("Running pod must never be deleted, got:\n%s", logged)
	}
	if strings.Contains(logged, "delete pod hive-finalized") {
		t.Errorf("pod with finalizers must never be deleted, got:\n%s", logged)
	}
	if _, suppressed := s.clusterUnreachableUntil[defaultClusterID]; suppressed {
		t.Error("successful pod list should mark the cluster reachable again")
	}
	if !strings.Contains(logBuf.String(), "reaped orphaned terminating pod") {
		t.Errorf("each deletion must be logged at INFO, got:\n%s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "orphaned-pod reap sweep complete") {
		t.Errorf("a reaping sweep must log its summary, got:\n%s", logBuf.String())
	}
}

// TestReapOrphanedPodsCleanFleetIsQuiet: a namespace with only healthy pods is
// scanned, nothing is deleted, and the sweep completes without the INFO
// summary (the healthy fleet stays quiet).
func TestReapOrphanedPodsCleanFleetIsQuiet(t *testing.T) {
	s := reaperSweepServer(t, ClusterConfig{ID: defaultClusterID, InCluster: true}, nil)
	podList := reaperPodListJSON(t, []orphanedPodCandidate{
		{Name: "hive-live", Phase: podPhaseRunning},
	})
	logPath := installReaperKubectl(t, podList, 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-clean-c3d4", Status: "assigned"}); err != nil {
		t.Fatal(err)
	}

	s.reapOrphanedPods()

	logged := readReaperKubectlLog(t, logPath)
	if !strings.Contains(logged, "get pods") {
		t.Errorf("expected the pod list read, got:\n%s", logged)
	}
	if strings.Contains(logged, "delete pod") {
		t.Errorf("healthy namespace must not see any delete, got:\n%s", logged)
	}
}

// TestReapOrphanedPodsDeleteFailureIsNonFatal: a failing force-delete is
// counted and logged at Warn, and the sweep still completes — the orphan is
// retried on the next interval rather than aborting the sweep.
func TestReapOrphanedPodsDeleteFailureIsNonFatal(t *testing.T) {
	var logBuf bytes.Buffer
	s := reaperSweepServer(t, ClusterConfig{ID: defaultClusterID, InCluster: true}, &logBuf)
	podList := reaperPodListJSON(t, []orphanedPodCandidate{
		{Name: "hive-stuck", DeletionTimestamp: time.Now().Add(-2 * time.Hour), Phase: "Failed"},
	})
	logPath := installReaperKubectl(t, podList, 0, 1)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-fail-e5f6", Status: "available"}); err != nil {
		t.Fatal(err)
	}

	s.reapOrphanedPods()

	logged := readReaperKubectlLog(t, logPath)
	if !strings.Contains(logged, "delete pod hive-stuck") {
		t.Errorf("expected a force-delete attempt, got:\n%s", logged)
	}
	if !strings.Contains(logBuf.String(), "force-delete failed") {
		t.Errorf("a failed delete must be logged at Warn, got:\n%s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "delete_failures=1") {
		t.Errorf("the sweep summary must count the failure, got:\n%s", logBuf.String())
	}
}

// TestReapOrphanedPodsListFailureIsLoud: when every pod list fails, no
// namespace is scanned and the sweep says so at Warn — a no-op sweep on a
// non-empty registry is a bug signal, not a quiet success.
func TestReapOrphanedPodsListFailureIsLoud(t *testing.T) {
	var logBuf bytes.Buffer
	s := reaperSweepServer(t, ClusterConfig{ID: defaultClusterID, InCluster: true}, &logBuf)
	logPath := installReaperKubectl(t, "{}", 1, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-nolist-a7b8", Status: "available"}); err != nil {
		t.Fatal(err)
	}

	s.reapOrphanedPods()

	logged := readReaperKubectlLog(t, logPath)
	if strings.Contains(logged, "delete pod") {
		t.Errorf("a failed list must never lead to a delete, got:\n%s", logged)
	}
	if !strings.Contains(logBuf.String(), "scanned NO namespaces") {
		t.Errorf("a sweep that scanned nothing must warn, got:\n%s", logBuf.String())
	}
}

// TestReapOrphanedPodsBadJSONIsNonFatal: unparseable kubectl output is logged
// at Warn and the namespace is skipped — never guessed at.
func TestReapOrphanedPodsBadJSONIsNonFatal(t *testing.T) {
	var logBuf bytes.Buffer
	s := reaperSweepServer(t, ClusterConfig{ID: defaultClusterID, InCluster: true}, &logBuf)
	logPath := installReaperKubectl(t, "not json at all", 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-badjson-c9d0", Status: "available"}); err != nil {
		t.Fatal(err)
	}

	s.reapOrphanedPods()

	logged := readReaperKubectlLog(t, logPath)
	if strings.Contains(logged, "delete pod") {
		t.Errorf("unparseable output must never lead to a delete, got:\n%s", logged)
	}
	if !strings.Contains(logBuf.String(), "could not parse pod list") {
		t.Errorf("a parse failure must be logged at Warn, got:\n%s", logBuf.String())
	}
}

// TestReapOrphanedPodsSkipsPullOnlyCluster: a pull-only cluster has no kubectl
// path from the hub. The sweep must not shell out at all and must count the
// skip so the gap is visible rather than silent.
func TestReapOrphanedPodsSkipsPullOnlyCluster(t *testing.T) {
	var logBuf bytes.Buffer
	s := reaperSweepServer(t, ClusterConfig{ID: defaultClusterID, PullOnly: true}, &logBuf)
	logPath := installReaperKubectl(t, "{}", 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-pullonly-e1f2", Status: "available"}); err != nil {
		t.Fatal(err)
	}

	s.reapOrphanedPods()

	if logged := readReaperKubectlLog(t, logPath); logged != "" {
		t.Errorf("pull-only cluster must never see kubectl, got:\n%s", logged)
	}
	if !strings.Contains(logBuf.String(), "namespaces_skipped_unreachable=1") {
		t.Errorf("the skip must be counted, got:\n%s", logBuf.String())
	}
}

// TestReapOrphanedPodsSkipsRecentlyUnreachableCluster: a cluster inside its
// unreachable-suppression window is skipped without a kubectl call, the same
// breaker every other hub-side kubectl caller honours.
func TestReapOrphanedPodsSkipsRecentlyUnreachableCluster(t *testing.T) {
	s := reaperSweepServer(t, ClusterConfig{ID: defaultClusterID, InCluster: true}, nil)
	logPath := installReaperKubectl(t, "{}", 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-suppressed-a3b4", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	s.clusterUnreachableUntil[defaultClusterID] = time.Now().Add(time.Minute)

	s.reapOrphanedPods()

	if logged := readReaperKubectlLog(t, logPath); logged != "" {
		t.Errorf("suppressed cluster must never see kubectl, got:\n%s", logged)
	}
}

// TestReapOrphanedPodsHonorsDeleteCap: a pod list with more orphans than
// orphanedPodMaxDeletesPerCycle stops at the cap, defers the remainder to the
// next sweep, and says so at Warn — grinding through an unexpected pile is the
// failure mode the cap exists to catch.
func TestReapOrphanedPodsHonorsDeleteCap(t *testing.T) {
	var logBuf bytes.Buffer
	s := reaperSweepServer(t, ClusterConfig{ID: defaultClusterID, InCluster: true}, &logBuf)
	var pods []orphanedPodCandidate
	for i := 0; i < orphanedPodMaxDeletesPerCycle+10; i++ {
		pods = append(pods, orphanedPodCandidate{
			Name:              fmt.Sprintf("hive-orphan-%03d", i),
			DeletionTimestamp: time.Now().Add(-2 * time.Hour),
			Phase:             "Failed",
		})
	}
	logPath := installReaperKubectl(t, reaperPodListJSON(t, pods), 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-cap-c5d6", Status: "available"}); err != nil {
		t.Fatal(err)
	}

	s.reapOrphanedPods()

	deletes := strings.Count(readReaperKubectlLog(t, logPath), "delete pod ")
	if deletes != orphanedPodMaxDeletesPerCycle {
		t.Errorf("sweep deleted %d pods, want exactly the cap of %d", deletes, orphanedPodMaxDeletesPerCycle)
	}
	if !strings.Contains(logBuf.String(), "hit the per-cycle delete cap") {
		t.Errorf("hitting the cap must be loud, got:\n%s", logBuf.String())
	}
}

// TestReapOrphanedPodsIfDueThrottles: the first call sweeps, a second call
// inside orphanedPodReapInterval is a no-op, so the poller loop can call it
// every tick without hammering kubectl.
func TestReapOrphanedPodsIfDueThrottles(t *testing.T) {
	s := reaperSweepServer(t, ClusterConfig{ID: defaultClusterID, InCluster: true}, nil)
	logPath := installReaperKubectl(t, reaperPodListJSON(t, nil), 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-throttle-e7f8", Status: "available"}); err != nil {
		t.Fatal(err)
	}

	s.reapOrphanedPodsIfDue()
	first := strings.Count(readReaperKubectlLog(t, logPath), "get pods")
	if first != 1 {
		t.Fatalf("first due call should sweep exactly once, got %d pod lists", first)
	}
	if s.lastOrphanedPodReap.IsZero() {
		t.Fatal("a due sweep must stamp lastOrphanedPodReap")
	}

	s.reapOrphanedPodsIfDue()
	if again := strings.Count(readReaperKubectlLog(t, logPath), "get pods"); again != first {
		t.Errorf("second call inside the interval must be a no-op, got %d pod lists", again)
	}
}
