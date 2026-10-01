package dashboard

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// contributeBody renders the /contribute landing page for the theme tests.
func contributeBody(t *testing.T) string {
	t.Helper()
	setupContributeEnv(t)
	s := NewServer(0, slog.Default())
	s.registerContributeRoutes()

	req := httptest.NewRequest(http.MethodGet, "/contribute", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /contribute: expected 200, got %d", w.Code)
	}
	return w.Body.String()
}

// The palette shipped a full light ramp keyed on :root[data-theme] but nothing
// ever wrote the attribute, so light was reachable only via an OS preference
// (#4549). These assertions pin the three pieces that make it selectable: a
// control that dispatches, JS that writes the attribute, and a pre-paint guard.
func TestContributeThemeToggleIsPresent(t *testing.T) {
	body := contributeBody(t)

	for _, want := range []string{
		`id="cc-theme-toggle"`,
		`data-action="cycle-theme"`,   // must dispatch through delegation, not on*=
		`class="theme-toggle"`,        // styled control, not a bare button
		`data-theme-mode="auto"`,      // default state is auto
		`class="theme-toggle__glyph"`, // the label parts ccApplyTheme rewrites
		`class="theme-toggle__text"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("theme control missing %q", want)
		}
	}

	// The control must not sit INSIDE role=tablist: a non-tab child of a tablist
	// is announced as a stray tab. It is a sibling of .page-tabs inside
	// .page-chrome, so the tablist must close before the button opens.
	tablistEnd := strings.Index(body, `data-panel="tab-profile">Profile</button>`)
	toggle := strings.Index(body, `id="cc-theme-toggle"`)
	if tablistEnd < 0 || toggle < 0 {
		t.Fatalf("anchors not found: tablistEnd=%d toggle=%d", tablistEnd, toggle)
	}
	if toggle < tablistEnd {
		t.Errorf("theme control renders inside the tablist (toggle=%d < lastTab=%d)", toggle, tablistEnd)
	}
	if !strings.Contains(body, `class="page-chrome"`) {
		t.Error("missing .page-chrome wrapper that pairs the tablist with the control")
	}
	// The tablist itself must keep its class and role — the tab JS and the
	// existing tab-order tests both key off them.
	if !strings.Contains(body, `<div class="page-tabs" role="tablist">`) {
		t.Error("page-tabs tablist changed shape")
	}
}

// Auto must remove the pinned attribute and the persisted layout key rather than
// resolve to a concrete theme, so a visitor who returns to auto keeps following
// the OS if it changes while the page is open.
func TestContributeThemeAutoFollowsSystemLive(t *testing.T) {
	body := contributeBody(t)

	for _, want := range []string{
		`if(mode==='auto')r.removeAttribute('data-theme');else r.setAttribute('data-theme',mode)`,
		`document.body.classList.toggle('light-mode',!!light)`,
		`var CC_THEME_ORDER=['auto','light','dark']`,
		`localStorage.removeItem(CC_THEME_KEY)`,
		`matchMedia('(prefers-color-scheme: light)')`,
		`addEventListener('change',ccSyncAutoTheme)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("theme cycle missing %q", want)
		}
	}
}

// The stored preference has to land before first paint or a pinned theme flashes
// the other one. The guard therefore runs in <head>, ahead of the stylesheet.
func TestContributeThemeGuardRunsBeforeStylesheet(t *testing.T) {
	body := contributeBody(t)

	guard := strings.Index(body, `k='hive-layout-mode'`)
	style := strings.Index(body, "<style>")
	head := strings.Index(body, "</head>")
	if guard < 0 || style < 0 || head < 0 {
		t.Fatalf("anchors not found: guard=%d style=%d head=%d", guard, style, head)
	}
	if guard > style {
		t.Errorf("FOUC guard runs after the stylesheet (guard=%d, <style>=%d)", guard, style)
	}
	if guard > head {
		t.Errorf("FOUC guard is not in <head> (guard=%d, </head>=%d)", guard, head)
	}
}

// The contributor operations theme control reuses the dashboard's persistence
// key and body.light-mode hook, while retaining data-theme only as a legacy and
// accent-override compatibility shim.
func TestContributeThemeReusesDashboardLayoutMode(t *testing.T) {
	body := contributeBody(t)

	if !strings.Contains(body, `var CC_THEME_KEY='hive-layout-mode'`) {
		t.Error("contribute theme must use the dashboard hive-layout-mode storage key")
	}
	if !strings.Contains(body, `localStorage.getItem('hive.contribute.theme')`) {
		t.Error("contribute page must still honor the legacy contributor theme key")
	}
	if !strings.Contains(body, `document.body.classList.toggle('light-mode',!!light)`) {
		t.Error("contribute page must apply the shared body.light-mode hook")
	}
}

// Inline on*= handler attributes are forbidden (script-src-attr 'none',
// ADR-0016). The control must carry no handler attribute of its own.
func TestContributeThemeToggleUsesNoInlineHandler(t *testing.T) {
	body := contributeBody(t)

	i := strings.Index(body, `id="cc-theme-toggle"`)
	if i < 0 {
		t.Fatal(`id="cc-theme-toggle" not found`)
	}
	start := strings.LastIndex(body[:i], "<button")
	end := strings.Index(body[i:], ">")
	if start < 0 || end < 0 {
		t.Fatalf("could not bound the control's tag: start=%d end=%d", start, end)
	}
	tag := body[start : i+end]
	for _, bad := range []string{"onclick=", "onkeydown=", "onmousedown=", "onchange="} {
		if strings.Contains(strings.ToLower(tag), bad) {
			t.Errorf("theme control carries inline handler %q (CSP script-src-attr is 'none'): %s", bad, tag)
		}
	}
}

// The contributor page must consume the shared token/component stylesheets and
// avoid carrying its own neutral light ramp. tokens.css owns the [data-theme]
// light block and color-scheme declarations for every dashboard surface.
func TestContributeLightRampComesFromSharedTokens(t *testing.T) {
	body := contributeBody(t)

	for _, want := range []string{
		`<link rel="stylesheet" href="/tokens.css">`,
		`<link rel="stylesheet" href="/components.css">`,
		":root{\n  color-scheme:dark;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shared theme contract missing %q", want)
		}
	}
	for _, localRamp := range []string{
		"--cc-bg:",
		"--cc-surface:",
		"--cc-text:",
		"--cc-muted:",
	} {
		if strings.Contains(body, localRamp) {
			t.Errorf("contribute page still carries local neutral ramp fragment %q", localRamp)
		}
	}
}

// The neutral ramp must not be re-hardcoded in markup: an inline style attribute
// beats any stylesheet rule short of !important, so a literal neutral hex there
// is unreachable by the light ramp. Accents stay literal on purpose.
func TestContributeMarkupHasNoHardcodedNeutrals(t *testing.T) {
	body := contributeBody(t)

	// Only inspect the contribute document, which ends at its own </html>.
	if i := strings.Index(body, "</html>"); i > 0 {
		body = body[:i]
	}
	markup := body
	if i := strings.Index(markup, "</style>"); i > 0 {
		markup = markup[i:] // skip the stylesheet; tokens are defined there
	}

	neutrals := []string{"#161b22", "#30363d", "#c9d1d9", "#8b949e", "#6e7681", "#e6edf3", "#21262d"}
	for _, hex := range neutrals {
		for _, line := range strings.Split(markup, "\n") {
			if !strings.Contains(line, "style=\"") || !strings.Contains(line, hex) {
				continue
			}
			// Only flag the hex when it is inside a style attribute.
			for _, seg := range strings.Split(line, "style=\"")[1:] {
				if j := strings.Index(seg, "\""); j >= 0 && strings.Contains(seg[:j], hex) {
					t.Errorf("hardcoded neutral %s in an inline style (light ramp cannot reach it): %.120s", hex, line)
				}
			}
		}
	}
}

// The page used to paint with the operator's dashboard theme and then swap in
// the viewer's contributor theme from a deferred script, so every load showed
// colours and radii settle (#9847). These assertions pin the fix: the head
// resolves the viewer's theme itself and writes a parser-inserted link before
// any stylesheet, the deferred script resolves the same id from the same key,
// and nothing re-fetches the sheet it already has.
func TestContributeThemeLinkIsResolvedInHead(t *testing.T) {
	body := contributeBody(t)

	link := strings.Index(body, `document.write('<link id="contributor-theme-css"`)
	style := strings.Index(body, "<style>")
	head := strings.Index(body, "</head>")
	if link < 0 || style < 0 || head < 0 {
		t.Fatalf("anchors not found: link=%d style=%d head=%d", link, style, head)
	}
	if link > style || link > head {
		t.Errorf("theme link must be written in <head> before the stylesheet (link=%d, <style>=%d, </head>=%d)", link, style, head)
	}
	if strings.Contains(body, `href="/api/theme.css?scope=contributor">`) {
		t.Error("the operator-theme link must be gone: it is what the viewer saw before the swap")
	}
	if !strings.Contains(body, `<noscript><link id="contributor-theme-css" rel="stylesheet" href="/api/theme.css?scope=contributor&theme=`+contributeDefaultThemeID+`"></noscript>`) {
		t.Error("no-JS visitors must still get the default contributor theme")
	}
	if !strings.Contains(body, `var ME_LEGACY_THEME_IDS=['`+contributeDefaultThemeID+`',`) {
		t.Errorf("head default %q must equal ME_LEGACY_THEME_IDS[0] in the page script", contributeDefaultThemeID)
	}
	// Both resolvers read the same key with the same legacy 1..7 mapping.
	if !strings.Contains(body, `localStorage.getItem('hive.me.cardStyle')`) || !strings.Contains(body, `var ME_STYLE_KEY='hive.me.cardStyle';`) {
		t.Error("the head resolver must read the same localStorage key as meThemeID() (ME_STYLE_KEY)")
	}
	if strings.Count(body, `String(n)===String(raw||'1')&&n>=1&&n<=`) < 2 {
		t.Error("the head resolver must apply the same legacy id mapping as meThemeID()")
	}
	if strings.Contains(body, `&v='+Date.now()`) {
		t.Error("theme stylesheet must not be cache-busted on every load; the server sets an ETag")
	}
	if !strings.Contains(body, `if(link&&link.getAttribute('href')!==href)link.href=href;`) {
		t.Error("applyContributorTheme must not re-assign the href the head already loaded")
	}
}
