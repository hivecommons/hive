package config

import (
	"errors"
	"strings"
	"testing"
)

// These tests exist because a previous version of the identity check passed
// against fixtures crafted to match it while being UNREACHABLE on real fleet
// data: all three of its markers (apiIsGHE, baseIsGHE, slugIsGHE) derive from
// fields that are empty on ~41 of ~51 production spokes. Every test here that
// asserts a rejection uses a shape that occurs on the real fleet.

// identityCombo names one of the 16 states of the four identity fields, with
// each field taking either its public value or its enterprise value.
type identityCombo struct {
	appID   int64
	slug    string
	apiURL  string
	baseURL string
}

func (c identityCombo) cfg() GitHubConfig {
	return GitHubConfig{
		AppID:   c.appID,
		AppSlug: c.slug,
		APIURL:  c.apiURL,
		BaseURL: c.baseURL,
	}
}

// TestIdentitySet_All16Combinations walks every combination of the four fields
// across their public and enterprise values. Exactly two are valid; the other
// fourteen are mixed forges and must be rejected.
//
// The public "value" for api_url and base_url is the EXPLICIT URL here; the
// empty-string form (which is what the fleet actually runs) is covered
// separately in TestIdentitySet_RealFleetShapes so a failure tells you which of
// the two is broken.
//
// NO EXCEPTIONS. app_slug IS derivable from app_id — a GitHub App has exactly
// one slug, fixed at registration. So the combination
//
//	app_id 5686 + slug kubestellar-hive + GHE api_url + GHE base_url
//
// names two different Apps and is REJECTED. Exactly 2 of the 16 are valid.
//
// An earlier revision of this test carved out an exception for a second slug
// on App 5686 ("ibm-hive"), citing pkg/hub/cluster_app_key_test.go. That
// citation was to a TEST FIXTURE, not a deployment: "ibm-hive" appears in no
// production config, no cluster entry, and on none of the 51 fleet spokes —
// live clusters.json carries "kubestellar-hive-ghe" for vllm-d. The exception
// let a GHE App under a non-"ghe" slug pass validation, which is the exact
// shape this file exists to reject. Any future exception here needs a citation
// to live config, not to a fixture. It is enumerated
// See the note on EnterpriseGitHubAppSlug.
func TestIdentitySet_All16Combinations(t *testing.T) {
	appIDs := []int64{PublicGitHubAppID, EnterpriseGitHubAppID}
	slugs := []string{PublicGitHubAppSlug, EnterpriseGitHubAppSlug}
	apiURLs := []string{DefaultGitHubAPIURL, EnterpriseGitHubAPIURL}
	baseURLs := []string{DefaultGitHubBaseURL, EnterpriseGitHubBaseURL}

	// Index 0 = public, 1 = enterprise. The forge of an identity is decided by
	// app_id, api_url and base_url; those three must agree.
	seen, rejected := 0, 0
	for ai, appID := range appIDs {
		for si, slug := range slugs {
			for pi, apiURL := range apiURLs {
				for bi, baseURL := range baseURLs {
					seen++
					combo := identityCombo{appID: appID, slug: slug, apiURL: apiURL, baseURL: baseURL}
					forgeAgrees := ai == pi && pi == bi
					// A GitHub App has exactly one slug, so the slug must match
					// the app_id — in BOTH directions. No tolerance.
					wantValid := forgeAgrees && si == ai

					err := RejectIdentitySet(combo.cfg())
					if err != nil {
						rejected++
					}
					if wantValid && err != nil {
						t.Errorf("valid identity %+v was REJECTED: %v", combo, err)
					}
					if !wantValid && err == nil {
						t.Errorf("mixed identity %+v was ACCEPTED — this is the class of state that took down seven hives", combo)
					}
					if !wantValid && err != nil && !errors.Is(err, ErrIdentitySetMismatch) {
						t.Errorf("rejection of %+v did not wrap ErrIdentitySetMismatch: %v", combo, err)
					}
				}
			}
		}
	}
	if seen != 16 {
		t.Fatalf("walked %d combinations, want 16", seen)
	}
	// Exactly 2 valid: public×public×public×public and GHE×GHE×GHE×GHE.
	// Pinning the count catches a rule that silently stops firing.
	if want := 14; rejected != want {
		t.Errorf("rejected %d of 16 combinations, want %d", rejected, want)
	}
}

// TestIdentitySet_Incident2026_07_31 pins the EXACT field set of the incident:
// a GHE app_id and slug pushed onto a public-GitHub hive whose api_url and
// base_url were never sent and so stayed empty. This is the write that must be
// refused.
func TestIdentitySet_Incident2026_07_31(t *testing.T) {
	incident := GitHubConfig{
		AppID:   EnterpriseGitHubAppID,   // 5686
		AppSlug: EnterpriseGitHubAppSlug, // kubestellar-hive-ghe
		APIURL:  "",                      // EMPTY — resolves to api.github.com
		BaseURL: "",                      // EMPTY
	}
	err := RejectIdentitySet(incident)
	if err == nil {
		t.Fatal("the 2026-07-31 incident field set was ACCEPTED — this is the write that produced 404 Integration not found on seven hives")
	}
	if !errors.Is(err, ErrIdentitySetMismatch) {
		t.Errorf("incident rejection did not wrap ErrIdentitySetMismatch: %v", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("rejection text %q does not name the observed failure", err)
	}
}

// TestIdentitySet_RealFleetShapes is the REACHABILITY test. It builds configs in
// the shapes that actually exist on the fleet — empty api_url, empty base_url,
// often an empty app_slug — and asserts the rule still fires when app_id names
// the wrong forge.
//
// If this test can be made to pass by a rule that keys only on api_url/base_url/
// app_slug string markers, the rule is unreachable in production. It cannot: on
// every rejected case below all three of those markers are false or empty.
func TestIdentitySet_RealFleetShapes(t *testing.T) {
	tests := []struct {
		name       string
		gh         GitHubConfig
		wantReject bool
	}{
		{
			// ~41 spokes. The single most important negative case: if this is
			// rejected, the healthy fleet breaks.
			name:       "healthy public spoke, all URLs empty",
			gh:         GitHubConfig{AppID: PublicGitHubAppID, AppSlug: PublicGitHubAppSlug},
			wantReject: false,
		},
		{
			name:       "healthy public spoke, URLs AND slug empty",
			gh:         GitHubConfig{AppID: PublicGitHubAppID},
			wantReject: false,
		},
		{
			// The incident, minus the slug — the slug marker is unavailable, so
			// only an app_id-keyed rule can catch this.
			name:       "GHE app_id on a public spoke, slug and URLs all empty",
			gh:         GitHubConfig{AppID: EnterpriseGitHubAppID},
			wantReject: true,
		},
		{
			// The incident as it landed.
			name:       "GHE app_id and slug, URLs empty",
			gh:         GitHubConfig{AppID: EnterpriseGitHubAppID, AppSlug: EnterpriseGitHubAppSlug},
			wantReject: true,
		},
		{
			// Healthy GHE spokes that carry api_url only (base_url: "") — a real
			// shape for pooled/placeholder GHE hives.
			name:       "healthy GHE spoke, base_url empty",
			gh:         GitHubConfig{AppID: EnterpriseGitHubAppID, AppSlug: EnterpriseGitHubAppSlug, APIURL: EnterpriseGitHubAPIURL},
			wantReject: false,
		},
		{
			// The mirror image: a public app_id on a hive pointed at GHE.
			name:       "public app_id with a GHE api_url",
			gh:         GitHubConfig{AppID: PublicGitHubAppID, APIURL: EnterpriseGitHubAPIURL},
			wantReject: true,
		},
		{
			// A public App wearing a GHE-marked slug. Caught by the pre-existing
			// slugIsGHE rule, which is legitimate in THIS direction: no valid
			// deployment names a public App "*-ghe".
			name:       "public app_id and URLs, enterprise slug",
			gh:         GitHubConfig{AppID: PublicGitHubAppID, AppSlug: EnterpriseGitHubAppSlug},
			wantReject: true,
		},
		{
			// A GHE App wearing a slug with no "ghe" marker. Every marker-based
			// rule is blind to this: slugIsGHE is false, and both URLs agree
			// with each other. Only the app_id -> slug rule catches it, which is
			// why that rule has to key on app_id rather than on string markers.
			name: "GHE app_id under a slug that is not its own",
			gh: GitHubConfig{
				AppID:   EnterpriseGitHubAppID,
				AppSlug: "ibm-hive",
				APIURL:  EnterpriseGitHubAPIURL,
				BaseURL: EnterpriseGitHubBaseURL,
			},
			wantReject: true,
		},
		{
			// The real fleet shape: slug UNSET. ResolvedAppSlug() supplies the
			// default, so an empty slug is not a mismatch and must be accepted —
			// most spokes run exactly this way.
			name: "GHE app_id with slug unset",
			gh: GitHubConfig{
				AppID:   EnterpriseGitHubAppID,
				APIURL:  EnterpriseGitHubAPIURL,
				BaseURL: EnterpriseGitHubBaseURL,
			},
			wantReject: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := RejectIdentitySet(tc.gh)
			if tc.wantReject && err == nil {
				t.Fatalf("%+v was ACCEPTED, want rejected", tc.gh)
			}
			if !tc.wantReject && err != nil {
				t.Fatalf("%+v was REJECTED, want accepted: %v", tc.gh, err)
			}
		})
	}
}

// TestIdentitySet_RuleIsReachableWithoutStringMarkers proves the new rule does
// not depend on any of the three legacy markers. The fixture has an empty
// api_url, an empty base_url and an empty app_slug — so apiIsGHE, baseIsGHE and
// slugIsGHE are ALL false — yet it is the damaged state and must be rejected.
//
// This is the assertion the previous check could not have made.
func TestIdentitySet_RuleIsReachableWithoutStringMarkers(t *testing.T) {
	gh := GitHubConfig{AppID: EnterpriseGitHubAppID} // app_id only; every string field empty

	if containsFold(gh.APIURL, gheAPIURLMarker) {
		t.Fatal("fixture has a GHE api_url — the reachability premise is broken")
	}
	if gh.BaseURL != "" {
		t.Fatal("fixture has a base_url — the reachability premise is broken")
	}
	if containsFold(gh.AppSlug, "ghe") {
		t.Fatal("fixture has a GHE slug — the reachability premise is broken")
	}

	if err := RejectIdentitySet(gh); err == nil {
		t.Fatal("a GHE app_id with every string marker empty was ACCEPTED — the rule is unreachable on real fleet data, which is exactly the defect this replaces")
	}
}

// TestIdentitySet_OneEmptyURLIsNotAContradiction pins the semantics the legacy
// base/api rules got wrong: an EMPTY url is "unset", not "public github.com".
//
// ResolvedBaseURL and HostLabel are explicit that a GHE hive legitimately
// carries only one of the two. While IdentitySetIssues merely rendered a
// dashboard panel, reading a blank as a contradiction was noise; wired to
// RejectIdentitySet it would refuse a GHE App push to every GHE hive that has
// not backfilled both URLs. The rules now require both to be populated before
// comparing them.
func TestIdentitySet_OneEmptyURLIsNotAContradiction(t *testing.T) {
	valid := []GitHubConfig{
		// The vllm-d cluster record: GHE base_url declared, api_url unset.
		{AppID: EnterpriseGitHubAppID, AppSlug: EnterpriseGitHubAppSlug, BaseURL: EnterpriseGitHubBaseURL},
		// Pooled/placeholder GHE hives: GHE api_url declared, base_url unset.
		{AppID: EnterpriseGitHubAppID, AppSlug: EnterpriseGitHubAppSlug, APIURL: EnterpriseGitHubAPIURL},
	}
	for _, gh := range valid {
		if err := RejectIdentitySet(gh); err != nil {
			t.Errorf("%+v was rejected, but one unset URL defers to the other — it is not a forge disagreement: %v", gh, err)
		}
	}

	// Both populated and disagreeing IS a contradiction and must still fire.
	both := GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		BaseURL: EnterpriseGitHubBaseURL,
		APIURL:  DefaultGitHubAPIURL,
	}
	if err := RejectIdentitySet(both); err == nil {
		t.Error("two populated URLs naming different forges were accepted")
	}
}

// TestIdentitySet_UnknownAppIDIsNotJudged guards against the rule becoming a
// gate on deployments this build does not know about. An App ID from a third
// forge, or a self-hosted deployment, carries no forge information and must not
// be treated as a mismatch.
func TestIdentitySet_UnknownAppIDIsNotJudged(t *testing.T) {
	for _, gh := range []GitHubConfig{
		{AppID: 4242424},
		{AppID: 4242424, APIURL: "https://git.example.com/api/v3", BaseURL: "https://git.example.com"},
		{AppID: PlaceholderAppID},
		{AppID: PlaceholderAppID, AppSlug: PublicGitHubAppSlug},
		{Token: "x"}, // token-auth hive, no App at all
	} {
		if err := RejectIdentitySet(gh); err != nil {
			t.Errorf("%+v was rejected, but this build knows nothing about that App: %v", gh, err)
		}
	}
}

// TestIdentitySet_ByteExactDefaults pins the identity constants against the
// resolver defaults they must agree with. A drift here silently un-reaches the
// rule: forgeOfAppID compares against DefaultGitHubBaseURL's host.
func TestIdentitySet_ByteExactDefaults(t *testing.T) {
	if got := (GitHubConfig{AppID: PublicGitHubAppID}).ResolvedAPIURL(); got != DefaultGitHubAPIURL {
		t.Errorf("ResolvedAPIURL on an empty api_url = %q, want %q", got, DefaultGitHubAPIURL)
	}
	if got := (GitHubConfig{AppID: PublicGitHubAppID}).ResolvedBaseURL(); got != DefaultGitHubBaseURL {
		t.Errorf("ResolvedBaseURL on an empty base_url = %q, want %q", got, DefaultGitHubBaseURL)
	}
	if got := (GitHubConfig{AppID: PublicGitHubAppID}).ResolvedAppSlug(); got != PublicGitHubAppSlug {
		t.Errorf("ResolvedAppSlug on an empty app_slug = %q, want %q", got, PublicGitHubAppSlug)
	}
	if forgeOfAppID(PublicGitHubAppID) != "github.com" {
		t.Errorf("forgeOfAppID(public) = %q, want github.com", forgeOfAppID(PublicGitHubAppID))
	}
	if forgeOfAppID(EnterpriseGitHubAppID) != "github.ibm.com" {
		t.Errorf("forgeOfAppID(enterprise) = %q, want github.ibm.com", forgeOfAppID(EnterpriseGitHubAppID))
	}
	if forgeOfAppID(0) != "" || forgeOfAppID(PlaceholderAppID) != "" {
		t.Error("forgeOfAppID must claim nothing for zero or the placeholder sentinel")
	}
}

// TestRejectIdentitySet_CleanConfigReturnsNil is the contract the write-path
// callers depend on: no issues means no error, so a healthy push applies.
func TestRejectIdentitySet_CleanConfigReturnsNil(t *testing.T) {
	if err := RejectIdentitySet(GitHubConfig{AppID: PublicGitHubAppID, AppSlug: PublicGitHubAppSlug}); err != nil {
		t.Fatalf("healthy public identity rejected: %v", err)
	}
	if err := RejectIdentitySet(GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		AppSlug: EnterpriseGitHubAppSlug,
		APIURL:  EnterpriseGitHubAPIURL,
		BaseURL: EnterpriseGitHubBaseURL,
	}); err != nil {
		t.Fatalf("healthy enterprise identity rejected: %v", err)
	}
}
