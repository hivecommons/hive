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

func TestNavbarUpgradePillCannotCoverCenterAgents(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`--topbar-grid-columns: auto minmax(0, 1fr) auto;`,
		`.oc-topbar-center { container: navbar-center / inline-size; position: relative; z-index: 1; justify-self: stretch;`,
		`.oc-topbar-right { position: relative; z-index: 2;`,
		`flex-wrap: nowrap;`,
		`.oc-navbar-upgrade { --oc-version-bee-lane: 44px; flex: 0 1 min(260px, 24vw); min-width: 0; max-width: min(260px, 24vw);`,
		`.agent-navbar-upnext::after { content: attr(data-agent-summary);`,
		`@container navbar-center (max-width: 560px)`,
		`.agent-navbar-tile .agent-tile-next { display: none; }`,
		`@container navbar-center (max-width: 340px)`,
		`.agent-navbar-upnext .agent-navbar-tile { display: none; }`,
		`@media (max-width: 900px) { .agent-navbar-upnext { display: none !important; } }`,
		`wrap.dataset.agentSummary = list.length ? String(list.length) + ' agents ▾' : '';`,
		`const detail = versionUpgradeProgressStatus(v || {}, progress);`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("navbar upgrade collision guard missing %q", want)
		}
	}
	if strings.Contains(html, `@media (max-width: 1280px) { .agent-navbar-upnext { display: none !important; } }`) {
		t.Fatal("agent navbar strip must not disappear at 1280px; center container queries own collision handling")
	}
}

func TestUpgradeBeeSVGKeepsTwoToneTokenPalette(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`.oc-version-bee-wing`,
		`.oc-version-bee-body`,
		`.oc-version-bee-stripe`,
		`.oc-version-bee-head`,
		`.oc-version-bee-stinger`,
		`fill: var(--amber)`,
		`stroke: var(--terminal-bg)`,
		`color-mix(in srgb, var(--panel) 80%, transparent)`,
		`function versionBeeGlyphHTML()`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("upgrade bee two-tone token palette missing %q", want)
		}
	}
	glyph := jsFunc(t, html, "versionBeeGlyphHTML")
	for _, want := range []string{
		`class="oc-version-bee-wing"`,
		`class="oc-version-bee-body"`,
		`class="oc-version-bee-stripe"`,
		`class="oc-version-bee-head"`,
		`class="oc-version-bee-stinger"`,
	} {
		if !strings.Contains(glyph, want) {
			t.Fatalf("upgrade bee SVG no longer exposes %q for token styling:\n%s", want, glyph)
		}
	}
	if strings.Contains(glyph, "currentColor") {
		t.Fatalf("upgrade bee SVG must not collapse to one-color currentColor styling:\n%s", glyph)
	}
}

func TestUpgradeBeeOrbitLoopIsSeamless(t *testing.T) {
	html := indexHTML(t)
	beeRule := cssRule(t, html, ".oc-version-bee")
	for _, want := range []string{
		"--oc-version-bee-duration: var(--bee-orbit-duration, 11s)",
		"animation-duration: var(--oc-version-bee-duration)",
		"animation-timing-function: linear",
		"animation-iteration-count: infinite",
		"animation-fill-mode: both",
		"will-change: transform",
		"transform-box: border-box",
		"contain: layout paint style",
	} {
		if !strings.Contains(beeRule, want) {
			t.Fatalf("orbiting bee rule missing seamless animation hint %q in %s", want, beeRule)
		}
	}
	for _, selector := range []string{".oc-version-bee--phase-b", ".oc-version-bee--phase-c"} {
		rule := cssRule(t, html, selector)
		if !strings.Contains(rule, "animation-delay: calc(var(--oc-version-bee-duration)") {
			t.Fatalf("%s should phase the persistent bee node without touching JS render state: %s", selector, rule)
		}
	}
	glyphRule := cssRule(t, html, ".oc-version-bee-glyph")
	for _, want := range []string{"transform-box: fill-box", "transform-origin: 50% 50%"} {
		if !strings.Contains(glyphRule, want) {
			t.Fatalf("bee SVG glyph should anchor transforms around its own box with %q in %s", want, glyphRule)
		}
	}

	orbit := cssKeyframesBody(t, html, "ocBeeOrbit")
	for _, want := range []string{
		"0% { opacity: 0.96; transform: translate3d(-50%, -50%, 0) rotate(0turn) translate3d(var(--oc-version-bee-radius, 17px), 0, 0);",
		"100% { opacity: 0.96; transform: translate3d(-50%, -50%, 0) rotate(1turn) translate3d(var(--oc-version-bee-radius, 17px), 0, 0);",
	} {
		if !strings.Contains(orbit, want) {
			t.Fatalf("upgrade bee orbit keyframes missing seamless endpoint %q in %s", want, orbit)
		}
	}
	for _, forbidden := range []string{"\n      50%", "rotate(-"} {
		if strings.Contains(orbit, forbidden) {
			t.Fatalf("upgrade bee orbit should avoid loop-boundary decomposition hitches from %q in %s", forbidden, orbit)
		}
	}
}

func TestUpgradeBeeTopbarRenderPatchesTextInPlace(t *testing.T) {
	html := indexHTML(t)
	helper := jsFunc(t, html, "versionSetHTMLPreservingUpgradeBee")
	for _, want := range []string{
		"currentProgress.parentNode === el",
		"currentTitle.innerHTML = nextTitle.innerHTML",
		"currentStatus.innerHTML = nextStatus.innerHTML",
	} {
		if !strings.Contains(helper, want) {
			t.Fatalf("upgrade bee topbar refresh should patch text in place without replacing the bee wrapper; missing %q in:\n%s", want, helper)
		}
	}
	chip := jsFunc(t, html, "renderVersionChip")
	navbar := jsFunc(t, html, "renderNavbarUpgradeIndicator")
	for _, fn := range []struct {
		name string
		body string
	}{
		{"renderVersionChip", chip},
		{"renderNavbarUpgradeIndicator", navbar},
	} {
		if !strings.Contains(fn.body, "versionSetHTMLPreservingUpgradeBee(") {
			t.Fatalf("%s must use the preserving render path:\n%s", fn.name, fn.body)
		}
	}
	layout := jsFunc(t, html, "applyLayout")
	for _, forbidden := range []string{"innerHTML", "replaceChildren", "oc-topbar"} {
		if forbidden == "oc-topbar" {
			if !strings.Contains(layout, "document.getElementById('oc-topbar')") || !strings.Contains(layout, "topbar.style.display") {
				t.Fatalf("applyLayout should only show the existing topbar, not rebuild it:\n%s", layout)
			}
			continue
		}
		if strings.Contains(layout, forbidden) {
			t.Fatalf("applyLayout must not rebuild topbar/upgrade ancestors via %s:\n%s", forbidden, layout)
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
