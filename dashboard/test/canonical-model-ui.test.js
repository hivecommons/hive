'use strict';

// #18718: the Cost card's by-model list and the contributor wall's model
// evidence name the canonical model id (modelfamily.Canonical, the id the
// Governor's PRs-by-model card groups by), with the raw spelling in a tooltip.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const dashboardDir = path.join(__dirname, '..', '..', 'src', 'pkg', 'dashboard');
const indexHtml = fs.readFileSync(path.join(dashboardDir, 'static', 'index.html'), 'utf8');
const costJs = fs.readFileSync(path.join(dashboardDir, 'static-src', 'js', 'cost.js'), 'utf8');
const landingGo = fs.readFileSync(path.join(dashboardDir, 'contribute_landing.go'), 'utf8');

function extractCostHelpers(src) {
  const start = src.indexOf('const costModelCanonical = m =>');
  const end = src.indexOf('const costModelTitle = m =>', start);
  assert.ok(start >= 0 && end > start, 'cost.js canonical helpers not found');
  const lineEnd = src.indexOf('\n', end);
  return src.slice(start, lineEnd);
}

function costHelpers() {
  const ctx = {
    esc: s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'),
  };
  vm.createContext(ctx);
  vm.runInContext(`${extractCostHelpers(costJs)}\nthis.label = costModelLabel; this.title = costModelTitle;`, ctx);
  return ctx;
}

test('Cost card by-model row shows the canonical id with the raw name in the tooltip', () => {
  const h = costHelpers();
  const row = { name: 'anthropic/claude-opus-5-5', canonical_model: 'claude-opus-5.5' };
  assert.equal(h.label(row), 'claude-opus-5.5');
  assert.equal(h.title(row), ' title="Reported as anthropic/claude-opus-5-5"');
});

test('Cost card keeps the raw name when there is no distinct canonical id', () => {
  const h = costHelpers();
  assert.equal(h.label({ name: 'claude-opus-5.5', canonical_model: 'claude-opus-5.5' }), 'claude-opus-5.5');
  assert.equal(h.title({ name: 'claude-opus-5.5', canonical_model: 'claude-opus-5.5' }), '');
  assert.equal(h.label({ name: 'old-model' }), 'old-model');
  assert.equal(h.title({ name: 'old-model' }), '');
  assert.equal(h.label({ name: 'auto', canonical_model: 'unknown' }), 'auto');
  assert.equal(h.label({}), 'unknown');
});

test('Cost card wiring is in both static-src and the generated index.html', () => {
  for (const expected of [
    'const costModelCanonical = m =>',
    '<td class="cmodel"${costModelTitle(m)}>${esc(costModelLabel(m))}</td>',
  ]) {
    assert.ok(costJs.includes(expected), `cost.js missing ${expected}`);
    assert.ok(indexHtml.includes(expected), `index.html missing ${expected}`);
  }
});

test('Contributor wall model evidence names the canonical model with the raw spelling in a tooltip', () => {
  for (const expected of [
    'p.model_evidence.canonical_model',
    'class="wall-canonical-model" title="Reported as \'+esc(evRaw)+\'">\'+esc(evCanon)+\'</span>',
    "evidence='<div class=\"ops-note\">model evidence'+evName+': '",
  ]) {
    assert.ok(landingGo.includes(expected), `contribute_landing.go missing ${expected}`);
  }
});
