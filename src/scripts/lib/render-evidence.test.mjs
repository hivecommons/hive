import test from 'node:test';
import assert from 'node:assert/strict';
import {
  RENDER_EVIDENCE_BUDGET_MS, RENDER_EVIDENCE_MARKER, RENDER_EVIDENCE_MAX_SECTIONS,
  affectedPages, sectionsForFile, planSections, shotFileName, classifyStatus,
  buildIndex, validateIndex, renderCommentBody, cleanText, isUIFile,
} from './render-evidence.mjs';

const IDS = ['overview-section', 'sessions-section', 'inbox-section', 'activity-monitor-section'];

test('the time budget is a named, finite constant', () => {
  assert.ok(Number.isFinite(RENDER_EVIDENCE_BUDGET_MS) && RENDER_EVIDENCE_BUDGET_MS > 0);
});

test('only dashboard sources and assets are UI files', () => {
  assert.equal(isUIFile('src/pkg/dashboard/static-src/css/inbox.css'), true);
  assert.equal(isUIFile('src/pkg/hub/assets/dashboard.html'), true);
  assert.equal(isUIFile('src/pkg/hub/server.go'), false);
  assert.equal(isUIFile('src/docs/telemetry.md'), false);
});

test('pages are chosen from the changed files', () => {
  const ids = (files) => affectedPages(files).map((p) => p.id);
  assert.deepEqual(ids(['src/pkg/dashboard/static-src/css/inbox.css']), ['hive']);
  assert.deepEqual(ids(['src/pkg/hub/static/my-hives.html']), ['hub-fleet']);
  assert.deepEqual(ids(['src/pkg/hub/assets/dashboard.html']), ['hub-dashboard']);
  assert.deepEqual(ids(['src/pkg/hub/static/tokens.css']).sort(), ['hub-dashboard', 'hub-fleet']);
  assert.deepEqual(ids(['README.md']), []);
  assert.equal(ids(['src/scripts/dashboard-render-evidence.mjs']).length, 3, 'tooling changes smoke-test every page');
  assert.equal(ids(['src/scripts/fixtures/render-evidence/api/version.json']).length, 3);
});

test('sections follow css/js stems and html partials', () => {
  assert.deepEqual(sectionsForFile('src/pkg/dashboard/static-src/css/sessions.css', IDS), ['sessions-section']);
  assert.deepEqual(sectionsForFile('src/pkg/dashboard/static-src/js/inbox-upload.js', IDS), ['inbox-section']);
  assert.deepEqual(sectionsForFile('src/pkg/dashboard/static-src/css/base.css', IDS), []);
  const read = () => '<div data-dashboard-section="inbox-section"></div><div data-dashboard-section="gone-section"></div>';
  assert.deepEqual(sectionsForFile('src/pkg/dashboard/static-src/html/inbox.html', IDS, read), ['inbox-section']);
});

test('section count is capped', () => {
  const many = IDS.concat(['a-section', 'b-section', 'c-section']);
  const files = many.map((id) => 'src/pkg/dashboard/static-src/css/' + id.replace('-section', '') + '.css');
  assert.ok(planSections(files, many).length <= RENDER_EVIDENCE_MAX_SECTIONS);
});

test('shot file names only use a conservative alphabet', () => {
  const name = shotFileName({ page: 'hive', section: '../x section', viewport: 'phone', theme: 'dark' });
  assert.match(name, /^[A-Za-z0-9_-]+\.png$/);
});

test('flake is neutral or partial, never a failure status', () => {
  assert.equal(classifyStatus({ planned: 8, captured: 8 }).status, 'ok');
  assert.equal(classifyStatus({ planned: 8, captured: 3, budgetExceeded: true }).status, 'partial');
  assert.equal(classifyStatus({ planned: 8, captured: 5 }).status, 'partial');
  assert.equal(classifyStatus({ planned: 8, captured: 0 }).status, 'neutral');
  assert.equal(classifyStatus({ planned: 0, captured: 0 }).status, 'neutral');
  const err = classifyStatus({ planned: 8, captured: 2, harnessError: 'Chrome missing' });
  assert.equal(err.status, 'neutral');
  assert.match(err.reason, /Chrome missing/);
  for (const s of ['ok', 'partial', 'neutral']) assert.notEqual(s, 'failure');
});

test('artifact data cannot inject markup, mentions or links into the comment', () => {
  const hostile = buildIndex({
    pr: 5, head: 'a'.repeat(40), status: 'partial',
    reason: 'see [x](http://evil) @someone <img src=x onerror=1> `tick` | cell',
    shots: [
      { file: 'ok-shot.png', page: 'hive', section: 'overview', viewport: 'phone', theme: 'dark', bytes: 2048 },
      { file: '../../etc/passwd', page: 'hive', section: 'overview', viewport: 'phone', theme: 'dark', bytes: 1 },
      { file: 'a.png', page: 'hive|x', section: 'o', viewport: 'phone', theme: 'dark', bytes: 1 },
      { file: 'b.png', page: 'hive', section: 'o', viewport: 'phone', theme: 'neon', bytes: 1 },
    ],
    skipped: [{ what: '[link](http://evil)', reason: '@someone' }],
    budgetMs: 1, elapsedMs: 1,
  });
  const index = validateIndex(hostile);
  assert.equal(index.shots.length, 1, 'unsafe shots are dropped');
  const body = renderCommentBody(index, { run: 'https://github.com/o/r/actions/runs/1', artifact: 'https://github.com/o/r/actions/runs/1/artifacts/2' });
  assert.ok(body.startsWith(RENDER_EVIDENCE_MARKER), 'sticky marker comes first');
  assert.ok(!body.includes('evil)'), body);
  assert.ok(!body.includes('@someone'), body);
  assert.ok(!body.includes('<img'), body);
  assert.match(body, /not live data/);
});

test('an index that is not the expected shape is rejected', () => {
  assert.equal(validateIndex(null), null);
  assert.equal(validateIndex({ schema: 2 }), null);
  assert.equal(validateIndex('x'), null);
});

test('cleanText bounds length', () => {
  assert.ok(cleanText('x'.repeat(1000)).length <= 160);
});
