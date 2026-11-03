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
