package dashboard

import (
	"strings"
	"testing"
)

// TestNavbarRightClusterCannotSpillOverTitle guards #13135: between the
// phone block (≤768px) and 1400px the right chip cluster must keep its
// max-content width, otherwise its flex-end overflow paints the feedback and
// theme buttons over the ACMM badge or project title.
func TestNavbarRightClusterCannotSpillOverTitle(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"@media (min-width: 769px) and (max-width: 1400px) {\n      .oc-topbar-right { min-width: max-content; }",
		"@media (min-width: 769px) and (max-width: 900px) {\n      #acmm-badge { display: none; }",
		"#welcome-topbar-btn .nav-chip__label { display: none; }\n    }",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("navbar #13135 overlap guard missing %q", want)
		}
	}
	if strings.Contains(html, "@media (min-width: 901px) and (max-width: 1400px) {\n      .oc-topbar-right") {
		t.Fatal("right cluster min-width guard must also cover 769-900px, where the dashboard was still overlapping")
	}
}

// TestNavbarChipsGoIconOnlyWhenTheBarIsNarrow guards the tablet case: the bar
// is the viewport minus the sidebar, so a wide sidebar on a 1180px iPad in
// landscape left an ~860px bar where the viewport media query above never
// fired. The full-width right cluster squeezed the title and ACMM badge into
// ~44px and WebKit drew them over the chips (and over the ☾ dark-mode
// button). The same icon-only rule therefore also keys on the bar's own width.
func TestNavbarChipsGoIconOnlyWhenTheBarIsNarrow(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".oc-topbar { container: oc-topbar / inline-size; }",
		"@container oc-topbar (max-width: 1000px) {\n      .oc-health-badge .nav-chip__label,\n      #oc-settings-btn .nav-chip__label,\n      #oc-subscription-gauge .nav-chip__label,\n      #welcome-topbar-btn .nav-chip__label { display: none; }",
		"@container oc-topbar (max-width: 760px) {\n      #acmm-badge { display: none; }",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("narrow-bar icon-only rule missing %q", want)
		}
	}
}
