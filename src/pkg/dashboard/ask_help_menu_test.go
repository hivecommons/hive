package dashboard

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAskHelpItemsStartHiddenAndOpenThePanel pins the markup of the Ask Hive
// entries (#19688): one in the top bar Help menu and one in the sidebar Help
// group, each right after Getting started, hidden until the panel is built,
// and both running openAskFromHelp. The sidebar entry is a focusable button
// that Enter and Space also run, so it works from the keyboard.
func TestAskHelpItemsStartHiddenAndOpenThePanel(t *testing.T) {
	page := indexHTML(t)
	top := regexp.MustCompile(`<button[^>]*id="ask-help-menu-item"[^>]*>`).FindString(page)
	for _, want := range []string{`class="avatar-menu-item"`, `role="menuitem"`, `style="display:none"`, `data-action="openAskFromHelp"`} {
		if !strings.Contains(top, want) {
			t.Errorf("top bar Ask Hive item missing %s: %s", want, top)
		}
	}
	side := regexp.MustCompile(`<a[^>]*id="ask-nav-item"[^>]*>`).FindString(page)
	for _, want := range []string{`class="oc-nav-item"`, `style="display:none"`, `role="button"`, `tabindex="0"`, `data-action="openAskFromHelp"`, `data-keydown-action="openAskFromHelp"`, `data-keys="Enter, "`, `data-prevent="1"`} {
		if !strings.Contains(side, want) {
			t.Errorf("sidebar Ask Hive item missing %s: %s", want, side)
		}
	}
	for _, pair := range [][2]string{
		{`id="welcome-help-menu-item"`, `id="ask-help-menu-item"`},
		{`id="welcome-nav-item"`, `id="ask-nav-item"`},
	} {
		a, b := strings.Index(page, pair[0]), strings.Index(page, pair[1])
		if a < 0 || b < a || strings.Contains(page[a:b], "</div>") {
			t.Errorf("%s must come right after %s in the same group", pair[1], pair[0])
		}
	}
	if !strings.Contains(page, `Promise.resolve(window.HiveAsk.mount(`) || !strings.Contains(page, `.then(panel => askShowHelpItems(!!panel));`) {
		t.Error("the loader must show the Ask Hive items only after the panel is built")
	}
}

// askHelpMenuStubJS is a small DOM for the Ask loader and the shared panel:
// element ids are registered on setAttribute, listeners fire on demand, and
// focus is recorded on document.activeElement.
const askHelpMenuStubJS = `
const assert = require('assert');
const byId = {};
class AskEl {
  constructor(tag) {
    this.tagName = String(tag).toUpperCase(); this.attrs = {}; this.children = []; this.listeners = {};
    this.hidden = false; this.style = { display: '' }; this.textContent = ''; this.value = ''; this.disabled = false;
    const self = this;
    this.classList = { set: new Set(), toggle(c, on) { if (on) this.set.add(c); else this.set.delete(c); }, contains(c) { return this.set.has(c); } };
  }
  setAttribute(k, v) { this.attrs[k] = String(v); if (k === 'id') byId[v] = this; }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  removeAttribute(k) { delete this.attrs[k]; }
  appendChild(c) { this.children.push(c); c.parentNode = this; return c; }
  addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
  fire(type, extra) { const e = Object.assign({ type, target: this, preventDefault() {}, stopPropagation() {} }, extra || {}); (this.listeners[type] || []).forEach(fn => fn(e)); }
  focus() { document.activeElement = this; }
  scrollIntoView() {}
}
global.AskEl = AskEl;
global.byId = byId;
global.document = {
  readyState: 'complete', activeElement: null,
  createElement: tag => new AskEl(tag),
  getElementById: id => byId[id] || null,
  head: new AskEl('head'), body: new AskEl('body'), documentElement: new AskEl('html'),
};
global.window = global;
function askItem(id) { const el = new AskEl('a'); el.setAttribute('id', id); el.style.display = 'none'; return el; }
const slot = new AskEl('span'); slot.setAttribute('id', 'hive-ask-slot');
const menuItem = askItem('ask-help-menu-item');
const navItem = askItem('ask-nav-item');
let statusBody = { enabled: true, consented: false, disclosure: 'notice', docs_url: 'https://docs.hivecommons.dev/', text_version: 'v1' };
global.fetch = () => Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(statusBody) });
const calls = [];
global.closeHelpMenu = returnFocus => calls.push(['closeHelpMenu', returnFocus]);
global.hiveCloseMobileDrawer = () => calls.push(['drawer']);
function askCurrentSection() { return ''; }
function askCurrentDocsHref() { return ''; }
let askPanelRequested = false;
const settle = () => new Promise(r => setTimeout(r, 0));
const loadedScript = () => document.head.children.find(c => c.tagName === 'SCRIPT');
`

// TestAskHelpItemsFollowThePanel runs the loader, the Ask Hive handler and
// the shared panel under node (#19688). With ask_enabled off nothing loads and
// the items stay hidden. With it on but the backend refusing this person, the
// items stay hidden and open() reports no panel. With the panel built, both
// items show, choosing one closes the Help menu and the phone drawer and opens
// the panel with focus inside it, and close() hands focus back to the opener.
func TestAskHelpItemsFollowThePanel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH: the Ask Hive menu wiring was NOT executed by this run")
	}
	panelPath, err := filepath.Abs(filepath.Join("..", "askai", "web", "ask-panel.js"))
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(panelPath)
	if err != nil {
		t.Fatal(err)
	}
	html := indexHTML(t)
	script := askHelpMenuStubJS + "\n" + strings.Join([]string{
		jsConstLine(t, html, "ASK_PANEL_SCRIPT"),
		jsConstLine(t, html, "ASK_PANEL_STYLE"),
		jsConstLine(t, html, "ASK_SLOT_ID"),
		jsConstLine(t, html, "ASK_HELP_ITEM_IDS"),
		jsFunc(t, html, "askShowHelpItems"),
		jsFunc(t, html, "openAskFromHelp"),
		jsFunc(t, html, "loadAskPanel"),
	}, "\n") + `
(async () => {
  // Off: nothing is requested, nothing is shown, the handler is inert.
  loadAskPanel({ ask_enabled: false });
  assert.strictEqual(document.head.children.length, 0, 'ask off must load nothing');
  assert.strictEqual(menuItem.style.display, 'none');
  assert.strictEqual(navItem.style.display, 'none');
  openAskFromHelp();

  // The component file itself, as the browser would run it from /ask-panel.js.
  require(` + string(quoted) + `);
  assert.strictEqual(typeof window.HiveAsk.open, 'function');
  assert.strictEqual(window.HiveAsk.open(), false, 'open() with no panel must report false');
  assert.strictEqual(window.HiveAsk.close(), false, 'close() with no panel must report false');

  // On, but the backend says Ask is not on for this person: still hidden.
  statusBody = { enabled: false };
  loadAskPanel({ ask_enabled: true });
  assert.ok(loadedScript(), 'ask on must load the panel script');
  assert.strictEqual(loadedScript().src, '/ask-panel.js');
  loadedScript().fire('load');
  await settle();
  assert.strictEqual(menuItem.style.display, 'none', 'no panel built: the Help item must stay hidden');
  assert.strictEqual(navItem.style.display, 'none');
  assert.strictEqual(byId['hive-ask-panel'], undefined);

  // On and built: both items show.
  statusBody = { enabled: true, consented: false, disclosure: 'notice', docs_url: 'https://docs.hivecommons.dev/', text_version: 'v1' };
  askPanelRequested = false;
  document.head.children.length = 0;
  loadAskPanel({ ask_enabled: true });
  loadedScript().fire('load');
  await settle();
  const panel = byId['hive-ask-panel'];
  assert.ok(panel, 'the panel must be built');
  assert.strictEqual(panel.hidden, true, 'the panel starts closed');
  assert.strictEqual(menuItem.style.display, '', 'the top bar Help item must show once the panel is built');
  assert.strictEqual(navItem.style.display, '', 'the sidebar Help item must show once the panel is built');
  const opener = slot.children[0];
  assert.strictEqual(opener.getAttribute('aria-expanded'), 'false');

  // Choosing Ask Hive: Help menu and drawer close, the panel opens with focus inside.
  calls.length = 0;
  openAskFromHelp();
  assert.deepStrictEqual(calls, [['closeHelpMenu', false], ['drawer']]);
  assert.strictEqual(panel.hidden, false, 'Ask Hive must open the panel');
  assert.strictEqual(opener.getAttribute('aria-expanded'), 'true');
  assert.ok(document.documentElement.classList.contains('hive-ask-open'));
  assert.strictEqual(document.activeElement && document.activeElement.textContent, 'I understand, continue', 'focus goes to the consent button before consent');

  // Choosing it again keeps the panel open and only moves focus back in.
  document.activeElement = null;
  assert.strictEqual(window.HiveAsk.open(), true);
  assert.strictEqual(panel.hidden, false);
  assert.strictEqual(document.activeElement && document.activeElement.textContent, 'I understand, continue');

  // close() reuses the panel's own close: hidden, collapsed, focus on the opener.
  assert.strictEqual(window.HiveAsk.close(), true);
  assert.strictEqual(panel.hidden, true);
  assert.strictEqual(opener.getAttribute('aria-expanded'), 'false');
  assert.ok(!document.documentElement.classList.contains('hive-ask-open'));
  assert.strictEqual(document.activeElement, opener);
  assert.strictEqual(window.HiveAsk.close(), true, 'close() on a closed panel is a no-op');
  assert.strictEqual(document.activeElement, opener);

  // The opener chip still toggles the same panel.
  opener.fire('click');
  assert.strictEqual(panel.hidden, false);
  opener.fire('click');
  assert.strictEqual(panel.hidden, true);
})().catch(e => { console.error(e); process.exit(1); });
`
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
}
