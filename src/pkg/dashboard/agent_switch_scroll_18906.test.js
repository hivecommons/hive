const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function functionSource(file, name) {
  const src = fs.readFileSync(path.join(__dirname, 'static-src/js', file), 'utf8');
  const start = src.indexOf(`    function ${name}(`);
  const end = src.indexOf('\n    }', start);
  assert.ok(start >= 0 && end >= 0, `missing ${name}`);
  return src.slice(start, end + 6);
}

test('changed method, model and effort controls retain focus without scrolling', () => {
  const document = { activeElement: null };
  const ctx = vm.createContext({ document });
  vm.runInContext(functionSource('agent-cards.js', 'morphDomNode'), ctx);
  for (const action of ['switchCli', 'switchModel', 'switchEffort']) {
    let focusOptions;
    const old = { nodeType: 1, tagName: 'SELECT', outerHTML: action + '-old', replaceWith(next) { this.replacement = next; } };
    const next = { nodeType: 1, tagName: 'SELECT', outerHTML: action + '-new', focus(options) { focusOptions = options; document.activeElement = this; } };
    document.activeElement = old;
    ctx.morphDomNode(old, next);
    assert.equal(old.replacement, next);
    assert.equal(document.activeElement, next);
    assert.equal(focusOptions.preventScroll, true);
  }
  let focused = false;
  const old = { nodeType: 1, tagName: 'SELECT', outerHTML: 'old', replaceWith() {} };
  document.activeElement = {};
  ctx.morphDomNode(old, { nodeType: 1, tagName: 'SELECT', outerHTML: 'new', focus() { focused = true; } });
  assert.equal(focused, false, 'refresh must not steal focus');
});

test('scroll anchor restores the same card after a layout change', () => {
  let top = -80;
  let scrollY = 500;
  const card = { getBoundingClientRect: () => ({ top, bottom: top + 200, height: 200 }), getAttribute: () => 'worker', getClientRects: () => [1] };
  const section = { isConnected: true, contains: c => c === card, getClientRects: () => [1], getBoundingClientRect: () => ({ top: -300, bottom: 500 }), querySelectorAll: () => [], querySelector: () => card };
  const window = { scrollY, scrollBy(_x, delta) { scrollY += delta; top -= delta; }, scrollTo(_x, y) { scrollY = y; } };
  const document = { activeElement: { closest: () => card }, querySelectorAll: () => [section] };
  const ctx = vm.createContext({ window, document });
  vm.runInContext(functionSource('refresh.js', 'captureScrollAnchor') + '\n' + functionSource('refresh.js', 'restoreScrollAnchor'), ctx);
  const anchor = ctx.captureScrollAnchor();
  top += 120;
  ctx.restoreScrollAnchor(anchor);
  assert.equal(top, -80);
  assert.equal(scrollY, 620);
});
