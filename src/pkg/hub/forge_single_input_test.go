package hub

import (
	"encoding/json"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// liveVllmD is the production clusters.json entry for vllm-d: a GHE-DEFAULT
// cluster that also names the public App, so hives on it may elect either.
const liveVllmD = `{
  "id": "vllm-d",
  "github_base_url": "https://github.ibm.com",
  "github_api_url": "https://github.ibm.com/api/v3",
  "github_app_id": 5686,
  "github_app_slug": "kubestellar-hive-ghe",
  "forges": { "github.com": { "app_id": 3568013, "app_slug": "kubestellar-hive" } }
}`

// TestHeartbeatPushSurvivesTheGuard drives appKeyConfigForHeartbeat — the
// function that CONTAINS the guard — rather than the resolver beside it.
//
// This distinction is the whole point. An earlier version of this test asserted
// on appIdentityForHive, which returns the correct identity whether or not the
// guard is fixed; the mutation that reverted the guard passed it. Only a test
// that calls the guarding function can prove the push is actually emitted.
//
// The bug: the guard validated the hive's resolved app_id (3568013, public)
// against api_url/base_url read from the CLUSTER (github.ibm.com), so
// RejectIdentitySet refused a set the hub had assembled itself, and 26 hives
// went unrepaired while the log said "nothing was sent".
func TestHeartbeatPushSurvivesTheGuard(t *testing.T) {
	withTempAppKeyDir(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	if err := storeClusterAppKey("vllm-d", testAppKeyPEM(t)); err != nil {
		t.Fatal(err)
	}
	var c ClusterConfig
	if err := json.Unmarshal([]byte(liveVllmD), &c); err != nil {
		t.Fatal(err)
	}
	s := &HubServer{logger: appKeyTestLogger(), clusters: map[string]ClusterConfig{"vllm-d": c}}

	// torch-spyre: repos live on github.com, so github_host records github.com.
	pub := &SaaSHive{ID: "hosted-torch-spyre", ClusterID: "vllm-d", GitHubHost: "github.com"}
	if err := saveSaaSHive(pub); err != nil {
		t.Fatal(err)
	}
	// Enrico: repos on github.ibm.com.
	ghe := &SaaSHive{ID: "hosted-vllmd-03", ClusterID: "vllm-d", GitHubHost: "github.ibm.com"}
	if err := saveSaaSHive(ghe); err != nil {
		t.Fatal(err)
	}

	t.Run("public hive: the push is EMITTED, not refused", func(t *testing.T) {
		got := s.appKeyConfigForHeartbeat("hosted-torch-spyre", "vllm-d", "", false, false,
			config.EnterpriseGitHubAppID, 148549259, s.logger, pub)
		if got == nil {
			t.Fatal("push was REFUSED (nil) — the guard is still validating against the cluster's forge instead of the hive's")
		}
		if got.AppID != config.PublicGitHubAppID {
			t.Fatalf("pushed app_id = %d, want %d", got.AppID, config.PublicGitHubAppID)
		}
	})

	t.Run("GHE hive still gets its own App", func(t *testing.T) {
		got := s.appKeyConfigForHeartbeat("hosted-vllmd-03", "vllm-d", "", false, false,
			0, 43057, s.logger, ghe)
		if got == nil {
			t.Fatal("GHE hive push refused")
		}
		if got.AppID != config.EnterpriseGitHubAppID {
			t.Fatalf("pushed app_id = %d, want %d", got.AppID, config.EnterpriseGitHubAppID)
		}
	})
}

// TestGitHubHostIsTheSingleInput pins that a hive's forge comes from one field.
func TestGitHubHostIsTheSingleInput(t *testing.T) {
	var c ClusterConfig
	if err := json.Unmarshal([]byte(liveVllmD), &c); err != nil {
		t.Fatal(err)
	}
	t.Run("legacy sentinel is normalized onto GitHubHost and the URLs cleared", func(t *testing.T) {
		legacy := &SaaSHive{ID: "old", ClusterID: "vllm-d", GitHubBaseURL: githubHostPublic}
		if !normalizeHiveForge(legacy, &c) {
			t.Fatal("expected normalization to change the hive")
		}
		if !isPublicForgeHost(legacy.GitHubHost) {
			t.Fatalf("GitHubHost = %q, want public", legacy.GitHubHost)
		}
		if legacy.GitHubBaseURL != "" || legacy.GitHubAPIURL != "" {
			t.Fatalf("per-hive URLs survived: base=%q api=%q — a second copy of the forge can drift from the first",
				legacy.GitHubBaseURL, legacy.GitHubAPIURL)
		}
		// And it must resolve the same way afterwards.
		if got := ResolveHiveIdentity(legacy, &c); got.AppID != config.PublicGitHubAppID {
			t.Fatalf("after normalization app_id = %d, want %d", got.AppID, config.PublicGitHubAppID)
		}
	})
	t.Run("a hive with no determinable forge is left alone", func(t *testing.T) {
		unknown := &SaaSHive{ID: "u", ClusterID: "vllm-d"}
		if normalizeHiveForge(unknown, &c) {
			t.Fatal("normalized a hive whose forge is unknown — that silently moves it to the cluster default")
		}
	})

	t.Run("URLs are NEVER cleared while the host is unset", func(t *testing.T) {
		// The clearing step removes the SECOND copy of the forge. Running it
		// when the host does not hold the fact destroys the only copy, leaving
		// the hive to inherit its cluster's forge — a public hive on vllm-d
		// would silently become GHE. An unresolvable per-hive URL is exactly
		// that case: electedForgeForHive cannot label it, so nothing may be
		// dropped.
		// A hive with NO forge evidence at all: nothing to normalize, and
		// nothing may be invented or erased.
		bare := &SaaSHive{ID: "bare", ClusterID: "vllm-d"}
		normalizeHiveForge(bare, &c)
		if bare.GitHubHost != "" {
			t.Fatalf("invented a forge %q for a hive that named none", bare.GitHubHost)
		}

		// A hive whose ONLY record is a per-hive URL keeps a usable record:
		// either the host now holds it, or the URL still does. Never neither.
		urlOnly := &SaaSHive{ID: "urlonly", ClusterID: "vllm-d", GitHubBaseURL: "https://github.ibm.com"}
		normalizeHiveForge(urlOnly, &c)
		if urlOnly.GitHubHost == "" && urlOnly.GitHubBaseURL == "" {
			t.Fatal("the hive's only record of its forge was erased — it will now inherit the cluster's App")
		}
		if !sameGitHubHost(urlOnly.GitHubHost, "github.ibm.com") {
			t.Fatalf("forge moved during normalization: host=%q", urlOnly.GitHubHost)
		}
	})
}
