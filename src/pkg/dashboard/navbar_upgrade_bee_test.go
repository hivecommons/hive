package dashboard

import (
	"strings"
	"testing"
)

func TestNavbarUpgradeBeeMarkupAndSharedRenderPath(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="oc-navbar-upgrade"`,
		`class="nav-chip nav-chip--status nav-chip--warn oc-navbar-upgrade"`,
		`data-action="focusVersionBlock"`,
		`.oc-navbar-upgrade`,
		`.oc-version-navbar-upgrade`,
		`--oc-version-bee-lane`,
		`padding-left: var(--oc-version-bee-lane)`,
		`Upgrade complete ✓`,
		`function renderNavbarUpgradeIndicator`,
		`function focusVersionBlock`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("navbar upgrade bee missing %q", want)
		}
	}

	surfaces := jsFunc(t, html, "renderVersionSurfaces")
	for _, want := range []string{
		"renderVersionChip(v, ctx);",
		"renderVersionMenu(v, ctx);",
		"renderNavbarUpgradeIndicator(v, ctx);",
	} {
		if !strings.Contains(surfaces, want) {
			t.Fatalf("renderVersionSurfaces does not call %q with the shared version context:\n%s", want, surfaces)
		}
	}

	fetch := jsFunc(t, html, "fetchGitVersion")
	if !strings.Contains(fetch, "renderVersionSurfaces(v, window._lastVersionContext);") {
		t.Fatal("fetchGitVersion no longer updates sidebar and navbar upgrade surfaces from the same render call")
	}
	if strings.Contains(fetch, "renderVersionChip(v, window._lastVersionContext)") || strings.Contains(fetch, "renderVersionMenu(v, window._lastVersionContext)") {
		t.Fatal("fetchGitVersion should not update sidebar version renderers separately from the navbar upgrade renderer")
	}

	tick := jsFunc(t, html, "tickNavbarClock")
	if !strings.Contains(tick, "refreshVersionProgressSurfaces === 'function'") || !strings.Contains(tick, "refreshVersionProgressSurfaces();") {
		t.Fatal("navbar clock tick should refresh elapsed upgrade text through the shared version surface renderer")
	}
}

func TestUpgradeBeeHostsDoNotClipOrbit(t *testing.T) {
	html := indexHTML(t)
	for _, selector := range []string{
		".oc-version-upgrade-progress",
		".oc-version-hive",
		".oc-version-chip.is-upgrading",
		".oc-navbar-upgrade",
		".oc-version-navbar-upgrade",
	} {
		rule := cssRule(t, html, selector)
		if strings.Contains(rule, "overflow: hidden") {
			t.Fatalf("%s clips the upgrade bee orbit: %s", selector, rule)
		}
		if !strings.Contains(rule, "overflow: visible") {
			t.Fatalf("%s should keep the upgrade bee orbit visible: %s", selector, rule)
		}
	}

	hiveRule := cssRule(t, html, ".oc-version-hive")
	if !strings.Contains(hiveRule, "z-index: 2") {
		t.Fatalf("bee hive host must stay above tile backgrounds: %s", hiveRule)
	}
	beeRule := cssRule(t, html, ".oc-version-bee")
	if !strings.Contains(beeRule, "z-index: 3") {
		t.Fatalf("orbiting bees must stack above their host: %s", beeRule)
	}

	copyRule := cssRule(t, html, ".oc-version-navbar-upgrade .oc-version-progress-copy")
	for _, want := range []string{"overflow: hidden", "white-space: nowrap"} {
		if !strings.Contains(copyRule, want) {
			t.Fatalf("navbar upgrade copy should retain text clipping %q in %s", want, copyRule)
		}
	}
	titleStatusRule := cssRule(t, html, ".oc-version-navbar-upgrade .oc-version-progress-title, .oc-version-navbar-upgrade .oc-version-progress-status")
	for _, want := range []string{"overflow: hidden", "text-overflow: ellipsis", "white-space: nowrap"} {
		if !strings.Contains(titleStatusRule, want) {
			t.Fatalf("navbar upgrade text should retain ellipsis %q in %s", want, titleStatusRule)
		}
	}
}

func TestNavbarUpgradeBeeDoesNotAddBareSetInterval(t *testing.T) {
	html := indexHTML(t)
	if got := strings.Count(html, "setInterval("); got > 11 {
		t.Fatalf("static/index.html has %d setInterval( occurrences; navbar upgrade bee must reuse existing ticks", got)
	}
	if strings.Contains(html, "setInterval(function() { refreshVersionProgressSurfaces") || strings.Contains(html, "setInterval(refreshVersionProgressSurfaces") {
		t.Fatal("navbar upgrade bee added its own interval instead of reusing the navbar clock tick")
	}
}
