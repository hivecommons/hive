package hub

import (
	"encoding/json"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// liveVllmDWithForges is the production /data/saas/clusters.json entry for
// vllm-d, verbatim, plus the additive "forges" map this PR makes meaningful.
//
// Parsed from JSON rather than built as a struct so the test exercises the real
// unmarshalling path an operator's edit takes. A struct literal would skip the
// json tags and could pass while the actual file shape does not.
const liveVllmDWithForges = `{
  "id": "vllm-d",
  "name": "vLLM-d (GPU)",
  "storage_class": "ocs-storagecluster-cephfs",
  "requires_scc": true,
  "has_gpu": true,
  "image_tag": "v2-latest",
  "github_base_url": "https://github.ibm.com",
  "github_api_url": "https://github.ibm.com/api/v3",
  "github_app_id": 5686,
  "github_app_slug": "kubestellar-hive-ghe",
  "forges": {
    "github.com": {
      "app_id": 3568013,
      "app_slug": "kubestellar-hive"
    }
  }
}`

// TestLiveVllmDEntryRepairsElectedHives pins the deadlock this PR exists to
// clear, against the real cluster entry.
//
// On 2026-07-31 the hub resolved app_id 3568013 from hive intent but
// api_url/base_url from the GHE cluster, and the identity validator refused the
// push every ~30s for 26 hives:
//
//	REFUSING to push cluster github app config: app_id 3568013 is the GitHub
//	App on github.com, but api_url (https://github.ibm.com/api/v3) and base_url
//	(https://github.ibm.com) resolve to github.ibm.com
//
// Note an EMPTY identity also satisfies RejectIdentitySet, so asserting only
// "the validator accepts it" proves nothing — an earlier draft of this test
// passed with app_id 0. The App assertions below are what give it teeth.
func TestLiveVllmDEntryRepairsElectedHives(t *testing.T) {
	var c ClusterConfig
	if err := json.Unmarshal([]byte(liveVllmDWithForges), &c); err != nil {
		t.Fatalf("production clusters.json entry does not parse: %v", err)
	}

	mustPassValidator := func(t *testing.T, id HiveIdentity) {
		t.Helper()
		if err := config.RejectIdentitySet(config.GitHubConfig{
			AppID: id.AppID, AppSlug: id.AppSlug, APIURL: id.APIURL, BaseURL: id.BaseURL,
		}); err != nil {
			t.Fatalf("identity refused by the validator (this is the live deadlock):\n  %v", err)
		}
	}

	t.Run("hive that elected github.com gets the PUBLIC App", func(t *testing.T) {
		got := ResolveHiveIdentity(&SaaSHive{
			ID: "hosted-torch-spyre-spyre-infer-94c2", GitHubHost: "github.com",
		}, &c)
		if got.AppID != config.PublicGitHubAppID {
			t.Fatalf("app_id = %d, want %d (public)", got.AppID, config.PublicGitHubAppID)
		}
		if got.AppSlug != config.PublicGitHubAppSlug {
			t.Fatalf("app_slug = %q, want %q", got.AppSlug, config.PublicGitHubAppSlug)
		}
		// EXPLICIT urls are now correct here. They used to be empty, matching how
		// every healthy public spoke is stored on disk — but a field that means
		// something by being absent is what let a GHE app_id sit beside an empty
		// api_url and read as *unset* rather than *mismatched*. The values are
		// the same ones the resolver would have derived; they are now stated.
		if got.APIURL != config.DefaultGitHubAPIURL || got.BaseURL != config.DefaultGitHubBaseURL {
			t.Fatalf("public hive should carry explicit urls, got api=%q base=%q", got.APIURL, got.BaseURL)
		}
		mustPassValidator(t, got)
	})

	t.Run("GHE hive is unaffected", func(t *testing.T) {
		got := ResolveHiveIdentity(&SaaSHive{
			ID: "hosted-available-vllmd-03", GitHubHost: "github.ibm.com",
		}, &c)
		if got.AppID != config.EnterpriseGitHubAppID {
			t.Fatalf("GHE hive app_id = %d, want %d", got.AppID, config.EnterpriseGitHubAppID)
		}
		mustPassValidator(t, got)
	})

	t.Run("no App leaks between forges on one cluster", func(t *testing.T) {
		pub := ResolveHiveIdentity(&SaaSHive{ID: "p", GitHubHost: "github.com"}, &c)
		ghe := ResolveHiveIdentity(&SaaSHive{ID: "g", GitHubHost: "github.ibm.com"}, &c)
		if pub.AppID == ghe.AppID {
			t.Fatalf("both forges resolved to App %d", pub.AppID)
		}
	})

	t.Run("without the forges map the election yields NO App, never the wrong one", func(t *testing.T) {
		var legacy ClusterConfig
		if err := json.Unmarshal([]byte(liveVllmDWithForges), &legacy); err != nil {
			t.Fatal(err)
		}
		legacy.Forges = nil // the entry as it exists in production TODAY
		got := ResolveHiveIdentity(&SaaSHive{ID: "x", GitHubHost: "github.com"}, &legacy)
		if got.AppID == config.EnterpriseGitHubAppID {
			t.Fatal("public hive was handed the GHE App — this is the 2026-07-31 incident")
		}
		if got.AppID != 0 {
			t.Fatalf("expected no App without a forges entry, got %d", got.AppID)
		}
	})
}
