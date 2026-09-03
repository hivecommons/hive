package hub

import (
	"encoding/json"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

const vllmDDual = `{
  "id": "vllm-d",
  "github_base_url": "https://github.ibm.com",
  "github_api_url": "https://github.ibm.com/api/v3",
  "github_app_id": 5686,
  "github_app_slug": "kubestellar-hive-ghe",
  "forges": { "github.com": { "app_id": 3568013 } }
}`

// TestResolvedPublicIdentityIsExplicit pins that a public election produces
// STATED values, not empty fields a downstream resolver has to fill in.
//
// Empty used to be the convention: base_url was empty on 48 of 51 spokes,
// api_url on 41, app_slug on 33 — implicit state was the fleet's normal
// condition. That is exactly what hid the 2026-07-31 incident, where a GHE
// app_id beside an empty api_url read as *unset* rather than *mismatched* and
// nothing flagged it until a token call returned 404.
func TestResolvedPublicIdentityIsExplicit(t *testing.T) {
	var c ClusterConfig
	if err := json.Unmarshal([]byte(vllmDDual), &c); err != nil {
		t.Fatal(err)
	}

	got := ResolveHiveIdentity(&SaaSHive{ID: "pub", GitHubHost: "github.com"}, &c)

	if got.APIURL != config.DefaultGitHubAPIURL {
		t.Errorf("APIURL = %q, want the explicit %q", got.APIURL, config.DefaultGitHubAPIURL)
	}
	if got.BaseURL != config.DefaultGitHubBaseURL {
		t.Errorf("BaseURL = %q, want the explicit %q", got.BaseURL, config.DefaultGitHubBaseURL)
	}
	// The cluster's forges entry names an app_id and NO slug. It must be filled
	// from the App ID rather than shipped empty — an empty slug builds an
	// install link against the public default, which is how ten GHE hives got a
	// link pointing at an App that does not exist on their host.
	if got.AppSlug != config.PublicGitHubAppSlug {
		t.Errorf("AppSlug = %q, want it filled from app_id as %q", got.AppSlug, config.PublicGitHubAppSlug)
	}
	// Explicit must still be COHERENT — this is not just non-empty.
	if err := config.RejectIdentitySet(config.GitHubConfig{
		AppID: got.AppID, AppSlug: got.AppSlug, APIURL: got.APIURL, BaseURL: got.BaseURL,
	}); err != nil {
		t.Fatalf("explicit identity is not self-consistent: %v", err)
	}
}

// TestUnknownAppKeepsAnEmptySlug: filling a blank slug must never become
// INVENTING one. daviddiaz0317-visual-hive runs app_id 4240368 — a third App
// with its own key — and a guessed slug there is a confident wrong answer where
// no answer is correct.
func TestUnknownAppKeepsAnEmptySlug(t *testing.T) {
	if got := config.SlugOfAppID(4240368); got != "" {
		t.Fatalf("SlugOfAppID(4240368) = %q, want \"\" — an unrecognised App must not be given a slug", got)
	}
}

// TestHealthyImplicitSpokesStillValidate is the most important negative case.
// 48 of 51 live spokes carry at least one empty identity field. Making the hub
// SERVE explicit values must not make those spokes' own stored config invalid,
// or the change breaks the healthy fleet on the way to fixing it.
func TestHealthyImplicitSpokesStillValidate(t *testing.T) {
	shapes := []config.GitHubConfig{
		{AppID: config.PublicGitHubAppID},                                            // all three empty — the console hive
		{AppID: config.PublicGitHubAppID, AppSlug: config.PublicGitHubAppSlug},       // slug only
		{AppID: config.EnterpriseGitHubAppID, APIURL: config.EnterpriseGitHubAPIURL}, // GHE, api only
		{AppID: config.EnterpriseGitHubAppID, AppSlug: config.EnterpriseGitHubAppSlug,
			APIURL: config.EnterpriseGitHubAPIURL, BaseURL: config.EnterpriseGitHubBaseURL},
	}
	for i, gh := range shapes {
		if err := config.RejectIdentitySet(gh); err != nil {
			t.Errorf("shape %d (%+v) rejected: %v — implicit spokes must keep working while the fleet converges", i, gh, err)
		}
	}
}
