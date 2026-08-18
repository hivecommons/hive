package hub

import "testing"

func TestUserProviderAndCanonicalID(t *testing.T) {
	cases := []struct {
		u            SaaSUser
		wantProvider string
		wantCanon    string
	}{
		// Legacy github-only record: no Provider/CanonicalID → resolves to github.
		{SaaSUser{GitHubUsername: "clubanderson"}, "github", "github:clubanderson"},
		// Explicit Provider field wins.
		{SaaSUser{GitHubUsername: "google:1078", Provider: "google"}, "google", "google:1078"},
		// CanonicalID drives provider when Provider is unset.
		{SaaSUser{GitHubUsername: "ignored", CanonicalID: "ibmid:xyz"}, "ibmid", "ibmid:xyz"},
		// github: canonical still resolves to github.
		{SaaSUser{GitHubUsername: "github:foo"}, "github", "github:foo"},
	}
	for _, c := range cases {
		if got := userProvider(&c.u); got != c.wantProvider {
			t.Errorf("userProvider(%+v) = %q, want %q", c.u, got, c.wantProvider)
		}
		if got := userCanonicalID(&c.u); got != c.wantCanon {
			t.Errorf("userCanonicalID(%+v) = %q, want %q", c.u, got, c.wantCanon)
		}
	}
	// nil-safety
	if userProvider(nil) != "" || userCanonicalID(nil) != "" {
		t.Error("nil user must return empty")
	}
}

// TestSaveUsesCanonicalIDWhenSet: a record carrying an explicit CanonicalID
// writes to that canonical file even when GitHubUsername holds something else.
func TestSaveUsesCanonicalIDWhenSet(t *testing.T) {
	withTempUsersDir(t)
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "legacy-ignored", CanonicalID: "google:5555", Provider: "google", Hives: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	// It must land at the canonical google file, not a legacy one.
	if u := loadSaaSUser("google:5555"); u == nil || u.CanonicalID != "google:5555" {
		t.Errorf("loadSaaSUser(google:5555) = %+v", u)
	}
}
