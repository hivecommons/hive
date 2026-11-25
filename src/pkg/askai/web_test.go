package askai

import (
	"regexp"
	"strings"
	"testing"
)

// TestPanelRendersBackendTextAsTextOnly pins the panel's rendering contract:
// whatever the backend returns goes into the page through textContent, never
// as markup, and the script talks only to this dashboard's own backend.
func TestPanelRendersBackendTextAsTextOnly(t *testing.T) {
	js := string(PanelJS)
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "kapa.ai", "http://"} {
		if strings.Contains(js, banned) {
			t.Errorf("panel script uses %q", banned)
		}
	}
	if regexp.MustCompile(`\.on[a-z]+\s*=`).MatchString(js) || regexp.MustCompile(`setAttribute\('on`).MatchString(js) {
		t.Error("panel script sets an inline event handler; it must run under script-src-attr 'none'")
	}
	// The only URL literal is the https check on allowlisted sources.
	if n := strings.Count(js, "https:"); n != 1 {
		t.Errorf("panel script names https %d times, want only the source check", n)
	}
	if !strings.Contains(js, "fetch(base + path") || strings.Count(js, "fetch(") != 1 {
		t.Error("the panel must have exactly one fetch, to its own backend base")
	}
}

// TestPanelAccessibilityContract checks the markup the design requires:
// a named region, a real button opener with aria-expanded, Escape closes and
// returns focus, a labelled input, a polite live region, a busy state, and
// the "AI generated" label as text.
func TestPanelAccessibilityContract(t *testing.T) {
	js := string(PanelJS)
	for _, want := range []string{
		"setAttribute('role', 'region')",
		"'aria-labelledby': 'hive-ask-title'",
		"'aria-expanded'",
		"'aria-controls': PANEL_ID",
		"e.key === 'Escape'",
		"opener.focus()",
		"for: 'hive-ask-input'",
		"'aria-live': 'polite'",
		"role: 'status'",
		"'aria-busy'",
		"AI generated, may be wrong.",
		"e.key === 'Enter' && !e.shiftKey",
		"rel: 'noopener noreferrer'",
		"maxlength: String(MAX_QUESTION_CHARS)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("panel script lacks %q", want)
		}
	}
	if !strings.Contains(js, "var MAX_QUESTION_CHARS = 1000;") || MaxQuestionRunes != 1000 {
		t.Error("the panel's question cap must match MaxQuestionRunes")
	}
	css := string(PanelCSS)
	for _, want := range []string{":focus-visible", "@media (max-width: 899px)", "html.hive-ask-open body", "[hidden]"} {
		if !strings.Contains(css, want) {
			t.Errorf("panel stylesheet lacks %q", want)
		}
	}
	for _, banned := range []string{"@import", "url(", "animation", "transition"} {
		if strings.Contains(css, banned) {
			t.Errorf("panel stylesheet uses %q", banned)
		}
	}
}

// TestPanelLayoutContract pins the layout fixes for the operator-reported
// clipping (the panel sat under the fixed dashboard header with its title row
// hidden): it starts below the header from the host's own height token,
// layers under that header, keeps a pinned title row with a close control,
// scrolls inside itself, and uses one type scale from the shared tokens.
func TestPanelLayoutContract(t *testing.T) {
	css := string(PanelCSS)
	for _, want := range []string{
		"var(--navbar-sticky-height, 0px)",
		"top: var(--hive-ask-top);",
		"var(--navbar-sticky-z,",
		"z-index: var(--hive-ask-z);",
		"position: sticky;",
		"overflow-y: auto;",
		"max-height: calc(100dvh - var(--hive-ask-top));",
		"--hive-ask-fs: var(--fs-base",
		"--hive-ask-fs-small: var(--fs-sm",
		"font-size: var(--fs-touch",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("panel stylesheet lacks %q", want)
		}
	}
	if strings.Contains(css, "z-index: 1000") || strings.Contains(css, "top: 0;\n  right: 0;") {
		t.Error("the panel must not sit at the top of the viewport under the fixed header")
	}
	js := string(PanelJS)
	for _, want := range []string{
		"'Ask Hive'",
		"className: 'hive-ask-close'",
		"className: 'hive-ask-body'",
		"className: 'hive-ask-actions'",
		"className: 'hive-ask-form-row'",
		"'Ask Hive a question'",
		"'Enter to send, Shift+Enter for a new line'",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("panel script lacks %q", want)
		}
	}
	for _, src := range []string{js, css} {
		if strings.Contains(src, "Ask AI") || strings.Contains(src, "—") {
			t.Error("panel copy must say Ask Hive, never Ask AI, and use no em-dashes")
		}
	}
}
