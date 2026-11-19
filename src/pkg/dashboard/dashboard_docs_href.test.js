// Help links on the v5 line open docs.hivecommons.dev for pages the docs site
// publishes, and GitHub for everything else (#19182).
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const src = fs.readFileSync(path.join(__dirname, 'static-src', 'js', 'sections.js'), 'utf8');
const m = src.match(/ {4}function dashboardDocsHref\(page, anchor\) \{[\s\S]*?\n {4}\}\n/);
assert.ok(m, 'dashboardDocsHref not found');

function hrefOn(line) {
  const window = { _hiveDocsLine: line };
  return new Function('window', m[0] + '; return dashboardDocsHref;')(window);
}

test('a published page on v5 opens the docs site with its anchor', () => {
  assert.strictEqual(
    hrefOn('v5')('dashboard-sections', 'governor'),
    'https://docs.hivecommons.dev/docs/hive/dashboard-sections#governor'
  );
  assert.strictEqual(
    hrefOn('v5')('dashboard-sections/activity-monitor', ''),
    'https://docs.hivecommons.dev/docs/hive/dashboard-sections/activity-monitor'
  );
});

test('an unknown line falls back to v5 and still uses the docs site', () => {
  assert.strictEqual(
    hrefOn('')('agent-configuration', 'acmm-levels'),
    'https://docs.hivecommons.dev/docs/hive/agent-configuration#acmm-levels'
  );
});

test('a page the docs site does not publish opens GitHub', () => {
  assert.strictEqual(
    hrefOn('v5')('governor-thresholds', 'scaling'),
    'https://github.com/hivecommons/hive/blob/v5/src/docs/governor-thresholds.md#scaling'
  );
});

test('another release line opens GitHub, because the docs site follows v5 only', () => {
  assert.strictEqual(
    hrefOn('v6')('dashboard-sections', 'runs'),
    'https://github.com/hivecommons/hive/blob/v6/src/docs/dashboard-sections.md#runs'
  );
});
