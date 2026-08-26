package config

import "testing"

// The optional Visual Hive Apps (issue #4030) are registered as identities
// ahead of the feature that uses them. These tests pin the properties that are
// silent when broken: an unset app_id must resolve to nothing, each registered
// App must resolve to exactly its own forge and slug, the Visual Hive Apps must
// stay distinct from the Hive Apps, and a third-party App must still get no
// claim at all.

// TestUnsetAppIDResolvesToNothing is the important one.
//
// Zero is what an UNSET config carries. Both Visual Hive Apps are now
// registered with real IDs, but this must keep holding: if a future edit ever
// reintroduces a zero-valued App constant, `case <thatConstant>` would claim
// every app-id-less hive as a registered App — a confident wrong answer where
// the file's contract demands no answer at all.
func TestUnsetAppIDResolvesToNothing(t *testing.T) {
	if got := forgeOfAppID(0); got != "" {
		t.Errorf("forgeOfAppID(0) = %q, want \"\" — an unset app_id must resolve to no forge", got)
	}
	if got := slugOfAppID(0); got != "" {
		t.Errorf("slugOfAppID(0) = %q, want \"\" — an unset app_id must resolve to no slug", got)
	}
	if IsVizHiveAppID(0) {
		t.Error("IsVizHiveAppID(0) = true — an unset app_id is not a registered App")
	}
	// The guard above is only meaningful while no App constant is itself 0.
	for _, tc := range []struct {
		name string
		id   int64
	}{
		{"VizHivePublicAppID", VizHivePublicAppID},
		{"VizHiveEnterpriseAppID", VizHiveEnterpriseAppID},
		{"PublicGitHubAppID", PublicGitHubAppID},
		{"EnterpriseGitHubAppID", EnterpriseGitHubAppID},
	} {
		if tc.id == 0 {
			t.Errorf("%s is 0 — a zero App constant collides with unset config; "+
				"remove its switch case until it has a real ID", tc.name)
		}
	}
}

// TestVizHiveAppsResolve is the positive control: both registered Apps resolve
// to their forge and their one slug. Without this, the zero test above could
// pass simply because nothing resolves at all.
func TestVizHiveAppsResolve(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    int64
		forge string
		slug  string
	}{
		{"public", VizHivePublicAppID, "github.com", VizHivePublicAppSlug},
		{"ghe", VizHiveEnterpriseAppID, "github.ibm.com", VizHiveEnterpriseAppSlug},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := forgeOfAppID(tc.id); got != tc.forge {
				t.Errorf("forgeOfAppID(%d) = %q, want %q", tc.id, got, tc.forge)
			}
			if got := slugOfAppID(tc.id); got != tc.slug {
				t.Errorf("slugOfAppID(%d) = %q, want %q", tc.id, got, tc.slug)
			}
			if !IsVizHiveAppID(tc.id) {
				t.Errorf("IsVizHiveAppID(%d) = false, want true", tc.id)
			}
		})
	}
}

// TestVizHiveAppsAreDistinctFromHiveApps guards the whole point of #4030: the
// Visual Hive Apps are SEPARATE from the Hive Apps, so that widening Visual
// Hive's permissions never touches an ordinary installation. A future edit that
// collapsed an ID onto a Hive App would silently undo that separation.
func TestVizHiveAppsAreDistinctFromHiveApps(t *testing.T) {
	if VizHiveEnterpriseAppID == EnterpriseGitHubAppID {
		t.Fatal("viz-hive GHE App ID collides with the Hive GHE App — they must be separate Apps")
	}
	if VizHiveEnterpriseAppID == PublicGitHubAppID {
		t.Fatal("viz-hive GHE App ID collides with the public Hive App")
	}
	if VizHivePublicAppID == PublicGitHubAppID {
		t.Fatal("viz-hive public App ID collides with the public Hive App — they must be separate Apps")
	}
	if VizHivePublicAppID == EnterpriseGitHubAppID {
		t.Fatal("viz-hive public App ID collides with the Hive GHE App")
	}
	if VizHivePublicAppID == VizHiveEnterpriseAppID {
		t.Fatal("the two viz-hive App IDs collide")
	}
	if VizHivePublicAppSlug == DefaultGitHubAppSlug {
		t.Fatal("viz-hive public slug collides with the Hive slug")
	}
	if VizHiveEnterpriseAppSlug == EnterpriseGitHubAppSlug {
		t.Fatal("viz-hive GHE slug collides with the Hive GHE slug")
	}
	// A Hive App must never be reported as a Visual Hive App.
	if IsVizHiveAppID(PublicGitHubAppID) || IsVizHiveAppID(EnterpriseGitHubAppID) {
		t.Error("IsVizHiveAppID matched a Hive App — the optional-App check must not claim the Hive App")
	}
}

// TestVizHiveGHESlugCarriesGHEMarker pins a subtle dependency. IdentitySetIssues
// derives slugIsGHE from the substring "ghe", so a GHE App under a non-"ghe"
// slug slips past the marker-based consistency rules — the exact defect the
// file's history records for the invented "ibm-hive" fixture.
func TestVizHiveGHESlugCarriesGHEMarker(t *testing.T) {
	if !containsFold(VizHiveEnterpriseAppSlug, "ghe") {
		t.Errorf("VizHiveEnterpriseAppSlug = %q lacks the \"ghe\" marker that IdentitySetIssues keys on",
			VizHiveEnterpriseAppSlug)
	}
	// Positive control: the public slug deliberately does NOT carry it, so the
	// marker actually discriminates rather than matching everything.
	if containsFold(VizHivePublicAppSlug, "ghe") {
		t.Errorf("VizHivePublicAppSlug = %q carries a \"ghe\" marker; it is a github.com App",
			VizHivePublicAppSlug)
	}
}

// TestUnknownAppIDStillResolvesToNothing pins the contract the file states
// repeatedly: an unrecognised App ID gets no claim, never an invented one.
// Adding the Visual Hive Apps must not have changed that for anyone else.
func TestUnknownAppIDStillResolvesToNothing(t *testing.T) {
	// 4240368 is a real third-party App (daviddiaz0317-visual-hive) that this
	// build does not own and must not describe.
	const thirdPartyAppID int64 = 4240368
	if got := forgeOfAppID(thirdPartyAppID); got != "" {
		t.Errorf("forgeOfAppID(%d) = %q, want \"\" — unknown must never be treated as a mismatch",
			thirdPartyAppID, got)
	}
	if got := slugOfAppID(thirdPartyAppID); got != "" {
		t.Errorf("slugOfAppID(%d) = %q, want \"\" — inventing a slug is a confident wrong answer",
			thirdPartyAppID, got)
	}
	if IsVizHiveAppID(thirdPartyAppID) {
		t.Error("IsVizHiveAppID matched a third-party App")
	}
}
