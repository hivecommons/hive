package hub

import (
	"io"
	"log/slog"
	"testing"
)

// ============================================================
// saas_provision.go — coverage for remaining sub-100% branches:
//   - mintClaimVanityURL: early-return and servable-error paths
//   - repairVanityURLForHive: early-exit for nil/available/empty-org/no-cluster
//   - kubectlArgsForCluster: in-cluster with env vars set
//
// Addresses hive#3543.
// ============================================================

func gapsLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}


// ── mintClaimVanityURL ──────────────────────────────────────────────────────

func TestMintClaimVanityURLNilHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: gapsLogger(), clusters: map[string]ClusterConfig{}}
	s.mintClaimVanityURL("nonexistent-hive")
}

func TestMintClaimVanityURLAlreadyHasVanity(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{ID: "hosted-test-vanity", Owner: "alice", Status: statusAssigned, VanityURL: "https://existing.hive.kubestellar.io"}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	s := &HubServer{logger: gapsLogger(), clusters: map[string]ClusterConfig{}}
	s.mintClaimVanityURL("hosted-test-vanity")

	loaded := loadSaaSHive("hosted-test-vanity")
	if loaded.VanityURL != "https://existing.hive.kubestellar.io" {
		t.Errorf("vanity URL changed unexpectedly: %s", loaded.VanityURL)
	}
}

func TestMintClaimVanityURLStatusAvailable(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{ID: "hosted-avail-hive", Owner: "bob", Status: statusAvailable}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	s := &HubServer{logger: gapsLogger(), clusters: map[string]ClusterConfig{}}
	s.mintClaimVanityURL("hosted-avail-hive")

	loaded := loadSaaSHive("hosted-avail-hive")
	if loaded.VanityURL != "" {
		t.Errorf("available placeholder should not get a vanity URL, got: %s", loaded.VanityURL)
	}
}

func TestMintClaimVanityURLNoCluster(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{ID: "hosted-nocluster", Owner: "alice", Status: statusAssigned, Org: "acme", PrimaryRepo: "app"}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	s := &HubServer{logger: gapsLogger(), clusters: map[string]ClusterConfig{}}
	s.mintClaimVanityURL("hosted-nocluster")

	loaded := loadSaaSHive("hosted-nocluster")
	if loaded.VanityURL != "" {
		t.Errorf("should not mint vanity URL without a cluster, got: %s", loaded.VanityURL)
	}
}

func TestMintClaimVanityURLServableError(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{
		ID: "hosted-svc-err", Owner: "alice", Status: statusAssigned,
		Org: "acme", PrimaryRepo: "repo", ClusterID: "hive-oke",
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	installKubectlScript(t, "#!/bin/sh\nexit 1\n")

	s := &HubServer{
		logger:   gapsLogger(),
		clusters: map[string]ClusterConfig{"hive-oke": *nginxIngressCluster()},
	}
	s.mintClaimVanityURL("hosted-svc-err")

	loaded := loadSaaSHive("hosted-svc-err")
	if loaded.VanityURL != "" {
		t.Errorf("should not adopt unservable vanity URL, got: %s", loaded.VanityURL)
	}
}

// ── repairVanityURLForHive ──────────────────────────────────────────────────

func TestRepairVanityURLForHiveNilHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	s := &HubServer{logger: gapsLogger(), clusters: map[string]ClusterConfig{}}
	if s.repairVanityURLForHive("nonexistent") {
		t.Error("should return false for nonexistent hive")
	}
}

func TestRepairVanityURLForHiveAvailable(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{ID: "hosted-avail", Status: statusAvailable, Org: "acme", PrimaryRepo: "repo"}
	saveSaaSHive(h)

	s := &HubServer{logger: gapsLogger(), clusters: map[string]ClusterConfig{}}
	if s.repairVanityURLForHive("hosted-avail") {
		t.Error("should return false for available placeholder")
	}
}

func TestRepairVanityURLForHiveAlreadyHasURL(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{ID: "hosted-has-url", Status: statusAssigned, VanityURL: "https://x.example.com", Org: "o", PrimaryRepo: "r"}
	saveSaaSHive(h)

	s := &HubServer{logger: gapsLogger(), clusters: map[string]ClusterConfig{}}
	if s.repairVanityURLForHive("hosted-has-url") {
		t.Error("should return false when vanity URL already set")
	}
}

func TestRepairVanityURLForHiveEmptyOrg(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{ID: "hosted-empty-org", Status: statusAssigned, Org: "", PrimaryRepo: "repo"}
	saveSaaSHive(h)

	s := &HubServer{logger: gapsLogger(), clusters: map[string]ClusterConfig{}}
	if s.repairVanityURLForHive("hosted-empty-org") {
		t.Error("should return false when org is empty")
	}
}

func TestRepairVanityURLForHiveEmptyPrimaryRepo(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{ID: "hosted-empty-repo", Status: statusAssigned, Org: "acme", PrimaryRepo: ""}
	saveSaaSHive(h)

	s := &HubServer{logger: gapsLogger(), clusters: map[string]ClusterConfig{}}
	if s.repairVanityURLForHive("hosted-empty-repo") {
		t.Error("should return false when primary repo is empty")
	}
}

func TestRepairVanityURLForHiveNoCluster(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{ID: "hosted-no-cluster", Status: statusAssigned, Org: "acme", PrimaryRepo: "repo"}
	saveSaaSHive(h)

	s := &HubServer{logger: gapsLogger(), clusters: map[string]ClusterConfig{}}
	if s.repairVanityURLForHive("hosted-no-cluster") {
		t.Error("should return false when no cluster available")
	}
}

func TestRepairVanityURLForHiveServableError(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	h := &SaaSHive{
		ID: "hosted-repair-fail", Status: statusAssigned,
		Org: "acme", PrimaryRepo: "repo", ClusterID: "hive-oke",
	}
	saveSaaSHive(h)

	installKubectlScript(t, "#!/bin/sh\nexit 1\n")

	s := &HubServer{
		logger:   gapsLogger(),
		clusters: map[string]ClusterConfig{"hive-oke": *nginxIngressCluster()},
	}
	if s.repairVanityURLForHive("hosted-repair-fail") {
		t.Error("should return false when vanity host cannot be made servable")
	}

	loaded := loadSaaSHive("hosted-repair-fail")
	if loaded.VanityURL != "" {
		t.Errorf("vanity URL should stay empty on servable failure, got: %s", loaded.VanityURL)
	}
}

// ── kubectlArgsForCluster ───────────────────────────────────────────────────

func TestKubectlArgsForClusterInClusterWithEnv(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "6443")

	cluster := &ClusterConfig{InCluster: true}
	args := kubectlArgsForCluster(cluster, "get", "pods")

	found := false
	for _, a := range args {
		if a == "--server" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected --server in args for in-cluster with env set, got: %v", args)
	}
	if args[len(args)-2] != "get" || args[len(args)-1] != "pods" {
		t.Errorf("command args not appended correctly: %v", args)
	}
}

func TestKubectlArgsForClusterRemote(t *testing.T) {
	cluster := &ClusterConfig{
		InCluster:      false,
		KubeconfigPath: "/home/user/.kube/config",
		Context:        "prod-ctx",
	}
	args := kubectlArgsForCluster(cluster, "apply", "-f", "manifest.yaml")

	if args[0] != "--kubeconfig" || args[1] != "/home/user/.kube/config" {
		t.Errorf("expected kubeconfig args, got: %v", args)
	}
	if args[2] != "--context" || args[3] != "prod-ctx" {
		t.Errorf("expected context args, got: %v", args)
	}
	if args[4] != "apply" {
		t.Errorf("command args not appended correctly: %v", args)
	}
}

