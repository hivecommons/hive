package config

import "testing"

// The optional Visual Hive Apps (issue #4030) are registered as identities
// ahead of the feature that uses them. These tests pin the two properties that
// are silent when broken: an unregistered App must not resolve to anything, and
// the registered one must resolve to exactly its own forge and slug.

// TestVizHivePublicAppIsNotResolvableWhileUnregistered is the important one.
//
// VizHivePublicAppID is 0 until someone registers kubestellar-viz-hive on
// github.com — and 0 is also what an UNSET config carries. If a future edit
// adds `case VizHivePublicAppID` to either lookup while the constant is still
// 0, every hive with no app_id would be described as a registered Visual Hive
// App, which is a confident wrong answer rather than a missing one.
func TestVizHivePublicAppIsNotResolvableWhileUnregistered(t *testing.T) {
	if VizHivePublicAppID != 0 {
		t.Skip("kubestellar-viz-hive has been registered; this guard no longer applies")
	}
	if got := forgeOfAppID(0); got != "" {
		t.Errorf("forgeOfAppID(0) = %q, want \"\" — an unset app_id must resolve to no forge", got)
	}
	if got := slugOfAppID(0); got != "" {
		t.Errorf("slugOfAppID(0) = %q, want \"\" — an unset app_id must resolve to no slug", got)
	}
	if IsVizHiveAppID(0) {
		t.Error("IsVizHiveAppID(0) = true — an unset app_id is not a registered App")
	}
}

// TestVizHiveEnterpriseAppResolves is the positive control for the test above:
// the App that IS registered resolves to its forge and its one slug. Without
// this, the zero-guard test could pass simply because nothing resolves at all.
func TestVizHiveEnterpriseAppResolves(t *testing.T) {
	if got, want := forgeOfAppID(VizHiveEnterpriseAppID), "github.ibm.com"; got != want {
		t.Errorf("forgeOfAppID(%d) = %q, want %q", VizHiveEnterpriseAppID, got, want)
	}
	if got, want := slugOfAppID(VizHiveEnterpriseAppID), VizHiveEnterpriseAppSlug; got != want {
		t.Errorf("slugOfAppID(%d) = %q, want %q", VizHiveEnterpriseAppID, got, want)
	}
	if !IsVizHiveAppID(VizHiveEnterpriseAppID) {
		t.Errorf("IsVizHiveAppID(%d) = false, want true", VizHiveEnterpriseAppID)
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
