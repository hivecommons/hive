package dashboard

import (
	"strings"
	"testing"
)

// The Features section heading sticks to the top of #config-body. Chromium
// measures a sticky child's `top` from the scroll container's content edge,
// so any padding-top on the body opened a band under the search row that
// rows scrolled through. The top inset therefore lives in a ::before spacer
// and the heading carries no vertical margin of its own.
func TestSettingsStickyHeadingSitsFlush(t *testing.T) {
	html := indexHTML(t)

	body := cssRule(t, html, ".config-body")
	if !strings.Contains(body, "padding: var(--sp-0) var(--config-body-pad) var(--config-body-pad)") {
		t.Error(".config-body must have no padding-top; a sticky heading would stick below it in Chromium")
	}
	spacer := cssRule(t, html, ".config-body::before")
	if !strings.Contains(spacer, "height: var(--config-body-pad)") {
		t.Error(".config-body::before must restore the top inset as scrolling content")
	}
	title := cssRule(t, html, ".features-section-title")
	for _, want := range []string{"position: sticky", "top: var(--sp-0)", "margin: var(--sp-0) calc(-1 * var(--config-body-pad))", "background: var(--panel-strong)"} {
		if !strings.Contains(title, want) {
			t.Errorf(".features-section-title missing %q", want)
		}
	}
	if strings.Contains(title, "margin: calc(-1") {
		t.Error(".features-section-title must not use a negative top margin: sticky boxes stick by their margin box")
	}
}

// The section rail beside the dialog body is derived from the rendered
// `.config-section > h3` headings, scrolls the body (not the window) and
// marks the section in view with aria-current.
func TestSettingsSectionRail(t *testing.T) {
	html := indexHTML(t)

	for _, want := range []string{
		`<div class="config-main">`,
		`<nav class="config-rail" id="config-rail" aria-label="Sections in this tab" hidden></nav>`,
		`<div class="config-body" id="config-body"></div>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Settings modal markup missing %s", want)
		}
	}
	if !strings.Contains(html, `const CONFIG_RAIL_HEADING_SELECTOR = '.config-section > h3';`) {
		t.Error("rail entries must come from .config-section > h3 headings, not a hand-maintained list")
	}

	sync := extractJSFunction(t, html, "configRailSync")
	for _, want := range []string{"configRailHeadings()", "CONFIG_RAIL_MIN_SECTIONS", `data-action="configRailGo"`, "rail.hidden = true", "rail.hidden = false", `<button type="button" class="config-rail-link"`} {
		if !strings.Contains(sync, want) {
			t.Errorf("configRailSync missing %s", want)
		}
	}
	heads := extractJSFunction(t, html, "configRailHeadings")
	if !strings.Contains(heads, "getClientRects().length > 0") {
		t.Error("configRailHeadings must drop hidden headings (search results, owner-only panels)")
	}
	highlight := extractJSFunction(t, html, "configRailHighlight")
	if !strings.Contains(highlight, `setAttribute('aria-current', 'true')`) || !strings.Contains(highlight, `removeAttribute('aria-current')`) {
		t.Error("configRailHighlight must mark exactly the current section with aria-current")
	}
	goFn := extractJSFunction(t, html, "configRailGo") + extractJSFunction(t, html, "configScrollBodyTo")
	if !strings.Contains(goFn, "getElementById('config-body')") || !strings.Contains(goFn, "body.scrollTo(") {
		t.Error("configRailGo must scroll #config-body")
	}
	if strings.Contains(goFn, "window.scrollTo") || strings.Contains(goFn, "scrollIntoView") {
		t.Error("configRailGo must not scroll the window")
	}
	bind := extractJSFunction(t, html, "configRailBind")
	if !strings.Contains(bind, "new MutationObserver(") || !strings.Contains(bind, "addEventListener('scroll'") {
		t.Error("configRailBind must re-sync on body mutations and re-highlight on scroll")
	}
	if !strings.Contains(extractJSFunction(t, html, "openConfigDialog"), "configRailBind();") {
		t.Error("openConfigDialog must bind the section rail")
	}

	for _, sel := range []string{".config-main", ".config-rail", ".config-rail-link", `.config-rail-link[aria-current="true"]`} {
		if !strings.Contains(html, sel+" {") {
			t.Errorf("stylesheet missing rule %s", sel)
		}
	}
	if !strings.Contains(html, "@container (max-width: 899px) { .config-rail { display: none; } }") {
		t.Error("the rail must collapse on narrow dialogs")
	}
}
