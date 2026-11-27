package hub

import (
	"strings"
	"testing"
)

// TestFmtUserTSUsesViewerTimeZone pins #20190: fmtUserTS used to format every
// timestamp in America/New_York and append a literal ' EDT', which is wrong for
// viewers outside New York and wrong for New York itself half the year. It must
// format in the viewer's zone and take the zone name from the formatter.
//
// String-anchored on the raw dashboardHTML, same as the other JS structure
// tests (see TestOfflineStatusDotCarriesTooltip).
func TestFmtUserTSUsesViewerTimeZone(t *testing.T) {
	start := strings.Index(dashboardHTML, "function fmtUserTS(ts) {")
	if start < 0 {
		t.Fatal("fmtUserTS not found in dashboard.html")
	}
	end := strings.Index(dashboardHTML[start:], "\n    }\n")
	if end < 0 {
		t.Fatal("could not find the end of fmtUserTS")
	}
	body := dashboardHTML[start : start+end]

	if strings.Contains(body, "America/New_York") {
		t.Error("fmtUserTS still pins timeZone to America/New_York; it must use the viewer's time zone")
	}
	if strings.Contains(body, "' EDT'") {
		t.Error("fmtUserTS still appends a hardcoded ' EDT' suffix; the zone must come from the formatter")
	}
	if !strings.Contains(body, "timeZoneName:'short'") {
		t.Error("fmtUserTS must ask the formatter for the zone name with timeZoneName:'short'")
	}
}
