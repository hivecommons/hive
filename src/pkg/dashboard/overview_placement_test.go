package dashboard

import (
	"strings"
	"testing"
)

// The Overview card is the first thing an operator should see, so it renders
// above the Governor panel and has its own entry in the sidebar's Overview
// group. Its legend keeps each count next to its band name instead of pushing
// it to the far edge of the card.
func TestOverviewPlacementAndNav(t *testing.T) {
	html := indexHTML(t)

	section := strings.Index(html, `id="overview-section"`)
	governor := strings.Index(html, `<div class="governor" id="governor">`)
	if section < 0 || governor < 0 {
		t.Fatalf("overview-section=%d governor=%d: markup missing", section, governor)
	}
	if section > governor {
		t.Errorf("overview-section (offset %d) must render above #governor (offset %d)", section, governor)
	}

	for _, want := range []string{
		`data-section="overview-section" data-action="ocNavigate" data-arg0="overview-section"`,
		`if (item.getAttribute('data-section') === 'governor') hide(item);`,
		`.overview-chart-legend-row { display: grid; grid-template-columns: auto minmax(0, max-content) max-content; justify-content: start;`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}

	if strings.Contains(html, `if (g.querySelector('.oc-nav-item[data-section="governor"]')) hide(g);`) {
		t.Errorf("ACMM gate must hide only the Governor nav item, not the whole Overview nav group")
	}
}
