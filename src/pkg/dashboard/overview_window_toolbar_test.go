package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestOverviewWindowPillsUseSharedInFlowToolbar(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		`.window-pills-toolbar { grid-column: 1 / -1; justify-self: end; max-width: 100%; flex-wrap: wrap; justify-content: flex-end; }`,
		`.window-pills-toolbar { justify-self: start; justify-content: flex-start; }`,
		`function overviewKPIWindowControls()`,
		`class="gov-pr-models-toggle window-pills-toolbar overview-window-toolbar"`,
		`data-action="setOverviewKPIWindow"`,
		`<div class="overview-headline sec-headline" data-collapsed-keep>`,
		`<div class="overview-kpis">`,
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("Overview window toolbar missing snippet %q", snippet)
		}
	}
	for _, banned := range []string{
		`.overview-kpi-range { position: absolute;`,
		`.overview-kpis { position: relative;`,
	} {
		if strings.Contains(html, banned) {
			t.Fatalf("Overview KPI window controls must be in normal flow, found %q", banned)
		}
	}
	render := jsFunc(t, html, "renderOverviewKPIs")
	toolbar := strings.Index(render, "${overviewKPIWindowControls()}")
	grid := strings.Index(render, `<div class="overview-kpis">`)
	if toolbar < 0 || grid < 0 || toolbar > grid {
		t.Fatalf("Overview KPI toolbar must render before the tile grid: toolbar=%d grid=%d", toolbar, grid)
	}
}

func TestOverviewWindowControlsPreserveBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Overview window controls were not executed")
	}
	html := indexHTML(t)
	script := `const assert = require('node:assert/strict');
const OVERVIEW_KPI_WINDOWS = { '24h': 24 * 3600e3, '7d': 7 * 24 * 3600e3 };
let _overviewKPIWindow = '7d';
function esc(v) { return String(v).replace(/[&<>"]/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[ch])); }
` + jsFunc(t, html, "overviewKPIWindowControls") + `
const out = overviewKPIWindowControls();
assert.match(out, /class="gov-pr-models-toggle window-pills-toolbar overview-window-toolbar"/);
assert.match(out, /data-stop="1"/);
assert.match(out, /data-action="setOverviewKPIWindow"/);
assert.match(out, /data-arg0="24h"[^>]*>24h<\/button>/);
assert.match(out, /class="active"[^>]*data-action="setOverviewKPIWindow"[^>]*data-arg0="7d"/);
assert.match(out, /aria-pressed="true"[^>]*>7d<\/button>/);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node Overview window controls failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
