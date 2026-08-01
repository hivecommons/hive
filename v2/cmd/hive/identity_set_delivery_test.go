package main

import (
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/hub"
)

// TestIdentityTravelsAsOneSet pins that the App and its forge URLs arrive
// together and are validated together.
//
// Before this, app_id/app_slug rode HeartbeatGitHubAppConfig while api_url rode
// HeartbeatProjectConfig — two structs dispatched to two independent callbacks
// (heartbeat.go:511 and :527), with nothing composing them. A forge switch
// deliberately sends api_url ALONE, so between beats the spoke held a public
// app_id against GHE urls: "404 Integration not found", the 2026-07-31 outage.
func TestIdentityTravelsAsOneSet(t *testing.T) {
	// A GHE hive being moved to public github.com — the switch that previously
	// arrived in two unordered halves.
	cur := config.GitHubConfig{
		AppID:   config.EnterpriseGitHubAppID,
		AppSlug: config.EnterpriseGitHubAppSlug,
		APIURL:  config.EnterpriseGitHubAPIURL,
		BaseURL: config.EnterpriseGitHubBaseURL,
	}

	t.Run("a complete public set is coherent and accepted", func(t *testing.T) {
		got := prospectiveGitHubIdentity(cur, &hub.HeartbeatGitHubAppConfig{
			AppID:   config.PublicGitHubAppID,
			AppSlug: config.PublicGitHubAppSlug,
			APIURL:  config.DefaultGitHubAPIURL,
			BaseURL: config.DefaultGitHubBaseURL,
		})
		if got == nil {
			t.Fatal("a full identity change produced no prospective set")
		}
		if got.APIURL != config.DefaultGitHubAPIURL || got.BaseURL != config.DefaultGitHubBaseURL {
			t.Fatalf("URLs did not travel with the App: api=%q base=%q", got.APIURL, got.BaseURL)
		}
		if err := config.RejectIdentitySet(*got); err != nil {
			t.Fatalf("complete set rejected: %v", err)
		}
	})

	t.Run("App WITHOUT its urls is caught as the half-set it is", func(t *testing.T) {
		// The old shape: public app_id delivered while the GHE urls remain.
		got := prospectiveGitHubIdentity(cur, &hub.HeartbeatGitHubAppConfig{
			AppID:   config.PublicGitHubAppID,
			AppSlug: config.PublicGitHubAppSlug,
		})
		if got == nil {
			t.Fatal("expected a prospective set")
		}
		if err := config.RejectIdentitySet(*got); err == nil {
			t.Fatal("a public App left sitting on GHE urls was ACCEPTED — this is the 404 Integration not found shape")
		}
	})

	t.Run("empty urls mean unchanged, never blank", func(t *testing.T) {
		// A healthy GHE hive receiving a key-only refresh must keep its urls.
		got := prospectiveGitHubIdentity(cur, &hub.HeartbeatGitHubAppConfig{
			AppID:      config.EnterpriseGitHubAppID,
			PrivateKey: "x",
		})
		if got == nil {
			return // nothing adopted at all is also fine
		}
		if got.APIURL != cur.APIURL || got.BaseURL != cur.BaseURL {
			t.Fatalf("empty urls blanked a working config: api=%q base=%q", got.APIURL, got.BaseURL)
		}
	})
}
