package config

import (
	"strings"
	"testing"
)

// TestForgeDerivationIsByteExactWithTodaysDefaults is the migration's no-op
// proof. A healthy public hive that gains `forge: github.com` must resolve to
// EXACTLY the values it resolves to today with every field empty — if these
// drift by a byte, the migration silently rewrites every healthy spoke.
func TestForgeDerivationIsByteExactWithTodaysDefaults(t *testing.T) {
	today := GitHubConfig{} // today's healthy public spoke: everything implicit
	migrated := GitHubConfig{Forge_: ForgePublic}

	if got, want := migrated.ResolvedAPIURL(), today.ResolvedAPIURL(); got != want {
		t.Errorf("api_url: migrated %q != today %q", got, want)
	}
	if got, want := migrated.ResolvedBaseURL(), today.ResolvedBaseURL(); got != want {
		t.Errorf("base_url: migrated %q != today %q", got, want)
	}
	if got, want := migrated.ResolvedAppSlug(), today.ResolvedAppSlug(); got != want {
		t.Errorf("app_slug: migrated %q != today %q", got, want)
	}
	// And those values are the documented constants, pinned literally so a
	// change to the defaults cannot pass by moving both sides together.
	if got := migrated.ResolvedAPIURL(); got != "https://api.github.com" {
		t.Errorf("api_url = %q, want https://api.github.com", got)
	}
	if got := migrated.ResolvedBaseURL(); got != "https://github.com" {
		t.Errorf("base_url = %q, want https://github.com", got)
	}
	if got := migrated.ResolvedAppSlug(); got != "kubestellar-hive" {
		t.Errorf("app_slug = %q, want kubestellar-hive", got)
	}
	if migrated.IsGHE() {
		t.Error("a github.com hive must not report IsGHE")
	}
}

// TestForgeIdentityAllSixteenCombinations is the test whose absence allowed the
// 2026-07-31 incident. The four identity fields have two valid settings each
// (public-shaped or GHE-shaped); of the 16 combinations exactly 2 are coherent
// and 14 are mixed forges that must be reported.
//
// The forge under test is declared EXPLICITLY on each case rather than inferred,
// because inference is what the incident exploited: with `forge` absent, an
// empty api_url reads as "unset" instead of "public", and the mixture becomes
// invisible. Anchoring against a declared forge is the whole fix.
func TestForgeIdentityAllSixteenCombinations(t *testing.T) {
	type variant struct {
		appID   int64
		appSlug string
		apiURL  string
		baseURL string
	}
	pub := variant{PublicGitHubAppID, PublicGitHubAppSlug, "", ""}
	ghe := variant{GHEIBMAppID, GHEIBMAppSlug, GHEIBMAPIURL, GHEIBMBaseURL}

	// isGHE[i] selects which forge's value each of the 4 fields takes.
	for mask := 0; mask < 16; mask++ {
		appIDGHE := mask&1 != 0
		slugGHE := mask&2 != 0
		apiGHE := mask&4 != 0
		baseGHE := mask&8 != 0

		pick := func(useGHE bool, p, g string) string {
			if useGHE {
				return g
			}
			return p
		}
		gh := GitHubConfig{
			AppSlug: pick(slugGHE, pub.appSlug, ghe.appSlug),
			APIURL:  pick(apiGHE, pub.apiURL, ghe.apiURL),
			BaseURL: pick(baseGHE, pub.baseURL, ghe.baseURL),
		}
		if appIDGHE {
			gh.AppID = ghe.appID
		} else {
			gh.AppID = pub.appID
		}

		// Declare the forge the set CLAIMS to be on: GHE if any field carries a
		// GHE value, else public. This is the anchor the pusher does not control.
		forge := ForgePublic
		if appIDGHE || slugGHE || apiGHE || baseGHE {
			forge = ForgeGHEIBM
		}
		gh.Forge_ = forge

		// COHERENCE RULE. The public forge's URL fields are the EMPTY STRING —
		// that is its canonical on-disk representation (see forgeIdentities).
		// Empty means "derive me from the forge", NOT "public", so an empty URL
		// beside a GHE declaration is an unset field awaiting derivation rather
		// than a contradiction. Only a field that NAMES the wrong forge is a
		// mismatch.
		//
		// This is deliberate, and it is what keeps the migration a no-op:
		// requiring every URL to be populated would flag the ~41 fleet spokes
		// running on implicit empty URLs.
		coherent := false
		if forge == ForgePublic {
			// Every field took its public value, and for the URLs that is empty.
			coherent = true
		} else {
			// On GHE, app_id and app_slug must both be the GHE ones (they have no
			// "unset" spelling here — the public value is a real, wrong value).
			// The URLs may be GHE-valued or empty.
			coherent = appIDGHE && slugGHE
		}

		issues := gh.ForgeIdentityMismatches()
		if coherent && len(issues) != 0 {
			t.Errorf("mask %04b (coherent on %s) was rejected: %v\n  config: %+v",
				mask, forge, issues, gh)
		}
		if !coherent && len(issues) == 0 {
			t.Errorf("mask %04b is a MIXED forge set on declared forge %s and was ACCEPTED: %+v",
				mask, forge, gh)
		}
	}
}

// TestForgeURLFieldsMustMatchDeclaredForge covers the case the 16-combination
// table structurally CANNOT reach.
//
// In that table the declared forge is inferred from the fields, so a GHE URL
// always makes the forge GHE and can never contradict it — the api_url/base_url
// checks are unreachable there. (Confirmed by mutation: deleting both checks
// left the table green.) The contradiction only exists when the forge is
// declared INDEPENDENTLY of the URLs, which is precisely what `forge:` makes
// possible and what a stale URL surviving a forge move looks like.
func TestForgeURLFieldsMustMatchDeclaredForge(t *testing.T) {
	cases := []struct {
		name      string
		gh        GitHubConfig
		wantField string
	}{
		{
			// A hive moved to public github.com whose GHE api_url was left behind.
			name: "stale GHE api_url on a public hive",
			gh: GitHubConfig{
				Forge_: ForgePublic, AppID: PublicGitHubAppID,
				AppSlug: PublicGitHubAppSlug, APIURL: GHEIBMAPIURL,
			},
			wantField: "api_url",
		},
		{
			name: "stale GHE base_url on a public hive",
			gh: GitHubConfig{
				Forge_: ForgePublic, AppID: PublicGitHubAppID,
				AppSlug: PublicGitHubAppSlug, BaseURL: GHEIBMBaseURL,
			},
			wantField: "base_url",
		},
		{
			// The reverse: a hive declared GHE still pointing at public GitHub.
			name: "public api_url on a GHE hive",
			gh: GitHubConfig{
				Forge_: ForgeGHEIBM, AppID: GHEIBMAppID,
				AppSlug: GHEIBMAppSlug, APIURL: DefaultGitHubAPIURL,
			},
			wantField: "api_url",
		},
		{
			name: "public base_url on a GHE hive",
			gh: GitHubConfig{
				Forge_: ForgeGHEIBM, AppID: GHEIBMAppID,
				AppSlug: GHEIBMAppSlug, BaseURL: DefaultGitHubBaseURL,
			},
			wantField: "base_url",
		},
		{
			// A URL naming a THIRD forge, on a hive declared public.
			name: "unrelated GHE host in api_url",
			gh: GitHubConfig{
				Forge_: ForgePublic, AppID: PublicGitHubAppID,
				AppSlug: PublicGitHubAppSlug, APIURL: "https://github.cisco.com/api/v3",
			},
			wantField: "api_url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := tc.gh.ForgeIdentityMismatches()
			if len(issues) == 0 {
				t.Fatalf("a %s naming a different forge than %s must be reported, got no issues for %+v",
					tc.wantField, tc.gh.Forge_, tc.gh)
			}
			if !strings.Contains(strings.Join(issues, " "), tc.wantField) {
				t.Errorf("the report must name %s, got %v", tc.wantField, issues)
			}
		})
	}

	// The matching NEGATIVE: URLs that agree with the declared forge, spelled
	// every way the fleet actually spells them, must stay silent.
	for _, ok := range []GitHubConfig{
		{Forge_: ForgePublic, AppID: PublicGitHubAppID, AppSlug: PublicGitHubAppSlug,
			APIURL: DefaultGitHubAPIURL, BaseURL: DefaultGitHubBaseURL},
		{Forge_: ForgeGHEIBM, AppID: GHEIBMAppID, AppSlug: GHEIBMAppSlug,
			APIURL: GHEIBMAPIURL, BaseURL: GHEIBMBaseURL},
		// Trailing slash / mixed case must normalise, not trip the check.
		{Forge_: ForgeGHEIBM, AppID: GHEIBMAppID, AppSlug: GHEIBMAppSlug,
			BaseURL: "https://GitHub.IBM.com/"},
	} {
		if issues := ok.ForgeIdentityMismatches(); len(issues) != 0 {
			t.Errorf("a coherent identity was reported as mismatched: %v\n  config: %+v", issues, ok)
		}
	}
}

// TestIncidentRegression2026_07_31 pins the exact field set that took down
// seven hives. The hub pushed the GHE app_id and app_slug onto hives whose
// elected forge was github.com, leaving api_url/base_url empty — so token
// creation went to api.github.com with a GHE App ID and returned
// "404 Integration not found", and agents authored as kubestellar-hive-ghe[bot]
// on public repos.
func TestIncidentRegression2026_07_31(t *testing.T) {
	damaged := GitHubConfig{
		Forge_:         ForgePublic, // the hive's election, per hub meta.json github_host
		AppID:          GHEIBMAppID, // 5686 — pushed from the vllm-d cluster entry
		AppSlug:        GHEIBMAppSlug,
		APIURL:         "", // never pushed — this is the field the failure split on
		BaseURL:        "",
		InstallationID: 149922991,
	}
	issues := damaged.ForgeIdentityMismatches()
	if len(issues) == 0 {
		t.Fatal("the 2026-07-31 identity set was ACCEPTED — this is the exact regression that must never return")
	}
	joined := strings.Join(issues, " | ")
	if !strings.Contains(joined, "app_id") {
		t.Errorf("the report must name app_id (the field that caused 404 Integration not found), got: %s", joined)
	}
	if !strings.Contains(joined, "app_slug") {
		t.Errorf("the report must name app_slug (the field that caused ghe[bot] authorship), got: %s", joined)
	}

	// The undetected variant: app_id ALONE is moved and the slug stays public.
	// IdentitySetIssues cannot see this — it compares the four fields against
	// each other and a lone GHE app_id contradicts nothing (verified: it
	// returns zero issues). Anchoring on the declared forge catches it.
	appIDOnly := GitHubConfig{Forge_: ForgePublic, AppID: GHEIBMAppID, AppSlug: PublicGitHubAppSlug}
	if len(IdentitySetIssues(appIDOnly)) != 0 {
		t.Log("note: IdentitySetIssues now detects the app_id-only shape; it did not when this test was written")
	}
	if len(appIDOnly.ForgeIdentityMismatches()) == 0 {
		t.Error("a GHE app_id alone on a public hive must be reported — it is the shape that returns 404 Integration not found")
	}
}

// TestForgeDualReadWindow proves a spoke that has NOT been backfilled with
// `forge:` resolves exactly as it does today. Nothing may break on the ~41
// spokes still running an implicit empty api_url.
func TestForgeDualReadWindow(t *testing.T) {
	cases := []struct {
		name string
		gh   GitHubConfig
		want string
	}{
		{"empty config infers public", GitHubConfig{}, ForgePublic},
		{"explicit public api_url", GitHubConfig{APIURL: "https://api.github.com"}, ForgePublic},
		{"GHE api_url only (pooled placeholder shape)",
			GitHubConfig{APIURL: GHEIBMAPIURL}, ForgeGHEIBM},
		{"GHE base_url only", GitHubConfig{BaseURL: GHEIBMBaseURL}, ForgeGHEIBM},
		{"both GHE URLs", GitHubConfig{APIURL: GHEIBMAPIURL, BaseURL: GHEIBMBaseURL}, ForgeGHEIBM},
		{"explicit forge wins over empty URLs", GitHubConfig{Forge_: ForgeGHEIBM}, ForgeGHEIBM},
		{"explicit forge normalises a URL spelling",
			GitHubConfig{Forge_: "https://github.ibm.com/"}, ForgeGHEIBM},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.gh.Forge(); got != tc.want {
				t.Errorf("Forge() = %q, want %q", got, tc.want)
			}
			// A hive with no explicit forge must never be reported as mismatched:
			// inference derives the forge FROM those fields, so they agree by
			// construction. Flagging them would alarm the whole un-backfilled fleet.
			if tc.gh.Forge_ == "" {
				if issues := tc.gh.ForgeIdentityMismatches(); len(issues) != 0 {
					t.Errorf("un-backfilled spoke reported as mismatched: %v", issues)
				}
			}
		})
	}
}

// TestResolvedAppIDCompletesTheResolverSet checks the new accessor mirrors
// ResolvedAppSlug: explicit wins, else derive from forge.
func TestResolvedAppIDCompletesTheResolverSet(t *testing.T) {
	if got := (GitHubConfig{Forge_: ForgePublic}).ResolvedAppID(); got != PublicGitHubAppID {
		t.Errorf("public derived app id = %d, want %d", got, PublicGitHubAppID)
	}
	if got := (GitHubConfig{Forge_: ForgeGHEIBM}).ResolvedAppID(); got != GHEIBMAppID {
		t.Errorf("GHE derived app id = %d, want %d", got, GHEIBMAppID)
	}
	// Explicit app_id wins — a third-forge hive keeps its own App.
	const thirdForgeApp int64 = 4242
	if got := (GitHubConfig{Forge_: ForgePublic, AppID: thirdForgeApp}).ResolvedAppID(); got != thirdForgeApp {
		t.Errorf("explicit app id = %d, want %d", got, thirdForgeApp)
	}
	// The placeholder sentinel must survive resolution: HasApp/IsPlaceholderApp
	// depend on seeing it rather than a derived real App ID.
	ph := GitHubConfig{Forge_: ForgePublic, AppID: PlaceholderAppID}
	if got := ph.ResolvedAppID(); got != PlaceholderAppID {
		t.Errorf("placeholder app id = %d, want the sentinel %d preserved", got, PlaceholderAppID)
	}
	if len(ph.ForgeIdentityMismatches()) != 0 {
		t.Error("the placeholder sentinel is forge-neutral and must not be reported as a mismatch")
	}
	// An unknown (third) forge derives nothing rather than guessing.
	if got := (GitHubConfig{Forge_: "github.example.com"}).ResolvedAppID(); got != 0 {
		t.Errorf("unknown forge derived app id = %d, want 0 (no guess)", got)
	}
	if issues := (GitHubConfig{Forge_: "github.example.com", AppID: thirdForgeApp, AppSlug: "custom"}).ForgeIdentityMismatches(); len(issues) != 0 {
		t.Errorf("a third-forge hive must keep explicit fields authoritative, got %v", issues)
	}
}
