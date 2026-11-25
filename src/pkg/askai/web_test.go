package askai

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
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
	// The one motion (the busy spinner) must sit inside a reduced-motion
	// no-preference block; outside it the stylesheet has no motion at all.
	still := css
	if i := strings.Index(still, reducedMotionOK); i >= 0 {
		end := strings.Index(still[i:], "\n}\n")
		if end < 0 {
			t.Fatal("unterminated reduced-motion block")
		}
		still = still[:i] + still[i+end:]
	}
	for _, banned := range []string{"@import", "url(", "animation", "transition", "@keyframes"} {
		if strings.Contains(still, banned) {
			t.Errorf("panel stylesheet uses %q outside %s", banned, reducedMotionOK)
		}
	}
}

// reducedMotionOK opens the only block where the panel may animate.
const reducedMotionOK = "@media (prefers-reduced-motion: no-preference) {"

// TestPanelTimeoutOutlastsServer pins that the browser waits longer than the
// server's own bound on one question, so a slow but successful answer (one
// took 27.7 s in the field) is never dropped by the page, and that the busy
// state names its timings as constants.
func TestPanelTimeoutOutlastsServer(t *testing.T) {
	js := string(PanelJS)
	m := regexp.MustCompile(`var ASK_TIMEOUT_MS = (\d+);`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("panel script lacks ASK_TIMEOUT_MS")
	}
	ms, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(ms) * time.Millisecond; got <= DefaultUpstreamTimeout+panelTimeoutMargin {
		t.Errorf("ASK_TIMEOUT_MS = %v, want more than the server bound %v plus %v", got, DefaultUpstreamTimeout, panelTimeoutMargin)
	}
	for _, want := range []string{
		"var STILL_WORKING_AFTER_MS = ",
		"input.readOnly = on;",
		"send.disabled = on;",
		"'Asking...'",
		"'hive-ask-spinner'",
		"'Retry'",
		".then(settle, settle);",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("panel script lacks %q", want)
		}
	}
}

// panelTimeoutMargin is the least headroom the page keeps over the server
// bound, for the relay hop and the response write.
const panelTimeoutMargin = 10 * time.Second

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
		"overscroll-behavior: contain;",
		"html.hive-ask-scroll-lock,\n  html.hive-ask-scroll-lock body {\n    overflow: hidden;",
		"html.hive-ask-open .hive-chat-fab",
		"clip-path: inset(50%);",
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
	// The script's bottom-sheet query must be the stylesheet's breakpoint.
	if !strings.Contains(js, "var SHEET_QUERY = '(max-width: 899px)';") || !strings.Contains(css, "@media (max-width: 899px)") {
		t.Error("SHEET_QUERY and the stylesheet's bottom-sheet breakpoint must match")
	}
	for _, want := range []string{
		"classList.toggle(SCROLL_LOCK_CLASS, open)",
		"'touchmove'",
		"{ passive: false }",
		"scrollIntoView({ block: 'start' })",
		"announce(view.kind === 'answer' ? 'Answer received.' : view.text)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("panel script lacks %q", want)
		}
	}
	for _, src := range []string{js, css} {
		if strings.Contains(src, "Ask AI") || strings.Contains(src, "\u2014") {
			t.Error("panel copy must say Ask Hive, never Ask AI, and use no em-dashes")
		}
	}
}
