package hub

import (
	"log/slog"
	"testing"
)

// ============================================================
// saas_provision.go — provisionHive / deprovisionHive / migrateHive
//
// Uses the scripted fake kubectl (returns 0 for every command) and dynamic
// storage so the OCI/NFS network branches are skipped. This exercises the
// manifest templating + kubectl apply happy path and the cleanup path.
// ============================================================

func dynamicCluster() *ClusterConfig {
	return &ClusterConfig{
		ID:           "hive-oke",
		Name:         "OKE",
		InCluster:    true,
		StorageType:  storageTypeDynamic,
		StorageClass: "ocifss",
		IngressType:  "nginx",
		IngressClass: "nginx",
		CertIssuer:   "letsencrypt-prod",
		Domain:       "hive.kubestellar.io",
	}
}

func TestProvisionHiveSuccess(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	h := &SaaSHive{ID: "hosted-acme-repo-abcd", Owner: "alice", Org: "acme", Repos: []string{"repo"}, PrimaryRepo: "repo", ACMMLevel: 2}
	req := &CreateHiveRequest{Org: "acme", Repos: "repo", PrimaryRepo: "repo", ACMMLevel: 2, ClusterID: "hive-oke"}

	if err := provisionHive(h, req, dynamicCluster(), slog.Default()); err != nil {
		t.Fatalf("provisionHive: %v", err)
	}
}

func TestProvisionHiveWithAppAuth(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	h := &SaaSHive{ID: "hosted-acme-app-wxyz", Owner: "alice", Org: "acme", Repos: []string{"repo"}, PrimaryRepo: "repo", ACMMLevel: 1, IsPublic: true}
	req := &CreateHiveRequest{
		Org: "acme", Repos: "repo", PrimaryRepo: "repo", ACMMLevel: 1, ClusterID: "hive-oke",
		AuthMethod: "app", AppID: "123", InstallationID: "456",
		AppPrivateKey: "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----",
	}
	if err := provisionHive(h, req, dynamicCluster(), slog.Default()); err != nil {
		t.Fatalf("provisionHive app auth: %v", err)
	}
}

func TestDeprovisionHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	// Owner record so the quota-decrement branch runs.
	saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{"hosted-x": "owner"}})
	h := &SaaSHive{ID: "hosted-x", Owner: "alice", ClusterID: "hive-oke"}
	saveSaaSHive(h)

	// NFS storage type triggers the PV-delete branch; no OCI IDs set so the
	// OCI network calls are skipped.
	cluster := &ClusterConfig{ID: "hive-oke", InCluster: true, StorageType: storageTypeNFS}
	deprovisionHive(h, cluster, slog.Default())

	// The user's hive entry should be removed.
	u := loadSaaSUser("alice")
	if u != nil {
		if _, ok := u.Hives["hosted-x"]; ok {
			t.Error("expected hive removed from user record after deprovision")
		}
	}
}

func TestDeprovisionHiveWithSCC(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	h := &SaaSHive{ID: "hosted-ocp", Owner: "bob", ClusterID: "ocp"}
	saveSaaSHive(h)
	// OpenShift cluster with SCC -> exercises the SCC-removal branch. Dynamic
	// storage so no PV/OCI cleanup.
	cluster := &ClusterConfig{ID: "ocp", InCluster: true, StorageType: storageTypeDynamic, RequiresSCC: true, SCCName: "anyuid"}
	deprovisionHive(h, cluster, slog.Default())
}

func TestMigrateHive(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	s := &HubServer{
		logger: slog.Default(),
		saveCh: make(chan struct{}, 1),
		clusters: map[string]ClusterConfig{
			"hive-oke": *dynamicCluster(),
			"target":   {ID: "target", Name: "Target", InCluster: true, StorageType: storageTypeDynamic, Domain: "target.example.com"},
		},
	}
	h := &SaaSHive{ID: "hosted-mig", Owner: "alice", Org: "acme", Repos: []string{"r"}, PrimaryRepo: "r", ACMMLevel: 2, ClusterID: "hive-oke"}
	saveSaaSHive(h)
	saveSaaSUser(&SaaSUser{GitHubUsername: "alice", Hives: map[string]string{"hosted-mig": "owner"}})
	s.registry.Hives = []RegistryEntry{{ID: "hosted-mig", ClusterID: "hive-oke"}}

	from := s.clusters["hive-oke"]
	to := s.clusters["target"]
	s.migrateHive(h, &from, &to)

	// After a successful migration the record points at the target cluster.
	got := loadSaaSHive("hosted-mig")
	if got == nil {
		t.Fatal("hive record missing after migration")
	}
	if got.ClusterID != "target" {
		t.Errorf("migrated cluster = %q, want target (migration_status=%q err=%q)", got.ClusterID, got.MigrationStatus, got.Error)
	}
}

func TestExtractYAMLValue_ProvCov(t *testing.T) {
	yaml := "app_id: 42\ninstallation_id: 99\nother: x"
	if got := extractYAMLValue(yaml, "app_id"); got != "42" {
		t.Errorf("app_id = %q, want 42", got)
	}
	if got := extractYAMLValue(yaml, "installation_id"); got != "99" {
		t.Errorf("installation_id = %q, want 99", got)
	}
	if got := extractYAMLValue(yaml, "missing"); got != "" {
		t.Errorf("missing = %q, want empty", got)
	}
}
