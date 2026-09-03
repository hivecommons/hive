package dashboard

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// These tests lock the fix for the vllmd-13 dead-end: after an operator
// "Reset Forge App", the raw github.base_url/app_slug go blank while the
// RESOLVED forge (what the Forge App tab displays) is still github.ibm.com.
// The install banner's visibility was gated on a non-empty
// githubAppInstallURL — empty exactly then, because AppInstallURL() refused
// to derive a GHE slug — so the banner stayed hidden while the auth state was
// literally "not-installed" and the Forge App tab offered no install action.
//
// PRIMARY RULE (checked first, sufficient by itself): a real App (app_id set,
// not the placeholder) with installation_id 0 lights the banner and the Forge
// App tab's install link immediately — no auth-state classification, recheck
// round-trip, or raw-URL sniffing may gate it.

// gheAppPostResetConfig is the exact on-disk shape vllmd-13 held after the
// reset: a real GHE app_id, installation_id 0, and ONLY api_url naming the
// forge (base_url and app_slug blank).
func gheAppPostResetConfig() config.GitHubConfig {
	return config.GitHubConfig{
		AppID:          config.EnterpriseGitHubAppID,
		InstallationID: 0,
		APIURL:         "https://github.ibm.com/api/v3",
		BaseURL:        "",
		AppSlug:        "",
	}
}

// TestUpdateStatus_InstallMissingFromConfigTruth: blank installation_id alone
// must mark the payload, force the requirement on, classify not-installed,
// and carry the RESOLVED forge's install URL + base URL — with no probe
// having run at all.
func TestUpdateStatus_InstallMissingFromConfigTruth(t *testing.T) {
	s := newFullServer(t)
	s.deps.Config.GitHub = gheAppPostResetConfig()

	status := &StatusPayload{}
	s.UpdateStatus(status)

	if !status.GitHubAppInstallMissing {
		t.Error("GitHubAppInstallMissing = false; a real app_id with installation_id 0 must set it — this is the primary trigger and needs no auth probe")
	}
	if !status.GitHubAppRequired {
		t.Error("GitHubAppRequired = false; config truth (installation_id 0) must force the banner requirement on")
	}
	if status.GitHubAppState != "not-installed" {
		t.Errorf("GitHubAppState = %q, want not-installed filled from config truth", status.GitHubAppState)
	}
	wantURL := "https://github.ibm.com/github-apps/" + config.EnterpriseGitHubAppSlug + "/installations/new"
	if status.GitHubAppInstallURL != wantURL {
		t.Errorf("GitHubAppInstallURL = %q, want %q (RESOLVED forge, not the blank raw fields)", status.GitHubAppInstallURL, wantURL)
	}
	if status.GitHubBaseURL != "https://github.ibm.com" {
		t.Errorf("GitHubBaseURL = %q, want the resolved https://github.ibm.com", status.GitHubBaseURL)
	}
}

// TestUpdateStatus_InstallMissingPreservesOperatorState: the config-truth
// override may only FILL an empty classification. An operator-side state
// (key-missing/key-invalid) must survive, or the banner would send a user to
// an install page for a failure only the hub operator can fix.
func TestUpdateStatus_InstallMissingPreservesOperatorState(t *testing.T) {
	s := newFullServer(t)
	s.deps.Config.GitHub = gheAppPostResetConfig()
	s.SetGitHubAppRequired(true)
	s.SetGitHubAppState("key-missing")

	status := &StatusPayload{}
	s.UpdateStatus(status)

	if status.GitHubAppState != "key-missing" {
		t.Errorf("GitHubAppState = %q, want the operator-side key-missing preserved", status.GitHubAppState)
	}
	if !status.GitHubAppInstallMissing {
		t.Error("GitHubAppInstallMissing must still report config truth alongside an operator-side state")
	}
}

// TestUpdateStatus_NoInstallMissingWhenInstalled: an installation present and
// no probe failure → nothing lights up (the banner-hidden case).
func TestUpdateStatus_NoInstallMissingWhenInstalled(t *testing.T) {
	s := newFullServer(t)
	g := gheAppPostResetConfig()
	g.InstallationID = 12345678
	s.deps.Config.GitHub = g

	status := &StatusPayload{}
	s.UpdateStatus(status)

	if status.GitHubAppInstallMissing {
		t.Error("GitHubAppInstallMissing = true with a non-zero installation_id")
	}
	if status.GitHubAppRequired {
		t.Error("GitHubAppRequired = true; config truth must not force the banner when an installation is present")
	}
	if status.GitHubAppState != "" {
		t.Errorf("GitHubAppState = %q, want empty when nothing classified anything", status.GitHubAppState)
	}
}

// TestUpdateStatus_PlaceholderAppNeverInstallMissing: the placeholder app_id
// means "no real App yet" — an operator-side condition with its own banner.
// It must never read as a user-fixable missing installation.
func TestUpdateStatus_PlaceholderAppNeverInstallMissing(t *testing.T) {
	s := newFullServer(t)
	g := gheAppPostResetConfig()
	g.AppID = config.PlaceholderAppID
	s.deps.Config.GitHub = g

	status := &StatusPayload{}
	s.UpdateStatus(status)

	if status.GitHubAppInstallMissing {
		t.Error("GitHubAppInstallMissing = true for the placeholder app_id — installing cannot help a hive with no real App assigned")
	}
}

// TestGitHubAppInstallMissingFieldIsExposed proves the primary trigger
// actually reaches the browser under the name the banner JS reads.
func TestGitHubAppInstallMissingFieldIsExposed(t *testing.T) {
	f, ok := reflect.TypeOf(StatusPayload{}).FieldByName("GitHubAppInstallMissing")
	if !ok {
		t.Fatal("StatusPayload has no GitHubAppInstallMissing field — the banner's primary trigger would be dead code")
	}
	if got := f.Tag.Get("json"); !strings.HasPrefix(got, "githubAppInstallMissing") {
		t.Errorf("GitHubAppInstallMissing json tag = %q, want it to serialise as githubAppInstallMissing", got)
	}
}

// TestGitHubAppBannerKeysOffInstallMissingAndAuthState locks the client-side
// gating: visibility must key off the missing installation (first) and the
// classified auth state — never off a non-empty install URL alone, which is
// the raw-field sniffing that suppressed the banner on vllmd-13.
func TestGitHubAppBannerKeysOffInstallMissingAndAuthState(t *testing.T) {
	html := spokeIndexHTML(t)

	i := strings.Index(html, "function renderGitHubAppBanner(data)")
	if i < 0 {
		t.Fatal("static/index.html has no renderGitHubAppBanner function")
	}
	end := strings.Index(html[i:], "\n    }")
	if end < 0 {
		t.Fatal("could not find the end of renderGitHubAppBanner")
	}
	body := html[i : i+end]

	required := []struct {
		snippet string
		why     string
	}{
		{"data.githubAppInstallMissing", "the banner must read the config-truth missing-installation flag"},
		{"var isInstallMissing", "the missing installation must be its own named trigger, evaluated before any URL check"},
		{"isNotInstalled || data.githubAppInstallURL || isPermIssue", "visibility must accept the auth/config trigger BEFORE requiring an install URL — an empty URL alone must never hide the banner"},
		{"(data.githubAppRequired || isInstallMissing)", "a missing installation must show the banner even if no probe has set the required flag yet"},
		{"GH_APP_STATE_WRONG_INSTALLATION", "wrong-installation is user-actionable and must trigger the install banner"},
	}
	for _, r := range required {
		if !strings.Contains(body, r.snippet) {
			t.Errorf("renderGitHubAppBanner is missing %q — %s", r.snippet, r.why)
		}
	}
	if !strings.Contains(html, "GH_APP_STATE_WRONG_INSTALLATION = 'wrong-installation'") {
		t.Error("the wrong-installation wire token must match github.AppStateWrongInstallation.String()")
	}

	// The primary trigger must be sufficient by itself: the old gate began
	// with `data.githubAppRequired && (data.githubAppInstallURL || ...` which
	// is exactly the shape that hid the banner. Its return would be a
	// regression.
	if strings.Contains(body, "if (data.githubAppRequired && (data.githubAppInstallURL || isPermIssue)) {") {
		t.Error("the old URL-gated visibility condition is back — a blank raw config would suppress the banner again")
	}
}

func TestRepoTargetBannerPinsFixCTA(t *testing.T) {
	html := spokeIndexHTML(t)
	required := []string{
		`id="repo-target-banner"`,
		"function renderRepoTargetBanner(data)",
		"data.repoTargetMisconfigured",
		"data.repoTargetIssue",
		"openConfigDialog('governor',null,'Repos')",
		"Fix in Repos",
	}
	for _, snippet := range required {
		if !strings.Contains(html, snippet) {
			t.Fatalf("repo target banner missing %q", snippet)
		}
	}
}

// TestForgeAppTabAlwaysOffersInstallAction locks the Forge App tab contract:
// installation_id 0 (with a real app_id) shows the install link FIRST and by
// itself; classified not-installed/wrong-installation also trigger it; the
// URL comes from the server-built install_url (the banner's builder, never a
// client-composed second scheme); a Re-check sits beside it; and a healthy
// installed config gets a subtle View App settings link instead.
func TestForgeAppTabAlwaysOffersInstallAction(t *testing.T) {
	html := spokeIndexHTML(t)

	i := strings.Index(html, "function forgeAppNeedsInstall(a)")
	if i < 0 {
		t.Fatal("static/index.html has no forgeAppNeedsInstall function — the Forge App tab is a dead end again")
	}
	end := strings.Index(html[i:], "\n    }")
	body := html[i : i+end]
	if !strings.Contains(body, "if (a.app_id && !a.installation_id) return true;") {
		t.Error("forgeAppNeedsInstall must check the missing installation_id FIRST and return true on it alone — no dependence on auth_state")
	}
	if !strings.Contains(body, "GH_APP_STATE_NOT_INSTALLED") || !strings.Contains(body, "GH_APP_STATE_WRONG_INSTALLATION") {
		t.Error("forgeAppNeedsInstall must also trigger on the classified not-installed / wrong-installation states")
	}

	ai := strings.Index(html, "function forgeAppActions(a)")
	if ai < 0 {
		t.Fatal("static/index.html has no forgeAppActions function")
	}
	aend := strings.Index(html[ai:], "\n    }")
	abody := html[ai : ai+aend]
	for _, r := range []struct{ snippet, why string }{
		{"a.install_url", "the tab must use the SERVER-built install URL (config.AppInstallURL, the banner's builder) — never compose its own"},
		{"Install GitHub App", "the install affordance must be a visible, labelled action"},
		{"forgeAppRecheck", "the Re-check affordance must sit next to the install link"},
		{"View App settings", "a healthy installed config gets the subtle settings link instead"},
		{"github.app_slug", "an unknown-forge GHE host with no slug must name the missing config rather than render a dead end"},
	} {
		if !strings.Contains(abody, r.snippet) {
			t.Errorf("forgeAppActions is missing %q — %s", r.snippet, r.why)
		}
	}

	// The actions row must actually be rendered inside the Active config card.
	ri := strings.Index(html, "function renderForgeApps(data)")
	if ri < 0 {
		t.Fatal("static/index.html has no renderForgeApps function")
	}
	rend := strings.Index(html[ri:], "\n    }")
	rbody := html[ri : ri+rend]
	if !strings.Contains(rbody, "forgeAppActions(a)") {
		t.Error("renderForgeApps does not render forgeAppActions — the affordance would be dead code")
	}

	// Re-check must reuse the banner's endpoint, not grow a second one.
	if !strings.Contains(html, "async function forgeAppRecheck") {
		t.Fatal("static/index.html has no forgeAppRecheck function")
	}
	fi := strings.Index(html, "async function forgeAppRecheck")
	fend := strings.Index(html[fi:], "\n    }")
	fbody := html[fi : fi+fend]
	if !strings.Contains(fbody, "/api/github-app/recheck") {
		t.Error("forgeAppRecheck must POST the existing /api/github-app/recheck endpoint")
	}
}
