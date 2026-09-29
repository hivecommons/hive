package dashboard

import (
	"strings"
	"testing"
)

func TestOverviewCSVExportStaticWiring9056(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`class="hv-btn btn-icon btn-sm overview-export-csv overview-chart-download" href="${esc(overviewExportURL(csvKind))}"`,
		`title="Download CSV" aria-label="${esc(csvTitle)}"`,
		`class="overview-band-download" href="${esc(overviewExportURL(kind, s.key))}"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing export link %q", want)
		}
	}
	if strings.Contains(html, ">Export CSV</a>") {
		t.Errorf("chart-card export action should be icon-only")
	}
	for _, gone := range []string{"downloadOverviewCsv", "overviewCsvRows", "function issueBandInfo", "function prBandInfo"} {
		if strings.Contains(html, gone) {
			t.Errorf("browser still contains %q", gone)
		}
	}
}
