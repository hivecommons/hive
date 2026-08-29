package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// intPtr is a local helper so these tests do not depend on helpers elsewhere.
func intPtr(i int) *int { return &i }

// baseOverlay returns an overlay that passes the plausibility guard. Tests
// mutate the specific field under test so a failure names one behaviour.
func baseOverlay() *Config {
	return &Config{
		Project: ProjectConfig{Org: "acme"},
		Agents:  map[string]AgentConfig{"scanner": {}},
	}
}

func baseSeed() *Config {
	return &Config{
		Project: ProjectConfig{Org: "acme"},
		Agents:  map[string]AgentConfig{"scanner": {}, "guide": {}},
	}
}

// ─────────────────────────────────────────────────────────────────────────
// GOLDEN TABLE — one test per row of the documented layering table.
//
// These pin CURRENT behaviour. PR 1 is a codification with no behaviour
// change, so a failure here means either the transcription is wrong or
// somebody changed which layer wins — the latter being a fleet migration, not
// a refactor. Treat a diff as a stop-and-report.
// ─────────────────────────────────────────────────────────────────────────

func TestGoldenTable_OverlayWinsByDefault(t *testing.T) {
	seed := baseSeed()
	seed.Project.PrimaryRepo = "seed-repo"
	overlay := baseOverlay()
	overlay.Project.PrimaryRepo = "overlay-repo"

	merged, prov := MergeLayers(seed, overlay)

	if merged.Project.PrimaryRepo != "overlay-repo" {
		t.Errorf("project.primary_repo = %q, want overlay to win with %q",
			merged.Project.PrimaryRepo, "overlay-repo")
	}
	if got := prov.LayerOf("project.primary_repo"); got != LayerDashboardOverlay {
		t.Errorf("provenance = %v, want %v", got, LayerDashboardOverlay)
	}
}

func TestGoldenTable_ACMMLevelOverlayWins(t *testing.T) {
	// Issue #1856: the seed carries only the provision-time level and goes
	// stale on any level change. A hive raised to L5 must not drop back to its
	// provisioned L3 on restart.
	seed := baseSeed()
	seed.ACMMLevel = intPtr(3)
	overlay := baseOverlay()
	overlay.ACMMLevel = intPtr(5)

	merged, prov := MergeLayers(seed, overlay)

	if merged.ACMMLevel == nil || *merged.ACMMLevel != 5 {
		t.Fatalf("acmm_level = %v, want overlay's 5 (issue #1856 regression)", merged.ACMMLevel)
	}
	if got := prov.LayerOf("acmm_level"); got != LayerDashboardOverlay {
		t.Errorf("provenance = %v, want %v", got, LayerDashboardOverlay)
	}
}

func TestGoldenTable_ACMMLevelSeedIsFallbackOnly(t *testing.T) {
	// The seed supplies the level ONLY when the overlay has none at all.
	seed := baseSeed()
	seed.ACMMLevel = intPtr(3)
	overlay := baseOverlay() // no ACMMLevel

	merged, prov := MergeLayers(seed, overlay)

	if merged.ACMMLevel == nil || *merged.ACMMLevel != 3 {
		t.Fatalf("acmm_level = %v, want seed fallback 3", merged.ACMMLevel)
	}
	if got := prov.LayerOf("acmm_level"); got != LayerSeed {
		t.Errorf("provenance = %v, want %v", got, LayerSeed)
	}
}

func TestGoldenTable_IsPublicSeedWins(t *testing.T) {
	// The ONLY unconditional seed-wins assignment in the whole merge.
	seed := baseSeed()
	seed.Hub.IsPublic = true
	overlay := baseOverlay()
	overlay.Hub.IsPublic = false

	merged, prov := MergeLayers(seed, overlay)

	if !merged.Hub.IsPublic {
		t.Error("hub.is_public = false, want seed's true to win")
	}
	if got := prov.LayerOf("hub.is_public"); got != LayerSeed {
		t.Errorf("provenance = %v, want %v", got, LayerSeed)
	}
}

func TestGoldenTable_GitHubOverlayWinsNormally(t *testing.T) {
	// Dashboard/hub-installed App creds live only in the overlay; the seed
	// keeps its provision-time placeholder. The overlay must win.
	seed := baseSeed()
	seed.GitHub = GitHubConfig{AppID: 1, InstallationID: 1, KeyFile: "/secrets/PLACEHOLDER.pem"}
	overlay := baseOverlay()
	overlay.GitHub = GitHubConfig{AppID: 3568013, InstallationID: 135058248, KeyFile: "/data/gh-app-key.pem"}

	merged, _ := MergeLayers(seed, overlay)

	if merged.GitHub.AppID != 3568013 {
		t.Errorf("github.app_id = %d, want overlay's real App 3568013", merged.GitHub.AppID)
	}
}

func TestGoldenTable_GitHubRatchetPreventsWipe(t *testing.T) {
	// Ratchet C: a placeholder-looking overlay must never downgrade a hive
	// that has a real App installed. Observed in production as a 401 loop
	// after a pod restart.
	seed := baseSeed()
	seed.GitHub = GitHubConfig{AppID: 3568013, InstallationID: 135058248, KeyFile: "/data/real.pem"}
	overlay := baseOverlay()
	overlay.GitHub = GitHubConfig{} // wiped / placeholder

	merged, prov := MergeLayers(seed, overlay)

	if merged.GitHub.AppID != 3568013 {
		t.Errorf("github.app_id = %d, want seed's real App preserved by the ratchet",
			merged.GitHub.AppID)
	}
	if !prov.GitHubRatchetFired {
		t.Error("GitHubRatchetFired = false, want the ratchet to be reported")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Plausibility guard — a trip means booting on the bare seed.
// ─────────────────────────────────────────────────────────────────────────

func TestOverlayRejected_NoAgentsIsLoud(t *testing.T) {
	seed := baseSeed()
	overlay := &Config{Project: ProjectConfig{Org: "acme"}} // no agents

	merged, prov := MergeLayers(seed, overlay)

	if !prov.OverlayRejected {
		t.Fatal("OverlayRejected = false, want a rejected overlay to be reported")
	}
	if !strings.Contains(prov.OverlayRejectReason, "agents") {
		t.Errorf("reason = %q, want it to name the missing agents", prov.OverlayRejectReason)
	}
	// The seed must be used as-is, exactly as at boot.
	if len(merged.Agents) != len(seed.Agents) {
		t.Errorf("merged agents = %d, want the seed's %d", len(merged.Agents), len(seed.Agents))
	}
}

func TestOverlayRejected_NoOrgIsLoud(t *testing.T) {
	seed := baseSeed()
	overlay := &Config{Agents: map[string]AgentConfig{"scanner": {}}} // no project.org

	_, prov := MergeLayers(seed, overlay)

	if !prov.OverlayRejected {
		t.Fatal("OverlayRejected = false, want a rejected overlay to be reported")
	}
	if !strings.Contains(prov.OverlayRejectReason, "project.org") {
		t.Errorf("reason = %q, want it to name project.org", prov.OverlayRejectReason)
	}
}

func TestAbsentOverlayIsNotARejection(t *testing.T) {
	// No overlay on disk is the ordinary first-boot state, not an anomaly.
	// Reporting it as a rejection would cry wolf on every fresh hive.
	_, prov := MergeLayers(baseSeed(), nil)

	if prov.OverlayRejected {
		t.Error("OverlayRejected = true for an absent overlay, want false")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// is_public key-presence fidelity — the one case where a naive Go
// transcription would silently diverge from the shell.
// ─────────────────────────────────────────────────────────────────────────

func TestIsPublicKeyPresenceFidelity(t *testing.T) {
	// Seed OMITS hub.is_public; overlay sets it true.
	//
	// The heredoc gates on `'is_public' in seed['hub']`, so it leaves the
	// overlay's true alone. A Go transcription over parsed structs cannot see
	// the difference between "absent" and "false" and would apply the seed's
	// zero value, silently flipping the hive to private.
	seedYAML := []byte("project:\n  org: acme\nagents:\n  scanner: {}\nhub:\n  url: https://hub\n")
	overlayYAML := []byte("project:\n  org: acme\nagents:\n  scanner: {}\nhub:\n  is_public: true\n")

	merged, prov, err := MergeLayersYAML(seedYAML, overlayYAML)
	if err != nil {
		t.Fatalf("MergeLayersYAML: %v", err)
	}

	if !merged.Hub.IsPublic {
		t.Error("hub.is_public = false, want the overlay's true preserved " +
			"because the seed omits the key entirely (heredoc fidelity)")
	}
	if got := prov.LayerOf("hub.is_public"); got == LayerSeed {
		t.Error("provenance credits the seed for a key the seed does not set")
	}
}

func TestIsPublicExplicitFalseInSeedStillWins(t *testing.T) {
	// Present-and-false is a real assertion and must beat the overlay, which
	// is what distinguishes this from the absent case above.
	seedYAML := []byte("project:\n  org: acme\nagents:\n  scanner: {}\nhub:\n  is_public: false\n")
	overlayYAML := []byte("project:\n  org: acme\nagents:\n  scanner: {}\nhub:\n  is_public: true\n")

	merged, prov, err := MergeLayersYAML(seedYAML, overlayYAML)
	if err != nil {
		t.Fatalf("MergeLayersYAML: %v", err)
	}

	if merged.Hub.IsPublic {
		t.Error("hub.is_public = true, want the seed's explicit false to win")
	}
	if got := prov.LayerOf("hub.is_public"); got != LayerSeed {
		t.Errorf("provenance = %v, want %v", got, LayerSeed)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Layer metadata — the operator-facing answers.
// ─────────────────────────────────────────────────────────────────────────

func TestSeedIsWritableByNobody(t *testing.T) {
	// The single most useful fact this package exposes IN KUBERNETES. The hub
	// has only a "get" against configmaps and the spoke's RBAC omits them
	// entirely, so editing the ConfigMap and restarting changes nothing — which
	// is what happened four times during the GHE incident.
	//
	// Explicitly forces Kubernetes mode: without this, the test's answer
	// depended on whether the test host looked like a pod, which is exactly
	// the gap #4971 shipped through (see
	// TestSeedIsWritableOutsideKubernetes for the other branch, and
	// TestProvenanceReportsSeedWritableOutsideKubernetes in pkg/dashboard for
	// the end-to-end regression test).
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1") // IsKubernetesPod() → true
	if LayerSeed.Writable() {
		t.Error("LayerSeed.Writable() = true, want false")
	}
	if w := LayerSeed.Writer(); !strings.Contains(w, "nobody") {
		t.Errorf("LayerSeed.Writer() = %q, want it to say nobody", w)
	}
	if p := LayerSeed.Path(); !strings.Contains(p, "ConfigMap") {
		t.Errorf("LayerSeed.Path() = %q, want it to name the ConfigMap", p)
	}
	if !LayerDashboardOverlay.Writable() {
		t.Error("LayerDashboardOverlay.Writable() = false, want true")
	}
}

// TestSeedIsWritableOutsideKubernetes is the #4971 regression test at the
// Layer-metadata level (see TestProvenanceReportsSeedWritableOutsideKubernetes
// in pkg/dashboard for the end-to-end HTTP version). Outside Kubernetes there
// is no ConfigMap and no RBAC to omit: LayerSeed is RuntimeConfigFile, a plain
// file the spoke rewrites on every save (saveLocked, config.go:5141). Reporting
// writable=false / writer=nobody / path=ConfigMap there sends an operator
// looking for something that does not exist while hiding the file that
// actually decides the value.
func TestSeedIsWritableOutsideKubernetes(t *testing.T) {
	// Force the non-Kubernetes branch explicitly: clear the env probe and
	// point the serviceaccount-token probe at a path that does not exist.
	// Relying on the host "not looking like a pod" made this test fail on
	// in-cluster CI runners and dev hives, where KUBERNETES_SERVICE_HOST is
	// set and the real token file exists.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	restore := SetSATokenFileForTest(filepath.Join(t.TempDir(), "no-such-sa-token"))
	defer restore()
	if !LayerSeed.Writable() {
		t.Error("LayerSeed.Writable() = false outside Kubernetes, want true — RuntimeConfigFile is writable by the spoke")
	}
	if w := LayerSeed.Writer(); strings.Contains(w, "nobody") {
		t.Errorf("LayerSeed.Writer() = %q, want it to name the spoke, not nobody (there is no RBAC to omit outside Kubernetes)", w)
	}
	if p := LayerSeed.Path(); strings.Contains(p, "ConfigMap") {
		t.Errorf("LayerSeed.Path() = %q, want the real runtime file, not a ConfigMap that does not exist on this hive", p)
	}
	if p := LayerSeed.Path(); p != RuntimeConfigFile {
		t.Errorf("LayerSeed.Path() = %q, want RuntimeConfigFile (%q)", p, RuntimeConfigFile)
	}
}

func TestReportNamesWhereToWrite(t *testing.T) {
	seed := baseSeed()
	overlay := baseOverlay()
	overlay.GitHub = GitHubConfig{AppID: 3568013, InstallationID: 1, KeyFile: "/data/k.pem"}

	merged, prov := MergeLayers(seed, overlay)
	report := prov.Report(merged)

	var found bool
	for _, f := range report {
		if f.Field == "github.app_id" {
			found = true
			if f.Path != DashboardOverlayFile {
				t.Errorf("github.app_id path = %q, want %q", f.Path, DashboardOverlayFile)
			}
			if !f.Writable {
				t.Error("github.app_id reported as not writable, want writable")
			}
		}
	}
	if !found {
		t.Fatal("report omits github.app_id")
	}
}

func TestReportRedactsToken(t *testing.T) {
	overlay := baseOverlay()
	overlay.GitHub = GitHubConfig{Token: "ghp_supersecret"}

	merged, prov := MergeLayers(baseSeed(), overlay)
	for _, f := range prov.Report(merged) {
		if strings.Contains(f.Value, "supersecret") {
			t.Fatalf("report leaked the token in field %s", f.Field)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// GitHub identity-set invariant — detection only, no behaviour change.
// ─────────────────────────────────────────────────────────────────────────

func TestIdentitySet_GHESlugWithEmptyAPIURLIsDetected(t *testing.T) {
	// The live regression: a GHE app_id/slug pushed WITHOUT api_url, so
	// api_url stayed empty and defaulted to api.github.com, producing
	// "404 Integration not found" on hives that worked an hour earlier.
	gh := GitHubConfig{AppID: 5686, AppSlug: "kubestellar-hive-ghe", APIURL: ""}

	issues := IdentitySetIssues(gh)
	if len(issues) == 0 {
		t.Fatal("no issues reported for a GHE slug with an empty api_url")
	}
	joined := strings.Join(issues, " ")
	if !strings.Contains(joined, "api.github.com") {
		t.Errorf("issue text %q does not explain the 404 cause", joined)
	}
}

func TestIdentitySet_ConsistentGHEIsClean(t *testing.T) {
	gh := GitHubConfig{
		AppID:   5686,
		AppSlug: "kubestellar-hive-ghe",
		APIURL:  "https://github.ibm.com/api/v3",
		BaseURL: "https://github.ibm.com",
	}
	if issues := IdentitySetIssues(gh); len(issues) != 0 {
		t.Errorf("consistent GHE identity reported issues: %v", issues)
	}
}

func TestIdentitySet_ConsistentPublicIsClean(t *testing.T) {
	// Public GitHub legitimately has an empty api_url, so the check must not
	// fire on the ordinary case — otherwise it is noise on ~48 hives.
	gh := GitHubConfig{AppID: 3568013, AppSlug: "kubestellar-hive", APIURL: ""}
	if issues := IdentitySetIssues(gh); len(issues) != 0 {
		t.Errorf("consistent public identity reported issues: %v", issues)
	}
}

func TestIdentitySet_MismatchedBaseAndAPIIsDetected(t *testing.T) {
	gh := GitHubConfig{
		AppID:   5686,
		BaseURL: "https://github.ibm.com",
		APIURL:  "https://api.github.com",
	}
	if issues := IdentitySetIssues(gh); len(issues) == 0 {
		t.Fatal("no issues reported for base_url/api_url naming different forges")
	}
}

func TestIdentitySet_NoAppConfiguredIsClean(t *testing.T) {
	// A token-auth hive has no App identity to be inconsistent about.
	if issues := IdentitySetIssues(GitHubConfig{Token: "x"}); len(issues) != 0 {
		t.Errorf("token-only hive reported identity issues: %v", issues)
	}
}
