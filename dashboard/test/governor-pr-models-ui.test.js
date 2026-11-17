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

test('Governor PRs by model shows unmeasured run columns as a dash and discloses coverage (#18713)', () => {
  for (const expected of [
    "const measured = Number(eff.runs || 0) > 0;",
    "measured ? pct(v) : '—'",
    'pr_runs: runPct(eff.verified_pr_run_rate)',
    'fail: runPct(eff.failure_rate)',
    'no_ship: runPct(eff.nothing_to_ship_rate)',
    'no runs recorded, not a measured 0%',
    'cov.closed_prs_partial',
    'cov.runs_partial',
    'first_pass_merge_rate_ci',
  ]) {
    assert.ok(indexHtml.includes(expected), `index.html missing ${expected}`);
  }
});
