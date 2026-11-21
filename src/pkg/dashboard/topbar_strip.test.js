// Phone top bar (responsive.css, max-width 768px): the right strip scrolls
// sideways when its chips don't fit. topbar-strip.js keeps it scrolled to
// the end (Help and the account menu) as chips come and go, until the user
// scrolls it. Runs the shipped source against a fake strip.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const src = fs.readFileSync(path.join(__dirname, 'static-src/js/topbar-strip.js'), 'utf8');

function load({ withStrip = true } = {}) {
  const listeners = {};
  const observers = [];
  const strip = withStrip ? {
    dataset: {}, scrollWidth: 400, clientWidth: 200, _left: 0,
    get scrollLeft() { return this._left; },
    set scrollLeft(v) { this._left = Math.max(0, Math.min(v, this.scrollWidth - this.clientWidth)); },
    addEventListener(type, fn) { listeners[type] = fn; },
  } : null;
  let queried = 0;
  const document = { querySelector(sel) { queried++; assert.strictEqual(sel, '.oc-topbar-right'); return strip; } };
  const window = { addEventListener(type, fn) { listeners['window:' + type] = fn; } };
  function MutationObserver(fn) { this.observe = () => observers.push(fn); }
  function ResizeObserver(fn) { this.observe = () => observers.push(fn); }
  const api = new Function('document', 'window', 'MutationObserver', 'ResizeObserver',
    src + '\nreturn { initTopbarStripScroll, topbarStripAtEnd };')(document, window, MutationObserver, ResizeObserver);
  const userScroll = (to) => { strip.scrollLeft = to; listeners.scroll(); };
  const change = (scrollWidth) => { strip.scrollWidth = scrollWidth; strip.scrollLeft = strip.scrollLeft; observers.forEach(fn => fn()); };
  return { api, strip, userScroll, change, observers, queried: () => queried };
}

test('starts scrolled to the end and follows chips while pinned', () => {
  const { strip, change, observers } = load();
  assert.strictEqual(observers.length, 2, 'resize and mutation observers');
  assert.strictEqual(strip.scrollLeft, 200);
  change(450);
  assert.strictEqual(strip.scrollLeft, 250, 'a new chip keeps the end in view');
});

test('a user scroll holds until they scroll back to the end', () => {
  const { strip, userScroll, change } = load();
  userScroll(0);
  change(450);
  assert.strictEqual(strip.scrollLeft, 0, 'an update does not yank the strip back');
  userScroll(250);
  change(500);
  assert.strictEqual(strip.scrollLeft, 300, 'back at the end, it pins again');
});

test('wires once and tolerates a page without the strip', () => {
  const { api, observers } = load();
  api.initTopbarStripScroll();
  assert.strictEqual(observers.length, 2);
  assert.doesNotThrow(() => load({ withStrip: false }));
});

test('at-end check allows sub-pixel rounding', () => {
  const { api } = load();
  assert.ok(api.topbarStripAtEnd({ scrollLeft: 198.5, clientWidth: 200, scrollWidth: 400 }));
  assert.ok(!api.topbarStripAtEnd({ scrollLeft: 190, clientWidth: 200, scrollWidth: 400 }));
});
