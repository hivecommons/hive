package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// The --sp-* ramp is supplied globally by tokens.css (:root), which head.html
// links before the inline stylesheet, so the ~300 var(--sp-*) usages outside
// .config-modal resolve. The copy inside .config-modal is a deliberate pin so a
// hovered theme preview cannot resize the settings modal (#15920 was filed on
// the false premise that the ramp was modal-only); that pin is asserted by
// TestAppearanceThemeHoverCannotResizeSettingsModal in theme_ui_test.go.
func TestSpacingRampSuppliedByTokensOnRoot(t *testing.T) {
	tb, err := staticFS.ReadFile("static/tokens.css")
	if err != nil {
		t.Fatalf("read tokens.css: %v", err)
	}
	rootRule := regexp.MustCompile(`(?s):root\s*\{(.*?)\n\}`).FindStringSubmatch(string(tb))
	if rootRule == nil {
		t.Fatal("tokens.css has no :root rule")
	}
	for _, want := range []string{
		"--sp-0: 0;", "--sp-1: 2px;", "--sp-2: 4px;", "--sp-3: 6px;", "--sp-4: 8px;",
		"--sp-5: 12px;", "--sp-6: 16px;", "--sp-7: 20px;", "--sp-8: 24px;", "--sp-9: 32px;",
	} {
		if !strings.Contains(rootRule[1], want) {
			t.Errorf("tokens.css :root lacks %s", want)
		}
	}

	html := indexHTML(t)
	link := strings.Index(html, `<link rel="stylesheet" href="/tokens.css">`)
	style := strings.Index(html, "<style>")
	if link < 0 || style < 0 || link > style {
		t.Fatalf("tokens.css link (%d) must precede the inline <style> (%d)", link, style)
	}
}
