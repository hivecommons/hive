package hub

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================
// saas_provision.go — rollback/cleanup paths, error handling,
// and fail-closed guarantees (#3543)
//
// Priority 1: deprovision cleanup runs even when OCI/SCC calls fail.
// Priority 2: provisionHive returns error (fail-closed) on kubectl failure.
// Priority 3: NFS/OCI error branches in provisionHive.
// ============================================================

// nfsClusterWithSCC returns a cluster whose configuration triggers every
// optional cleanup path: NFS storage (PV + OCI cleanup) and SCC.
func nfsClusterWithSCC() *ClusterConfig {
	return &ClusterConfig{
		ID:          "hive-oke",
		InCluster:   true,
		StorageType: storageTypeNFS,
		IngressType: "nginx",
		Domain:      "hive.kubestellar.io",
		RequiresSCC: true,
		SCCName:     "restricted-v2",
	}
}

// TestDeprovisionHive_OCI_Cleanup verifies that deprovisionHive attempts to
// delete OCI export and file system resources when their IDs are recorded,
// and that cleanup CONTINUES even when those deletions fail (no OCI config in
// test env). This pins the guarantee that partial cleanup does not orphan later
// steps: the hive directory and user quota are still cleaned even if OCI fails.
func TestDeprovisionHive_OCI_Cleanup(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	// Create user and hive records.
	saveSaaSUser(&SaaSUser{GitHubUsername: "bob", Hives: map[string]string{"hosted-oci-test": "owner"}})
	h := &SaaSHive{
		ID:              "hosted-oci-test",
		Owner:           "bob",
		ClusterID:       "hive-oke",
		OCIExportID:     "ocid1.export.oc1.iad.aaaa",
		OCIFileSystemID: "ocid1.filesystem.oc1.iad.bbbb",
	}
	saveSaaSHive(h)

	cluster := &ClusterConfig{
		ID:          "hive-oke",
		InCluster:   true,
		StorageType: storageTypeNFS,
	}

	deprovisionHive(h, cluster, slog.Default())

	// ── Assert: hive directory removed ──
	hiveDir := filepath.Join(saasHivesDir, "hosted-oci-test")
	if _, err := os.Stat(hiveDir); err == nil {
		t.Error("hive directory should be removed after deprovision")
	}

	// ── Assert: user quota decremented ──
	u := loadSaaSUser("bob")
	if u != nil {
		if _, ok := u.Hives["hosted-oci-test"]; ok {
			t.Error("hive entry should be removed from user record even when OCI cleanup fails")
		}
	}
}

// TestDeprovisionHive_SCC_Cleanup verifies that the SCC binding removal step
// runs when cluster.RequiresSCC is true, and that deprovision completes even
// if the SCC removal command fails (our scripted kubectl doesn't know `adm`).
func TestDeprovisionHive_SCC_Cleanup(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	saveSaaSUser(&SaaSUser{GitHubUsername: "carol", Hives: map[string]string{"hosted-scc": "owner"}})
	h := &SaaSHive{ID: "hosted-scc", Owner: "carol", ClusterID: "hive-oke"}
	saveSaaSHive(h)

	cluster := nfsClusterWithSCC()

	deprovisionHive(h, cluster, slog.Default())

	// ── Assert: hive directory removed despite SCC command failure ──
	hiveDir := filepath.Join(saasHivesDir, "hosted-scc")
	if _, err := os.Stat(hiveDir); err == nil {
		t.Error("hive directory should be removed even when SCC removal fails")
	}

	// ── Assert: user quota still decremented ──
	u := loadSaaSUser("carol")
	if u != nil {
		if _, ok := u.Hives["hosted-scc"]; ok {
			t.Error("user record should be cleaned up regardless of SCC failure")
		}
	}
}

// TestProvisionHive_KubectlApplyFailure verifies that provisionHive returns an
// error (fail-closed) when kubectl apply fails. This is the critical guarantee:
// a failed apply must NOT report success.
func TestProvisionHive_KubectlApplyFailure(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// Install a kubectl that fails on "apply".
	installKubectlScript(t, `#!/bin/sh
case "$*" in
  *apply*) echo "error: connection refused" >&2; exit 1 ;;
  *) exit 0 ;;
esac
`)

	h := &SaaSHive{ID: "hosted-fail-apply", Owner: "alice", Org: "acme", PrimaryRepo: "repo"}
	req := &CreateHiveRequest{Org: "acme", Repos: "repo", PrimaryRepo: "repo", ClusterID: "hive-oke"}
	cluster := dynamicCluster()

	err := provisionHive(h, req, cluster, nil, slog.Default())
	if err == nil {
		t.Fatal("provisionHive MUST return an error when kubectl apply fails — it reported success (fail-open bug)")
	}
	if !strings.Contains(err.Error(), "provisioning failed") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestProvisionHive_NFS_OCI_ErrorPath verifies that provisionHive with NFS
// storage type executes the OCI creation branch and continues (non-fatal)
// when createOCIFileSystem fails (no OCI config available in test env).
func TestProvisionHive_NFS_OCI_ErrorPath(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	// Unset any OCI env vars to ensure createOCIFileSystem fails.
	t.Setenv("OCI_REGION", "")
	t.Setenv("OCI_COMPARTMENT_ID", "")

	h := &SaaSHive{ID: "hosted-nfs-test", Owner: "dave", Org: "org1", PrimaryRepo: "r1"}
	req := &CreateHiveRequest{Org: "org1", Repos: "r1", PrimaryRepo: "r1", ClusterID: "hive-oke"}
	cluster := &ClusterConfig{
		ID:           "hive-oke",
		InCluster:    true,
		StorageType:  storageTypeNFS,
		IngressType:  "nginx",
		IngressClass: "nginx",
		CertIssuer:   "letsencrypt-prod",
		Domain:       "hive.kubestellar.io",
	}

	err := provisionHive(h, req, cluster, nil, slog.Default())
	if err != nil {
		t.Fatalf("provisionHive with NFS should succeed even when OCI fails: %v", err)
	}

	// Verify OCI IDs are NOT set (creation failed).
	if h.OCIFileSystemID != "" {
		t.Error("OCIFileSystemID should be empty when OCI creation fails")
	}
	if h.OCIExportID != "" {
		t.Error("OCIExportID should be empty when OCI creation fails")
	}
}

// TestProvisionHive_HubSecret_EnvVar verifies that provisionHive picks up
// the HIVE_HUB_SECRET environment variable (vs. the file fallback).
func TestProvisionHive_HubSecret_EnvVar(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	const testSecret = "env-secret-value-42"
	t.Setenv("HIVE_HUB_SECRET", testSecret)

	h := &SaaSHive{ID: "hosted-secret-env", Owner: "eve", Org: "org2", PrimaryRepo: "r2"}
	req := &CreateHiveRequest{Org: "org2", Repos: "r2", PrimaryRepo: "r2", ClusterID: "hive-oke"}
	cluster := dynamicCluster()

	// The test succeeds if provisionHive doesn't crash on the template render —
	// the HubSecret value flows into the K8s manifest. We just verify no error.
	if err := provisionHive(h, req, cluster, nil, slog.Default()); err != nil {
		t.Fatalf("provisionHive failed: %v", err)
	}
}

// TestProvisionHive_OpenShiftRoute verifies that provisionHive correctly
// renders templates with OpenShift Route configuration (IsOpenShiftRoute=true,
// RequiresSCC=true, custom SCCName).
func TestProvisionHive_OpenShiftRoute(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	h := &SaaSHive{ID: "hosted-ocp-test", Owner: "frank", Org: "ocporg", PrimaryRepo: "ocprepo"}
	req := &CreateHiveRequest{Org: "ocporg", Repos: "ocprepo", PrimaryRepo: "ocprepo", ClusterID: "hive-ocp"}
	cluster := &ClusterConfig{
		ID:              "hive-ocp",
		InCluster:       true,
		StorageType:     storageTypeDynamic,
		StorageClass:    "gp3",
		IngressType:     ingressTypeOpenShiftRoute,
		Domain:          "apps.ocp.example.com",
		RequiresSCC:     true,
		SCCName:         "restricted-v2",
		ImagePullPolicy: "IfNotPresent",
		ImageTag:        "v2.1.0",
	}

	if err := provisionHive(h, req, cluster, nil, slog.Default()); err != nil {
		t.Fatalf("provisionHive with OpenShift Route config: %v", err)
	}
}

// TestProvisionHive_GHE verifies provisioning a GitHub Enterprise hive
// (effectiveGitHubBaseURL non-empty triggers HasGHE, GitHubAppSlug branches).
func TestProvisionHive_GHE(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	h := &SaaSHive{
		ID: "hosted-ghe-test", Owner: "grace", Org: "ibmorg", PrimaryRepo: "internal-repo",
		Repos: []string{"github.ibm.com/ibmorg/internal-repo"},
	}
	req := &CreateHiveRequest{
		Org: "ibmorg", Repos: "github.ibm.com/ibmorg/internal-repo",
		PrimaryRepo: "internal-repo", ClusterID: "hive-oke",
		AuthMethod: "app", AppID: "99999",
	}
	cluster := &ClusterConfig{
		ID:            "hive-oke",
		InCluster:     true,
		StorageType:   storageTypeDynamic,
		IngressType:   "nginx",
		Domain:        "hive.kubestellar.io",
		GitHubBaseURL: "https://github.ibm.com",
		GitHubAPIURL:  "https://github.ibm.com/api/v3",
		GitHubAppSlug: "kubestellar-hive-ghe",
		GitHubAppID:   55555,
	}

	if err := provisionHive(h, req, cluster, nil, slog.Default()); err != nil {
		t.Fatalf("provisionHive GHE: %v", err)
	}
}

// TestProvisionHive_ManifestDirFailure verifies provisionHive returns an error
// when the manifest file cannot be created (e.g. read-only directory).
func TestProvisionHive_ManifestDirFailure(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()

	// Make the hives dir read-only so MkdirAll for manifests subdir fails
	// on the actual file creation (os.Create).
	hiveDir := filepath.Join(saasHivesDir, "hosted-readonly", "manifests")
	os.MkdirAll(hiveDir, 0o755)
	// Remove write permission on the manifests directory.
	os.Chmod(hiveDir, 0o555)
	defer os.Chmod(hiveDir, 0o755)

	h := &SaaSHive{ID: "hosted-readonly", Owner: "hal", Org: "ro", PrimaryRepo: "r"}
	req := &CreateHiveRequest{Org: "ro", Repos: "r", PrimaryRepo: "r", ClusterID: "hive-oke"}
	cluster := dynamicCluster()

	err := provisionHive(h, req, cluster, nil, slog.Default())
	if err == nil {
		t.Fatal("provisionHive should fail when manifest directory is not writable")
	}
	if !strings.Contains(err.Error(), "create manifest") {
		t.Errorf("expected 'create manifest' error, got: %v", err)
	}
}
