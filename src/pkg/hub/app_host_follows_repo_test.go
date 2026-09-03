package hub

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

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

	// A PUBLIC github.com cluster that names its own App ID must hand it to its
	// hives. This is the placeholder-sentinel fix: the public cluster carried no
	// github_app_id, so every hive it provisioned kept config.PlaceholderAppID
	// and could never authenticate. The rule is per-HOST, not GHE-only.
	publicCluster := &ClusterConfig{GitHubAppID: 3568013}
	if got := resolveProvisionAppID("", &SaaSHive{}, publicCluster); got != "3568013" {
		t.Errorf("public cluster app_id = %q, want the cluster's configured App 3568013", got)
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

// TestDecideAppKeySync_RepairsPlaceholderSentinel locks the fleet-repair half of
// the placeholder-app_id fix.
//
// A spoke provisioned as a placeholder carries config.PlaceholderAppID plus its
// own per-hive key. Before this, the per-hive key acted as an override and the
// reconcile refused to touch it (appKeyReasonPerHiveOverride), so the sentinel
// survived every heartbeat forever — the hive could never authenticate no matter
// what its owner did. The sentinel is never a deliberate pin, so it must be
// repaired even against a per-hive key, and even when that key's fingerprint
// happens to match (a correct key cannot rescue a nonexistent app_id).
func TestDecideAppKeySync_RepairsPlaceholderSentinel(t *testing.T) {
	cluster := &clusterAppIdentity{AppID: 3568013, Fingerprint: "cluster-fp"}

	for _, tc := range []struct {
		name        string
		fingerprint string
		hasPerHive  bool
	}{
		{"per-hive key, different fingerprint", "other-fp", true},
		{"per-hive key, matching fingerprint", "cluster-fp", true},
		{"no key at all", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decideAppKeySync(tc.fingerprint, tc.hasPerHive, false, false, config.PlaceholderAppID, cluster)
			if !got.Push {
				t.Errorf("sentinel app_id not repaired (%s): %+v", tc.name, got)
			}
			if got.Reason != appKeyReasonPlaceholderAppID {
				t.Errorf("Reason = %q, want %q", got.Reason, appKeyReasonPlaceholderAppID)
			}
		})
	}

	// A spoke on a genuinely different (real) App is still protected — the
	// sentinel is the ONLY app_id treated as unconditionally wrong.
	const someOtherRealApp = 4240368
	if got := decideAppKeySync("other-fp", true, false, false, someOtherRealApp, cluster); got.Push {
		t.Errorf("a real non-matching app_id must stay protected as a deliberate pin: %+v", got)
	}
}

// clusterGitHubConfig must derive the forge base-or-api, so a GHE cluster recorded
// with ONLY an api_url (blank base_url — the common state after heartbeat, which
// never delivers base_url) is still recognised as GHE: its host pill reads the
// enterprise host and its App install URL uses the GHE /github-apps/ path (not the
// public /apps/ path, which 404s on a GHE host).
func TestClusterGitHubConfig_BaseOrAPI(t *testing.T) {
	// GHE cluster, api_url only (no base_url), GHE slug set.
	gheAPIOnly := &ClusterConfig{
		GitHubAPIURL:  "https://github.ibm.com/api/v3",
		GitHubAppSlug: "kubestellar-hive-ghe",
	}
	gh := clusterGitHubConfig(gheAPIOnly)
	if host := gh.HostLabel(); host != "github.ibm.com" {
		t.Errorf("api-only GHE cluster HostLabel = %q, want github.ibm.com (base-or-api)", host)
	}
	if !gh.IsGHE() {
		t.Error("api-only GHE cluster must be recognised as GHE")
	}
	if url := gh.AppInstallURL(); url != "https://github.ibm.com/github-apps/kubestellar-hive-ghe/installations/new" {
		t.Errorf("api-only GHE install URL = %q, want the GHE /github-apps/ path", url)
	}

	// A genuine public cluster (nothing set) still reads github.com and the public
	// /apps/ install path.
	pub := clusterGitHubConfig(&ClusterConfig{})
	if host := pub.HostLabel(); host != "github.com" {
		t.Errorf("public cluster HostLabel = %q, want github.com", host)
	}
	if pub.IsGHE() {
		t.Error("public cluster must not be flagged GHE")
	}
}
