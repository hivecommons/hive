package hub

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// netAdminSweepServer builds a HubServer whose saasHivesDir points at a
// per-test TempDir and whose only cluster is an in-cluster default, so
// reconcileNetAdmin exercises its real hive-selection and kubectl-shelling
// paths against fixtures instead of live hub state.
func netAdminSweepServer(t *testing.T) *HubServer {
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

// installNetAdminKubectl writes a fake kubectl that logs every invocation and
// scripts the two calls the sweep makes: `get` prints getOut and exits with
// getExit, `patch` exits with patchExit. Returns the invocation log path.
func installNetAdminKubectl(t *testing.T, getOut string, getExit, patchExit int) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kubectl.log")
	script := `#!/bin/sh
echo "$*" >> "` + logPath + `"
case " $* " in
*" get "*)
	printf '%s' '` + getOut + `'
	exit ` + strconv.Itoa(getExit) + `
	;;
*" patch "*)
	exit ` + strconv.Itoa(patchExit) + `
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

func readKubectlLog(t *testing.T, logPath string) string {
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

// TestReconcileNetAdminPatchesDriftedHive is the happy remediation path: a
// swept hive whose Deployment reports no NET_ADMIN gets exactly one JSON patch
// against deployment/hive in its hosted namespace, and the successful kubectl
// round-trip clears any unreachable suppression for the cluster.
func TestReconcileNetAdminPatchesDriftedHive(t *testing.T) {
	s := netAdminSweepServer(t)
	logPath := installNetAdminKubectl(t, "[]", 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-drift-a1b2", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	// Pre-arm an expired suppression entry so the success path visibly clears it.
	s.clusterUnreachableUntil[defaultClusterID] = time.Now().Add(-time.Minute)

	s.reconcileNetAdmin()

	logged := readKubectlLog(t, logPath)
	ns := hiveHostedNamespacePrefix + "hosted-drift-a1b2"
	if !strings.Contains(logged, "get deployment hive -n "+ns) {
		t.Errorf("expected a securityContext read for %s, got:\n%s", ns, logged)
	}
	if !strings.Contains(logged, "patch deployment hive -n "+ns) {
		t.Errorf("expected a NET_ADMIN patch for %s, got:\n%s", ns, logged)
	}
	if !strings.Contains(logged, netAdminCapability) {
		t.Errorf("patch args should carry %s, got:\n%s", netAdminCapability, logged)
	}
	if _, suppressed := s.clusterUnreachableUntil[defaultClusterID]; suppressed {
		t.Error("successful patch should mark the cluster reachable again")
	}
}

// TestReconcileNetAdminSkipsCorrectHive: a hive already carrying NET_ADMIN is
// read but never patched — the idempotence that guarantees the sweep can never
// loop a rolling update.
func TestReconcileNetAdminSkipsCorrectHive(t *testing.T) {
	s := netAdminSweepServer(t)
	logPath := installNetAdminKubectl(t, "[NET_ADMIN]", 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-ok-c3d4", Status: "assigned"}); err != nil {
		t.Fatal(err)
	}

	s.reconcileNetAdmin()

	logged := readKubectlLog(t, logPath)
	if !strings.Contains(logged, "get deployment hive") {
		t.Errorf("expected the securityContext read, got:\n%s", logged)
	}
	if strings.Contains(logged, "patch") {
		t.Errorf("already-correct hive must not be patched, got:\n%s", logged)
	}
}

// TestReconcileNetAdminGetFailureIsNonFatal: a failing `kubectl get` (deployment
// missing, transient error) skips the hive without patching and without arming
// the cluster-unreachable breaker — the next sweep simply retries.
func TestReconcileNetAdminGetFailureIsNonFatal(t *testing.T) {
	s := netAdminSweepServer(t)
	logPath := installNetAdminKubectl(t, "", 1, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-gone-e5f6", Status: "available"}); err != nil {
		t.Fatal(err)
	}

	s.reconcileNetAdmin()

	logged := readKubectlLog(t, logPath)
	if strings.Contains(logged, "patch") {
		t.Errorf("failed read must not lead to a patch, got:\n%s", logged)
	}
	if s.clusterRecentlyUnreachable(defaultClusterID) {
		t.Error("a failed get must not arm the unreachable breaker")
	}
}

// TestReconcileNetAdminPatchFailureArmsBreaker: a drifted hive whose patch
// fails arms the cluster-unreachable suppression so the sweep does not burn a
// kubectl timeout per hive on a downed cluster.
func TestReconcileNetAdminPatchFailureArmsBreaker(t *testing.T) {
	s := netAdminSweepServer(t)
	installNetAdminKubectl(t, "[]", 0, 1)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-down-g7h8", Status: "available"}); err != nil {
		t.Fatal(err)
	}

	s.reconcileNetAdmin()

	if !s.clusterRecentlyUnreachable(defaultClusterID) {
		t.Error("a failed patch must arm the unreachable breaker for the cluster")
	}
}

// TestReconcileNetAdminSkipsSuppressedCluster: hives on a cluster inside its
// unreachable TTL are skipped entirely — no kubectl invocations at all.
func TestReconcileNetAdminSkipsSuppressedCluster(t *testing.T) {
	s := netAdminSweepServer(t)
	logPath := installNetAdminKubectl(t, "[]", 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-supp-i9j0", Status: "available"}); err != nil {
		t.Fatal(err)
	}
	s.clusterUnreachableUntil[defaultClusterID] = time.Now().Add(time.Hour)

	s.reconcileNetAdmin()

	if logged := readKubectlLog(t, logPath); logged != "" {
		t.Errorf("suppressed cluster must see no kubectl calls, got:\n%s", logged)
	}
}

// TestReconcileNetAdminSkipsUnresolvableCluster: a hive whose cluster_id
// resolves to no configured cluster (and no default fallback) is skipped
// before any kubectl call.
func TestReconcileNetAdminSkipsUnresolvableCluster(t *testing.T) {
	s := netAdminSweepServer(t)
	s.clusters = map[string]ClusterConfig{} // no default either
	logPath := installNetAdminKubectl(t, "[]", 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-noclu-k1l2", Status: "available", ClusterID: "ghost"}); err != nil {
		t.Fatal(err)
	}

	s.reconcileNetAdmin()

	if logged := readKubectlLog(t, logPath); logged != "" {
		t.Errorf("unresolvable cluster must see no kubectl calls, got:\n%s", logged)
	}
}

// TestReconcileNetAdminEmptySelectionIsQuietNoOp: a fleet where every hive is
// still provisioning selects nobody — the sweep must terminate without any
// kubectl call (this drives the considered==0 warn branch, the exact silent
// no-op failure the selection accounting exists to surface).
func TestReconcileNetAdminEmptySelectionIsQuietNoOp(t *testing.T) {
	s := netAdminSweepServer(t)
	logPath := installNetAdminKubectl(t, "[]", 0, 0)
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-new-m3n4", Status: "provisioning"}); err != nil {
		t.Fatal(err)
	}

	s.reconcileNetAdmin()

	if logged := readKubectlLog(t, logPath); logged != "" {
		t.Errorf("all-provisioning fleet must see no kubectl calls, got:\n%s", logged)
	}
}
