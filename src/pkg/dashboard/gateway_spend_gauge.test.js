// Gateway spend rows on the usage gauge: /api/providers/headroom's
// gateway_spend becomes model.spend, and a gateway whose models have no price
// says "price unknown" instead of $0.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const plain = value => JSON.parse(JSON.stringify(value));

function load() {
  const src = fs.readFileSync(path.join(__dirname, 'static-src/js/subscription-gauge.js'), 'utf8');
  const model = src.match(/function subscriptionGaugeModel\([^\n]*\) \{[\s\S]*?\n    \}/);
  const text = src.match(/function subscriptionGaugeSpendText\([^\n]*\) \{[\s\S]*?\n    \}/);
  assert.ok(model && text, 'gauge helpers found');
  const ctx = vm.createContext({});
  vm.runInContext(model[0] + '\n' + text[0] + '\nthis.model = subscriptionGaugeModel; this.text = subscriptionGaugeSpendText;', ctx);
  return ctx;
}
const G = load();
const NOW = Date.parse('2026-11-16T18:00:00Z');

test('gateway_spend rows become model.spend even with no plan reading', () => {
  const m = G.model({ providers: [], used_by: {}, gateway_spend: [
    { gateway: 'openrouter', kind: 'openrouter', agents: ['scout'], usd: 1.234, tokens: 900, unknown_price_models: [] },
    { gateway: 'local-vllm', kind: 'vllm', agents: ['worker'], usd: 0, tokens: 5000, unknown_price_models: ['Qwen/Qwen3-32B'] },
    { gateway: 'corp', kind: 'litellm', agents: ['a', 'b'], usd: 2, unknown_price_models: ['granite'] },
    { gateway: 'idle', kind: 'custom', agents: [], usd: 0 },
  ] }, NOW);
  assert.equal(m.shown, false, 'no plan row');
  assert.deepEqual(plain(m.spend.map(r => [r.gateway, r.state])), [['openrouter', 'priced'], ['local-vllm', 'unknown'], ['corp', 'partial']]);
  assert.deepEqual(plain(m.spend[2].usedBy), ['a', 'b']);
});

test('spend text: priced, partial (unknown not counted), unknown (never $0)', () => {
  const m = G.model({ providers: [], gateway_spend: [
    { gateway: 'openrouter', agents: ['scout'], usd: 1.234, unknown_price_models: [] },
    { gateway: 'corp', agents: ['a'], usd: 2, unknown_price_models: ['granite'] },
    { gateway: 'local-vllm', agents: ['w'], usd: 0, unknown_price_models: ['Qwen/Qwen3-32B'] },
  ] }, NOW);
  assert.equal(G.text(m.spend[0]), '$1.23 at gateway prices (estimate from token counts).');
  assert.equal(G.text(m.spend[1]), '$2.00 at gateway prices, plus price unknown for granite (not counted).');
  const unknown = G.text(m.spend[2]);
  assert.match(unknown, /^Price unknown \(Qwen\/Qwen3-32B\)/);
  assert.ok(!unknown.includes('$0'), 'an unpriced gateway never reads as $0');
});

test('plan rows keep working and carry spend alongside', () => {
  const m = G.model({ providers: [{ provider: 'anthropic', available: true, captured_at: '2026-11-16T17:59:00Z',
    limits: [{ id: 'weekly_all', kind: 'weekly', pct_remaining: 60, duration_mins: 10080, resets_at: '2026-11-20T00:00:00Z' }] }],
    used_by: { anthropic: ['claude-agent'] },
    gateway_spend: [{ gateway: 'openrouter', agents: ['scout'], usd: 0.5, unknown_price_models: [] }] }, NOW);
  assert.equal(m.state, 'reading');
  assert.equal(m.rows.length, 1);
  assert.equal(m.spend.length, 1);
  const none = G.model({ providers: [] }, NOW);
  assert.deepEqual(plain(none.spend), []);
});
