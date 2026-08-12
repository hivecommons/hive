package hub

import "testing"

// TestIsHubAdmin_DefaultAndLegacyShim: with no HIVE_HUB_ADMINS, the default
// admin (github:clubanderson) matches whether presented bare-legacy or canonical.
func TestIsHubAdmin_DefaultAndLegacyShim(t *testing.T) {
	t.Setenv(hubAdminsEnv, "") // force default

	for _, id := range []string{"clubanderson", "github:clubanderson", "GitHub:ClubAnderson"} {
		if !isHubAdmin(id) {
			t.Errorf("isHubAdmin(%q) = false, want true (default admin, legacy/canonical/case)", id)
		}
	}
	for _, id := range []string{"", "someone-else", "github:someone-else"} {
		if isHubAdmin(id) {
			t.Errorf("isHubAdmin(%q) = true, want false", id)
		}
	}
}

// TestIsHubAdmin_CrossProviderIsNotAdmin is the load-bearing security property
// (plan Risk #1): a user whose SUBJECT happens to be "clubanderson" on a
// DIFFERENT provider must NOT inherit GitHub admin. If this ever passes as true,
// registering the admin's name on Google/IBMid would grant admin.
func TestIsHubAdmin_CrossProviderIsNotAdmin(t *testing.T) {
	t.Setenv(hubAdminsEnv, "")
	for _, id := range []string{"google:clubanderson", "ibmid:clubanderson", "redhat:clubanderson"} {
		if isHubAdmin(id) {
			t.Fatalf("SECURITY: isHubAdmin(%q) = true — a same-subject identity on another provider must NEVER be admin", id)
		}
	}
}

// TestIsHubAdmin_EnvOverride: HIVE_HUB_ADMINS replaces the default set and
// accepts both bare-legacy and canonical (incl. non-GitHub) entries.
func TestIsHubAdmin_EnvOverride(t *testing.T) {
	t.Setenv(hubAdminsEnv, "github:alice, google:1078, bob")

	for _, id := range []string{"alice", "github:alice", "google:1078", "bob", "github:bob"} {
		if !isHubAdmin(id) {
			t.Errorf("isHubAdmin(%q) = false, want true under env override", id)
		}
	}
	// The default is NO LONGER admin once the env overrides the set.
	if isHubAdmin("clubanderson") {
		t.Error("isHubAdmin(clubanderson) should be false when HIVE_HUB_ADMINS excludes it")
	}
	// Cross-provider still isolated.
	if isHubAdmin("google:alice") {
		t.Error("google:alice must not match github:alice admin entry")
	}
}

// TestPrimaryHubAdmin returns the canonical primary admin — the first env entry,
// else the canonicalized default.
func TestPrimaryHubAdmin(t *testing.T) {
	t.Setenv(hubAdminsEnv, "")
	if got := primaryHubAdmin(); got != "github:clubanderson" {
		t.Errorf("primaryHubAdmin() default = %q, want github:clubanderson", got)
	}
	t.Setenv(hubAdminsEnv, "google:1078, github:alice")
	if got := primaryHubAdmin(); got != "google:1078" {
		t.Errorf("primaryHubAdmin() = %q, want google:1078 (first env entry)", got)
	}
}
