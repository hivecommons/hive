package hub

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestPendingIdentityCarriesTheForgeURLs pins that the DURABLE pending record
// delivers a complete identity set, not just the App half.
//
// The record stored APIURL from the moment it was introduced and never sent it,
// and had no BaseURL field at all. So a forge switch — the operation this record
// exists to make reliable — delivered a new app_id while the spoke kept its
// previous api_url/base_url: "404 Integration not found", the exact half-set
// that took 26 hives down on 2026-07-31.
func TestPendingIdentityCarriesTheForgeURLs(t *testing.T) {
	oldHives := saasHivesDir
	saasHivesDir = t.TempDir()
	t.Cleanup(func() { saasHivesDir = oldHives })

	// A hive being switched from GHE to public github.com.
	h := &SaaSHive{
		ID:        "switching-hive",
		ClusterID: "vllm-d",
		PendingAppConfig: &PendingAppIdentity{
			AppID:          config.PublicGitHubAppID,
			AppSlug:        config.PublicGitHubAppSlug,
			InstallationID: 148549259,
			APIURL:         config.DefaultGitHubAPIURL,
			BaseURL:        config.DefaultGitHubBaseURL,
			Reason:         "forge switch to github.com",
		},
	}
	if err := saveSaaSHive(h); err != nil {
		t.Fatal(err)
	}

	s := &HubServer{logger: appKeyTestLogger()}

	// The spoke still reports its OLD GHE identity — nothing confirmed yet.
	got := s.pendingAppIdentityForHeartbeat(&HeartbeatPayload{
		HiveID:      "switching-hive",
		GitHubAppID: config.EnterpriseGitHubAppID,
	})
	if got == nil {
		t.Fatal("no identity pushed for an unconfirmed pending record")
	}
	if got.APIURL == "" || got.BaseURL == "" {
		t.Fatalf("the App was sent WITHOUT its forge urls (api=%q base=%q) — the spoke would pair it with the GHE urls it already has",
			got.APIURL, got.BaseURL)
	}
	// What is sent must itself be a coherent set.
	if err := config.RejectIdentitySet(config.GitHubConfig{
		AppID: got.AppID, AppSlug: got.AppSlug, APIURL: got.APIURL, BaseURL: got.BaseURL,
	}); err != nil {
		t.Fatalf("the pending record emitted an inconsistent set: %v", err)
	}
}
