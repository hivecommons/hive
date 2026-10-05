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

func TestNavbarUpgradeBeeDoesNotAddBareSetInterval(t *testing.T) {
	html := indexHTML(t)
	if got := strings.Count(html, "setInterval("); got > 11 {
		t.Fatalf("static/index.html has %d setInterval( occurrences; navbar upgrade bee must reuse existing ticks", got)
	}
	if strings.Contains(html, "setInterval(function() { refreshVersionProgressSurfaces") || strings.Contains(html, "setInterval(refreshVersionProgressSurfaces") {
		t.Fatal("navbar upgrade bee added its own interval instead of reusing the navbar clock tick")
	}
}
