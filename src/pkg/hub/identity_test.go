package hub

import (
	"strings"
	"testing"
)

// GitHub identity consistency.
//
// The live regression these pin: a cluster-config change pushed a GHE app_id to
// a set of hives WITHOUT api_url. The spokes ended up with a GHE App pointed at
// api.github.com and every token request failed with
// "404 Integration not found" on hives that had been working an hour earlier.
//
// The risk runs both ways, so the tests are organised that way:
//   - failing to DETECT a split identity leaves hives broken and invisible;
//   - firing on a healthy public-GitHub hive (empty api_url is correct there)
//     makes the signal noise across ~48 hives and it gets ignored.

// ─────────────────────────────────────────────────────────────────────────
// DETECT: inconsistent identities must be reported
// ─────────────────────────────────────────────────────────────────────────

// TestGHESlugWithEmptyAPIURLIsDetected is the exact live failure.
func TestGHESlugWithEmptyAPIURLIsDetected(t *testing.T) {
	issues := IdentitySetIssues(IdentitySet{
		AppID:   5686,
		AppSlug: "kubestellar-hive-ghe",
		APIURL:  "",
	})
	if len(issues) == 0 {
		t.Fatal("no issues for a GHE slug with an empty api_url — this is the state " +
			"that 404s every token request")
	}
	joined := strings.Join(issues, " ")
	if !strings.Contains(joined, "api.github.com") {
		t.Errorf("issue text %q does not explain that an empty api_url defaults to "+
			"api.github.com, which is the cause an operator needs", joined)
	}
}

// TestGHESlugWithPublicAPIURLIsDetected: explicitly public, not merely empty.
func TestGHESlugWithPublicAPIURLIsDetected(t *testing.T) {
	issues := IdentitySetIssues(IdentitySet{
		AppID:   5686,
		AppSlug: "kubestellar-hive-ghe",
		APIURL:  "https://api.github.com",
	})
	if len(issues) == 0 {
		t.Fatal("no issues for a GHE slug pointed at api.github.com")
	}
}

// TestBaseURLDisagreeingWithAPIURLIsDetected covers both directions, since
// base_url is never pushed and can be left behind by an api_url move.
func TestBaseURLDisagreeingWithAPIURLIsDetected(t *testing.T) {
	cases := []struct {
		name    string
		set     IdentitySet
		wantSub string
	}{
		{
			name:    "GHE base, public api",
			set:     IdentitySet{AppID: 5686, BaseURL: "https://github.ibm.com", APIURL: "https://api.github.com"},
			wantSub: "same forge",
		},
		{
			name:    "GHE api, public base",
			set:     IdentitySet{AppID: 5686, BaseURL: "https://github.com", APIURL: "https://github.ibm.com/api/v3"},
			wantSub: "same forge",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues := IdentitySetIssues(tc.set)
			if len(issues) == 0 {
				t.Fatalf("no issues for %s", tc.name)
			}
			if !strings.Contains(strings.Join(issues, " "), tc.wantSub) {
				t.Errorf("issues %v do not mention %q", issues, tc.wantSub)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────
// DO NOT FIRE: healthy identities must stay silent
// ─────────────────────────────────────────────────────────────────────────

// TestConsistentPublicIsClean is the majority of the fleet. Public GitHub
// legitimately has an EMPTY api_url, so a naive "api_url must be set" check
// would fire on ~48 hives and be ignored.
func TestConsistentPublicIsClean(t *testing.T) {
	if issues := IdentitySetIssues(IdentitySet{
		AppID:   3568013,
		AppSlug: "kubestellar-hive",
		APIURL:  "",
	}); len(issues) != 0 {
		t.Errorf("a healthy public-GitHub identity reported issues: %v", issues)
	}
}

func TestConsistentGHEIsClean(t *testing.T) {
	if issues := IdentitySetIssues(IdentitySet{
		AppID:   5686,
		AppSlug: "kubestellar-hive-ghe",
		APIURL:  "https://github.ibm.com/api/v3",
		BaseURL: "https://github.ibm.com",
	}); len(issues) != 0 {
		t.Errorf("a healthy GHE identity reported issues: %v", issues)
	}
}

// TestNoAppConfiguredIsClean: a token-auth hive has no identity to be
// inconsistent about.
func TestNoAppConfiguredIsClean(t *testing.T) {
	if issues := IdentitySetIssues(IdentitySet{APIURL: "https://github.ibm.com/api/v3"}); len(issues) != 0 {
		t.Errorf("a hive with no App reported identity issues: %v", issues)
	}
}

// TestSilenceIsNotAMismatch: a spoke too old to report app_slug/base_url must
// read as UNKNOWN, never as a split identity. Inferring breakage from silence
// is exactly the silent-failure class this work exists to end.
func TestSilenceIsNotAMismatch(t *testing.T) {
	if issues := IdentitySetIssues(IdentitySet{AppID: 5686}); len(issues) != 0 {
		t.Errorf("an under-reporting spoke was flagged as inconsistent: %v", issues)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// REFUSE: an inconsistent push must not be armed
// ─────────────────────────────────────────────────────────────────────────

// TestValidateIdentityPushRefusesSplit is the guard the cluster-config path
// lacked. #2360's forge endpoint already refuses rather than half-applies.
func TestValidateIdentityPushRefusesSplit(t *testing.T) {
	reason := ValidateIdentityPush(IdentitySet{
		AppID:   5686,
		AppSlug: "kubestellar-hive-ghe",
		APIURL:  "",
	})
	if reason == "" {
		t.Fatal("ValidateIdentityPush allowed a GHE App with an empty api_url — " +
			"the push that broke live hives")
	}
	if !strings.Contains(reason, "refusing") {
		t.Errorf("reason %q should state that the push is refused", reason)
	}
}

func TestValidateIdentityPushAllowsConsistent(t *testing.T) {
	if reason := ValidateIdentityPush(IdentitySet{
		AppID:   5686,
		AppSlug: "kubestellar-hive-ghe",
		APIURL:  "https://github.ibm.com/api/v3",
		BaseURL: "https://github.ibm.com",
	}); reason != "" {
		t.Errorf("a consistent GHE push was refused: %s", reason)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// CONFIRM: delivery is only complete when the spoke reports the set back
// ─────────────────────────────────────────────────────────────────────────

// TestIdentityDeliveredRequiresFullMatch: before the app_slug/installation_id
// read-back fields, these components were structurally unconfirmable — the hub
// could push them and never learn whether they landed.
func TestIdentityDeliveredRequiresFullMatch(t *testing.T) {
	want := &PendingAppIdentity{AppID: 5686, AppSlug: "kubestellar-hive-ghe", InstallationID: 99}

	if identityDelivered(want, &HeartbeatPayload{GitHubAppID: 5686}) {
		t.Error("delivery confirmed on app_id alone; the slug and installation id " +
			"were never reported and may not have landed")
	}
	if identityDelivered(want, &HeartbeatPayload{
		GitHubAppID: 5686, GitHubAppSlug: "kubestellar-hive-ghe",
	}) {
		t.Error("delivery confirmed without the installation id being reported back")
	}
	if !identityDelivered(want, &HeartbeatPayload{
		GitHubAppID: 5686, GitHubAppSlug: "kubestellar-hive-ghe", GitHubInstallationID: 99,
	}) {
		t.Error("a spoke reporting the complete requested identity was not accepted")
	}
}

// TestIdentityDeliveredIgnoresUnrequestedFields: a zero installation_id means
// "leave the spoke's alone", so it is not something to confirm. Requiring it
// would leave the latch armed forever.
func TestIdentityDeliveredIgnoresUnrequestedFields(t *testing.T) {
	want := &PendingAppIdentity{AppID: 5686, AppSlug: "kubestellar-hive-ghe"}
	if !identityDelivered(want, &HeartbeatPayload{
		GitHubAppID: 5686, GitHubAppSlug: "kubestellar-hive-ghe", GitHubInstallationID: 12345,
	}) {
		t.Error("an unrequested installation id blocked confirmation")
	}
}

// TestIdentityDeliveredSlugIsCaseInsensitive: GitHub slugs are case-insensitive,
// so a case difference must not hold the delivery open forever.
func TestIdentityDeliveredSlugIsCaseInsensitive(t *testing.T) {
	want := &PendingAppIdentity{AppID: 5686, AppSlug: "KubeStellar-Hive-GHE"}
	if !identityDelivered(want, &HeartbeatPayload{
		GitHubAppID: 5686, GitHubAppSlug: "kubestellar-hive-ghe",
	}) {
		t.Error("a case-different slug was treated as unconfirmed")
	}
}

// TestSpokeIdentityFromPayloadCarriesTheWholeSet: without this the hub cannot
// see a half-applied identity at all.
func TestSpokeIdentityFromPayloadCarriesTheWholeSet(t *testing.T) {
	got := SpokeIdentityFromPayload(&HeartbeatPayload{
		GitHubAppID:          5686,
		GitHubAppSlug:        "kubestellar-hive-ghe",
		GitHubInstallationID: 146551814,
		GitHubAPIURL:         "",
		GitHubBaseURL:        "",
	})
	if got.AppID != 5686 || got.AppSlug != "kubestellar-hive-ghe" || got.InstallationID != 146551814 {
		t.Fatalf("identity not carried through: %+v", got)
	}
	// And that reconstructed set must be recognised as broken — this is the
	// live state of the affected hives.
	if len(IdentitySetIssues(got)) == 0 {
		t.Error("the reconstructed live-broken identity was not flagged")
	}
}
