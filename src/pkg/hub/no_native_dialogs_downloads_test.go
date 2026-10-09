package hub

import (
	"regexp"
	"strings"
	"testing"
)

// TestHubUIHasNoNativeDialogsOrForcedDownloads keeps native dialogs and
// synthesized browser downloads out of both hub UI sources (#11263).
func TestHubUIHasNoNativeDialogsOrForcedDownloads(t *testing.T) {
	staticHTML, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	sources := map[string]string{
		"dashboard.html": dashboardHTML,
		"index.html":     string(staticHTML),
	}
	native := regexp.MustCompile(`(^|[^\w.])(window\.)?(prompt|alert|confirm)\s*\(`)
	download := regexp.MustCompile(`\.download\s*=|createObjectURL\s*\(`)
	for name, src := range sources {
		for i, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "//") {
				continue
			}
			if native.MatchString(line) {
				t.Errorf("%s:%d: native browser dialog is forbidden; use hivePrompt/hiveConfirm/hiveToast: %s", name, i+1, truncate(trimmed, 100))
			}
			if download.MatchString(line) {
				t.Errorf("%s:%d: forced browser download is forbidden; render in-app with showTextExport: %s", name, i+1, truncate(trimmed, 100))
			}
		}
	}
}
