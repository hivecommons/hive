// "PRs by model" row tooltip splits closed-unmerged PRs by why they were
// closed (hivecommons/hive#18719): superseded, duplicate, other. A server
// that predates closed_unmerged_reasons, or a row with nothing closed,
// keeps the old line unchanged.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

function pick(re, what) {
  const m = html.match(re);
  assert.ok(m, what + ' not found in index.html');
  return m[0];
}

const src = [
  'let govPRModelsSort = "effectiveness"; let govPRModelsWindow = "7d"; let govPRModelsData = null;',
  'const escapeHtml = s => String(s).replace(/[&<>"\']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",\'"\':"&quot;","\'":"&#39;"}[c]));',
  'const dashboardSectionHelpMark = () => "";',
  'const isSectionCollapsed = () => false;',
  'const formatFixTime = () => "";',
  'const fmtUSD = n => n == null ? "—" : "$" + Number(n).toFixed(2);',
  pick(/function governorPRModelHiveCostTip\(b, data\) \{[\s\S]*?\n    \}\n/, 'governorPRModelHiveCostTip'),
  pick(/function governorPRModelsHiveCostNote\(data\) \{[\s\S]*?\n    \}\n/, 'governorPRModelsHiveCostNote'),
  pick(/function governorPRModelUnattributed\(b\) \{[\s\S]*?\n    \}\n/, 'governorPRModelUnattributed'),
  pick(/function governorPRModelSliceDetail\(b\) \{[\s\S]*?\n    \}\n/, 'governorPRModelSliceDetail'),
  pick(/const GOV_PR_MODEL_COLUMNS = \[[\s\S]*?\n    \];\n/, 'GOV_PR_MODEL_COLUMNS'),
  pick(/function governorPRModelRows\(data\) \{[\s\S]*?\n    \}\n/, 'governorPRModelRows'),
  // Per-model PR list (#18722): rows render a PRs toggle and, when open, the list.
  'let govPRModelPRsOpen = ""; const govPRModelPRsCache = {}; const loadGovernorPRModelPRs = () => {};',
  pick(/function normalizeGovernorPRModelsWindowName\(windowName\) \{[\s\S]*?\n    \}\n/, 'normalizeGovernorPRModelsWindowName'),
  pick(/function governorPRModelPRsKey\(windowName, model\) \{[\s\S]*?\n    \}\n/, 'governorPRModelPRsKey'),
  pick(/function governorPRModelPRsToggleHtml\(model\) \{[\s\S]*?\n    \}\n/, 'governorPRModelPRsToggleHtml'),
  pick(/function governorPRModelPRListHtml\(model\) \{[\s\S]*?\n    \}\n/, 'governorPRModelPRListHtml'),
  pick(/function renderGovernorPRModelsTile\(\) \{[\s\S]*?\n    \}\n\n    function renderGovernor\(/, 'renderGovernorPRModelsTile').replace(/\n\n    function renderGovernor\($/, ''),
].join('\n');
const lifted = new Function(src + '; return { render: data => { govPRModelsData = data; return renderGovernorPRModelsTile(); } };')();

const bucket = (model, closed, reasons) => ({
  model, backend: 'claude', backends: ['claude'], prs: 10, merged: 10 - closed, open: 0, closed_unmerged: closed,
  ...(reasons ? { closed_unmerged_reasons: reasons } : {}),
  rework: {}, effectiveness: {}, sources: [],
});

const render = b => lifted.render({ window: '7d', total: b.prs, min_merged_prs: 5, buckets: [b] });

test('row tooltip splits closed-unmerged PRs by close reason', () => {
  const out = render(bucket('claude-opus-5.5', 6, { superseded: 3, duplicate: 1, other: 2 }));
  assert.ok(out.includes('6 closed-unmerged (3 superseded · 1 duplicate · 2 other)'), 'split rendered after the closed-unmerged count');
});

test('no split when the server predates the field or nothing was closed', () => {
  const old = render(bucket('claude-opus-5.5', 2, null));
  assert.ok(old.includes('2 closed-unmerged\n'), 'old line kept');
  assert.ok(!old.includes('superseded'), 'no split without the field');
  const none = render(bucket('claude-opus-5.5', 0, { superseded: 0, duplicate: 0, other: 0 }));
  assert.ok(!none.includes('superseded'), 'no split when nothing was closed');
});
