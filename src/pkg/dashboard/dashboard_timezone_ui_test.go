package dashboard

import (
	"strings"
	"testing"
)

func TestDashboardTimezonePreferenceControlsExist(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"DASHBOARD_TIMEZONE_STORAGE_KEY",
		"hive-dashboard-display-timezone",
		"id=\"dashboard-timezone-settings\"",
		"id=\"dashboard-timezone-select\"",
		"Browser local (default)",
		"Hive server",
		"id=\"dashboard-timezone-custom\"",
		"Intl.supportedValuesOf('timeZone')",
		"function formatDashboardDateTime(",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static/index.html missing timezone UI/helper marker %q", want)
		}
	}
}

func TestDashboardDatesUseSharedTimezoneHelpers(t *testing.T) {
	html := indexHTML(t)
	for _, banned := range []string{"toLocaleString", "toLocaleTimeString", "toLocaleDateString"} {
		if strings.Contains(html, banned) {
			t.Fatalf("static/index.html still calls %s directly; use the shared dashboard timezone helpers", banned)
		}
	}
}
