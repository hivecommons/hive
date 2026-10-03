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

function extractVar(name) {
  const re = new RegExp(`var ${name}=([^;]+);`);
  const match = indexHtml.match(re);
  assert.ok(match, `${name} variable exists`);
  return `var ${name}=${match[1]};`;
}

test('dashboard notices are structurally pinned before dashboard sections', () => {
  const rootStart = indexHtml.indexOf('<div id="hive-dashboard-root"');
  assert.notEqual(rootStart, -1, 'dashboard root exists');
  const rootOpenEnd = indexHtml.indexOf('>', rootStart);
  const firstChild = indexHtml.slice(rootOpenEnd + 1).match(/\S[\s\S]*?<([a-z0-9-]+)([^>]*)>/i);
  assert.ok(firstChild, 'dashboard root has a first child element');
  assert.match(firstChild[0], /id="dash-notices"/, 'dash-notices is the first dashboard root child');

  const noticesStart = indexHtml.indexOf('<div id="dash-notices"');
  const firstSection = indexHtml.indexOf('data-dashboard-section=');
  assert.ok(noticesStart >= 0 && firstSection > noticesStart, 'dash-notices precedes the first dashboard section');
  const noticesOpen = indexHtml.slice(noticesStart, indexHtml.indexOf('>', noticesStart));
  assert.doesNotMatch(noticesOpen, /data-dashboard-section/, 'dash-notices is not a layout section');
  assert.ok(indexHtml.indexOf('id="release-status"', noticesStart) < indexHtml.indexOf('id="planning-intro"', noticesStart), 'channel notice renders before planning notice');
});

test('dashboard layout migration ignores the pinned notices slot', () => {
  const context = { exports: {} };
  const source = [
    extractVar('DASHBOARD_LAYOUT_VERSION'),
    extractVar('DASHBOARD_LAYOUT_TEMPLATE'),
    'function dashboardFeatureSectionHidden(){ return false; }',
    extractFunction('dashboardLayoutAllIds'),
    extractFunction('dashboardLayoutNormalize'),
    'exports.dashboardLayoutNormalize = dashboardLayoutNormalize;',
  ].join('\n');
  vm.runInNewContext(source, context);
  const normalized = context.exports.dashboardLayoutNormalize({ v: 1, main: ['faq-section', 'dash-notices', 'overview-section'] });
  assert.equal(normalized.main.includes('dash-notices'), false);
  assert.equal(normalized.main[0], 'faq-section');
  // Sections missing from the saved layout slot in next to their template
  // neighbours (#10397): Overview remains the first default section, followed
  // by Governor and the v6 Runs section.
  assert.equal(normalized.main[1], 'overview-section');
  assert.equal(normalized.main[2], 'governor');
  assert.equal(normalized.main[3], 'runs-section');
});
