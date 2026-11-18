'use strict';

// Cost card: usage by sandbox follows the timeframe selector (#18968). The
// table and the agent cards' spend chips read GET /api/cost/agents for the
// active window; until it has answered for that window the chips fall back to
// the browser's own sum.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const indexHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'pkg', 'dashboard', 'static', 'index.html'), 'utf8');

function extractFunction(name) {
  const start = indexHtml.indexOf('function ' + name + '(');
  assert.notEqual(start, -1, `${name} function exists`);
  const brace = indexHtml.indexOf('{', start);
  let depth = 0;
  for (let i = brace; i < indexHtml.length; i++) {
    const ch = indexHtml[i];
    if (ch === '{') depth++;
    if (ch === '}') depth--;
    if (depth === 0) return indexHtml.slice(start, i + 1);
  }
  throw new Error(`unterminated ${name}`);
}

const DAY = 24 * 3600 * 1000;
const NOW = Date.UTC(2026, 10, 18, 12, 0, 0);

function load() {
  const context = { exports: {}, Date, Math, Number, String, encodeURIComponent };
  vm.runInNewContext([
    'let _costAgentWindow = null;',
    "let _costRange = 'daily';",
    'let WIN = null;',
    'let HIST = [];',
    "const COST_SORTS = { sandbox: { key: 'usd', dir: 'desc' } };",
    "const COST_RANGES = { daily: { label: 'daily' } };",
    'function costWindow() { return WIN; }',
    'function costHist() { return HIST; }',
    "function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/\"/g, '&quot;'); }",
    "function fmtTokens(n) { return String(n); }",
    "function fmtUSD(n) { return '$' + Number(n).toFixed(2); }",
    "function fmtCostDateTime(ms) { return new Date(ms).toISOString(); }",
    extractFunction('costCmp'),
    extractFunction('sortCostRows'),
    extractFunction('costAgentWindowQuery'),
    extractFunction('costAgentWindowData'),
    extractFunction('agentWindowSpend'),
    extractFunction('costWindowTokensCell'),
    extractFunction('costSandboxRangeRowsHtml'),
    extractFunction('costSandboxCoverageNote'),
    'exports.setWin = (w) => { WIN = w; };',
    'exports.setHist = (h) => { HIST = h; };',
    'exports.setData = (key, data) => { const byName = {}; for (const a of data.agents) byName[a.name] = a; _costAgentWindow = { key, data, byName }; };',
    'exports.query = costAgentWindowQuery;',
    'exports.spend = agentWindowSpend;',
    'exports.rows = costSandboxRangeRowsHtml;',
    'exports.note = costSandboxCoverageNote;',
  ].join('\n'), context);
  return context.exports;
}

const daily = { key: 'daily', start: NOW - 7 * DAY, end: NOW + DAY };
const weekly = { key: 'weekly', start: NOW - 28 * DAY, end: NOW + 3 * DAY };
const pastCustom = { key: 'custom', start: NOW - 3 * DAY, end: NOW - DAY };

test('a window that runs to now is asked without until and keyed "now"', () => {
  const m = load();
  m.setWin(daily);
  const q = m.query(daily, NOW);
  assert.equal(q.key, `daily|${daily.start}|now`);
  assert.equal(q.qs, `since=${encodeURIComponent(new Date(daily.start).toISOString())}`);
});

test('a custom range that ended sends its end', () => {
  const m = load();
  const q = m.query(pastCustom, NOW);
  assert.equal(q.key, `custom|${pastCustom.start}|${pastCustom.end}`);
  assert.match(q.qs, /&until=/);
});

test('the chips read the server row for the active window, and fall back when the range changes', () => {
  const m = load();
  m.setWin(daily);
  m.setHist([
    { timestamp: NOW - 2 * DAY, agents: { scanner: 1 } },
    { timestamp: NOW - DAY, agents: { scanner: 4 } },
  ]);
  m.setData(m.query().key, { agents: [{ name: 'scanner', usd: 2.5 }] });
  assert.equal(m.spend('scanner'), 2.5, 'server row wins for the window it answered');
  assert.equal(m.spend('reviewer'), null, 'an agent the server did not list has no spend in the window');

  m.setWin(weekly);
  assert.equal(m.spend('scanner'), 3, 'a new range falls back to the browser sum until the server answers');
});

test('rows are highest spend first, and unknown tokens read as a dash, never 0', () => {
  const m = load();
  const out = m.rows({ agents: [
    { name: 'scanner', usd: 1, total_tokens: 900, input: 100, output: 50, cache_read: 750, cache_create: 0 },
    { name: 'reviewer', usd: 4, total_tokens: null, input: null, output: null, cache_read: null, cache_create: null },
  ] });
  assert.ok(out.indexOf('reviewer') < out.indexOf('scanner'), 'reviewer ($4) before scanner ($1)');
  assert.match(out, /Not zero\./);
  assert.match(out, /cache read 750/);
});

test('a range that starts before the snapshots says so', () => {
  const m = load();
  assert.equal(m.note({ coverage: 'full' }), '');
  assert.match(m.note({ coverage: 'partial', data_since: '2026-10-19T12:00:00Z' }), /The cost snapshots start 2026-10-19/);
});
