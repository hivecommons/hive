'use strict';

// The contributor-first tile (agents.<name>.contributor_first): the governor
// is withholding a resident agent's kicks because a contributor relay is
// acting as the role. The tile must name the lane and who covers it, say when
// the resident agent resumes during the grace, render nothing when no lane is
// covered, and escape everything it prints.

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

function loadTile() {
  const context = { exports: {} };
  const source = [
    "function esc(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/\"/g, '&quot;'); }",
    extractFunction('contributorFirstFactsHtml'),
    'exports.contributorFirstFactsHtml = contributorFirstFactsHtml;',
  ].join('\n');
  vm.runInNewContext(source, context);
  return context.exports.contributorFirstFactsHtml;
}

test('contributor-first tile renders nothing when no lane is covered', () => {
  const tile = loadTile();
  assert.equal(tile(undefined, 'stat'), '');
  assert.equal(tile({}, 'stat'), '');
  assert.equal(tile({ covered_lanes: {} }), '');
  assert.equal(tile({ covered_lanes: null }), '');
});

test('contributor-first tile names the lane and who covers it, in both variants', () => {
  const tile = loadTile();
  const gov = { covered_lanes: { quality: { contributors: ['alice', 'bob'], since: '2026-11-03T12:00:00Z' } } };
  const stat = tile(gov, 'stat');
  assert.match(stat, /id="gov-contributor-first-tile"/);
  assert.match(stat, /class="label">contributor-first</);
  assert.match(stat, /quality covered by alice, bob, resident kicks withheld/);
  const metric = tile(gov);
  assert.match(metric, /class="gm-label">contributor-first</);
  assert.match(metric, /quality covered by alice, bob, resident kicks withheld/);
  assert.doesNotMatch(metric, /gov-contributor-first-tile/);
});

test('contributor-first tile says when the resident agent resumes during the grace', () => {
  const tile = loadTile();
  const graceUntil = new Date(Date.now() + 5 * 60 * 1000).toISOString();
  const html = tile({ covered_lanes: { quality: { contributors: [], grace_until: graceUntil } } }, 'stat');
  assert.match(html, /quality cover ended, resident kicks resume in ~5 min unless a relay returns/);
  const noGrace = tile({ covered_lanes: { quality: {} } }, 'stat');
  assert.match(noGrace, /resident kicks resume after grace/);
});

test('contributor-first tile lists lanes in a stable order and escapes what it prints', () => {
  const tile = loadTile();
  const html = tile({ covered_lanes: {
    scanner: { contributors: ['dave'] },
    quality: { contributors: ['<img src=x>'] },
  } }, 'stat');
  assert.ok(html.indexOf('quality covered by') < html.indexOf('scanner covered by'), 'lanes are sorted');
  assert.doesNotMatch(html, /<img src=x>/);
  assert.match(html, /&lt;img src=x&gt;/);
});
