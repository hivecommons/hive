'use strict';

// Delivery SLA panel helpers in src/pkg/dashboard/static/index.html: the
// sparkline series builder (carry-forward over quiet days), the target
// colour state, hour formatting and the tile markup. Pure functions run in a
// vm context; no browser.

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

function extractConst(name) {
  const start = indexHtml.indexOf(`const ${name} = `);
  assert.notEqual(start, -1, `${name} constant exists`);
  const end = indexHtml.indexOf(';\n', start);
  return indexHtml.slice(start, end + 1);
}

function load() {
  const ctx = {
    window: { _lastStatus: null },
    esc: s => String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])),
    renderSparkline: (el, series, opts) => `<svg data-series="${series.join(',')}" data-color="${opts.color}"></svg>`,
    overviewKPISparkTitle: () => 'summary',
    _overviewSLAWindow: '7d',
    _overviewSLAExpanded: false,
  };
  vm.createContext(ctx);
  const src = [
    extractConst('OVERVIEW_SLA_SPANS'),
    extractConst('OVERVIEW_SLA_WARN_RATIO'),
    extractFunction('overviewSLAState'),
    extractFunction('fmtSLAHours'),
    extractFunction('overviewSLASeries'),
    extractFunction('overviewSLASpark'),
    extractFunction('overviewSLAInfoHTML'),
    extractFunction('overviewSLATile'),
  ].join('\n');
  vm.runInContext(src, ctx);
  return ctx;
}

test('overviewSLASeries carries the last p50 forward over empty days and never dips to zero', () => {
  const ctx = load();
  const series = vm.runInContext(
    'overviewSLASeries([{start_to_pr_p50: 2}, {start_to_pr_p50: 0}, {}, {start_to_pr_p50: 5}, {start_to_pr_p50: 0}], "start_to_pr_p50")', ctx);
  assert.equal(JSON.stringify(series), '[2,2,2,5,5]');
  assert.equal(JSON.stringify(vm.runInContext('overviewSLASeries(null, "x")', ctx)), '[]');
});

test('overviewSLAState colours against the target with a 1.5x amber band', () => {
  const ctx = load();
  const state = (p50, n, target) => vm.runInContext(`overviewSLAState(${p50}, ${n}, ${target})`, ctx);
  assert.equal(state(3, 4, 4), 'ok');
  assert.equal(state(4, 1, 4), 'ok');
  assert.equal(state(5, 1, 4), 'warn');
  assert.equal(state(6, 1, 4), 'warn');
  assert.equal(state(6.1, 1, 4), 'breach');
  assert.equal(state(1, 0, 4), 'none', 'no samples is uncoloured');
  assert.equal(state(1, 3, 0), 'none', 'no target is uncoloured');
});

test('fmtSLAHours picks minutes, hours or days', () => {
  const ctx = load();
  const fmt = h => vm.runInContext(`fmtSLAHours(${h})`, ctx);
  assert.equal(fmt(0.5), '30m');
  assert.equal(fmt(2.34), '2.3h');
  assert.equal(fmt(14), '14h');
  assert.equal(fmt(72), '3.0d');
  assert.equal(fmt('NaN'), '—');
});

test('overviewSLATile renders the p50, n, target and a coloured sparkline', () => {
  const ctx = load();
  ctx.report = {
    windows: { '7d': { start_to_pr: { p50_h: 6.5, p95_h: 12, n: 3 } } },
    history: [{ start_to_pr_p50: 2 }, { start_to_pr_p50: 6.5 }],
  };
  const html = vm.runInContext('overviewSLATile(OVERVIEW_SLA_SPANS[0], report, { start_to_pr_h: 4 })', ctx);
  assert.match(html, /data-sla-state="breach"/);
  assert.match(html, /data-sla-span="start_to_pr"/);
  assert.match(html, /overview-kpi-value[^>]*>6\.5h</);
  assert.match(html, /n=3/);
  assert.match(html, /target 4\.0h/);
  assert.match(html, /data-series="2,6\.5"/);
  assert.match(html, /data-color="var\(--overview-sla-color\)"/, 'sparkline inherits the tile state colour');
  assert.match(html, /class="config-tooltip" role="tooltip"/, 'uses the shared tooltip helper markup');

  const empty = vm.runInContext('overviewSLATile(OVERVIEW_SLA_SPANS[1], { windows: {}, history: [] }, { pr_to_merge_h: 24 })', ctx);
  assert.match(empty, /data-sla-state="none"/);
  assert.match(empty, /overview-kpi-value[^>]*>—</);
  assert.doesNotMatch(empty, /<svg/, 'no sparkline without history');
});
