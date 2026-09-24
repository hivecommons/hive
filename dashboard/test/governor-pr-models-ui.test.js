'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const indexHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'pkg', 'dashboard', 'static', 'index.html'), 'utf8');

test('Governor PRs by model renders effectiveness controls and columns', () => {
  for (const expected of [
    "govPRModelsSort = 'effectiveness'",
    'setGovernorPRModelsSort',
    'governorPRModelRows(data)',
    'data-action="setGovernorPRModelsSort"',
    'PR runs',
    'No ship',
    'nothing-to-ship',
    'rank requires',
  ]) {
    assert.match(indexHtml, new RegExp(expected.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
  }
});
