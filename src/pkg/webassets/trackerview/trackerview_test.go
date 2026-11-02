package trackerview

import (
	"io/fs"
	"strings"
	"testing"
)

// TestFilesMatchEmbed pins that Files names exactly the embedded assets.
func TestFilesMatchEmbed(t *testing.T) {
	embedded, err := fs.Glob(FS, "*.js")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, name := range Files {
		listed[name] = true
	}
	for _, name := range embedded {
		if !listed[name] {
			t.Errorf("%s is embedded but not listed in Files", name)
		}
	}
	if len(embedded) != len(Files) {
		t.Errorf("embedded %v, Files %v", embedded, Files)
	}
}

// TestAssetDefinesViewHelpersAndStaysPure pins the helpers the hub page calls
// and that the asset touches no page state: it is parsing and arithmetic only.
func TestAssetDefinesViewHelpersAndStaysPure(t *testing.T) {
	b, err := fs.ReadFile(FS, "tracker-view.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, d := range []string{
		"function stageTrackerDefaultFilter() {",
		"function stageTrackerNormalizeFilter(raw) {",
		"function stageTrackerFilterFromUrl(href) {",
		"function stageTrackerViewUrl(href, filter, group) {",
		"function stageTrackerParsePlaylist(href) {",
		"function stageTrackerPlaylistIndex(startMs, nowMs, intervalMs, n) {",
		"function stageTrackerPlaylistRemainingMs(startMs, nowMs, intervalMs) {",
		"function stageTrackerPlaylistNextLabel(remainingMs) {",
	} {
		if !strings.Contains(src, d) {
			t.Errorf("tracker-view.js does not define %q", d)
		}
	}
	for _, banned := range []string{"data-action", "addEventListener", "fetch(", "localStorage", "sessionStorage", "document.", "window.", "history."} {
		if strings.Contains(src, banned) {
			t.Errorf("tracker-view.js contains %q; the shared view asset must stay pure", banned)
		}
	}
}
