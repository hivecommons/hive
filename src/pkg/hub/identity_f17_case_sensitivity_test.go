package hub

import "testing"

// AUDIT F17: OIDC subject case-folding permitted cross-account confusion.
//
// canonicalEqual used to be a flat strings.EqualFold over the whole canonical
// id. The comment justified it with "two subs that differ only by case are not
// issued" — true for GitHub, NOT guaranteed for any OIDC provider, whose `sub`
// is a case-SENSITIVE opaque string. Under the fold, an operator-run IdP that
// issues (or lets a user self-register) `custom:AbC` handed that user everything
// owned by `custom:abc`.
//
// TestF17OIDCSubjectsCompareExactly is the regression.
// TestF17GitHubLoginsStillFold is the POSITIVE CONTROL: a comparison that simply
// returned false for everything, or that dropped the legacy shim, would "pass"
// the regression while breaking every real ownership check and locking every
// pre-migration user out of their own hives.

// TestF17OIDCSubjectsCompareExactly pins the strict lane: for every non-GitHub
// provider, two subjects differing only by case are DIFFERENT users.
func TestF17OIDCSubjectsCompareExactly(t *testing.T) {
	// Every non-GitHub provider in identityProviders. A base64url `sub` (which
	// Entra, Okta and Keycloak all emit) differs by case routinely, so this is
	// not a theoretical shape.
	for _, provider := range []string{"google", "ibmid", "redhat", "custom", "microsoft"} {
		t.Run(provider, func(t *testing.T) {
			owner := provider + ":AbCdEf"
			attacker := provider + ":abcdef"
			if canonicalEqual(owner, attacker) {
				t.Errorf("canonicalEqual(%q, %q) = true; OIDC subs are case-sensitive, "+
					"folding them lets one IdP account impersonate another", owner, attacker)
			}
			// ...and the exact same sub must still match, or the strictness is
			// just a broken comparison.
			if !canonicalEqual(owner, provider+":AbCdEf") {
				t.Errorf("canonicalEqual(%q, itself) = false; identical subs must match", owner)
			}
		})
	}
}

// TestF17OIDCProviderLabelStillFolds: the PROVIDER keyword is wire noise from a
// closed set, not user-controlled data, so it still folds. Only the SUBJECT got
// strict.
func TestF17OIDCProviderLabelStillFolds(t *testing.T) {
	if !canonicalEqual("Google:1078", "google:1078") {
		t.Error("provider label should fold: Google: and google: are the same provider")
	}
	if !canonicalEqual("CUSTOM:sub-1", "custom:sub-1") {
		t.Error("provider label should fold for custom: too")
	}
}

// TestF17GitHubLoginsStillFold is the positive control. GitHub logins ARE
// case-insensitive, and the legacy shim depends on the fold: a stored bare
// "ClubAnderson" must match an authenticated "github:clubanderson". If a fix
// made everything case-sensitive, this fails — which is exactly the signal that
// the fix went too far.
func TestF17GitHubLoginsStillFold(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"github:ClubAnderson", "github:clubanderson", true},
		{"ClubAnderson", "github:clubanderson", true},
		{"GitHub:ClubAnderson", "clubanderson", true},
		{"clubanderson", "clubanderson", true},
		// Different GitHub logins are still different users.
		{"github:clubanderson", "github:someoneelse", false},
		// Cross-provider never matches, whatever the case.
		{"google:clubanderson", "github:clubanderson", false},
		{"custom:AbC", "github:abc", false},
	}
	for _, c := range cases {
		if got := canonicalEqual(c.a, c.b); got != c.want {
			t.Errorf("canonicalEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestF17EmptyAndMalformedIdentities: an empty or unparseable identity must
// never match a real one. Ownership checks compare a stored Owner (which can be
// empty on a half-written record) against an authenticated caller, so "" == ""
// returning true is fine but "" == someone is not.
func TestF17EmptyAndMalformedIdentities(t *testing.T) {
	if canonicalEqual("", "github:clubanderson") {
		t.Error("empty identity must not match a real user")
	}
	if canonicalEqual("github:clubanderson", "") {
		t.Error("empty identity must not match a real user (reversed)")
	}
	// Unknown provider → unparseable → exact comparison, never a fold.
	if canonicalEqual("bogusprovider:AbC", "bogusprovider:abc") {
		t.Error("unknown-provider ids must compare exactly, not fold")
	}
	if !canonicalEqual("bogusprovider:AbC", "bogusprovider:AbC") {
		t.Error("identical unknown-provider ids should still match")
	}
}
