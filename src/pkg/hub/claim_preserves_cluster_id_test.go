package hub

import (
	"io"
	"log/slog"
	"testing"
)

// newMultiClusterTestHub returns a hub with both the public (hive-oke) pool and
// the GPU (vllm-d) pool configured, plus a discard logger. This mirrors the
// live multi-cluster hub where the cluster_id-drop bug mis-routed vllm-d hives
// to hive-oke's App/host defaults.
func newMultiClusterTestHub() *HubServer {
	return &HubServer{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		clusters: map[string]ClusterConfig{
			defaultClusterID: {
				ID:   defaultClusterID,
				Name: "hive-oke",
			},
			gpuClusterID: {
				ID:            gpuClusterID,
				Name:          "vllm-d",
				GitHubBaseURL: gheTestBaseURL,
				GitHubAPIURL:  gheTestAPIURL,
			},
		},
	}
}

// TestEnsureClusterIDForClaim covers the guard directly: a hive's own non-blank
// cluster is never overridden, a blank one falls back to the caller's pool when
// that pool is real, and otherwise to the default. It must never leave a blank.
func TestEnsureClusterIDForClaim(t *testing.T) {
	s := newMultiClusterTestHub()

	tests := []struct {
		name         string
		startCluster string
		poolFallback string
		want         string
	}{
		{
			name:         "own vllm-d cluster is preserved, pool is ignored",
			startCluster: gpuClusterID,
			poolFallback: defaultClusterID, // even a mismatching pool must not win
			want:         gpuClusterID,
		},
		{
			name:         "own hive-oke cluster is preserved",
			startCluster: defaultClusterID,
			poolFallback: gpuClusterID,
			want:         defaultClusterID,
		},
		{
			name:         "blank falls back to the vllm-d pool it was drawn from",
			startCluster: "",
			poolFallback: gpuClusterID,
			want:         gpuClusterID,
		},
		{
			name:         "blank with an unknown pool falls back to the default",
			startCluster: "",
			poolFallback: "cluster-that-does-not-exist",
			want:         defaultClusterID,
		},
		{
			name:         "blank with no pool falls back to the default",
			startCluster: "",
			poolFallback: "",
			want:         defaultClusterID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &SaaSHive{ID: "hosted-x", ClusterID: tc.startCluster}
			s.ensureClusterIDForClaim(h, tc.poolFallback)
			if h.ClusterID != tc.want {
				t.Errorf("ClusterID = %q, want %q", h.ClusterID, tc.want)
			}
			if h.ClusterID == "" {
				t.Error("ClusterID is blank after the claim guard — clusterForHive would mis-route to hive-oke")
			}
		})
	}
}

// TestAssignPreservesVllmdClusterID is the regression test for the reported bug:
// claiming/assigning a vllm-d placeholder must keep it on vllm-d in meta.json —
// it must NOT become hive-oke or blank. We exercise the exact ClusterID
// preservation the assign handler performs, then round-trip through
// saveSaaSHive/loadSaaSHive (which is where the field actually vanished on the
// live PVC via omitempty) and assert clusterForHive still resolves to vllm-d.
func TestAssignPreservesVllmdClusterID(t *testing.T) {
	useTempHiveDir(t)
	s := newMultiClusterTestHub()

	// A vllm-d placeholder as it sits before a claim.
	placeholder := &SaaSHive{
		ID:        "hosted-available-vllmd-01",
		Owner:     hubAdminUsername,
		Status:    statusAvailable,
		ClusterID: gpuClusterID,
	}
	if err := saveSaaSHive(placeholder); err != nil {
		t.Fatal(err)
	}

	// Mirror the assign handler: load the placeholder, rewrite its project, and
	// apply the ClusterID guard before saving.
	h := loadSaaSHive(placeholder.ID)
	if h == nil {
		t.Fatal("placeholder disappeared")
	}
	h.Owner = "taylormgeorge91"
	h.Org = "katamari"
	h.Repos = []string{"ibm-aiops-orchestrator"}
	h.PrimaryRepo = "ibm-aiops-orchestrator"
	h.Status = ""
	s.ensureClusterIDForClaim(h, "")
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	// The whole point: after the claim round-trips through disk, cluster_id must
	// still be vllm-d, not hive-oke and not blank.
	reloaded := loadSaaSHive(placeholder.ID)
	if reloaded == nil {
		t.Fatal("claimed hive disappeared")
	}
	if reloaded.ClusterID != gpuClusterID {
		t.Fatalf("claimed vllm-d hive lost its cluster: ClusterID = %q, want %q", reloaded.ClusterID, gpuClusterID)
	}
	if got := s.clusterForHive(reloaded); got == nil || got.ID != gpuClusterID {
		t.Fatalf("clusterForHive resolved a claimed vllm-d hive to the wrong cluster: %+v (want %s)", got, gpuClusterID)
	}
	// And it must carry vllm-d's GHE host defaults, not hive-oke's public github.com.
	if got := s.clusterForHive(reloaded); got.GitHubBaseURL != gheTestBaseURL {
		t.Errorf("claimed vllm-d hive got the wrong cluster's GitHub base URL: %q, want %q", got.GitHubBaseURL, gheTestBaseURL)
	}
}

// TestApprovePreservesPoolClusterID proves the approve path stamps the pool it
// drew from: a placeholder that somehow lost its cluster_id, when approved into
// the vllm-d pool, is stamped vllm-d — NOT left blank to resolve to hive-oke.
func TestApprovePreservesPoolClusterID(t *testing.T) {
	useTempHiveDir(t)
	s := newMultiClusterTestHub()

	// Simulate the degraded on-disk state: a placeholder with NO cluster_id.
	// findAvailablePlaceholder treats blank as hive-oke, but the approve path
	// pulled it from the vllm-d pool (private auth_method) and must record that.
	h := &SaaSHive{
		ID:     "hosted-approve-blank",
		Owner:  "taylormgeorge91",
		Org:    "katamari",
		Status: "",
	}
	pool := poolClusterForAuthMethod(authMethodPrivate) // vllm-d
	s.ensureClusterIDForClaim(h, pool)
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	reloaded := loadSaaSHive(h.ID)
	if reloaded == nil {
		t.Fatal("approved hive disappeared")
	}
	if reloaded.ClusterID != gpuClusterID {
		t.Errorf("approved private hive did not inherit the vllm-d pool: ClusterID = %q, want %q", reloaded.ClusterID, gpuClusterID)
	}
}
