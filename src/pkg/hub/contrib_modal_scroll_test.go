package hub

import (
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
)

// TestContribModalScrollLock pins the #4805 fix on the hub landing page's
// contribute modal: the page behind the overlay must be scroll-locked while
// the modal is open (and restored on every close path — button, backdrop,
// Escape), and both the backdrop and the 80vh panel must contain scroll
// chaining so a wheel at the panel's boundary cannot scroll the hub page.
func TestContribModalScrollLock(t *testing.T) {
	b, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read hub static html: %v", err)
	}
	html := string(b)

	for _, snippet := range []string{
		// One close function shared by button, backdrop, and Escape — it is
		// what restores the body scroll.
		"function closeContribModal() {",
		"document.body.style.overflow = '';",
		// Lock on open.
		"document.body.style.overflow = 'hidden';",
		// Backdrop click and Escape close paths.
		"if(event.target===this)closeContribModal()",
		"function hubLandingDismissTopModal() {",
		"if (e.key !== 'Escape') return;",
		"if (!hubLandingDismissTopModal()) return;",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("hub static/index.html is missing %q — contribute-modal scroll lock regressed (#4805)", snippet)
		}
	}

	// Containment on the backdrop and the scrollable panel.
	idx := strings.Index(html, `id="contrib-modal"`)
	if idx < 0 {
		t.Fatal("hub static/index.html has no contrib-modal")
	}
	region := html[idx:]
	if cut := strings.Index(region, "</h2>"); cut > 0 {
		region = region[:cut]
	}
	if strings.Count(region, "overscroll-behavior:contain") < 2 {
		t.Error("contrib-modal backdrop/panel are missing overscroll-behavior:contain — modal scroll chains to the hub page (#4805)")
	}
}

func TestContribModalKeepsKeyboardFocus(t *testing.T) {
	html, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	var landingScript string
	for _, script := range webstatic.ExtractInlineScripts(html) {
		if strings.Contains(string(script), "function openContributeInfo(") {
			landingScript = string(script)
			break
		}
	}
	if landingScript == "" {
		t.Fatal("hub landing page script not found")
	}
	runNodeAssertions(t, `
const assert = require('node:assert/strict');
const focusable = (id = '', top = 100) => ({
  id,
  getBoundingClientRect() { return {top, bottom: top + 30, left: 20, right: 120}; },
  focus(options) { this.focusOptions = options; document.activeElement = this; }
});
const signIn = focusable(), close = focusable();
const search = Object.assign(focusable('hive-search'), {value: ''});
const modal = {
  style: {display: 'none'},
  querySelectorAll() { return [signIn, close]; }
};
let trigger, triggerTop = 100;
const elements = {
  'contrib-modal': modal,
  'contrib-close': close,
  'contrib-hive-name': {},
  'contrib-hive-id': {},
  'contrib-access-area': {},
  'hive-table-container': {
    set innerHTML(markup) {
      if (trigger) {
        if (document.activeElement === trigger) document.activeElement = document.body;
        delete elements[trigger.id];
      }
      const button = markup.match(/<button\b[^>]*>Contribute<\/button>/);
      const id = button?.[0].match(/ id="([^"]+)"/)?.[1];
      trigger = id ? focusable(id, triggerTop) : undefined;
      if (trigger) elements[id] = trigger;
    }
  },
  'hive-search': search
};
const page = [{}, {}, {}];
const document = Object.assign(new EventTarget(), {
  body: {style: {}},
  getElementById(id) { return elements[id]; },
  querySelectorAll() { return page; },
  createElement() { return {set textContent(value) { this.innerHTML = String(value); }}; }
});
const window = {innerHeight: 720, innerWidth: 1280, addEventListener() {}};
// Keep background fetches and timers idle while testing the landing script.
const fetch = () => new Promise(() => {});
const setInterval = () => {};
function press(key, shiftKey = false) {
  const event = Object.assign(new Event('keydown', {cancelable: true}), {key, shiftKey});
  assert.equal(document.dispatchEvent(event), false);
}
`+landingScript+`
(async () => {
  const hive = {name: 'Example Hive', id: 'example', hiveType: 'hosted'};
  _allMainHives = [hive];
  renderHives();
  assert.ok(trigger, 'rendered Contribute button must have an ID');
  trigger.focus();

  await openContributeInfo(hive.name, hive.id);
  assert.equal(document.activeElement, close);
  assert.ok(page.every(el => el.inert));
  press('Tab', true);
  assert.equal(document.activeElement, signIn);
  press('Tab');
  assert.equal(document.activeElement, close);
  press('Tab');
  assert.equal(document.activeElement, signIn);
  press('Tab', true);
  assert.equal(document.activeElement, close);

  renderHives();
  assert.equal(document.activeElement, close);
  press('Escape');
  const replacement = trigger;
  assert.equal(document.activeElement, replacement);
  assert.ok(page.every(el => !el.inert));

  renderHives();
  assert.notEqual(trigger, replacement);
  assert.equal(document.activeElement, trigger);
  assert.deepEqual(trigger.focusOptions, {preventScroll: true});

  triggerTop = window.innerHeight + 30;
  renderHives();
  assert.equal(document.activeElement, trigger);
  assert.deepEqual(trigger.focusOptions, {preventScroll: false});
  renderHives();
  assert.deepEqual(trigger.focusOptions, {preventScroll: true});

  _allMainHives = [];
  renderHives();
  assert.equal(document.activeElement, search);

  triggerTop = 100;
  _allMainHives = [hive];
  renderHives();
  await openContributeInfo(hive.name, hive.id);
  _allMainHives = [];
  renderHives();
  press('Escape');
  assert.equal(document.activeElement, search);
})();
`)
}
