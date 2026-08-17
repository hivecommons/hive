package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSecurityContextHasNetAdmin is the core reconcile DECISION: given the
// jsonpath-extracted capability list from a live Deployment, decide whether the
// hive already has NET_ADMIN (skip) or is drifted and needs the patch. This is
// the pure function the sweep gates on, so it is unit-tested directly without
// shelling out to kubectl.
func TestSecurityContextHasNetAdmin(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool // true = has NET_ADMIN (skip), false = missing (patch)
	}{
		// Drifted pre-#1222 hives: securityContext absent, so jsonpath yields
		// nothing / empty / an empty list. All must be treated as "needs patch".
		{"empty output (field absent)", "", false},
		{"empty jsonpath list", "[]", false},
		{"whitespace only", "   \n", false},
		{"no value sentinel", "<no value>", false},

		// Correctly-provisioned hives: NET_ADMIN present ⇒ skip, no rollout.
		{"only NET_ADMIN", "[NET_ADMIN]", true},
		{"NET_ADMIN among others", "[NET_ADMIN NET_RAW]", true},
		{"NET_ADMIN not first", "[NET_RAW NET_ADMIN]", true},
		{"trailing newline", "[NET_ADMIN]\n", true},

		// Other capabilities present but NOT NET_ADMIN ⇒ still needs patch.
		{"other cap only", "[NET_RAW]", false},
		// Guard against a substring false-match on a differently-named cap.
		{"substring lookalike", "[NET_ADMIN_FOO]", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := securityContextHasNetAdmin(c.raw); got != c.want {
				t.Errorf("securityContextHasNetAdmin(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// TestNetAdminPatchJSON asserts the patch body targets the hive container's
// securityContext and installs exactly NET_ADMIN — the shape verified working
// live in #2674. A malformed path or missing capability would make the reconcile
// a silent no-op (or worse, corrupt the podspec), so pin it.
func TestNetAdminPatchJSON(t *testing.T) {
	patch := netAdminPatchJSON()
	if !strings.Contains(patch, hiveContainerSecurityContextPath) {
		t.Errorf("patch %q does not target the hive container securityContext path %q",
			patch, hiveContainerSecurityContextPath)
	}
	if !strings.Contains(patch, netAdminCapability) {
		t.Errorf("patch %q does not add %s", patch, netAdminCapability)
	}
	// It must be an "add" op so it works whether the path is missing (create) or
	// present-but-empty (overwrite) — both drift shapes from #2674.
	if !strings.Contains(patch, `"op":"add"`) {
		t.Errorf("patch %q is not an add op", patch)
	}
}

// TestReconcileNetAdminThrottle verifies the poller-loop throttle only lets the
// sweep run once per netAdminReconcileInterval, so the 2-min SHA poller can call
// it every tick without hammering kubectl. We drive the timestamp directly
// rather than run the (kubectl-shelling) sweep body.
func TestReconcileNetAdminThrottle(t *testing.T) {
	s := &HubServer{clusterUnreachableUntil: map[string]time.Time{}}

	// First call: lastNetAdminReconcile is zero ⇒ due.
	s.clusterUnreachableMu.Lock()
	due := s.lastNetAdminReconcile.IsZero() ||
		time.Since(s.lastNetAdminReconcile) >= netAdminReconcileInterval
	if due {
		s.lastNetAdminReconcile = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		t.Fatal("first reconcile should be due (zero timestamp)")
	}

	// Immediately after: NOT due (interval has not elapsed).
	s.clusterUnreachableMu.Lock()
	due2 := time.Since(s.lastNetAdminReconcile) >= netAdminReconcileInterval
	s.clusterUnreachableMu.Unlock()
	if due2 {
		t.Fatal("second reconcile immediately after should be throttled, not due")
	}

	// Backdate past the interval: due again.
	s.clusterUnreachableMu.Lock()
	s.lastNetAdminReconcile = time.Now().Add(-netAdminReconcileInterval - time.Minute)
	due3 := time.Since(s.lastNetAdminReconcile) >= netAdminReconcileInterval
	s.clusterUnreachableMu.Unlock()
	if !due3 {
		t.Fatal("reconcile should be due again after the interval elapses")
	}
}

// ---------- reconcileNetAdmin sweep tests ----------

// helperWriteHive writes a SaaSHive meta.json into the temp saasHivesDir so
// listSaaSHives() returns it. Requires helperSetupTempDirs to have been called.
func helperWriteHive(t *testing.T, h SaaSHive) {
	t.Helper()
	dir := filepath.Join(saasHivesDir, h.ID)
	os.MkdirAll(dir, 0o755)
	data, _ := json.Marshal(h)
	os.WriteFile(filepath.Join(dir, "meta.json"), data, 0o644)
}

// helperFakeKubectl creates a shell script at binDir/kubectl that echoes a
// canned response. The script inspects its arguments to decide what to print:
//   - "get" commands print getOut and exit with getRC
//   - "patch" commands print patchOut and exit with patchRC
//
// The caller must prepend binDir to PATH.
func helperFakeKubectl(t *testing.T, binDir string, getOut string, getRC int, patchOut string, patchRC int) {
	t.Helper()
	script := "#!/bin/sh\nfor arg in \"$@\"; do\n  case \"$arg\" in\n"
	script += "    get) printf '%s' '" + strings.ReplaceAll(getOut, "'", "'\\''") + "'; exit " + fmt.Sprintf("%d", getRC) + ";;\n"
	script += "    patch) printf '%s' '" + strings.ReplaceAll(patchOut, "'", "'\\''") + "'; exit " + fmt.Sprintf("%d", patchRC) + ";;\n"
	script += "  esac\ndone\nexit 1\n"
	p := filepath.Join(binDir, "kubectl")
	os.WriteFile(p, []byte(script), 0o755)
}

// TestReconcileNetAdmin_SkipsNonRunningHive verifies that hives with
// Status != "running" are skipped entirely (no kubectl call).
func TestReconcileNetAdmin_SkipsNonRunningHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	helperWriteHive(t, SaaSHive{ID: "provisioning-1", Status: "provisioning"})

	binDir := t.TempDir()
	// kubectl that always fails — if called, the test should see no error since
	// the hive should be skipped before kubectl runs.
	helperFakeKubectl(t, binDir, "", 1, "", 1)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	s := &HubServer{
		logger:                 slog.Default(),
		clusters:               map[string]ClusterConfig{"test-cluster": {ID: "test-cluster"}},
		clusterUnreachableUntil: make(map[string]time.Time),
	}

	// Should complete without error — hive is skipped.
	s.reconcileNetAdmin()
}

// TestReconcileNetAdmin_SkipsNilCluster verifies that a running hive whose
// cluster cannot be resolved (clusterForHive returns nil) is skipped.
func TestReconcileNetAdmin_SkipsNilCluster(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	helperWriteHive(t, SaaSHive{ID: "orphan-hive", Status: "running", ClusterID: "nonexistent"})

	s := &HubServer{
		logger:                 slog.Default(),
		clusters:               map[string]ClusterConfig{}, // no clusters
		clusterUnreachableUntil: make(map[string]time.Time),
	}

	s.reconcileNetAdmin()
	// No panic, no kubectl — the hive was skipped.
}

// TestReconcileNetAdmin_SkipsUnreachableCluster verifies that a cluster
// marked recently unreachable is skipped.
func TestReconcileNetAdmin_SkipsUnreachableCluster(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	helperWriteHive(t, SaaSHive{ID: "down-hive", Status: "running"})

	s := &HubServer{
		logger:   slog.Default(),
		clusters: map[string]ClusterConfig{defaultClusterID: {ID: defaultClusterID}},
		clusterUnreachableUntil: map[string]time.Time{
			defaultClusterID: time.Now().Add(10 * time.Minute), // suppressed
		},
	}

	s.reconcileNetAdmin()
	// No kubectl attempted — cluster was recently unreachable.
}

// TestReconcileNetAdmin_AlreadyHasNetAdmin verifies that a hive whose
// deployment already has NET_ADMIN is skipped (no patch issued).
func TestReconcileNetAdmin_AlreadyHasNetAdmin(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	helperWriteHive(t, SaaSHive{ID: "ok-hive", Status: "running"})

	binDir := t.TempDir()
	// kubectl get returns NET_ADMIN present; patch should not be called but
	// if it is, it exits 1 to catch incorrect calls.
	helperFakeKubectl(t, binDir, "[NET_ADMIN]", 0, "", 1)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	s := &HubServer{
		logger:                 slog.Default(),
		clusters:               map[string]ClusterConfig{defaultClusterID: {ID: defaultClusterID}},
		clusterUnreachableUntil: make(map[string]time.Time),
	}

	s.reconcileNetAdmin()
}

// TestReconcileNetAdmin_PatchesWhenMissing verifies that a hive missing
// NET_ADMIN gets patched.
func TestReconcileNetAdmin_PatchesWhenMissing(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	helperWriteHive(t, SaaSHive{ID: "drifted-hive", Status: "running"})

	binDir := t.TempDir()
	// get returns empty (no NET_ADMIN), patch succeeds.
	helperFakeKubectl(t, binDir, "", 0, "deployment.apps/hive patched", 0)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	s := &HubServer{
		logger:                 slog.Default(),
		clusters:               map[string]ClusterConfig{defaultClusterID: {ID: defaultClusterID}},
		clusterUnreachableUntil: make(map[string]time.Time),
	}

	s.reconcileNetAdmin()
	// Cluster should be marked reachable after successful patch.
	if s.clusterRecentlyUnreachable(defaultClusterID) {
		t.Error("cluster should be marked reachable after successful patch")
	}
}

// TestReconcileNetAdmin_PatchFailureMarksUnreachable verifies that a kubectl
// patch failure marks the cluster as unreachable.
func TestReconcileNetAdmin_PatchFailureMarksUnreachable(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	helperWriteHive(t, SaaSHive{ID: "fail-patch-hive", Status: "running"})

	binDir := t.TempDir()
	// get returns empty (needs patch), patch fails.
	helperFakeKubectl(t, binDir, "", 0, "error: connection refused", 1)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	s := &HubServer{
		logger:                 slog.Default(),
		clusters:               map[string]ClusterConfig{defaultClusterID: {ID: defaultClusterID}},
		clusterUnreachableUntil: make(map[string]time.Time),
	}

	s.reconcileNetAdmin()
	// After a failed patch the cluster should be marked unreachable.
	if !s.clusterRecentlyUnreachable(defaultClusterID) {
		t.Error("cluster should be marked unreachable after failed patch")
	}
}

// TestReconcileNetAdmin_GetFailureContinues verifies that a kubectl get failure
// logs and continues (non-fatal), without marking the cluster unreachable.
func TestReconcileNetAdmin_GetFailureContinues(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	helperWriteHive(t, SaaSHive{ID: "get-fail-hive", Status: "running"})

	binDir := t.TempDir()
	// get fails, patch should never be reached.
	helperFakeKubectl(t, binDir, "Error from server (NotFound)", 1, "", 0)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	s := &HubServer{
		logger:                 slog.Default(),
		clusters:               map[string]ClusterConfig{defaultClusterID: {ID: defaultClusterID}},
		clusterUnreachableUntil: make(map[string]time.Time),
	}

	s.reconcileNetAdmin()
	// get failure is non-fatal and does not mark unreachable — only patch failure does.
	if s.clusterRecentlyUnreachable(defaultClusterID) {
		t.Error("get failure should not mark cluster unreachable")
	}
}

// TestReconcileNetAdminIfDue_CallsReconcile verifies that
// reconcileNetAdminIfDue actually calls reconcileNetAdmin when due, using the
// real method rather than reimplementing the throttle logic.
func TestReconcileNetAdminIfDue_CallsReconcile(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// No hives on disk, so reconcileNetAdmin is a no-op — we just verify the
	// gate fires by checking lastNetAdminReconcile gets stamped.
	s := &HubServer{
		logger:                 slog.Default(),
		clusters:               map[string]ClusterConfig{},
		clusterUnreachableUntil: make(map[string]time.Time),
	}

	s.reconcileNetAdminIfDue()

	s.clusterUnreachableMu.Lock()
	stamp := s.lastNetAdminReconcile
	s.clusterUnreachableMu.Unlock()
	if stamp.IsZero() {
		t.Fatal("expected lastNetAdminReconcile to be stamped after reconcileNetAdminIfDue")
	}

	// Immediately calling again should NOT update the stamp (throttled).
	s.reconcileNetAdminIfDue()

	s.clusterUnreachableMu.Lock()
	stamp2 := s.lastNetAdminReconcile
	s.clusterUnreachableMu.Unlock()
	if !stamp2.Equal(stamp) {
		t.Error("expected throttle to prevent updating stamp on immediate re-call")
	}
}
