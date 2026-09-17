package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// These tests pin the modal scroll-lock STRUCTURE in the embedded
// static/index.html. The behavior itself (a wheel event over the dimmed
// dashboard doing nothing) is browser-only and cannot be executed here; what
// these guarantee is that the specific CSS-spec trap behind #7250 cannot
// silently return.
//
// The trap: `html { overflow-x: hidden }` makes <html> the viewport's scroll
// container, because setting any non-`visible` overflow on the root element
// promotes it. A `body.modal-open { overflow: hidden }` rule then clips
// nothing at all — body's overflow propagates to the viewport ONLY while
// html's overflow is `visible`. That is why #2766 and #4805 both "fixed" this
// and it kept reproducing: the lock was being applied to an element that no
// longer controlled page scroll.

// TestModalScrollLockCoversHTMLElement is the core guard. A body-only lock is
// a no-op on this document, so the rule must name html.modal-open too.
func TestModalScrollLockCoversHTMLElement(t *testing.T) {
	html := indexHTML(t)

	lock := regexp.MustCompile(`html\.modal-open\s*,\s*body\.modal-open\s*\{[^}]*overflow:\s*hidden`)
	if !lock.MatchString(html) {
		t.Error("index.html has no `html.modal-open, body.modal-open { overflow: hidden }` rule — " +
			"a body-only scroll lock is a no-op while <html> is the scroll container (#7250)")
	}

	// Pin the precondition that makes the above necessary, so that if someone
	// ever removes it the reason for this test is still discoverable rather
	// than looking like cargo cult.
	if !strings.Contains(html, "html { overflow-x: hidden;") {
		t.Log("note: `html { overflow-x: hidden }` is gone; the html.modal-open lock is now belt-and-braces rather than load-bearing")
	}
}

// TestModalScrollLockSyncTogglesDocumentElement pins the JS half. The CSS rule
// above only fires if something actually puts .modal-open on <html>;
// installModalScrollLock's sync() previously toggled document.body alone.
func TestModalScrollLockSyncTogglesDocumentElement(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"document.documentElement.classList.toggle('modal-open', open);",
		"document.body.classList.toggle('modal-open', open);",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q — the modal scroll lock no longer locks the real scroll container (#7250)", snippet)
		}
	}
}

// TestConfigBodyCanAlwaysScroll pins the belt-and-braces half: .config-body is
// a flex child, and a flex item's default min-height:auto sizes it to its
// content. On a short tab that leaves nothing to scroll, so overscroll-behavior
// (which only applies to an element that is itself scrollable) never engages
// and the wheel chains to the page behind.
func TestConfigBodyCanAlwaysScroll(t *testing.T) {
	html := indexHTML(t)

	start := strings.Index(html, ".config-body {")
	if start < 0 {
		t.Fatal("index.html has no .config-body rule")
	}
	end := strings.Index(html[start:], "}")
	if end < 0 {
		t.Fatal(".config-body rule is unterminated")
	}
	rule := html[start : start+end]

	for _, decl := range []string{"overflow-y: auto", "min-height: 0", "overscroll-behavior: contain"} {
		if !strings.Contains(rule, decl) {
			t.Errorf(".config-body is missing %q — modal body may stop being a scroll container (#7250)", decl)
		}
	}
}
