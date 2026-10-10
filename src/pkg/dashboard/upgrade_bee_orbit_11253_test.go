package dashboard

import (
	"strings"
	"testing"
)

func TestUpgradeBeeOrbitSharedImplementation11253(t *testing.T) {
	html := indexHTML(t)

	if got := strings.Count(html, "@keyframes ocBeeOrbit"); got != 1 {
		t.Fatalf("expected exactly one ocBeeOrbit keyframes block, got %d", got)
	}
	orbit := cssKeyframesBody(t, html, "ocBeeOrbit")
	for _, want := range []string{
		"0% { opacity: 0.96; transform: translate3d(-50%, -50%, 0) translate3d(var(--oc-version-bee-orbit-offset-x, 0), var(--oc-version-bee-orbit-offset-y, 0), 0) rotate(0turn);",
		"100% { opacity: 0.96; transform: translate3d(-50%, -50%, 0) translate3d(var(--oc-version-bee-orbit-offset-x, 0), var(--oc-version-bee-orbit-offset-y, 0), 0) rotate(1turn);",
	} {
		if !strings.Contains(orbit, want) {
			t.Fatalf("bee orbit keyframes should loop a seamless 360 degrees, missing %q in %s", want, orbit)
		}
	}

	orbitRule := cssRule(t, html, ".oc-version-bee-orbit")
	for _, want := range []string{"left: 50%", "top: 50%", "transform-origin: 50% 50%", "animation-name: ocBeeOrbit", "animation-timing-function: linear", "animation-iteration-count: infinite"} {
		if !strings.Contains(orbitRule, want) {
			t.Fatalf("shared orbit wrapper missing %q in %s", want, orbitRule)
		}
	}
	if strings.Count(html, "animation-name: ocBeeOrbit") != 1 {
		t.Fatal("only the shared orbit class may run ocBeeOrbit")
	}

	hiveRule := cssRule(t, html, ".oc-version-hive")
	for _, want := range []string{
		"--oc-version-hive-size: 28px",
		"--oc-version-bee-size: 16px",
		"--oc-version-bee-radius: 9px",
		"--oc-version-bee-orbit-offset-x: 8px",
		"--oc-version-bee-orbit-offset-y: -7px",
	} {
		if !strings.Contains(hiveRule, want) {
			t.Fatalf("hive rule should derive bee size/radius from the hive size, missing %q in %s", want, hiveRule)
		}
	}
	beeRule := cssRule(t, html, ".oc-version-bee")
	if !strings.Contains(beeRule, "translate3d(var(--oc-version-bee-radius, 9px), 0, 0) rotate(90deg)") {
		t.Fatalf("bee should sit at translateX(radius) inside the rotating wrapper: %s", beeRule)
	}
	glyphRule := cssRule(t, html, ".oc-version-bee-glyph")
	if !strings.Contains(glyphRule, "width: var(--oc-version-bee-size)") || !strings.Contains(glyphRule, "height: auto") {
		t.Fatalf("missing base bee SVG size guard: %s", glyphRule)
	}
	// Sites may only change the hive size, never bee geometry.
	if strings.Contains(html, ".oc-version-navbar-upgrade .oc-version-bee") {
		t.Fatal("navbar upgrade must not carry site-specific bee geometry")
	}

	helper := jsFunc(t, html, "versionUpgradeHiveHTML")
	if got := strings.Count(helper, `oc-version-bee-orbit oc-version-bee--phase-`); got != 3 {
		t.Fatalf("shared hive helper should render three evenly phased orbit wrappers, got %d", got)
	}
	for _, fn := range []string{"versionBeeProgressHTML", "versionNavbarUpgradeHTML"} {
		if !strings.Contains(jsFunc(t, html, fn), "versionUpgradeHiveHTML(key)") {
			t.Fatalf("%s must use the shared hive helper", fn)
		}
	}
	if got := strings.Count(html, "versionUpgradeHiveHTML("); got != 3 {
		t.Fatalf("expected helper definition plus two call sites, got %d references", got)
	}
}

func TestUpgradeBeeRefreshKeepsBeeNodes11253(t *testing.T) {
	html := indexHTML(t)
	helper := jsFunc(t, html, "versionSetHTMLPreservingUpgradeBee")
	for _, want := range []string{
		"currentTitle.innerHTML = nextTitle.innerHTML",
		"currentStatus.innerHTML = nextStatus.innerHTML",
		"nextHive.replaceWith(currentHive)",
	} {
		if !strings.Contains(helper, want) {
			t.Fatalf("refresh path should keep existing bee nodes; missing %q in:\n%s", want, helper)
		}
	}
	if strings.Contains(helper, "currentHive.innerHTML") || strings.Contains(helper, "oc-version-bee-orbit") {
		t.Fatal("refresh path must not rebuild orbiting bee nodes")
	}
}
