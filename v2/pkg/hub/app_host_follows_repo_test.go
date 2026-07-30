package hub

import "testing"

// The App ID provisioned for a hive must follow the hive's GitHub HOST:
// a GHE hive gets the cluster's GitHub Enterprise App, never the public
// github.com App (which cannot mint tokens against github.ibm.com).
func TestResolveProvisionAppID_GHEUsesClusterApp(t *testing.T) {
	gheCluster := &ClusterConfig{
		GitHubBaseURL: "https://github.ibm.com",
		GitHubAPIURL:  "https://github.ibm.com/api/v3",
		GitHubAppID:   5686, // the enterprise App
	}
	// A GHE hive (inherits the cluster's GHE base URL) seeded with the public
	// github.com app_id in its request must be corrected to the cluster's GHE App.
	gheHive := &SaaSHive{} // blank host → inherits the GHE cluster default
	if got := resolveProvisionAppID("3568013", gheHive, gheCluster); got != "5686" {
		t.Errorf("GHE hive app_id = %q, want cluster GHE App 5686 (not the public github.com app)", got)
	}

	// A hive that explicitly opts to public github.com (sentinel) on a GHE
	// cluster is NOT GHE — it must keep the request's public app_id.
	publicOnGHECluster := &SaaSHive{GitHubBaseURL: githubHostPublic}
	if got := resolveProvisionAppID("3568013", publicOnGHECluster, gheCluster); got != "3568013" {
		t.Errorf("public-override hive on GHE cluster app_id = %q, want request value 3568013", got)
	}
}

// Public-GitHub hives (and clusters with no GHE App) keep the request's app_id
// verbatim — the fix must not disturb the public path.
func TestResolveProvisionAppID_PublicUnchanged(t *testing.T) {
	publicCluster := &ClusterConfig{} // no GitHubBaseURL, no GitHubAppID
	hive := &SaaSHive{}
	if got := resolveProvisionAppID("3568013", hive, publicCluster); got != "3568013" {
		t.Errorf("public hive app_id = %q, want request value 3568013", got)
	}
	// A GHE cluster that has NOT configured a GHE App id falls back to the
	// request value rather than emitting 0.
	gheClusterNoApp := &ClusterConfig{GitHubBaseURL: "https://github.ibm.com", GitHubAPIURL: "https://github.ibm.com/api/v3"}
	if got := resolveProvisionAppID("3568013", &SaaSHive{}, gheClusterNoApp); got != "3568013" {
		t.Errorf("GHE cluster w/o App app_id = %q, want request value 3568013 (no cluster App to use)", got)
	}
}
