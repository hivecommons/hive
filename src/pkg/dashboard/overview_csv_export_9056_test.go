package dashboard

import (
	"strings"
	"testing"
)

func TestOverviewCSVExportStaticWiring9056(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`class="hv-btn btn-sm overview-export-csv" href="${esc(overviewExportURL(csvKind))}"`,
		`class="overview-band-download" href="${esc(overviewExportURL(kind, s.key))}"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing export link %q", want)
		}
	}
	for _, gone := range []string{"downloadOverviewCsv", "overviewCsvRows", "function issueBandInfo", "function prBandInfo"} {
		if strings.Contains(html, gone) {
			t.Errorf("browser still contains %q", gone)
		}
	}
}
