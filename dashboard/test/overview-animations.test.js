'use strict';

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

test('Overview KPI animation honors reduced motion by rendering the final value immediately', () => {
  let rafCalls = 0;
  const node = {
    textContent: '0',
    getAttribute(name) {
      return {
        'data-overview-kpi-key': 'open-prs',
        'data-overview-kpi-value': '42',
      }[name] || '';
    },
  };
  const context = {
    exports: {},
    document: { documentElement: {} },
    window: {
      matchMedia: () => ({ matches: true }),
      requestAnimationFrame: () => { rafCalls++; },
      getComputedStyle: () => ({ getPropertyValue: () => '800ms' }),
    },
    performance: { now: () => 0 },
  };
  const source = [
    'const _overviewKPIValues = {};',
    extractFunction('overviewPrefersReducedMotion'),
    extractFunction('overviewCssDurationMs'),
    extractFunction('animateOverviewKPIs'),
    'exports.animateOverviewKPIs = animateOverviewKPIs;',
  ].join('\n');
  vm.runInNewContext(source, context);
  context.exports.animateOverviewKPIs({ querySelectorAll: () => [node] }, {});
  assert.equal(node.textContent, '42');
  assert.equal(rafCalls, 0);
});

test('Overview identical data signatures do not re-trigger data animations', () => {
  const context = { exports: {} };
  const source = [
    "let _overviewAnimationDataSignature = '';",
    extractFunction('overviewShouldAnimateData'),
    'exports.overviewShouldAnimateData = overviewShouldAnimateData;',
  ].join('\n');
  vm.runInNewContext(source, context);
  const signature = JSON.stringify({ repos: ['hivecommons/hive'], issues: [['ready', 1]], prs: [['open', 2]] });
  assert.equal(context.exports.overviewShouldAnimateData(signature), true);
  assert.equal(context.exports.overviewShouldAnimateData(signature), false);
  assert.equal(context.exports.overviewShouldAnimateData(signature.replace('2', '3')), true);
});
