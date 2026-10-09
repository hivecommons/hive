package dashboard

import (
	"strings"
	"testing"
)

func TestOverviewCSVExportStaticWiring9056(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`class="hv-btn btn-icon btn-sm overview-export-csv overview-chart-download" data-action="overviewOpenExport"`,
		`data-arg0="${esc(overviewExportURL(csvKind))}" title="${esc(csvTitle)}" aria-label="${esc(csvTitle)}"`,
		`class="overview-band-download" data-action="overviewOpenExport"`,
		`data-arg0="${esc(overviewExportURL(kind, s.key))}"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing export link %q", want)
		}
	}
	if strings.Contains(html, ">Export CSV</a>") || strings.Contains(html, ">Export CSV</button>") {
		t.Errorf("chart-card export action should be icon-only")
	}
	for _, gone := range []string{"downloadOverviewCsv", "overviewCsvRows", "function issueBandInfo", "function prBandInfo"} {
		if strings.Contains(html, gone) {
			t.Errorf("browser still contains %q", gone)
		}
	}
}
