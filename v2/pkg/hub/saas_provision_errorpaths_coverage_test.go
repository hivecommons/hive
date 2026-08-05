package hub

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================
// saas_provision.go — vanity-ingress mirroring, provision/migrate error
// paths, quota/cleanup branches, watcher edges (#2559)
//
// Every test here reuses the package's established serial seams:
//   - helperSetupTempDirs   -> swaps saas path globals, restored on cleanup
//   - a scripted kubectl on PATH via t.Setenv (per-test, auto-restored)
//   - s.vanityHostServable  -> the HubServer function-var DI seam
// No test writes a shared package-level global that outlives it, so the
// -race coverage run stays free of the shared-global race class (#2510/#2651).
// ============================================================

func provTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// installKubectlScript writes an arbitrary kubectl script to a temp dir and
// prepends it to PATH for the test. The body branches on "$*".
func installKubectlScript(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func nginxIngressCluster() *ClusterConfig {
	return &ClusterConfig{
		ID: "hive-oke", InCluster: true, IngressType: "nginx",
		IngressClass: "nginx", Domain: "hive.kubestellar.io",
	}
}

// ── addVanityHostToIngress ──────────────────────────────────────────────────

// TestAddVanityHostToIngressNilOrEmpty covers the guard.
func TestAddVanityHostToIngressNilOrEmpty(t *testing.T) {
	s := &HubServer{logger: provTestLogger()}
	if err := s.addVanityHostToIngress("h1", "", nginxIngressCluster()); err == nil {
		t.Error("empty vanity host should error")
	}
	if err := s.addVanityHostToIngress("h1", "v.example.com", nil); err == nil {
		t.Error("nil cluster should error")
	}
}

// TestAddVanityHostToIngressNginxPatches drives the nginx branch: the scripted
// kubectl returns an Ingress JSON whose rules carry the placeholder host (so the
// clone-rule branch runs) and a tls block missing the vanity host (so the
// tls-append branch runs), then succeeds on every patch. patched>0 => nil.
func TestAddVanityHostToIngressNginxPatches(t *testing.T) {
	cluster := nginxIngressCluster()
	placeholder := "h1." + cluster.Domain
	ingressJSON := `{"spec":{"rules":[{"host":"` + placeholder + `","http":{"paths":[]}}],"tls":[{"hosts":["` + placeholder + `"]}]}}`
	// get ingress hive|hive-contribute|hive-terminal -> JSON; patch -> success;
	// any other ingress name still returns the same JSON (all three patched).
	installKubectlScript(t, `#!/bin/sh
case "$*" in
  *"get ingress"*"-o json"*) cat <<'JSON'
`+ingressJSON+`
JSON
    ;;
  *"patch ingress"*) exit 0 ;;
  *) echo "{}" ;;
esac
exit 0
`)
	s := &HubServer{logger: provTestLogger()}
	if err := s.addVanityHostToIngress("h1", "vanity.hive.kubestellar.io", cluster); err != nil {
		t.Fatalf("nginx vanity patch: %v", err)
	}
}

// TestAddVanityHostToIngressNginxNoIngress covers the patched==0 error: kubectl
// get always fails (no ingress on the spoke), so the loop continues past all
// three and returns the "no ingress found" error.
func TestAddVanityHostToIngressNginxNoIngress(t *testing.T) {
	installKubectlScript(t, "#!/bin/sh\nexit 1\n")
	s := &HubServer{logger: provTestLogger()}
	if err := s.addVanityHostToIngress("h1", "vanity.hive.kubestellar.io", nginxIngressCluster()); err == nil {
		t.Error("no reachable ingress should error")
	}
}

// TestAddVanityHostToIngressNginxPatchFails covers the patch-error return: the
// ingress exists but the patch command fails.
func TestAddVanityHostToIngressNginxPatchFails(t *testing.T) {
	cluster := nginxIngressCluster()
	placeholder := "h1." + cluster.Domain
	ingressJSON := `{"spec":{"rules":[{"host":"` + placeholder + `","http":{"paths":[]}}],"tls":[{"hosts":["` + placeholder + `"]}]}}`
	installKubectlScript(t, `#!/bin/sh
case "$*" in
  *"get ingress"*"-o json"*) cat <<'JSON'
`+ingressJSON+`
JSON
    ;;
  *"patch ingress"*) exit 3 ;;
  *) echo "{}" ;;
esac
exit 0
`)
	s := &HubServer{logger: provTestLogger()}
	if err := s.addVanityHostToIngress("h1", "vanity.hive.kubestellar.io", cluster); err == nil {
		t.Error("failed patch should surface an error")
	}
}

// TestAddVanityHostToIngressOpenShiftRoute drives the OpenShift Route branch:
// the scripted kubectl returns a targetPort|path for each source route and
// succeeds on apply, so applied>0 => nil.
func TestAddVanityHostToIngressOpenShiftRoute(t *testing.T) {
	cluster := &ClusterConfig{
		ID: "ocp", InCluster: true, IngressType: ingressTypeOpenShiftRoute,
		Domain: "apps.ocp.example.com",
	}
	installKubectlScript(t, `#!/bin/sh
case "$*" in
  *"get route"*) printf '8080|/' ;;
  *"apply"*) exit 0 ;;
  *) echo "{}" ;;
esac
exit 0
`)
	s := &HubServer{logger: provTestLogger()}
	if err := s.addVanityHostToIngress("h1", "vanity.apps.ocp.example.com", cluster); err != nil {
		t.Fatalf("openshift route vanity: %v", err)
	}
}

// TestAddVanityHostToIngressOpenShiftNoRoute covers the applied==0 error: no
// source route exists so nothing is mirrored.
func TestAddVanityHostToIngressOpenShiftNoRoute(t *testing.T) {
	cluster := &ClusterConfig{
		ID: "ocp", InCluster: true, IngressType: ingressTypeOpenShiftRoute,
		Domain: "apps.ocp.example.com",
	}
	installKubectlScript(t, "#!/bin/sh\nexit 1\n")
	s := &HubServer{logger: provTestLogger()}
	if err := s.addVanityHostToIngress("h1", "vanity.apps.ocp.example.com", cluster); err == nil {
		t.Error("no source route should error")
	}
}

// TestAddVanityHostToIngressOpenShiftApplyFails covers the route apply-error.
func TestAddVanityHostToIngressOpenShiftApplyFails(t *testing.T) {
	cluster := &ClusterConfig{
		ID: "ocp", InCluster: true, IngressType: ingressTypeOpenShiftRoute,
		Domain: "apps.ocp.example.com",
	}
	installKubectlScript(t, `#!/bin/sh
case "$*" in
  *"get route"*) printf '8080|/' ;;
  *"apply"*) exit 5 ;;
  *) echo "{}" ;;
esac
exit 0
`)
	s := &HubServer{logger: provTestLogger()}
	if err := s.addVanityHostToIngress("h1", "vanity.apps.ocp.example.com", cluster); err == nil {
		t.Error("failed route apply should surface an error")
	}
}

// ── provisionHive error path ────────────────────────────────────────────────

// TestProvisionHiveApplyFails covers the kubectl-apply-failure return: the
// manifest is templated and written, but kubectl apply exits non-zero, so
// provisionHive returns the sanitized error (and removes the token-bearing
// manifest along the way).
func TestProvisionHiveApplyFails(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	// kubectl fails on apply; dynamic storage so the OCI/NFS branch is skipped.
	installKubectlScript(t, "#!/bin/sh\nexit 1\n")

	h := &SaaSHive{ID: "hosted-fail-repo-abcd", Owner: "alice", Org: "acme", Repos: []string{"repo"}, PrimaryRepo: "repo", ACMMLevel: 2}
	req := &CreateHiveRequest{Org: "acme", Repos: "repo", PrimaryRepo: "repo", ACMMLevel: 2, ClusterID: "hive-oke"}
	err := provisionHive(h, req, dynamicCluster(), nil, provTestLogger())
	if err == nil {
		t.Fatal("expected provisionHive to fail when kubectl apply fails")
	}
	if !strings.Contains(err.Error(), "provisioning failed") {
		t.Errorf("unexpected error: %v", err)
	}
	// The token-bearing manifest must be gone after the (failed) apply.
	manifest := filepath.Join(saasHivesDir, h.ID, "manifests", "all.yaml")
	if _, statErr := os.Stat(manifest); !os.IsNotExist(statErr) {
		t.Errorf("manifest with plaintext token should have been removed, stat err=%v", statErr)
	}
}

// ── migrateHive provisioning-failure path ───────────────────────────────────

// TestMigrateHiveProvisionFails covers the migration branch where the target
// provision fails: the hive is marked failed, status restored to running, and
// the source is NOT deprovisioned.
func TestMigrateHiveProvisionFails(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	// kubectl apply fails everywhere -> provisionHive on the target fails.
	installKubectlScript(t, "#!/bin/sh\nexit 1\n")

	s := &HubServer{
		logger: provTestLogger(),
		saveCh: make(chan struct{}, 1),
		clusters: map[string]ClusterConfig{
			"hive-oke": *dynamicCluster(),
			"target":   {ID: "target", Name: "Target", InCluster: true, StorageType: storageTypeDynamic, Domain: "target.example.com"},
		},
	}
	h := &SaaSHive{ID: "hosted-migfail", Owner: "alice", Org: "acme", Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 2, ClusterID: "hive-oke", Status: "running"}
	saveSaaSHive(h)
	s.registry.Hives = []RegistryEntry{{ID: "hosted-migfail", ClusterID: "hive-oke"}}

	from := s.clusters["hive-oke"]
	to := s.clusters["target"]
	s.migrateHive(h, &from, &to)

	got := loadSaaSHive("hosted-migfail")
	if got == nil {
		t.Fatal("hive record missing after failed migration")
	}
	if got.MigrationStatus != "failed" {
		t.Errorf("MigrationStatus = %q, want failed", got.MigrationStatus)
	}
	if got.Status != "running" {
		t.Errorf("Status = %q, want running (restored)", got.Status)
	}
	if got.ClusterID != "hive-oke" {
		t.Errorf("ClusterID = %q, want hive-oke (unchanged on failed migration)", got.ClusterID)
	}
}

// ── repairVanityURLsForClaimedHives repaired>0 ──────────────────────────────

// TestRepairVanityURLsForClaimedHivesRepairsOne seeds one repairable claimed
// hive (real org/repo, no vanity URL) with a servability seam that succeeds, so
// the fleet-level sweep repairs exactly one and runs the repaired>0 log branch.
func TestRepairVanityURLsForClaimedHivesRepairsOne(t *testing.T) {
	useTempHiveDir(t)
	s := newVanityRepairTestHub()

	// Repairable: claimed (not statusAvailable), org+repo set, no vanity URL.
	saveSaaSHive(&SaaSHive{
		ID: "hosted-claimed-oke-repairme", Status: "running", Owner: "octo", Org: "octo",
		Repos: []string{"widget"}, PrimaryRepo: "widget", ACMMLevel: 2,
		ClusterID: defaultClusterID, ClaimDelivered: true,
	})
	// Not repairable: unclaimed placeholder (skipped).
	saveSaaSHive(&SaaSHive{
		ID: "hosted-available-oke-skip", Status: statusAvailable, ClusterID: defaultClusterID,
	})

	if n := s.repairVanityURLsForClaimedHives(); n != 1 {
		t.Errorf("repaired count = %d, want 1", n)
	}
	got := loadSaaSHive("hosted-claimed-oke-repairme")
	if got == nil || got.VanityURL == "" {
		t.Errorf("claimed hive should have a minted vanity URL, got %+v", got)
	}
}

// ── startProvisionWatcher edge branches ─────────────────────────────────────

// TestProvisionWatcherNoClusterAndMigrationClear covers two watcher branches in
// one pass: a provisioning hive whose ClusterID names no known cluster (the
// "no cluster config" continue), and a provisioning hive with
// MigrationStatus=completed that flips to running and has its migration tracking
// cleared.
func TestProvisionWatcherNoClusterAndMigrationClear(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installReplicaKubectl(t) // reports availableReplicas=1

	oldInterval := provisionPollInterval
	provisionPollInterval = 15 * time.Millisecond
	defer func() { provisionPollInterval = oldInterval }()

	now := time.Now().UTC().Format(time.RFC3339)
	// Unknown cluster whose default fallback is ALSO absent from s.clusters
	// below (the map is keyed by "mig-cluster", not defaultClusterID), so
	// clusterForHive returns nil and the "no cluster config" branch runs.
	saveSaaSHive(&SaaSHive{
		ID: "prov-nocluster", Owner: "alice", Status: "provisioning",
		ClusterID: "does-not-exist", CreatedAt: now,
	})
	// Migration-completed hive on a known (non-default) cluster -> flips to
	// running and clears migration fields.
	saveSaaSHive(&SaaSHive{
		ID: "prov-migrated", Owner: "alice", Status: "provisioning", ClusterID: "mig-cluster",
		CreatedAt: now, MigrationStatus: "completed", MigrationFrom: "old", MigrationTo: "mig-cluster",
		MigrationStartedAt: now,
	})

	// No defaultClusterID entry, so an unknown ClusterID resolves to nil rather
	// than silently falling back to a default cluster.
	s := &HubServer{logger: provTestLogger(), clusters: map[string]ClusterConfig{"mig-cluster": {ID: "mig-cluster", InCluster: true}}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.startProvisionWatcher(ctx) }()
	defer func() { cancel(); <-done }()

	deadline := time.After(3 * time.Second)
	for {
		mig := loadSaaSHive("prov-migrated")
		if mig != nil && mig.Status == "running" && mig.MigrationStatus == "" {
			// The no-cluster hive must be untouched (still provisioning).
			nc := loadSaaSHive("prov-nocluster")
			if nc != nil && nc.Status != "provisioning" {
				t.Errorf("no-cluster hive should remain provisioning, got %q", nc.Status)
			}
			return
		}
		select {
		case <-deadline:
			t.Logf("watcher did not settle (migrated=%v)", statusOf(loadSaaSHive("prov-migrated")))
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// ── small pure helpers ──────────────────────────────────────────────────────

// TestForgeHostKeyPublicAndTrim covers the public-host normalisation branch.
func TestForgeHostKeyPublicAndTrim(t *testing.T) {
	if got := forgeHostKey("https://api.github.com/api/v3"); got != publicForgeHost {
		t.Errorf("forgeHostKey(api.github.com) = %q, want %q", got, publicForgeHost)
	}
	if got := forgeHostKey(""); got != publicForgeHost {
		t.Errorf("forgeHostKey(empty) = %q, want %q", got, publicForgeHost)
	}
	if got := forgeHostKey("github.ibm.com/"); got != "github.ibm.com" {
		t.Errorf("forgeHostKey(github.ibm.com/) = %q", got)
	}
}

// TestForgesForClusterNil covers the nil-cluster branch.
func TestForgesForClusterNil(t *testing.T) {
	if out := forgesForCluster(nil); len(out) != 0 {
		t.Errorf("forgesForCluster(nil) = %v, want empty", out)
	}
}

// TestEffectiveGitHubAPIURLPublicPin covers the explicit public-API pin branch.
func TestEffectiveGitHubAPIURLPublicPin(t *testing.T) {
	cluster := &ClusterConfig{GitHubAPIURL: "https://ghe.example.com/api/v3"}
	// Hive explicitly pins the public API -> "" even though the cluster is GHE.
	h := &SaaSHive{GitHubAPIURL: "https://api.github.com"}
	if got := effectiveGitHubAPIURL(h, cluster); got != "" {
		t.Errorf("public-pinned API = %q, want empty", got)
	}
	// Hive pins the public web host -> API also resolves public ("").
	h2 := &SaaSHive{GitHubBaseURL: "https://github.com"}
	if got := effectiveGitHubAPIURL(h2, cluster); got != "" {
		t.Errorf("public web pin should imply public API, got %q", got)
	}
}
