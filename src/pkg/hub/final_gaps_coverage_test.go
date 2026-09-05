package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================
// saas_provision.go — provisionHive NFS+OCI branch + readSAToken success
// ============================================================

func TestProvisionHiveNFSWithOCI(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	installScriptedKubectl(t)

	// OCI FSS + export creation succeed against a local server -> the NFS/OCI
	// branch of provisionHive runs and stores the returned OCIDs.
	ociSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"ocid1.thing.test"}`))
	}))
	defer ociSrv.Close()
	injectOCIConfig(t, testOCIConfig(t))
	pointOCIEndpointAt(t, ociSrv)

	h := &SaaSHive{ID: "hosted-nfs", Owner: "alice", Org: "acme", Repos: []string{"repo"}, PrimaryRepo: "repo", ACMMLevel: 2}
	req := &CreateHiveRequest{Org: "acme", Repos: "repo", PrimaryRepo: "repo", ACMMLevel: 2, ClusterID: "hive-oke"}
	// NFS storage triggers the OCI create branch.
	cluster := &ClusterConfig{ID: "hive-oke", InCluster: true, StorageType: storageTypeNFS, Domain: "hive.kubestellar.io"}

	if err := provisionHive(h, req, cluster, nil, slog.Default()); err != nil {
		t.Fatalf("provisionHive NFS+OCI: %v", err)
	}
	if h.OCIFileSystemID == "" || h.OCIExportID == "" {
		t.Errorf("expected OCI IDs recorded, got fs=%q export=%q", h.OCIFileSystemID, h.OCIExportID)
	}
}

// ============================================================
// webhook.go — pushGitHubConfigToSpoke first-attempt failure then success
// ============================================================

func TestPushGitHubConfigToSpokeRetryThenSucceed(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			// Force a client-side failure on the first attempt by hijacking and
			// closing the connection abruptly.
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Shorten the retry delay AND the per-attempt timeout so the test is fast:
	// the forced connection failure would otherwise hang each attempt for the
	// full 30s push timeout (minutes across all retries).
	oldDelay, oldTimeout := webhookRetryDelay, webhookPushTimeout
	webhookRetryDelay = time.Millisecond
	webhookPushTimeout = 2 * time.Second
	defer func() { webhookRetryDelay = oldDelay; webhookPushTimeout = oldTimeout }()

	s := &HubServer{logger: slog.Default(), clusters: map[string]ClusterConfig{"hive-oke": {ID: "hive-oke"}}}
	hive := &RegistryEntry{ID: "h1", ClusterID: "hive-oke", DashboardURL: srv.URL}
	s.pushGitHubConfigToSpoke(hive, 100, 200)
	if atomic.LoadInt32(&hits) < 2 {
		t.Errorf("expected a retry after the first failure, hits=%d", atomic.LoadInt32(&hits))
	}
}
