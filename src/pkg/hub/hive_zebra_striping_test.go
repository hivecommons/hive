package hub

import (
	"strings"
	"testing"
)

// TestSpokeTableHasZebraStriping guards the alternating-row-shading affordance
// on the fleet spoke table: odd hive rows (by render index) get the
// .hive-row-alt class, driven from buildRow's index parameter (NOT CSS
// nth-child, which would mis-phase a hive's optional second <tr>). A subtle
// currentColor-alpha stripe helps the eye track one hive's cells across the
// wide table, in both light and dark themes.
func TestSpokeTableHasZebraStriping(t *testing.T) {
	mustContain := []string{
		// The stripe style exists, keyed off the per-row class.
		".hive-table tr.hive-row-alt td { background:",
		// The main hive row is stamped on odd indices.
		`((i % 2 === 1) ? ' class="hive-row-alt"' : '')`,
	}
	for _, s := range mustContain {
		if !strings.Contains(dashboardHTML, s) {
			t.Errorf("dashboardHTML missing zebra-striping snippet %q", s)
		}
	}
	// The stripe must be declared BEFORE the hover rule so :hover wins.
	altIdx := strings.Index(dashboardHTML, ".hive-table tr.hive-row-alt td")
	hoverIdx := strings.Index(dashboardHTML, ".hive-table tr:hover td")
	if altIdx < 0 || hoverIdx < 0 || altIdx > hoverIdx {
		t.Errorf("zebra stripe rule must be declared before the :hover rule so hover overrides the stripe (alt=%d hover=%d)", altIdx, hoverIdx)
	}
}
