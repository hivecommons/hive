package webstatic

import (
	"os"
	"strings"
	"testing"
)

// TestFleetRunFigureIsInVisibleGovernorGrid pins that the fleet
// "running N · $X this hour" figure renders inside the Governor section's
// gov-grid, not only in the compact .oc-gov-strip (display:none), where it
// first landed and was never on screen.
func TestFleetRunFigureIsInVisibleGovernorGrid(t *testing.T) {
	body, err := os.ReadFile("../static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	grid := strings.Index(html, `<div class="gov-grid sec-headline"`)
	if grid < 0 {
		t.Fatal("governor gov-grid not found")
	}
	end := strings.Index(html[grid:], `<div class="temp-gauge"`)
	if end < 0 {
		t.Fatal("governor gauge not found after gov-grid")
	}
	if !strings.Contains(html[grid:grid+end], "${fleetRunStatHtml()}") {
		t.Fatal("fleetRunStatHtml() is not rendered inside the Governor gov-grid")
	}
	if !strings.Contains(html, `id="gov-fleet-run"`) || !strings.Contains(html, `class="gov-stat fleet-run-stat" data-fleet-cost-host title="RUNNING`) {
		t.Fatal("fleet run gov-stat markup missing")
	}
}
