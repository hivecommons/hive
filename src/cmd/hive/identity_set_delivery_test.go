package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	hub "github.com/hivecommons/hive/pkg/hub/spoke"
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

// TestGHEClaimAdoptedAsWholeSet is the spoke side of the never-blank-for-GHE
// fix: a placeholder provisioned PUBLIC (app_id 3568013, blank urls) is claimed
// for a github.ibm.com org, and the hub delivers the COMPLETE GHE set on one
// beat. The spoke must adopt all four fields together and end up coherent — the
// EPM symptom was exactly the opposite: it kept the public app_id and blank urls
// and silently talked to github.com.
func TestGHEClaimAdoptedAsWholeSet(t *testing.T) {
	// The starting state of an EPM placeholder before its claim converges.
	publicStart := config.GitHubConfig{
		AppID:   config.PublicGitHubAppID,
		AppSlug: config.PublicGitHubAppSlug,
		// blank urls == public github.com, the placeholder's provisioned state
	}

	t.Run("the complete GHE set is coherent and fully adopted", func(t *testing.T) {
		got := prospectiveGitHubIdentity(publicStart, &hub.HeartbeatGitHubAppConfig{
			AppID:   config.EnterpriseGitHubAppID,
			AppSlug: config.EnterpriseGitHubAppSlug,
			APIURL:  config.EnterpriseGitHubAPIURL,
			BaseURL: config.EnterpriseGitHubBaseURL,
		})
		if got == nil {
			t.Fatal("a full GHE identity change produced no prospective set")
		}
		if got.AppID != config.EnterpriseGitHubAppID ||
			got.AppSlug != config.EnterpriseGitHubAppSlug ||
			got.APIURL != config.EnterpriseGitHubAPIURL ||
			got.BaseURL != config.EnterpriseGitHubBaseURL {
			t.Fatalf("the GHE set was not adopted whole: %+v", *got)
		}
		if err := config.RejectIdentitySet(*got); err != nil {
			t.Fatalf("the complete GHE set was rejected as incoherent: %v", err)
		}
	})

	t.Run("the GHE App WITHOUT its urls is caught as the EPM half-set", func(t *testing.T) {
		// The regression shape: the GHE app_id/slug arrive but the urls do not, so
		// the spoke would pair the GHE App with its own blank (public) urls —
		// app_id 5686 aimed at api.github.com, "404 Integration not found".
		got := prospectiveGitHubIdentity(publicStart, &hub.HeartbeatGitHubAppConfig{
			AppID:   config.EnterpriseGitHubAppID,
			AppSlug: config.EnterpriseGitHubAppSlug,
		})
		if got == nil {
			t.Fatal("expected a prospective set to validate")
		}
		if err := config.RejectIdentitySet(*got); err == nil {
			t.Fatal("a GHE App left on blank/public urls was ACCEPTED — the EPM half-set must be refused so the whole delivery is retried, never half-applied")
		}
	})
}
