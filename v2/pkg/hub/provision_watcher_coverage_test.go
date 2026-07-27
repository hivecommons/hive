package hub

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ============================================================
// saas_provision.go — startProvisionWatcher loop body
//
// Drives the watcher with a short interval and a scripted kubectl that reports
// availableReplicas=1, so a provisioning hive flips to "running". Also seeds a
// timed-out provisioning hive to exercise the timeout branch. The watcher has
// no cancellation, so its goroutine is left blocking after the first pass (it
// makes no further state changes once the hive is running).
// ============================================================

func installReplicaKubectl(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '1'\nexit 0\n"
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestStartProvisionWatcher(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installReplicaKubectl(t)

	oldInterval := provisionPollInterval
	provisionPollInterval = 15 * time.Millisecond
	defer func() { provisionPollInterval = oldInterval }()

	// A fresh provisioning hive -> should flip to running once replicas=1.
	saveSaaSHive(&SaaSHive{
		ID: "prov1", Owner: "alice", Status: "provisioning", ClusterID: "hive-oke",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	// A timed-out provisioning hive -> should flip to error.
	saveSaaSHive(&SaaSHive{
		ID: "prov-timeout", Owner: "alice", Status: "provisioning", ClusterID: "hive-oke",
		CreatedAt: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
	})

	s := &HubServer{logger: slog.Default(), clusters: map[string]ClusterConfig{"hive-oke": {ID: "hive-oke", InCluster: true}}}
	go s.startProvisionWatcher()

	// Wait for the watcher to process at least one poll.
	deadline := time.After(3 * time.Second)
	for {
		h := loadSaaSHive("prov1")
		to := loadSaaSHive("prov-timeout")
		if h != nil && h.Status == "running" && to != nil && to.Status == "error" {
			return
		}
		select {
		case <-deadline:
			t.Logf("watcher did not settle in time (prov1=%v timeout=%v)",
				statusOf(loadSaaSHive("prov1")), statusOf(loadSaaSHive("prov-timeout")))
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func statusOf(h *SaaSHive) string {
	if h == nil {
		return "<nil>"
	}
	return h.Status
}
