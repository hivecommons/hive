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

// Extracts `function name(` / `async function name(` from index.html by brace matching.
function extractFunction(name) {
  const re = new RegExp(`(?:async )?function ${name}\\(`);
  const m = re.exec(indexHtml);
  assert.ok(m, `index.html does not define ${name}()`);
  const open = indexHtml.indexOf('{', m.index + m[0].length);
  let depth = 0;
  for (let i = open; i < indexHtml.length; i++) {
    if (indexHtml[i] === '{') depth++;
    else if (indexHtml[i] === '}' && --depth === 0) return indexHtml.slice(m.index, i + 1);
  }
  assert.fail(`unbalanced braces in ${name}()`);
}

test('Opening a model row requests model=… and lists its PRs with GitHub links (#18722)', async () => {
  const vm = require('node:vm');
  const fetchURLs = [];
  const sandbox = {
    fetchURLs,
    renders: 0,
    escapeHtml: s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'),
    fetch: async url => {
      fetchURLs.push(url);
      return {
        ok: true,
        json: async () => ({
          prs: [
            { repo: 'org/a', number: 2, title: 'reworked <b>', url: 'https://github.com/org/a/pull/2', outcome: 'merged', review_rounds: 2, fix_attempts: 1, agent: 'fixer', backend: 'pi' },
            { repo: 'org/b', number: 4, url: 'https://github.com/org/b/pull/4', outcome: 'closed_unmerged', review_rounds: 1, fix_attempts: 2, first_pass: false },
          ],
          prs_truncated: true,
        }),
      };
    },
  };
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  const src = [
    "let govPRModelsWindow = '30d';",
    "let govPRModelPRsOpen = '';",
    'const govPRModelPRsCache = {};',
    'const govPRModelPRsLoading = {};',
    'function renderGovernorFromLastStatus() { renders++; }',
    ...['normalizeGovernorPRModelsWindowName', 'governorPRModelPRsKey', 'loadGovernorPRModelPRs', 'toggleGovernorPRModelPRs', 'governorPRModelPRsToggleHtml', 'governorPRModelPRListHtml'].map(extractFunction),
    'this.api = { toggleGovernorPRModelPRs, governorPRModelPRsToggleHtml, governorPRModelPRListHtml, state: () => govPRModelPRsOpen };',
  ].join('\n');
  vm.runInContext(src, sandbox);
  const api = sandbox.api;

  assert.match(api.governorPRModelPRsToggleHtml('claude-opus-5.5'), /data-action="toggleGovernorPRModelPRs" data-arg0="claude-opus-5\.5"/);
  assert.match(api.governorPRModelPRsToggleHtml('claude-opus-5.5'), /aria-expanded="false"/);
  assert.equal(api.governorPRModelPRListHtml('claude-opus-5.5'), '', 'a closed row renders no list');

  await api.toggleGovernorPRModelPRs('claude-opus-5.5');
  assert.deepEqual(fetchURLs, ['/api/governor/pr-models?window=30d&model=claude-opus-5.5']);
  assert.equal(api.state(), 'claude-opus-5.5');
  assert.match(api.governorPRModelPRsToggleHtml('claude-opus-5.5'), /aria-expanded="true"/);
  const html = api.governorPRModelPRListHtml('claude-opus-5.5');
  assert.match(html, /<a href="https:\/\/github\.com\/org\/a\/pull\/2" target="_blank" rel="noopener">org\/a#2<\/a>/);
  assert.match(html, /<a href="https:\/\/github\.com\/org\/b\/pull\/4" target="_blank" rel="noopener">org\/b#4<\/a>/);
  assert.match(html, /reworked &lt;b&gt;/, 'titles are escaped');
  assert.match(html, /closed-unmerged/);
  assert.match(html, /Showing the first 2 PRs/, 'a truncated list says so');
  assert.equal(api.governorPRModelPRListHtml('gpt-6.1-sol'), '', 'other rows stay closed');

  // Reopening the same model in the same window uses the cache.
  await api.toggleGovernorPRModelPRs('claude-opus-5.5');
  assert.equal(api.state(), '');
  await api.toggleGovernorPRModelPRs('claude-opus-5.5');
  assert.equal(fetchURLs.length, 1, 'cached per window and model');
});

test('Governor PRs by model tooltip lists per-backend rework and the effort split (#18715)', () => {
  for (const expected of [
    'const sliceRework = s =>',
    'first pass ${s.first_pass_merged || 0}/${s.sample_prs || 0}',
    '${s.closed_unmerged || 0} closed-unmerged',
    'avg fix attempts ${(s.avg_fix_attempts || 0).toFixed(1)}',
    "${src.backend || 'unknown'}: ${sliceRework(src)}",
    "(b.efforts || []).map(e => `effort ${e.effort || 'unknown'}: ${sliceRework(e)}`)",
    "'\\n\\nBy effort:\\n' + effortBreakdown",
  ]) {
    assert.ok(indexHtml.includes(expected), `index.html missing ${expected}`);
  }
});

test('Governor PRs by model tooltip lists repos and weekly trend when present (#18716)', () => {
  const vm = require('node:vm');
  const ctx = {};
  vm.createContext(ctx);
  vm.runInContext(extractFunction('governorPRModelSliceDetail') + '\nthis.detail = governorPRModelSliceDetail;', ctx);
  const detail = ctx.detail;

  const text = detail({
    repos: [
      { repo: 'org/alpha', prs: 3, merged: 2, first_pass_merge_rate: 0.5, avg_fix_attempts: 0.5 },
      { repo: 'org/beta', prs: 1, merged: 0 },
    ],
    weeks: [
      { week_start: '2026-09-14', prs: 1, merged: 1, first_pass_merge_rate: 1, avg_fix_attempts: 0 },
      { week_start: '2026-09-21', prs: 2, merged: 2, first_pass_merge_rate: 0.5, avg_fix_attempts: 0.5 },
    ],
  });
  assert.match(text, /^By repository:\n/);
  assert.ok(text.includes('org/alpha: 3 PRs (2 merged, first-pass 50%, avg fix 0.5)'), text);
  assert.ok(text.includes('org/beta: 1 PRs (0 merged)'), text);
  assert.ok(text.includes('Weekly trend'), text);
  assert.ok(text.indexOf('2026-09-14: 1 PRs (1 merged, first-pass 100%, avg fix 0.0)') < text.indexOf('2026-09-21: 2 PRs'), text);

  // 7d buckets carry repos but no weeks.
  const sevenDay = detail({ repos: [{ repo: 'org/alpha', prs: 1, merged: 1, first_pass_merge_rate: 1 }] });
  assert.ok(sevenDay.includes('By repository:'), sevenDay);
  assert.ok(!sevenDay.includes('Weekly trend'), sevenDay);

  // Buckets without either (older servers) add nothing to the tooltip.
  assert.equal(detail({}), '');
  assert.equal(detail({ repos: [], weeks: [] }), '');

  assert.ok(indexHtml.includes("${sliceDetail ? '\\n\\n' + sliceDetail : ''}"), 'tooltip appends the slice detail');
});
