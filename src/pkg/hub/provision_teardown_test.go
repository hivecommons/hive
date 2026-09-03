package hub

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
// Teardown on the FAILURE path (issue #5768)
// ============================================================================
//
// The leak was not a delete that raced a terminating PVC and not a test
// harness that forgot a cleanup hook: it was that the failure path had NO
// teardown at all. `kubectl apply -f` is not transactional and the Namespace is
// the first object in k8sManifestTemplate, so a failure on any later object
// leaves the namespace (and its PVCs) on the cluster while provisionHive
// returns an error and its callers only record Status="error".
//
// These tests pin the behaviour that closes that hole, and they are written to
// FAIL against the old code: the assertion is that a `delete namespace` is
// actually issued when the apply fails, which is exactly what did not happen.

// installRecordingKubectl installs a fake kubectl that appends every
// invocation to a log file and fails `apply` with the given exit code. It
// returns a function reading back the recorded invocations.
//
// applyExit=1 reproduces the real leak trigger: the namespace is created by the
// first object in the manifest, and then a later object is rejected.
func installRecordingKubectl(t *testing.T, applyExit int) func() []string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")

	script := `#!/bin/sh
echo "$*" >> "` + logPath + `"
case "$*" in
  *apply*)
    echo "error: unable to create PersistentVolumeClaim: no persistent volumes available" >&2
    exit ` + itoa(applyExit) + `
    ;;
esac
exit 0
`
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func() []string {
		b, err := os.ReadFile(logPath)
		if err != nil {
			return nil
		}
		var out []string
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if strings.TrimSpace(line) != "" {
				out = append(out, line)
			}
		}
		return out
	}
}

// TestProvisionFailureTearsDownNamespace is the regression test for #5768.
//
// It drives provisionHive against a kubectl whose `apply` fails — the exact
// shape of the real incident, a PVC that cannot bind — and asserts that a
// `delete namespace` for the hive's namespace is issued before the error is
// returned. Against the pre-fix code no delete is ever issued and this fails.
func TestProvisionFailureTearsDownNamespace(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	calls := installRecordingKubectl(t, 1)

	h := &SaaSHive{
		ID:          "hosted-leaky-org-project-abcd",
		Owner:       "alice",
		Org:         "example-org",
		ProjectName: "project",
		ClusterID:   "hive-oke",
		Repos:       []string{"repo"},
		PrimaryRepo: "repo",
	}
	req := &CreateHiveRequest{
		Org:         "example-org",
		Repos:       "repo",
		PrimaryRepo: "repo",
		ProjectName: "project",
		ClusterID:   "hive-oke",
		GitHubToken: "ghp_test",
	}
	cluster := &ClusterConfig{ID: "hive-oke", InCluster: true, StorageType: storageTypeDynamic, Domain: "example.test"}

	err := provisionHive(h, req, cluster, nil, slog.Default())
	if err == nil {
		t.Fatal("expected provisionHive to fail when kubectl apply fails")
	}

	wantNS := "hive-hosted-" + h.ID
	sawApply := false
	sawDelete := false
	for _, c := range calls() {
		if strings.Contains(c, "apply") {
			sawApply = true
		}
		if strings.Contains(c, "delete namespace "+wantNS) {
			sawDelete = true
		}
	}
	if !sawApply {
		t.Fatal("test did not exercise the apply path at all")
	}
	if !sawDelete {
		t.Fatalf("REGRESSION #5768: provisioning failed but never deleted namespace %q.\n"+
			"kubectl apply is not transactional and the Namespace is the FIRST object in the\n"+
			"manifest, so the namespace and its PVCs are already on the cluster. Without this\n"+
			"delete they leak forever — nothing else revisits a hive that never finished\n"+
			"provisioning.\nkubectl calls were:\n  %s",
			wantNS, strings.Join(calls(), "\n  "))
	}
}

// TestTeardownPartialProvisionSkipsPullOnlyCluster asserts the teardown does
// not try to run kubectl against a cluster the hub has no kubectl path into.
// Attempting it would burn a timeout on the provisioning worker and could not
// succeed; the residue there is a known gap, logged rather than silent.
func TestTeardownPartialProvisionSkipsPullOnly(t *testing.T) {
	calls := installRecordingKubectl(t, 0)
	cluster := &ClusterConfig{ID: "pull-only", PullOnly: true}
	teardownPartialProvision(cluster, "hive-hosted-x", "x", slog.Default())
	for _, c := range calls() {
		if strings.Contains(c, "delete") {
			t.Errorf("teardown must not issue kubectl against a pull-only cluster, got: %q", c)
		}
	}
}

// TestTeardownPartialProvisionIgnoresEmptyInputs guards the trivially unsafe
// call shapes — a nil cluster or an empty namespace must never turn into a
// `kubectl delete namespace ""`.
func TestTeardownPartialProvisionIgnoresEmptyInputs(t *testing.T) {
	calls := installRecordingKubectl(t, 0)
	teardownPartialProvision(nil, "hive-hosted-x", "x", slog.Default())
	teardownPartialProvision(&ClusterConfig{ID: "c", InCluster: true}, "", "x", slog.Default())
	teardownPartialProvision(&ClusterConfig{ID: "c", InCluster: true}, "   ", "x", slog.Default())
	if got := calls(); len(got) != 0 {
		t.Errorf("teardown issued kubectl for an empty namespace or nil cluster: %v", got)
	}
}
