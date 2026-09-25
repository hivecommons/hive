package hub

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var (
	styleBlockRe   = regexp.MustCompile(`(?s)<style[^>]*>(.*?)</style>`)
	rootFontRuleRe = regexp.MustCompile(`(?s)(?:^|[\s,}])(?:html|body|:root)\s*(?:,[^{]*)?\{[^}]*font(?:-family)?\s*:`)
)

// TestHubPagesSetBaseFont guards against the regression where a hub page's
// body rule lost its font-family during the design-token migration and the
// page fell back to the browser's default serif font. The shared
// components.css deliberately sets no base font, so every full hub page must
// set one on html, body, or :root.
func TestHubPagesSetBaseFont(t *testing.T) {
	var pages []string
	for _, pattern := range []string{"assets/*.html", "static/*.html"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		pages = append(pages, matches...)
	}
	if len(pages) == 0 {
		t.Fatal("no hub pages found")
	}
	for _, page := range pages {
		raw, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("reading %s: %v", page, err)
		}
		blocks := styleBlockRe.FindAllSubmatch(raw, -1)
		if len(blocks) == 0 {
			continue
		}
		found := false
		for _, b := range blocks {
			if rootFontRuleRe.Match(b[1]) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: no font-family on html, body or :root; the page will render in the browser's default serif font (use font-family: var(--font-ui))", page)
		}
	}
}
