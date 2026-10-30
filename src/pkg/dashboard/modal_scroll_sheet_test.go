package dashboard

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestModalScrollSheetWiring pins the shared long-content modal convention:
// the What-changed (release notes) dialog and the other long-body dialogs
// carry .modal-scroll, the sheet decorator runs for every hive dialog, the
// tokens the recipe consumes exist, and the CSS has the narrow-viewport
// full-screen sheet rule plus the scroll-end fade affordance.
func TestModalScrollSheetWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		// What changed dialog + every other long-body hive dialog adopts the shared class.
		`var body = rnEl('div', 'release-notes-body modal-scroll');`,
		`hiveModalSheetDecorate(overlay, function() { close(false); });`,
		`<div class="hive-dialog-body long modal-scroll">${hiveDialogEscape(text)}</div>`,
		`<div class="hive-dialog-body long modal-scroll hive-export-body">`,
		`<div class="hive-dialog-body long modal-scroll">` + "\n" + `            <p>PRs currently labelled`,
		`<div class="nous-config-body modal-scroll">${bodyHtml}</div>`,
		`'<div class="acmm-dialog modal-scroll">'`,
		`<div class="config-body modal-scroll" id="config-body"></div>`,
		`<div class="config-body welcome-body modal-scroll" id="welcome-body"></div>`,
		`<div class="feedback-body iphone-widget-body modal-scroll">`,
		// Shared JS: sync + init + sheet decorator, wired into the dialog helper
		// and into the scroll-lock observer so static/injected bodies get it too.
		`const HIVE_MODAL_SCROLL_EPS = 2;`,
		`function hiveModalScrollSync(el)`,
		`function hiveModalScrollInit(el)`,
		`function hiveModalSheetDecorate(overlay, onClose)`,
		`if (typeof hiveModalSheetDecorate === 'function') hiveModalSheetDecorate(overlay, () => close(cancelValue));`,
		`document.querySelectorAll('.modal-scroll:not([data-modal-scroll-wired])').forEach(hiveModalScrollInit);`,
		`el.addEventListener('scroll', sync, { passive: true });`,
		`x.className = 'modal-sheet-close';`,
		`x.setAttribute('aria-label', 'Close');`,
		// CSS: scroll region, affordance, and the mobile full-screen sheet.
		`.modal-scroll { position: relative; overflow-y: auto; -webkit-overflow-scrolling: touch; overscroll-behavior: contain; scrollbar-gutter: stable;`,
		`.modal-scroll::-webkit-scrollbar { width: var(--modal-scrollbar-w); }`,
		`.modal-scroll::after { content: ''; display: block; position: sticky; bottom: 0;`,
		`.modal-scroll.is-scrollable-more::after { opacity: 1; }`,
		`@media (max-width: 640px), ((pointer: coarse) and (max-width: 820px)) {`,
		`.hive-dialog-overlay.has-modal-sheet { padding: var(--sp-0); align-items: stretch;`,
		`.hive-dialog.modal-sheet {`,
		`height: 100vh; height: 100dvh;`,
		`max-height: 100vh; max-height: 100dvh;`,
		`padding-top: calc(var(--sp-5) + var(--safe-area-top));`,
		`padding-bottom: calc(var(--sp-5) + var(--safe-area-bottom));`,
		`.hive-dialog.modal-sheet .modal-scroll { flex: 1 1 auto; min-height: 0; max-height: none;`,
		`.hive-dialog.modal-sheet .modal-sheet-close { display: inline-flex; }`,
		`width: var(--modal-sheet-close-size); height: var(--modal-sheet-close-size);`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing modal scroll/sheet wiring %q", want)
		}
	}
	// The old per-dialog scroller must not fight the shared one.
	if strings.Contains(html, ".release-notes-body { max-height: min(55vh, 520px); overflow: auto;") {
		t.Error(".release-notes-body still owns overflow; .modal-scroll is the scroller")
	}
	// Token-only CSS (ADR-0018): the sheet/affordance block names no literal colour.
	start := strings.Index(html, "/* .modal-scroll — shared long-content scroll region")
	end := strings.Index(html, ".modal-sheet-close:hover")
	if start < 0 || end < start {
		t.Fatal("modal-scroll CSS block not found")
	}
	if css := html[start:end]; strings.Contains(css, "#") || strings.Contains(css, "rgb(") || strings.Contains(css, "rgba(") {
		t.Errorf("modal-scroll CSS must use tokens only, found a literal colour:\n%s", css)
	}
	tokens := tokensCSS(t)
	for _, tok := range []string{
		"--modal-scroll-fade-h: 40px;",
		"--modal-scroll-fade:",
		"--modal-scrollbar-w:",
		"--modal-sheet-close-size: 44px;",
		"--modal-sheet-close-glyph:",
		"--safe-area-top: env(safe-area-inset-top, 0px);",
		"--safe-area-bottom: env(safe-area-inset-bottom, 0px);",
	} {
		if !strings.Contains(tokens, tok) {
			t.Errorf("tokens.css lacks %s", tok)
		}
	}
}

// TestModalScrollAffordanceToggles drives hiveModalScrollSync/Init under node
// with a stub scroll container: a long body shows the more-affordance at the
// top, keeps it mid-way, clears it at the bottom (within the epsilon), and a
// body that fits never shows it; the sheet decorator adds the ✕ and the sheet
// classes once, and the ✕ invokes the close callback.
func TestModalScrollAffordanceToggles(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — modal scroll affordance was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		jsConstLine(t, html, "HIVE_MODAL_SCROLL_EPS"),
		jsFunc(t, html, "hiveModalScrollSync"),
		jsFunc(t, html, "hiveModalScrollInit"),
		jsFunc(t, html, "hiveModalSheetDecorate"),
	}, "\n") + `
function classList(set) {
  return { contains: c => set.has(c), add(...cs) { cs.forEach(c => set.add(c)); }, remove(...cs) { cs.forEach(c => set.delete(c)); },
    toggle(c, on) { if (on === undefined) on = !set.has(c); if (on) set.add(c); else set.delete(c); return on; } };
}
function scroller(scrollHeight, clientHeight) {
  const set = new Set(['modal-scroll']);
  const el = { scrollHeight, clientHeight, scrollTop: 0, dataset: {}, listeners: {}, classes: set, classList: classList(set),
    addEventListener(type, fn) { this.listeners[type] = fn; } };
  return el;
}
global.window = { addEventListener() {} };
global.ResizeObserver = undefined; global.MutationObserver = undefined;

// long body: affordance on at top, on mid-way, off at the bottom, back on when scrolled up
const long = scroller(1200, 400);
hiveModalScrollInit(long);
assert.strictEqual(long.dataset.modalScrollWired, '1');
assert.ok(long.classes.has('is-scrollable-more'), 'at top: more content below');
long.scrollTop = 500; long.listeners.scroll();
assert.ok(long.classes.has('is-scrollable-more'), 'mid-way: still more below');
long.scrollTop = 800; long.listeners.scroll();
assert.ok(!long.classes.has('is-scrollable-more'), 'at bottom: affordance cleared');
long.scrollTop = 799; long.listeners.scroll();
assert.ok(!long.classes.has('is-scrollable-more'), 'within epsilon of the bottom counts as the end (iOS fractional scrollTop)');
long.scrollTop = 0; long.listeners.scroll();
assert.ok(long.classes.has('is-scrollable-more'), 'scrolled back up: affordance returns');
// init is idempotent: re-wiring only re-syncs
long.scrollTop = 800; hiveModalScrollInit(long);
assert.ok(!long.classes.has('is-scrollable-more'));

// short body: never shows the affordance
const short = scroller(300, 400);
hiveModalScrollInit(short);
assert.ok(!short.classes.has('is-scrollable-more'), 'content fits: no affordance');

// sync on a bare element that lacks the class adds it via init
const bare = scroller(900, 300); bare.classes.delete('modal-scroll');
hiveModalScrollInit(bare);
assert.ok(bare.classes.has('modal-scroll'));

// sheet decorator: adds ✕ once, marks dialog + overlay, ✕ closes
let closed = 0;
const dlgSet = new Set(['hive-dialog']);
const children = [];
const dlg = { classList: classList(dlgSet), firstChild: 'title', insertBefore(n) { children.unshift(n); },
  querySelector(sel) { return sel === '.modal-sheet-close' ? children.find(c => c.className === 'modal-sheet-close') || null : null; } };
const ovSet = new Set(['hive-dialog-overlay']);
const overlay = { classList: classList(ovSet), querySelectorAll(sel) { return sel === '.modal-scroll' ? [long] : []; },
  querySelector(sel) { return sel === '.hive-dialog' ? dlg : null; } };
global.document = { createElement(tag) { return { tag, attrs: {}, setAttribute(k, v) { this.attrs[k] = v; }, addEventListener(t, fn) { this.onclick = fn; } }; } };
hiveModalSheetDecorate(overlay, () => closed++);
assert.ok(dlgSet.has('modal-sheet'), 'dialog becomes a sheet on narrow viewports');
assert.ok(ovSet.has('has-modal-sheet'));
assert.strictEqual(children.length, 1);
assert.strictEqual(children[0].className, 'modal-sheet-close');
assert.strictEqual(children[0].attrs['aria-label'], 'Close');
assert.strictEqual(children[0].type, 'button');
children[0].onclick();
assert.strictEqual(closed, 1, 'the ✕ closes the dialog');
hiveModalSheetDecorate(overlay, () => closed++);
assert.strictEqual(children.length, 1, 'decorating twice adds no second ✕');

// dialogs without a long body are left alone (desktop + mobile unchanged)
const plainSet = new Set(['hive-dialog']);
const plainDlg = { classList: classList(plainSet), insertBefore() { throw new Error('must not add ✕'); }, querySelector() { return null; } };
const plainOv = { classList: classList(new Set()), querySelectorAll() { return []; }, querySelector() { return plainDlg; } };
hiveModalSheetDecorate(plainOv, () => {});
assert.ok(!plainSet.has('modal-sheet'));
`
	cmd := exec.Command(node, "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node modal scroll fixture failed: %v\n%s", err, out)
	}
}

// TestModalScrollFadeFollowsTheme pins the light-mode fix for the scroll-edge
// fade: a var() inside a custom property resolves where that property is
// declared, so the :root --modal-scroll-fade is fixed to the dark
// --panel-strong. The light block must re-declare it against the light
// panel surface, and the fade rule must consume the token, not a literal.
func TestModalScrollFadeFollowsTheme(t *testing.T) {
	tokens := tokensCSS(t)
	const decl = "--modal-scroll-fade: color-mix(in srgb, var(--panel-strong) 96%, transparent);"
	light := strings.Index(tokens, "body.light-mode {")
	if light < 0 {
		t.Fatal("tokens.css lacks the light-mode block")
	}
	if !strings.Contains(tokens[:light], decl) {
		t.Error("tokens.css :root block lacks the surface-based --modal-scroll-fade")
	}
	if !strings.Contains(tokens[light:], decl) {
		t.Error("tokens.css light block does not re-declare --modal-scroll-fade; the fade would stay dark in light mode")
	}
	for _, v := range strings.Split(tokens, "\n") {
		// "rgb(" rather than "rgb": the colour space in color-mix(in srgb, …) is not a literal.
		if strings.Contains(v, "--modal-scroll-fade:") && (strings.Contains(v, "#") || strings.Contains(v, "rgb(") || strings.Contains(v, "rgba(")) {
			t.Errorf("--modal-scroll-fade must derive from a surface token, found a literal: %s", strings.TrimSpace(v))
		}
	}
	html := indexHTML(t)
	if !strings.Contains(html, "background: linear-gradient(to bottom, transparent, var(--modal-scroll-fade));") {
		t.Error(".modal-scroll::after fade does not consume var(--modal-scroll-fade)")
	}
	hub, err := os.ReadFile("../hub/static/tokens.css")
	if err != nil {
		t.Fatal(err)
	}
	if string(hub) != tokens {
		t.Error("pkg/hub/static/tokens.css drifted from pkg/dashboard/static/tokens.css")
	}
}
