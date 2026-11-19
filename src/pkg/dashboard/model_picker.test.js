// Run the gateway model picker's pure helpers (js/model-picker.js) against
// recorded OpenRouter catalog shapes: filtering, provider grouping, the custom
// entry and price display. The dashboard script is one classic script, so the
// file is evaluated in a sandbox with the two globals it touches injected.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const plain = value => JSON.parse(JSON.stringify(value));

function esc(s) {
  if (typeof s !== 'string') return '';
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

function load() {
  const src = fs.readFileSync(path.join(__dirname, 'static-src/js/model-picker.js'), 'utf8');
  const ctx = vm.createContext({ esc, document: { addEventListener() {} } });
  vm.runInContext(src + '\nthis.__combos = _modelCombos; this.__min = MODEL_COMBOBOX_MIN_OPTIONS;', ctx);
  return ctx;
}
const P = load();

// Per-token prices as /api/openrouter/models serves them (model_info).
const INFO = [
  { id: 'deepseek/deepseek-v4.1-flash', name: 'DeepSeek: DeepSeek V4.1 Flash', context_length: 1048576, pricing: { prompt: 0.00000014, completion: 0.00000042, input_cache_read: 0.0000000042 } },
  { id: 'z-ai/glm-5.3', name: 'Z.ai: GLM 5.3', context_length: 1048576, pricing: { prompt: 0.0000000699, completion: 0.0000014, input_cache_read: 0.0000000689 } },
  { id: 'z-ai/glm-5.3-flash', name: 'Z.ai: GLM 5.3 Flash', context_length: 1048576, pricing: { prompt: 0.0000000399, completion: 0.0000000784 } },
  { id: 'anthropic/claude-sonnet-5.5', name: 'Anthropic: Claude Sonnet 5.5', context_length: 1000000, pricing: { prompt: 0.000002, completion: 0.00001 } },
  { id: 'openrouter/auto', name: 'Auto Router', context_length: 2000000, pricing: {} },
  { id: 'meta/free-model', name: 'Free', context_length: 200000, pricing: { prompt: 0, completion: 0 } },
];
const IDS = INFO.map(m => m.id).concat(['bare-model']);

test('price per 1M: small prices keep significant digits, zero is free, missing is unknown', () => {
  assert.equal(P.formatModelPricePerM(0.00000014), '$0.14');
  assert.equal(P.formatModelPricePerM(0.0000014), '$1.40');
  assert.equal(P.formatModelPricePerM(0.0000000042), '$0.0042');
  assert.equal(P.formatModelPricePerM(0.0000000699), '$0.07');
  assert.equal(P.formatModelPricePerM(0), '$0');
  assert.equal(P.formatModelPricePerM(undefined), '');
  assert.equal(P.formatModelPricePerM(null), '');
  assert.equal(P.formatModelPricePerM(-1), '');
});

test('option detail shows in/out (+cached) per 1M and context, or says price unknown', () => {
  const idx = P.modelInfoIndex(INFO);
  assert.equal(P.modelPriceLabel(idx['deepseek/deepseek-v4.1-flash']), '$0.14 in · $0.42 out /1M · $0.0042 cached · 1M ctx');
  assert.equal(P.modelPriceLabel(idx['anthropic/claude-sonnet-5.5']), '$2.00 in · $10.00 out /1M · 1M ctx');
  assert.equal(P.modelPriceLabel(idx['openrouter/auto']), 'price unknown · 2M ctx');
  assert.equal(P.modelPriceLabel(idx['meta/free-model']), '$0 in · $0 out /1M · 200K ctx');
  assert.equal(P.modelPriceLabel(undefined), '');
});

test('filter matches id or display name, every term, case-insensitively', () => {
  const idx = P.modelInfoIndex(INFO);
  assert.deepEqual(plain(P.filterModelOptions(IDS, 'GLM flash', idx)), ['z-ai/glm-5.3-flash']);
  assert.deepEqual(plain(P.filterModelOptions(IDS, 'sonnet', idx)), ['anthropic/claude-sonnet-5.5']);
  // "Z.ai" lives only in the display name.
  assert.deepEqual(plain(P.filterModelOptions(IDS, 'z.ai', idx)), ['z-ai/glm-5.3', 'z-ai/glm-5.3-flash']);
  assert.equal(P.filterModelOptions(IDS, '', idx).length, IDS.length);
  assert.deepEqual(plain(P.filterModelOptions(IDS, 'nope', idx)), []);
});

test('groups by provider prefix, sorted, bare ids last, suggestions pinned first and not repeated', () => {
  const groups = P.groupModelOptions(IDS, [{ id: 'deepseek/deepseek-v4.1-flash', label: 'DeepSeek' }, { id: 'retired/model', label: 'gone' }]);
  assert.deepEqual(plain(groups.map(g => g.provider)), ['Suggested', 'anthropic', 'meta', 'openrouter', 'z-ai', 'other']);
  assert.deepEqual(plain(groups[0].ids), ['deepseek/deepseek-v4.1-flash']);
  assert.ok(!groups.some(g => g.provider === 'deepseek'), 'pinned id must not repeat in its provider group');
  assert.deepEqual(plain(groups[groups.length - 1].ids), ['bare-model']);
  assert.equal(P.modelProviderOf('~anthropic/claude-sonnet-latest'), 'anthropic');
  assert.equal(P.modelProviderOf('bare'), '');
});

test('listbox html: ARIA options, prices, suggestion labels, and the custom entry', () => {
  P.modelComboboxHtml({ id: 'c1', ids: IDS, info: INFO, suggested: [{ id: 'z-ai/glm-5.3', label: 'GLM-5.3 (strong, cheap)' }], current: 'z-ai/glm-5.3' });
  const all = P.modelComboListHtml('c1', '');
  assert.match(all, /role="option"[^>]*data-value="z-ai\/glm-5.3"/);
  assert.ok(all.includes('GLM-5.3 (strong, cheap)'), 'suggestion label shown');
  assert.ok(all.includes('$0.14 in · $0.42 out /1M'), 'price shown');
  assert.ok(all.includes('Custom… (type any model id)'), 'empty custom entry always offered');

  const typed = P.modelComboListHtml('c1', 'acme/brand-new');
  assert.ok(typed.includes('No catalog model matches'), 'no-match notice');
  assert.match(typed, /data-custom="1"[^>]*data-arg1="acme\/brand-new"/);
  assert.ok(typed.includes('Use “acme/brand-new” as a custom model id'));

  const exact = P.modelComboListHtml('c1', 'z-ai/glm-5.3');
  assert.match(exact, /data-custom="1"[^>]*data-arg1=""/, 'an exact catalog id is not re-offered as custom');
});

test('combobox markup is an ARIA combobox owning a hidden listbox, value is the current id', () => {
  const html = P.modelComboboxHtml({ id: 'c2', inputId: 'gw-default-model', ids: IDS, info: INFO, current: 'deepseek/deepseek-v4.1-flash', label: 'Default model' });
  assert.match(html, /<input[^>]*id="gw-default-model"[^>]*role="combobox"[^>]*aria-expanded="false"[^>]*aria-controls="c2-list"/);
  assert.match(html, /<ul id="c2-list" class="model-combo-list" role="listbox" hidden>/);
  assert.match(html, /value="deepseek\/deepseek-v4.1-flash"/);
  assert.match(html, /data-keydown-action="modelComboKeydown" data-keys="\*"/);
  assert.ok(html.includes('1M ctx'), 'current model detail rendered');
  assert.ok(!/style=/.test(html), 'no inline styles (style ratchet)');
});

test('native selects group only large multi-provider lists', () => {
  const many = Array.from({ length: P.__min }, (_, i) => (i % 2 ? 'a/' : 'b/') + i);
  assert.equal(P.shouldGroupModelOptions(many), true);
  assert.equal(P.shouldGroupModelOptions(many.slice(0, 3)), false);
  assert.equal(P.shouldGroupModelOptions(many.map(v => 'a/' + v)), false);
  const html = P.modelOptgroupsHtml(['b/1', 'a/2'], v => `<option value="${v}">${v}</option>`);
  assert.equal(html, '<optgroup label="a"><option value="a/2">a/2</option></optgroup><optgroup label="b"><option value="b/1">b/1</option></optgroup>');
});
