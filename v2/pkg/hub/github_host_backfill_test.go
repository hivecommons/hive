package hub

import "testing"

// The vllm-d gap: placeholders provisioned before their cluster gained GHE
// settings never inherit them, so the heartbeat pushes an empty API URL and
// the spoke keeps api.github.com plus the public app_id.
func TestBackfillGitHubHostFromCluster(t *testing.T) {
	gheCluster := &ClusterConfig{
		GitHubBaseURL: "https://github.ibm.com",
		GitHubAPIURL:  "https://github.ibm.com/api/v3",
	}
	publicCluster := &ClusterConfig{}

	tests := []struct {
		name    string
		hive    *SaaSHive
		cluster *ClusterConfig
		want    string
	}{
		{
			name:    "blank host on a GHE cluster is backfilled",
			hive:    &SaaSHive{},
			cluster: gheCluster,
			want:    "github.ibm.com",
		},
		{
			name:    "existing host is never overwritten",
			hive:    &SaaSHive{GitHubHost: "github.example.com"},
			cluster: gheCluster,
			want:    "",
		},
		{
			name:    "explicit public override stays public",
			hive:    &SaaSHive{GitHubBaseURL: githubHostPublic},
			cluster: gheCluster,
			want:    "",
		},
		{
			name:    "hive-level GHE override wins over the cluster default",
			hive:    &SaaSHive{GitHubBaseURL: "https://ghe.other.com"},
			cluster: gheCluster,
			want:    "ghe.other.com",
		},
		{
			name:    "public cluster has nothing to backfill",
			hive:    &SaaSHive{},
			cluster: publicCluster,
			want:    "",
		},
		{
			name:    "nil cluster is a no-op",
			hive:    &SaaSHive{},
			cluster: nil,
			want:    "",
		},
		{
			name:    "nil hive is a no-op",
			hive:    nil,
			cluster: gheCluster,
			want:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := backfillGitHubHostFromCluster(tc.hive, tc.cluster); got != tc.want {
				t.Errorf("backfillGitHubHostFromCluster = %q, want %q", got, tc.want)
			}
		})
	}
}

// The backfilled host must be what actually drives the heartbeat's API URL.
func TestBackfilledHostProducesGHEAPIURL(t *testing.T) {
	host := backfillGitHubHostFromCluster(&SaaSHive{}, &ClusterConfig{
		GitHubBaseURL: "https://github.ibm.com",
	})
	if got := gheAPIURLForHost(host); got != "https://github.ibm.com/api/v3" {
		t.Errorf("gheAPIURLForHost(%q) = %q, want https://github.ibm.com/api/v3", host, got)
	}
	// And a public hive must still push nothing.
	if got := gheAPIURLForHost(backfillGitHubHostFromCluster(&SaaSHive{}, &ClusterConfig{})); got != "" {
		t.Errorf("public hive pushed API URL %q, want empty", got)
	}
}
