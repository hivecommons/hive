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

class FakeClassList {
  constructor(initial = '') { this.set = new Set(initial.split(/\s+/).filter(Boolean)); }
  add(...names) { names.forEach(n => this.set.add(n)); }
  remove(...names) { names.forEach(n => this.set.delete(n)); }
  contains(name) { return this.set.has(name); }
}

class FakeEl {
  constructor(name, classes = '', attrs = {}) {
    this.name = name;
    this.classList = new FakeClassList(classes);
    this.attrs = attrs;
    this.children = [];
    this.parentElement = null;
    this.style = {};
    this.scrollHeight = 200;
  }
  append(...children) {
    children.forEach(child => { child.parentElement = this; this.children.push(child); });
    return this;
  }
  matches(selector) {
    return selector.split(',').some(part => {
      const s = part.trim();
      if (s === '.sec-headline') return this.classList.contains('sec-headline');
      if (s === '[data-collapsed-keep]') return this.attrs['data-collapsed-keep'] !== undefined;
      if (s === '.collapsed-hidden') return this.classList.contains('collapsed-hidden');
      if (s === '.collapsed-control-hidden') return this.classList.contains('collapsed-control-hidden');
      if (s === '[data-collapsed-control]') return this.attrs['data-collapsed-control'] !== undefined;
      if (s === '.collapsed-control') return this.classList.contains('collapsed-control');
      if (s === '.dash-card-body') return this.classList.contains('dash-card-body');
      if (s === '.section-body') return this.classList.contains('section-body');
      return false;
    });
  }
  querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  querySelectorAll(selector) {
    const direct = selector.includes(' ') ? selector.split(/\s+/).pop() : selector;
    const out = [];
    const walk = node => {
      node.children.forEach(child => {
        if (child.matches(direct)) out.push(child);
        walk(child);
      });
    };
    walk(this);
    return out;
  }
  closest(selector) {
    for (let n = this; n; n = n.parentElement) if (n.matches(selector)) return n;
    return null;
  }
}

function loadCollapseHelpers() {
  const context = { exports: {}, setTimeout: () => {} };
  const source = [
    "const SECTION_EXPANDED_MAX_HEIGHT = 'none';",
    'const SECTION_TRANSITION_MS = 200;',
    "const SECTION_MAX_HEIGHT_PROPERTY = 'max-height';",
    extractFunction('sectionHeadlineSelector'),
    extractFunction('clearCollapsedHeadlineVisibility'),
    extractFunction('applyCollapsedHeadlineVisibility'),
    extractFunction('releaseSectionMaxHeightAfterTransition'),
    extractFunction('applySectionBodyCollapse'),
    'exports.applySectionBodyCollapse = applySectionBodyCollapse;',
  ].join('\n');
  vm.runInNewContext(source, context);
  return context.exports.applySectionBodyCollapse;
}

const headlineSections = [
  'overview-section',
  'governor',
  'pr-throughput-section',
  'token-panel',
  'cost-panel',
  'knowledge-section',
];

test('collapsed dashboard sections retain only their full-width headline rows', () => {
  const applySectionBodyCollapse = loadCollapseHelpers();
  for (const sectionId of headlineSections) {
    const body = new FakeEl(`${sectionId}-body`, 'dash-card-body section-body');
    const before = new FakeEl('before-row');
    const shell = new FakeEl('shell');
    const headline = new FakeEl('headline', 'sec-headline', { 'data-collapsed-keep': '' });
    const control = new FakeEl('control', 'collapsed-control', { 'data-collapsed-control': '' });
    const after = new FakeEl('after-row');
    headline.append(control);
    shell.append(headline, after);
    body.append(before, shell);

    applySectionBodyCollapse(body, true, false);

    assert.equal(body.classList.contains('collapsed'), true, `${sectionId}: body collapsed`);
    assert.equal(body.classList.contains('collapsed-with-headline'), true, `${sectionId}: headline mode`);
    assert.equal(headline.classList.contains('collapsed-hidden'), false, `${sectionId}: headline remains visible`);
    assert.equal(before.classList.contains('collapsed-hidden'), true, `${sectionId}: prior body child hidden`);
    assert.equal(after.classList.contains('collapsed-hidden'), true, `${sectionId}: non-headline sibling hidden`);
    assert.equal(control.classList.contains('collapsed-control-hidden'), true, `${sectionId}: controls hidden while collapsed`);
  }
});

test('sections without a headline keep using the collapsed summary fallback', () => {
  const applySectionBodyCollapse = loadCollapseHelpers();
  const body = new FakeEl('audit-body', 'dash-card-body section-body').append(new FakeEl('audit-panel'));
  applySectionBodyCollapse(body, true, false);
  assert.equal(body.classList.contains('collapsed'), true);
  assert.equal(body.classList.contains('collapsed-with-headline'), false);
  assert.match(indexHtml, /\.dash-card\.collapsed \.dash-card-summary \{ display: inline-flex/);
  assert.match(indexHtml, /\.dash-card\.collapsed\.has-collapsed-headline \.dash-card-summary \{ display: none; \}/);
});
