package hub

// Tests for the OIDC identity DISPLAY fix: non-GitHub users (IBMid, Google,
// Microsoft) rendering as their human name instead of a raw opaque provider
// subject ("ibmid:5500087VJB", "google:1078…", "microsoft:AAAA…") wherever
// access identities are listed.
//
// This is presentation only — the identity KEY (SaaSUser.GitHubUsername, the
// value used for allowlist matching, RejectIdentitySet, and grants) is never
// touched by anything here. Every case below asserts DisplayLabel/the wire
// map is derived FROM the raw key, never that the raw key itself changed.

import (
	"strings"
	"testing"
)

// TestAccessForHiveDisplayLabelPrecedence pins the precedence
// provisionRequestUserIdentity already documents, reused verbatim by
// accessForHive: linked GitHub login → recognizable GitHub login → email →
// DisplayName → FullName → raw key. accessForHive must not invent a second,
// competing resolver.
func TestAccessForHiveDisplayLabelPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		user     SaaSUser
		wantName string
		wantProv string
	}{
		{
			name: "github user with no extra claims falls back to the login itself",
			user: SaaSUser{
				GitHubUsername: "clubanderson",
				Hives:          map[string]string{"h1": "owner"},
			},
			wantName: "clubanderson",
			wantProv: "github",
		},
		{
			name: "ibmid user with an OIDC display name shows the name",
			user: SaaSUser{
				GitHubUsername: "ibmid:5500087VJB",
				Provider:       "ibmid",
				DisplayName:    "Jane Doe",
				Hives:          map[string]string{"h1": "read"},
			},
			wantName: "Jane Doe",
			wantProv: "ibmid",
		},
		{
			name: "google user with an OIDC display name shows the name",
			user: SaaSUser{
				GitHubUsername: "google:107812345678901234567",
				Provider:       "google",
				DisplayName:    "Priya Patel",
				Hives:          map[string]string{"h1": "read"},
			},
			wantName: "Priya Patel",
			wantProv: "google",
		},
		{
			name: "microsoft user with an OIDC display name shows the name",
			user: SaaSUser{
				GitHubUsername: "microsoft:AAAABBBBCCCCDDDD",
				Provider:       "microsoft",
				DisplayName:    "Sam Lee",
				Hives:          map[string]string{"h1": "read"},
			},
			wantName: "Sam Lee",
			wantProv: "microsoft",
		},
		{
			name: "no display name but email claim falls back to email",
			user: SaaSUser{
				GitHubUsername: "ibmid:6600099XYZ",
				Provider:       "ibmid",
				Email:          "kim@example.com",
				Hives:          map[string]string{"h1": "read"},
			},
			wantName: "kim@example.com",
			wantProv: "ibmid",
		},
		{
			name: "no display name but admin-entered full name falls back to it",
			user: SaaSUser{
				GitHubUsername: "google:99988877766655544433",
				Provider:       "google",
				FullName:       "Operator Entered Name",
				Hives:          map[string]string{"h1": "read"},
			},
			wantName: "Operator Entered Name",
			wantProv: "google",
		},
		{
			name: "linked github login wins over everything for a non-github primary",
			user: SaaSUser{
				GitHubUsername:    "ibmid:7700011AAA",
				Provider:          "ibmid",
				DisplayName:       "Should Not Win",
				LinkedGitHubLogin: "realghlogin",
				Hives:             map[string]string{"h1": "read"},
			},
			wantName: "realghlogin",
			wantProv: "ibmid",
		},
		{
			// CRITICAL: no human name is known anywhere — never logged in, or the
			// provider returned no name claim. The row MUST still render the raw
			// key, never blank/undefined.
			name: "no name anywhere falls back cleanly to the raw key",
			user: SaaSUser{
				GitHubUsername: "microsoft:NONAMECLAIM0000",
				Provider:       "microsoft",
				Hives:          map[string]string{"h1": "read"},
			},
			wantName: "microsoft:NONAMECLAIM0000",
			wantProv: "microsoft",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			access := accessForHive("h1", []SaaSUser{c.user}, false)
			if len(access) != 1 {
				t.Fatalf("access rows = %d, want 1", len(access))
			}
			row := access[0]
			// The identity KEY must never change — this is display only.
			if row.Username != c.user.GitHubUsername {
				t.Fatalf("Username = %q, want unchanged raw key %q", row.Username, c.user.GitHubUsername)
			}
			if row.DisplayLabel != c.wantName {
				t.Errorf("DisplayLabel = %q, want %q", row.DisplayLabel, c.wantName)
			}
			if row.Provider != c.wantProv {
				t.Errorf("Provider = %q, want %q", row.Provider, c.wantProv)
			}
		})
	}
}

// TestAuthorizedUserNamesOmitsRawKeyFallback asserts the heartbeat's cosmetic
// name map only carries an entry when a friendlier name than the raw key was
// actually found — a key with nothing better maps to nothing, so the spoke's
// own "no entry means show the raw key" fallback kicks in uniformly instead
// of the hub sending a redundant self-referential mapping.
func TestAuthorizedUserNamesOmitsRawKeyFallback(t *testing.T) {
	withTempSaaSDirs(t)
	putHiveOwnedBy(t, "h1", "owner1")
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "owner1",
		Hives:          map[string]string{"h1": "owner"},
	}); err != nil {
		t.Fatalf("saveSaaSUser owner1: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "ibmid:5500087VJB",
		Provider:       "ibmid",
		DisplayName:    "Jane Doe",
		Hives:          map[string]string{"h1": "read"},
	}); err != nil {
		t.Fatalf("saveSaaSUser ibmid user: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{
		GitHubUsername: "google:NONAMECLAIM",
		Provider:       "google",
		Hives:          map[string]string{"h1": "read"},
	}); err != nil {
		t.Fatalf("saveSaaSUser google user: %v", err)
	}

	users, names := authorizedUsersAndNamesForHiveID("h1")

	foundOwner, foundJane, foundGoogle := false, false, false
	for _, e := range users {
		switch {
		case strings.HasPrefix(e, "owner1:"):
			foundOwner = true
		case strings.HasPrefix(e, "ibmid:5500087VJB:"):
			foundJane = true
		case strings.HasPrefix(e, "google:NONAMECLAIM:"):
			foundGoogle = true
		}
	}
	if !foundOwner || !foundJane || !foundGoogle {
		t.Fatalf("authorized users list missing expected entries: %v", users)
	}

	if got := names["ibmid:5500087VJB"]; got != "Jane Doe" {
		t.Errorf(`names["ibmid:5500087VJB"] = %q, want "Jane Doe"`, got)
	}
	if _, ok := names["google:NONAMECLAIM"]; ok {
		t.Errorf("names has an entry for a key with no known human name; want it absent so the spoke falls back to the raw key")
	}
	// A GitHub owner with no display/full name/email is also "native" — no
	// entry expected, same reasoning.
	if _, ok := names["owner1"]; ok {
		t.Errorf("names has a self-referential entry for a plain GitHub login with no extra claims")
	}
}

// TestAuthorizedUsersAndNamesNoSaaSRecord pins the nil contract: a hive with
// no SaaS record gets nil for both return values, so a non-hosted spoke's own
// locally configured allowlist (and its own AuthorizedUserNames, if any) is
// left completely untouched.
func TestAuthorizedUsersAndNamesNoSaaSRecord(t *testing.T) {
	withTempSaaSDirs(t)
	users, names := authorizedUsersAndNamesForHiveID("no-such-hive")
	if users != nil {
		t.Errorf("users = %v, want nil", users)
	}
	if names != nil {
		t.Errorf("names = %v, want nil", names)
	}
}

// TestManageAccessRowsRenderFriendlyNameAndKeepRawKeyDiscoverable pins the
// Manage Access modal's row markup: it must read the server-resolved
// display_label/provider fields (not re-derive a name from the raw key
// client-side), and the raw key must stay reachable (title attribute + a
// muted secondary line) even when a friendly name is shown.
func TestManageAccessRowsRenderFriendlyNameAndKeepRawKeyDiscoverable(t *testing.T) {
	if !strings.Contains(dashboardHTML, "var hasFriendlyName = !!(u.display_label && u.display_label !== u.username)") {
		t.Error("Manage Access rows must resolve friendliness from the server-provided display_label, not re-derive one client-side")
	}
	if !strings.Contains(dashboardHTML, `title="Auth key">' + esc(u.username) + '</span>'`) {
		t.Error("the raw auth key must stay visible as a muted secondary line when a friendly name is shown")
	}
	if !strings.Contains(dashboardHTML, "var primaryLabel = hasFriendlyName ? u.display_label : u.username;") {
		t.Error("a row with no known friendly name must still render the raw key, never blank/undefined")
	}
	// The provider icon must come from the server's classification when
	// present, falling back to the existing prefix-based classifier — never
	// hard-coded to "github".
	if !strings.Contains(dashboardHTML, "var provider = u.provider || identityProviderFromKey(u.username);") {
		t.Error("Manage Access rows must classify the provider icon from u.provider (falling back to identityProviderFromKey), not assume github")
	}
}

// TestInlineAccessAvatarHandlesNonGitHubIdentity pins the third surface: the
// "My Hives" co-member face avatars (hiveAccessAvatars / inlineAccessAvatar).
// A non-GitHub key must not get the plain linkedAvatar, which always builds a
// github.com/<key>.png URL — wrong host for an ibmid/google/microsoft
// subject, so it 404s and (worse) links to whatever github.com account
// happens to sit at that URL-shaped path.
func TestInlineAccessAvatarHandlesNonGitHubIdentity(t *testing.T) {
	if !strings.Contains(dashboardHTML, "var provider = a.provider || identityProviderFromKey(uname);") {
		t.Error("inlineAccessAvatar must classify the provider from a.provider (falling back to identityProviderFromKey)")
	}
	if !strings.Contains(dashboardHTML, "provider === 'github'\n        ? linkedAvatar(uname, INLINE_ACCESS_AVATAR_PX, accessAvatarTitle(a), extraStyle)") {
		t.Error("inlineAccessAvatar must only use the GitHub-profile-linked avatar for provider === 'github'")
	}
	if !strings.Contains(dashboardHTML, "if (label && label !== uname) lines.splice(0, 0, label);") {
		t.Error("accessAvatarTitle must show the resolved friendly name ahead of the raw key when one is known")
	}
}
