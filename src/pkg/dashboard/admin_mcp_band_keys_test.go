package dashboard

import (
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

// adminmcp cannot import this package, so issues_by_band and prs_by_band carry
// their own copy of the band keys. The copy must be every band the Overview
// export can answer with: when it missed "outside", the tool listed that band
// in bands[] and refused it as a filter.
func TestAdminMCPBandKeysAreTheBandsTheOverviewExportAnswersWith(t *testing.T) {
	want := map[string][]string{
		adminmcp.ToolIssuesByBand: append(append([]string{}, issueBandOrder...), overviewBandOutside),
		adminmcp.ToolPrsByBand:    append([]string{}, prBandOrder...),
	}
	for _, tool := range adminmcp.Tools() {
		name, _ := tool["name"].(string)
		keys, ok := want[name]
		if !ok {
			continue
		}
		props, _ := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		band, _ := props["band"].(map[string]any)
		got, _ := band["enum"].([]string)
		if !reflect.DeepEqual(got, keys) {
			t.Errorf("%s accepts bands %v, the Overview export answers with %v", name, got, keys)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("tool %s not found", name)
	}
}
