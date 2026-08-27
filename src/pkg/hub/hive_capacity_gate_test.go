package hub

import "testing"

// TestClusterAtMaxHives covers the per-cluster provisioning ceiling: counting
// by ClusterID (with the empty-ID → default-cluster fallback) and the
// unlimited-when-zero contract.
func TestClusterAtMaxHives(t *testing.T) {
	dir := t.TempDir()
	origDir := saasHivesDir
	saasHivesDir = dir
	t.Cleanup(func() { saasHivesDir = origDir })

	// Two hives on cluster-a, one legacy record with no ClusterID (counts
	// against the default cluster), one on cluster-b.
	for _, h := range []*SaaSHive{
		{ID: "a1", ClusterID: "cluster-a", Status: statusAvailable},
		{ID: "a2", ClusterID: "cluster-a", Status: statusAssigned},
		{ID: "legacy", Status: statusAssigned},
		{ID: "b1", ClusterID: "cluster-b", Status: statusAvailable},
	} {
		if err := saveSaaSHive(h); err != nil {
			t.Fatalf("save %s: %v", h.ID, err)
		}
	}

	if n := clusterHiveCount("cluster-a"); n != 2 {
		t.Errorf("clusterHiveCount(cluster-a) = %d, want 2", n)
	}
	if n := clusterHiveCount(defaultClusterID); n != 1 {
		t.Errorf("clusterHiveCount(default) = %d, want 1 (legacy empty ClusterID)", n)
	}

	cases := []struct {
		name     string
		cluster  *ClusterConfig
		wantFull bool
	}{
		{"zero max is unlimited", &ClusterConfig{ID: "cluster-a", MaxHives: 0}, false},
		{"below the ceiling", &ClusterConfig{ID: "cluster-a", MaxHives: 3}, false},
		{"at the ceiling", &ClusterConfig{ID: "cluster-a", MaxHives: 2}, true},
		{"over the ceiling", &ClusterConfig{ID: "cluster-a", MaxHives: 1}, true},
		{"nil cluster never gates", nil, false},
		{"other cluster counts separately", &ClusterConfig{ID: "cluster-b", MaxHives: 2}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full, _ := clusterAtMaxHives(tc.cluster)
			if full != tc.wantFull {
				t.Errorf("clusterAtMaxHives = %v, want %v", full, tc.wantFull)
			}
		})
	}
}
