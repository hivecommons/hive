package hub

// End-to-end tests for reconcilePerHiveEnv — the sweep LOOP itself, not its
// helpers. The helpers (drift, patch JSON, rate-limit predicate, status
// filter) each have their own tests in perhive_env_reconcile_test.go, but the
// loop that wires them together shipped with a selection bug that no helper
// test could catch, and before these tests it had no coverage at all: nothing
// exercised the kubectl read/patch path, the unreachable accounting, the
// observation bookkeeping, or the stale-hive eviction.
//
// kubectl is faked with a shell script placed FIRST on PATH. The sweep builds
// its commands with exec.CommandContext("kubectl", ...), so the fake receives
// exactly the arguments production would send, and the tests drive real
// subprocess execution end to end without touching any cluster. Per-hive
// behaviour is keyed off the -n hive-hosted-<id> namespace argument.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installFakeKubectl writes a fake kubectl into a temp dir, prepends that dir
// to PATH, and returns the dir. Per-hive responses are configured with files
// in the same dir:
//
//	env-<hive>.json   — body returned for `get deployment hive -n hive-hosted-<hive>`
//	(absent)          — the get FAILS, like a missing Deployment or a dead cluster
//	patch-fail-<hive> — `patch` for that hive exits non-zero
//
// Every invocation is appended to invocations.log for assertions.
func installFakeKubectl(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
echo "$*" >> "$FAKE_KUBECTL_DIR/invocations.log"
mode=""
ns=""
prev=""
for a in "$@"; do
  if [ -z "$mode" ]; then
    case "$a" in
      get|patch) mode="$a" ;;
    esac
  fi
  if [ "$prev" = "-n" ]; then ns="$a"; fi
  prev="$a"
done
hive="${ns#hive-hosted-}"
case "$mode" in
  get)
    if [ -f "$FAKE_KUBECTL_DIR/env-$hive.json" ]; then
      cat "$FAKE_KUBECTL_DIR/env-$hive.json"
      exit 0
    fi
    echo "Error from server (NotFound): deployments.apps \"hive\" not found" >&2
    exit 1
    ;;
  patch)
    if [ -f "$FAKE_KUBECTL_DIR/patch-fail-$hive" ]; then
      echo "patch refused by fake kubectl" >&2
      exit 1
    fi
    exit 0
    ;;
esac
echo "fake kubectl: unrecognised invocation" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_KUBECTL_DIR", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// fakeDeploymentEnv installs the env list the fake kubectl returns for one
// hive's Deployment read.
func fakeDeploymentEnv(t *testing.T, dir, hiveID string, env []deploymentEnvVar) {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env-"+hiveID+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// convergedEnv builds a fully-converged live env list for hiveID — exactly the
// desired vars, so the sweep must issue NO patch for it.
func convergedEnv(t *testing.T, hiveID string) []deploymentEnvVar {
	t.Helper()
	want := desiredPerHiveEnv(hiveID)
	if want == nil {
		t.Fatal("fixture hive derived to nil — is the test master set?")
	}
	env := []deploymentEnvVar{{Name: "HIVE_ID", Value: hiveID}}
	for name, value := range want {
		env = append(env, deploymentEnvVar{Name: name, Value: value})
	}
	return env
}

// fakeKubectlInvocations returns the logged fake-kubectl invocations, one
// argv-joined line per call.
func fakeKubectlInvocations(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "invocations.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func countPatchInvocations(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	for _, line := range fakeKubectlInvocations(t, dir) {
		if strings.Contains(line, " patch deployment hive ") {
			n++
		}
	}
	return n
}

// TestReconcilePerHiveEnvSweepMixedFleet runs ONE sweep over a fleet holding
// every posture the loop distinguishes, and asserts each hive lands in the
// right bucket. Everything here is per-sweep state, so one run must produce
// all of it consistently:
//
//	converged   → read, observed, NOT patched (the no-restart-storm branch)
//	drifted     → read, observed WITH the drift, patched via kubectl
//	unreadable  → get fails: unreachable, NOT observed (unread ≠ converged)
//	bad JSON    → parse fails: unreachable, NOT observed
//	no cluster  → registry resolves nothing: unreachable, no kubectl call
//	pull-only   → unreachable by declaration, no kubectl call
//	provisioning→ skipped by status, never considered
func TestReconcilePerHiveEnvSweepMixedFleet(t *testing.T) {
	withTestMaster(t, perHiveEnvTestMaster)
	withTempHivesDir(t)
	fake := installFakeKubectl(t)

	s := &HubServer{
		logger: appKeyTestLogger(),
		clusters: map[string]ClusterConfig{
			"c1":   {ID: "c1", InCluster: true},
			"pull": {ID: "pull", PullOnly: true},
		},
	}

	// A hive the hub observed in some earlier sweep but which no longer exists
	// in the registry: the sweep must evict it, or a deprovisioned spoke pins
	// the readiness counts non-zero forever.
	s.recordPerHiveEnvObservation("long-gone", perHiveEnvObservation{Observed: time.Now()})

	mustSaveHive(t, &SaaSHive{ID: "conv", ClusterID: "c1", Status: "available"})
	mustSaveHive(t, &SaaSHive{ID: "drift", ClusterID: "c1", Status: "available"})
	mustSaveHive(t, &SaaSHive{ID: "unread", ClusterID: "c1", Status: "available"})
	mustSaveHive(t, &SaaSHive{ID: "badjson", ClusterID: "c1", Status: "available"})
	mustSaveHive(t, &SaaSHive{ID: "ghostcluster", ClusterID: "ghost", Status: "available"})
	mustSaveHive(t, &SaaSHive{ID: "pullhive", ClusterID: "pull", Status: "available"})
	mustSaveHive(t, &SaaSHive{ID: "provhive", ClusterID: "c1", Status: "provisioning"})

	fakeDeploymentEnv(t, fake, "conv", convergedEnv(t, "conv"))
	// drift: converged except the terminal key is missing entirely.
	driftedEnv := convergedEnv(t, "drift")
	trimmed := driftedEnv[:0]
	for _, e := range driftedEnv {
		if e.Name != EnvTerminalKey {
			trimmed = append(trimmed, e)
		}
	}
	fakeDeploymentEnv(t, fake, "drift", trimmed)
	// unread: no env file installed, so its get FAILS.
	if err := os.WriteFile(filepath.Join(fake, "env-badjson.json"), []byte("not json at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	s.reconcilePerHiveEnv()

	s.perHiveEnvMu.Lock()
	considered := s.perHiveEnvConsidered
	skipped := s.perHiveEnvSkippedByStatus
	unreachable := s.perHiveEnvUnreachable
	unreachableClusters := append([]string(nil), s.perHiveEnvUnreachableClusters...)
	seen := make(map[string]perHiveEnvObservation, len(s.perHiveEnvSeen))
	for id, obs := range s.perHiveEnvSeen {
		seen[id] = obs
	}
	s.perHiveEnvMu.Unlock()

	if considered != 6 {
		t.Errorf("considered = %d, want 6 (everything but the provisioning hive)", considered)
	}
	if skipped != 1 {
		t.Errorf("skippedByStatus = %d, want 1 (the provisioning hive)", skipped)
	}
	// ghostcluster (no registry entry), pullhive (pull-only), unread (get
	// failed), badjson (unparseable read) — all four are hives a rotation
	// would strand, and none may be counted as converged.
	if unreachable != 4 {
		t.Errorf("unreachable = %d, want 4 (ghostcluster, pullhive, unread, badjson)", unreachable)
	}
	if got, want := strings.Join(unreachableClusters, ","), "c1,pull"; got != want {
		t.Errorf("unreachableClusters = %q, want %q (sorted; ghostcluster has no cluster ID to name)", got, want)
	}

	// Observations: only the two hives whose Deployment was actually READ.
	convObs, ok := seen["conv"]
	if !ok {
		t.Fatal("converged hive was read but not recorded — the readiness denominator undercounts")
	}
	if len(convObs.MissingVars) != 0 {
		t.Errorf("converged hive recorded drift %v, want none", convObs.MissingVars)
	}
	driftObs, ok := seen["drift"]
	if !ok {
		t.Fatal("drifted hive was read but not recorded")
	}
	if len(driftObs.MissingVars) != 1 || driftObs.MissingVars[0] != EnvTerminalKey {
		t.Errorf("drifted hive recorded MissingVars = %v, want exactly [%s]", driftObs.MissingVars, EnvTerminalKey)
	}
	for _, id := range []string{"unread", "badjson", "ghostcluster", "pullhive", "provhive"} {
		if _, ok := seen[id]; ok {
			t.Errorf("hive %q was never successfully read but has an observation — an unread hive must not count as converged", id)
		}
	}
	if _, ok := seen["long-gone"]; ok {
		t.Error("deprovisioned hive survived the sweep — stale observations pin the counts non-zero forever")
	}

	// Exactly ONE patch: the drifted hive. A patch for the converged hive is
	// the rolling-restart storm; a patch for anything unreachable is a write
	// based on no read.
	if n := countPatchInvocations(t, fake); n != 1 {
		t.Errorf("kubectl patch invoked %d times, want exactly 1 (the drifted hive)", n)
	}
	patched := false
	for _, line := range fakeKubectlInvocations(t, fake) {
		if strings.Contains(line, " patch deployment hive ") && strings.Contains(line, "hive-hosted-drift") {
			patched = true
		}
	}
	if !patched {
		t.Error("the one patch did not target hive-hosted-drift")
	}
	// The two kubectl-level failures happened on c1, but the sweep also
	// SUCCEEDED against c1 (conv, drift): a per-hive read failure must not trip
	// the cluster breaker, or one missing Deployment would blind the hub to a
	// whole cluster for the TTL.
	if s.clusterRecentlyUnreachable("c1") {
		t.Error("c1 tripped the unreachable breaker from per-hive read failures despite successful reads in the same sweep")
	}
}

// TestReconcilePerHiveEnvSweepRateLimit drives the sweep over a fully-drifted
// fleet larger than the per-cycle cap and asserts the LOOP enforces it: the
// helper predicate is tested elsewhere, but only the sweep decides to keep
// READING (and recording) after the patch budget is spent.
func TestReconcilePerHiveEnvSweepRateLimit(t *testing.T) {
	withTestMaster(t, perHiveEnvTestMaster)
	withTempHivesDir(t)
	fake := installFakeKubectl(t)

	s := &HubServer{
		logger:   appKeyTestLogger(),
		clusters: map[string]ClusterConfig{"c1": {ID: "c1", InCluster: true}},
	}

	const fleetSize = perHiveEnvMaxPatchesPerCycle + 2
	for i := 0; i < fleetSize; i++ {
		id := fmt.Sprintf("drifted-%d", i)
		mustSaveHive(t, &SaaSHive{ID: id, ClusterID: "c1", Status: "available"})
		// Empty env list: every desired var is missing, maximal drift.
		fakeDeploymentEnv(t, fake, id, []deploymentEnvVar{})
	}

	s.reconcilePerHiveEnv()

	if n := countPatchInvocations(t, fake); n != perHiveEnvMaxPatchesPerCycle {
		t.Errorf("kubectl patch invoked %d times, want the cap of %d — each patch rolls a tenant pod",
			n, perHiveEnvMaxPatchesPerCycle)
	}
	// The cap limits WRITES only. Every hive must still have been read and
	// recorded, so the readiness surface reflects the whole fleet while
	// remediation is rate-limited.
	s.perHiveEnvMu.Lock()
	observed := len(s.perHiveEnvSeen)
	s.perHiveEnvMu.Unlock()
	if observed != fleetSize {
		t.Errorf("observed %d hives, want all %d — the rate limit must defer patches, not reads", observed, fleetSize)
	}
}

// TestReconcilePerHiveEnvSweepPatchFailure: a failed patch must not count
// against the observation bookkeeping (the read succeeded), must trip the
// cluster's unreachable breaker so the next cycle backs off, and must leave
// the sweep alive for later hives rather than aborting it.
func TestReconcilePerHiveEnvSweepPatchFailure(t *testing.T) {
	withTestMaster(t, perHiveEnvTestMaster)
	withTempHivesDir(t)
	fake := installFakeKubectl(t)

	s := &HubServer{
		logger:   appKeyTestLogger(),
		clusters: map[string]ClusterConfig{"c1": {ID: "c1", InCluster: true}},
	}

	mustSaveHive(t, &SaaSHive{ID: "failpatch", ClusterID: "c1", Status: "available"})
	fakeDeploymentEnv(t, fake, "failpatch", []deploymentEnvVar{})
	if err := os.WriteFile(filepath.Join(fake, "patch-fail-failpatch"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	s.reconcilePerHiveEnv()

	if n := countPatchInvocations(t, fake); n != 1 {
		t.Fatalf("kubectl patch invoked %d times, want 1 (attempted and failed)", n)
	}
	s.perHiveEnvMu.Lock()
	obs, ok := s.perHiveEnvSeen["failpatch"]
	s.perHiveEnvMu.Unlock()
	if !ok {
		t.Fatal("read succeeded but no observation recorded — a failed PATCH must not erase a successful READ")
	}
	if len(obs.MissingVars) == 0 {
		t.Error("observation shows no drift for a hive whose patch just failed")
	}
	if !s.clusterRecentlyUnreachable("c1") {
		t.Error("failed patch did not trip the cluster breaker — the next sweep would burn the same timeout immediately")
	}
}
