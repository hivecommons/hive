package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// The --sp-* ramp must live on :root: ~300 var(--sp-*) usages (and --modal-gap
// on :root itself) sit outside .config-modal and resolve to invalid otherwise.
func TestSpacingRampDeclaredOnRoot(t *testing.T) {
	html := indexHTML(t)
	root := regexp.MustCompile(`(?s):root\s*\{[^}]*--sp-8:\s*24px`)
	if !root.MatchString(html) {
		t.Fatal("--sp-* ramp is not declared inside a :root rule")
	}
	if i := strings.Index(html, ".config-modal {"); i >= 0 {
		end := strings.Index(html[i:], "}")
		if strings.Contains(html[i:i+end], "--sp-0:") {
			t.Fatal("--sp-* ramp is still duplicated inside .config-modal")
		}
	}
}
