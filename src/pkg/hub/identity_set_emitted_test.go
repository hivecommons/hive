package hub

import (
	"encoding/json"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const vllmDWithForges = `{
  "id": "vllm-d",
  "github_base_url": "https://github.ibm.com",
  "github_api_url": "https://github.ibm.com/api/v3",
  "github_app_id": 5686,
  "github_app_slug": "kubestellar-hive-ghe",
  "forges": { "github.com": { "app_id": 3568013, "app_slug": "kubestellar-hive" } }
}`

// TestHubEmitsTheCompleteIdentitySet pins that what the hub SENDS is a full,
// self-consistent identity — not an App whose forge urls the spoke must obtain
// from a different channel on a different beat.
//
// Validating the set hub-side and then shipping only half of it leaves the
// check meaningless: the spoke reassembles the App against whatever urls it
// already had, which is how a public app_id came to sit on GHE urls and 404.
func TestHubEmitsTheCompleteIdentitySet(t *testing.T) {
	withTempAppKeyDir(t)
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	if err := storeClusterAppKey("vllm-d", testAppKeyPEM(t)); err != nil {
		t.Fatal(err)
	}
	var c ClusterConfig
	if err := json.Unmarshal([]byte(vllmDWithForges), &c); err != nil {
		t.Fatal(err)
	}
	s := &HubServer{logger: appKeyTestLogger(), clusters: map[string]ClusterConfig{"vllm-d": c}}

	ghe := &SaaSHive{ID: "ghe-hive", ClusterID: "vllm-d", GitHubHost: "github.ibm.com"}
	if err := saveSaaSHive(ghe); err != nil {
		t.Fatal(err)
	}

	t.Run("GHE hive: the emitted set carries its forge urls", func(t *testing.T) {
		got := s.appKeyConfigForHeartbeat("ghe-hive", "vllm-d", "", false, false, 0, 43057, s.logger, ghe)
		if got == nil {
			t.Fatal("no config emitted")
		}
		if got.APIURL == "" && got.BaseURL == "" {
			t.Fatal("the App was sent with NO forge urls — the spoke must then pair it with whatever it already had, which is the half-identity this change removes")
		}
		// What is sent must itself be a valid set.
		if err := config.RejectIdentitySet(config.GitHubConfig{
			AppID: got.AppID, AppSlug: got.AppSlug, APIURL: got.APIURL, BaseURL: got.BaseURL,
		}); err != nil {
			t.Fatalf("the hub emitted an inconsistent set: %v", err)
		}
		if got.AppID != config.EnterpriseGitHubAppID {
			t.Fatalf("app_id = %d, want %d", got.AppID, config.EnterpriseGitHubAppID)
		}
	})
}
