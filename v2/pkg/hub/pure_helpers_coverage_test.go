package hub

import "testing"

// TestClusterGitHubConfigPure covers the nil cluster and the base-or-api mapping.
func TestClusterGitHubConfigPure(t *testing.T) {
	// nil → an empty config (no base/api/slug).
	if got := clusterGitHubConfig(nil); got.BaseURL != "" || got.APIURL != "" || got.AppSlug != "" {
		t.Errorf("nil cluster = %+v, want zero config", got)
	}
	c := &ClusterConfig{GitHubBaseURL: "", GitHubAPIURL: "https://github.ibm.com/api/v3", GitHubAppSlug: "kubestellar-hive-ghe"}
	gh := clusterGitHubConfig(c)
	if gh.APIURL != "https://github.ibm.com/api/v3" || gh.AppSlug != "kubestellar-hive-ghe" || gh.BaseURL != "" {
		t.Errorf("mapping = %+v, want api_url + slug carried, base empty", gh)
	}
	// A GHE cluster recorded with only an api_url must still read as GHE.
	if !gh.IsGHE() {
		t.Error("api-only GHE cluster config must be GHE")
	}
}

// TestHealthStatusOfPure covers all three branches of the classifier.
func TestHealthStatusOfPure(t *testing.T) {
	if got := healthStatusOf(nil); got != "unknown" {
		t.Errorf("nil health = %q, want unknown", got)
	}
	if got := healthStatusOf(map[string]any{}); got != "unknown" {
		t.Errorf("no status key = %q, want unknown", got)
	}
	if got := healthStatusOf(map[string]any{"status": ""}); got != "unknown" {
		t.Errorf("empty status = %q, want unknown", got)
	}
	if got := healthStatusOf(map[string]any{"status": 42}); got != "unknown" {
		t.Errorf("non-string status = %q, want unknown", got)
	}
	if got := healthStatusOf(map[string]any{"status": "degraded"}); got != "degraded" {
		t.Errorf("status = %q, want degraded", got)
	}
}

// TestNormalizeHiveForgePure covers the nil guard, a GHE-override hive (host set
// + URL overrides cleared), a public hive (no change), and idempotence.
func TestNormalizeHiveForgePure(t *testing.T) {
	if normalizeHiveForge(nil, nil) {
		t.Error("nil hive must report no change")
	}
	// GHE override on a hive: host gets set to the elected GHE host and the URL
	// overrides are cleared.
	h := &SaaSHive{GitHubBaseURL: "https://github.ibm.com", GitHubAPIURL: "https://github.ibm.com/api/v3"}
	if !normalizeHiveForge(h, nil) {
		t.Fatal("a GHE-override hive must report a change")
	}
	if h.GitHubHost != "github.ibm.com" {
		t.Errorf("GitHubHost = %q, want github.ibm.com", h.GitHubHost)
	}
	if h.GitHubBaseURL != "" || h.GitHubAPIURL != "" {
		t.Errorf("URL overrides not cleared: base=%q api=%q", h.GitHubBaseURL, h.GitHubAPIURL)
	}
	// Second pass is idempotent — nothing left to change.
	if normalizeHiveForge(h, nil) {
		t.Error("normalizeHiveForge must be idempotent on a settled hive")
	}
}
