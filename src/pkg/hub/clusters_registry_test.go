package hub

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the fail-closed cluster registry loader (audit 8, §6 item 11).
//
// The shape of this file is deliberate. A fail-closed change is trivially
// "passed" by a loader that rejects everything, so every rejection test here is
// paired with a POSITIVE CONTROL that proves the same code still loads the real
// registry and still produces the same routing decisions. The live-shaped
// fixture below is the control that does most of that work.

// clustersFatalRecorder captures the refusal instead of exiting the process.
type clustersFatalRecorder struct {
	fired bool
	msg   string
}

// captureClustersFatal substitutes the process-exit hook for the duration of a
// test and restores it afterwards.
func captureClustersFatal(t *testing.T) *clustersFatalRecorder {
	t.Helper()
	rec := &clustersFatalRecorder{}
	old := clustersFatal
	clustersFatal = func(msg string) {
		rec.fired = true
		rec.msg = msg
	}
	t.Cleanup(func() { clustersFatal = old })
	return rec
}

// pointClustersConfigAt redirects the registry path at a temp file.
func pointClustersConfigAt(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "clusters.json")
	if contents != "" {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := clustersConfigPath
	clustersConfigPath = path
	t.Cleanup(func() { clustersConfigPath = old })
	return path
}

// liveShapedRegistry mirrors the THREE entries on the production hub, including
// the two pull_only clusters, so the positive controls exercise the real
// topology rather than a toy one. Field values are structural only — no
// credentials, no kubeconfig contents.
const liveShapedRegistry = `[
  {"id":"hive-oke","name":"OKE","in_cluster":true,"domain":"hive.kubestellar.io","arch":"arm64","image_tag":"v4-latest"},
  {"id":"vllm-d","name":"vLLM-D","in_cluster":false,"pull_only":true,"kubeconfig_path":"/etc/hive/kubeconfigs/vllm-d.yaml","domain":"vllmd.example.com","arch":"amd64","image_tag":"v4-latest"},
  {"id":"a-ks-wec2","name":"WEC2","in_cluster":false,"pull_only":true,"kubeconfig_path":"/etc/hive/kubeconfigs/a-ks-wec2.yaml","domain":"wec2.example.com","arch":"amd64","image_tag":"v4-latest"}
]`

// ---------------------------------------------------------------------------
// POSITIVE CONTROL: a valid registry still loads and still routes identically.
// ---------------------------------------------------------------------------

// TestValidRegistryStillLoadsAndRoutesIdentically is THE control that stops a
// "reject everything" loader from passing this suite. It asserts the live-shaped
// registry loads, that the hub does NOT refuse, and — the part that matters —
// that every pull_only routing decision is byte-for-byte what it was before.
func TestValidRegistryStillLoadsAndRoutesIdentically(t *testing.T) {
	pointClustersConfigAt(t, liveShapedRegistry)
	fatal := captureClustersFatal(t)

	clusters := loadClusters(slog.Default())

	if fatal.fired {
		t.Fatalf("a VALID registry made the hub refuse to start: %s", fatal.msg)
	}
	if len(clusters) != 3 {
		t.Fatalf("loaded %d clusters, want 3 (ids=%v)", len(clusters), clusterIDsOf(clusters))
	}

	// PullOnly must round-trip exactly. This is the flag that gates whether the
	// hub attempts to WRITE to a cluster; getting it wrong in either direction
	// is the outage this fix must not cause.
	wantPullOnly := map[string]bool{
		"hive-oke":  false,
		"vllm-d":    true,
		"a-ks-wec2": true,
	}
	for id, want := range wantPullOnly {
		c, ok := clusters[id]
		if !ok {
			t.Fatalf("cluster %q missing from the loaded registry", id)
		}
		if c.PullOnly != want {
			t.Errorf("cluster %q PullOnly = %v, want %v", id, c.PullOnly, want)
		}
	}

	// And the derived routing decision, which is what actually gates kubectl.
	wantReachable := map[string]bool{
		"hive-oke":  true,  // in_cluster, not pull_only — the hub MUST write here
		"vllm-d":    false, // pull_only wins over a present kubeconfig_path
		"a-ks-wec2": false,
	}
	for id, want := range wantReachable {
		c := clusters[id]
		if got := c.KubectlReachable(); got != want {
			t.Errorf("cluster %q KubectlReachable() = %v, want %v", id, got, want)
		}
	}
}

// TestPullOnlyWithKubeconfigStaysUnreachableAfterLoad pins the specific
// precedence the audit called out: a pull_only cluster that DOES carry a
// kubeconfig_path (both live ones do) must still be unreachable. A loader that
// "helpfully" cleared PullOnly because a kubeconfig exists would hand the hub
// write access to 48 spokes it must not touch.
func TestPullOnlyWithKubeconfigStaysUnreachableAfterLoad(t *testing.T) {
	pointClustersConfigAt(t, liveShapedRegistry)
	captureClustersFatal(t)

	clusters := loadClusters(slog.Default())

	c, ok := clusters["vllm-d"]
	if !ok {
		t.Fatal("vllm-d missing from the loaded registry")
	}
	if c.KubeconfigPath == "" {
		t.Fatal("fixture is wrong: vllm-d should carry a kubeconfig_path")
	}
	if c.KubectlReachable() {
		t.Error("pull_only cluster with a kubeconfig_path is kubectl-reachable; " +
			"pull_only must win over a present kubeconfig")
	}
}

// TestValidRegistryIsNotQuarantined guards the repair path from eating a good
// file: nothing about a successful load may rename or remove it.
func TestValidRegistryIsNotQuarantined(t *testing.T) {
	path := pointClustersConfigAt(t, liveShapedRegistry)
	captureClustersFatal(t)

	loadClusters(slog.Default())

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a valid clusters.json no longer exists after load: %v", err)
	}
	if _, err := os.Stat(path + clustersQuarantineSuffix); err == nil {
		t.Error("a valid clusters.json was quarantined")
	}
}

// ---------------------------------------------------------------------------
// FAIL-CLOSED: corruption must refuse, not yield a partial/empty registry.
// ---------------------------------------------------------------------------

// TestMalformedRegistryFailsClosed is the direct regression for the audit
// finding. The old loader returned an empty map here.
func TestMalformedRegistryFailsClosed(t *testing.T) {
	pointClustersConfigAt(t, `{not an array`)
	fatal := captureClustersFatal(t)

	clusters, outcome := loadClustersChecked(slog.Default())

	if outcome != clustersUntrusted {
		t.Fatalf("outcome = %v, want clustersUntrusted", outcome)
	}
	if len(clusters) != 0 {
		t.Fatalf("untrusted load returned %d clusters, want none", len(clusters))
	}

	_ = fatal
}

// TestTruncatedRegistryFailsClosedAndDoesNotDisableHiveOKE is the exact failure
// mode an interrupted `kubectl cp` produces — the mechanism that left the
// .bak-akswec2 sibling on the live hub.
//
// The assertion that matters is the SECOND one: it is not enough that the load
// fails, it must not leave a registry in which hive-oke has silently vanished.
func TestTruncatedRegistryFailsClosedAndDoesNotDisableHiveOKE(t *testing.T) {
	// A prefix of the real file — valid JSON right up to the cut.
	truncated := liveShapedRegistry[:len(liveShapedRegistry)/2]
	pointClustersConfigAt(t, truncated)
	fatal := captureClustersFatal(t)

	clusters := loadClusters(slog.Default())

	if !fatal.fired {
		t.Error("a truncated clusters.json did not make the hub refuse to start")
	}
	if _, ok := clusters[defaultClusterID]; ok {
		t.Errorf("a truncated registry yielded a %q entry; the hub must refuse, "+
			"not silently substitute a default that re-routes every pull-only hive", defaultClusterID)
	}
	if len(clusters) != 0 {
		t.Errorf("a truncated registry yielded %d clusters, want none", len(clusters))
	}
}

// TestEmptyArrayRegistryFailsClosed covers a file that parses perfectly and
// says nothing. It must not be read as "no clusters configured" — the operator
// placed a registry here, so the hub cannot act on it.
func TestEmptyArrayRegistryFailsClosed(t *testing.T) {
	pointClustersConfigAt(t, `[]`)
	captureClustersFatal(t)

	clusters, outcome := loadClustersChecked(slog.Default())

	if outcome != clustersUntrusted {
		t.Fatalf("outcome = %v, want clustersUntrusted for a registry with no usable cluster", outcome)
	}
	if len(clusters) != 0 {
		t.Fatalf("got %d clusters, want none", len(clusters))
	}
}

// TestAllEntriesInvalidFailsClosed: the file parses and has entries, but every
// one is rejected by the admission rules. Same conclusion as an empty array.
func TestAllEntriesInvalidFailsClosed(t *testing.T) {
	pointClustersConfigAt(t, `[{"id":"","domain":"x"},{"id":"nodomain","in_cluster":true}]`)
	captureClustersFatal(t)

	_, outcome := loadClustersChecked(slog.Default())

	if outcome != clustersUntrusted {
		t.Fatalf("outcome = %v, want clustersUntrusted when no entry survives validation", outcome)
	}
}

// TestMalformedRegistryIsQuarantinedNotDiscarded asserts the bad bytes are
// preserved for an operator rather than overwritten.
func TestMalformedRegistryIsQuarantinedNotDiscarded(t *testing.T) {
	const bad = `{not an array`
	path := pointClustersConfigAt(t, bad)
	captureClustersFatal(t)

	loadClustersChecked(slog.Default())

	quarantine := path + clustersQuarantineSuffix
	data, err := os.ReadFile(quarantine)
	if err != nil {
		t.Fatalf("unparseable registry was not quarantined to %s: %v", quarantine, err)
	}
	if string(data) != bad {
		t.Errorf("quarantined bytes = %q, want the original %q", data, bad)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the unparseable file is still at the live path; it should have been renamed aside")
	}
}

// ---------------------------------------------------------------------------
// ABSENT is not corrupt — the backward-compatibility path must survive.
// ---------------------------------------------------------------------------

// TestAbsentRegistryStillYieldsDefault is the other direction of the control:
// this fix must NOT turn a fresh deployment into a refusal. A missing file is a
// positive fact ("never configured"), not a corrupt one.
func TestAbsentRegistryStillYieldsDefault(t *testing.T) {
	pointClustersConfigAt(t, "") // writes nothing — path does not exist
	fatal := captureClustersFatal(t)

	clusters, outcome := loadClustersChecked(slog.Default())

	if fatal.fired {
		t.Fatalf("an ABSENT clusters.json made the hub refuse to start: %s", fatal.msg)
	}
	if outcome != clustersAbsent {
		t.Fatalf("outcome = %v, want clustersAbsent", outcome)
	}
	c, ok := clusters[defaultClusterID]
	if !ok {
		t.Fatalf("absent registry did not yield the %q default", defaultClusterID)
	}
	// The default must be WRITABLE — it is the hub's own cluster.
	if c.PullOnly {
		t.Error("the synthesized default is pull_only; the hub must be able to write to its own cluster")
	}
	if !c.KubectlReachable() {
		t.Error("the synthesized default is not kubectl-reachable")
	}
}

// ---------------------------------------------------------------------------
// The .bak-* sibling must never be mistaken for the live file.
// ---------------------------------------------------------------------------

// TestBackupSiblingIsNeverLoadedAsLiveRegistry seeds a VALID .bak- sibling
// beside a corrupt live file. If the loader ever preferred a parseable sibling,
// it would route the fleet from a stale hand-edited snapshot — silently. The
// hub must refuse instead.
func TestBackupSiblingIsNeverLoadedAsLiveRegistry(t *testing.T) {
	path := pointClustersConfigAt(t, `{not an array`)
	sibling := path + clustersBackupPrefix + "akswec2"
	if err := os.WriteFile(sibling, []byte(liveShapedRegistry), 0o644); err != nil {
		t.Fatal(err)
	}
	captureClustersFatal(t)

	clusters, outcome := loadClustersChecked(slog.Default())

	if outcome != clustersUntrusted {
		t.Fatalf("outcome = %v, want clustersUntrusted; a valid .bak- sibling must not rescue a corrupt live file", outcome)
	}
	if len(clusters) != 0 {
		t.Fatalf("loaded %d clusters from a corrupt live file with a valid sibling present, want none", len(clusters))
	}
}

// TestSidecarPathsAreNotTheLiveRegistry pins the naming property directly.
func TestSidecarPathsAreNotTheLiveRegistry(t *testing.T) {
	pointClustersConfigAt(t, liveShapedRegistry)
	base := clustersConfigPath

	sidecars := []string{
		base + ".bak-akswec2",
		base + ".bak-20260813",
		base + ".corrupt",
		base + ".tmp",
	}
	for _, p := range sidecars {
		if !isClustersSidecarPath(p) {
			t.Errorf("isClustersSidecarPath(%q) = false, want true", p)
		}
	}

	// The live path itself is NOT a sidecar — the positive control that stops
	// a predicate returning true for everything.
	if isClustersSidecarPath(base) {
		t.Errorf("isClustersSidecarPath(%q) = true for the LIVE path, want false", base)
	}
	if isClustersSidecarPath("/etc/hive/other.json") {
		t.Error("isClustersSidecarPath matched an unrelated path")
	}
}

// ---------------------------------------------------------------------------
// Round-trip: the struct must not lose fields it was given.
// ---------------------------------------------------------------------------

// TestClusterConfigRoundTripsPullOnly guards against a marshal/unmarshal
// asymmetry silently clearing the flag. pull_only carries `omitempty`, so a
// false value vanishes from the output — that is fine (absent reads as false)
// but a TRUE value disappearing would be an outage.
func TestClusterConfigRoundTripsPullOnly(t *testing.T) {
	in := ClusterConfig{
		ID:             "vllm-d",
		Name:           "vLLM-D",
		Domain:         "vllmd.example.com",
		KubeconfigPath: "/etc/hive/kubeconfigs/vllm-d.yaml",
		PullOnly:       true,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"pull_only":true`) {
		t.Fatalf("pull_only:true did not survive marshal: %s", data)
	}
	var out ClusterConfig
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !out.PullOnly {
		t.Error("PullOnly was lost in the round trip")
	}
	if out.KubectlReachable() {
		t.Error("round-tripped pull_only cluster became kubectl-reachable")
	}
}

// clusterIDsOf is a small helper for failure messages.
func clusterIDsOf(m map[string]ClusterConfig) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	return ids
}
