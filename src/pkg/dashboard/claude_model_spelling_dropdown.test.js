// A claude agent stored with the Copilot spelling of its model
// (claude-sonnet-5.5) must not be shown as if the valid claude-sonnet-5-5
// option were selected: the Claude CLI rejects the dotted id, and the
// tolerant dotted/dashed match hid that from the owner while every kick
// failed. The dropdown shows the stored value and flags it instead.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const src = fs.readFileSync(path.join(__dirname, 'static-src', 'js', 'backends-tokens.js'), 'utf8');

// Pull a top-level `function name(...) {...}` out of a source by brace matching.
function fn(name) {
  const start = src.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  let i = src.indexOf('{', src.indexOf(')', start));
  let depth = 0;
  for (; i < src.length; i++) {
    if (src[i] === '{') depth++;
    else if (src[i] === '}' && --depth === 0) return src.slice(start, i + 1);
  }
  throw new Error('unbalanced ' + name);
}

function constLine(name) {
  const m = src.match(new RegExp('const ' + name + ' = [^;]*;'));
  assert.ok(m, name + ' not found');
  return m[0];
}

const NAMES = ['_normalizeModel', '_modelsEqual', '_modelIdMatches', '_modelIdMatchesForBackend', 'invalidModelSpellingFor', 'modelOptionsHtml'];
const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const LISTS = {
  claude: [{ value: 'claude-sonnet-5-5', label: 'Sonnet 5.5' }, { value: 'sonnet', label: 'sonnet' }],
  copilot: [{ value: 'claude-sonnet-5.5', label: 'claude-sonnet-5.5' }],
};
const P = new Function('esc', 'LISTS', [
  constLine('STRICT_MODEL_ID_BACKENDS'),
  'const SINGLE_OPTION_BACKENDS = [];',
  'function modelsForBackend(b) { return LISTS[b] || []; }',
  ...NAMES.map(fn),
  'return { ' + NAMES.join(', ') + ' };',
].join('\n'))(esc, LISTS);

test('claude: a dotted stored id is shown as stored and flagged, not matched to the dashed option', () => {
  const html = P.modelOptionsHtml('claude', 'claude-sonnet-5.5');
  assert.match(html, /<option value="claude-sonnet-5\.5" selected>claude-sonnet-5\.5 \(⚠ not valid for claude; pick claude-sonnet-5-5\)/);
  assert.doesNotMatch(html, /<option value="claude-sonnet-5-5" selected>/);
});

test('claude: the valid dashed id still preselects its option', () => {
  const html = P.modelOptionsHtml('claude', 'claude-sonnet-5-5');
  assert.match(html, /<option value="claude-sonnet-5-5" selected>/);
  assert.doesNotMatch(html, /not valid/);
});

test('claude: an unknown id keeps the plain (current) marker', () => {
  const html = P.modelOptionsHtml('claude', 'claude-future-9');
  assert.match(html, /claude-future-9 \(current\)/);
});

test('copilot keeps the tolerant dotted/dashed match', () => {
  const html = P.modelOptionsHtml('copilot', 'claude-sonnet-5-5');
  assert.match(html, /<option value="claude-sonnet-5\.5" selected>/);
  assert.equal(P.invalidModelSpellingFor('copilot', LISTS.copilot, 'claude-sonnet-5-5'), '');
});
