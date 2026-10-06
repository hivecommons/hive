// Tests for bin/vibe-kanban-mirror.js (hivecommons/hive#10641, Phase 0).
//
// The vibe-kanban side is a fake MCP stdio server
// (bin/testdata/vibe-kanban-mcp/fake-server.js) spawned exactly the way the
// real `npx -y vibe-kanban@latest --mcp` is; the hive side is a stubbed fetch
// serving /api/contribute/queue and /api/contribute/fleet payloads.
//
// Run: node bin/vibe-kanban-mirror.test.js

'use strict';

const assert = require('assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const mirror = require('./vibe-kanban-mirror.js');

const FAKE_SERVER = path.join(__dirname, 'testdata', 'vibe-kanban-mcp', 'fake-server.js');
const PROJECT_ID = '11111111-2222-4333-8444-555555555555';
const HIVE_URL = 'http://hive.test/';
const REPO = 'acme/widgets';

const tests = [];
function test(name, fn) {
  tests.push({ name, fn });
}

function hiveFetch({ queue = [], work = [] }) {
  const calls = [];
  const impl = async (url) => {
    calls.push(url);
    let body;
    if (url === 'http://hive.test/api/contribute/queue') body = { queue, queue_total: queue.length, held_total: 0 };
    else if (url === 'http://hive.test/api/contribute/fleet') body = { clankers: [], work };
    else return { ok: false, status: 404, json: async () => ({}) };
    return { ok: true, status: 200, json: async () => JSON.parse(JSON.stringify(body)) };
  };
  impl.calls = calls;
  return impl;
}

function newBoard({ tags = [{ id: 'tag-hive', project_id: PROJECT_ID, name: 'hive', color: '0 0% 50%' }] } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'vk-mirror-test-'));
  const statePath = path.join(dir, 'board.json');
  fs.writeFileSync(statePath, JSON.stringify({ projectId: PROJECT_ID, issues: [], tags, issueTags: [], calls: [], seq: 0 }));
  return {
    statePath,
    read: () => JSON.parse(fs.readFileSync(statePath, 'utf8')),
    cleanup: () => fs.rmSync(dir, { recursive: true, force: true }),
  };
}

function envFor(board, extra = {}) {
  return {
    ...process.env,
    FAKE_VK_STATE: board.statePath,
    HIVE_DASHBOARD_URL: HIVE_URL,
    VIBE_KANBAN_REPO: REPO,
    VIBE_KANBAN_PROJECT_ID: PROJECT_ID,
    VIBE_KANBAN_MCP_CMD: `"${process.execPath}" "${FAKE_SERVER}"`,
    ...extra,
  };
}

async function run(argv, env, fetchImpl) {
  const lines = [];
  const code = await mirror.main(argv, env, { fetchImpl, log: (l) => lines.push(String(l)) });
  return { code, lines };
}

const QUEUE = [
  { repo: 'acme/widgets', number: 42, title: 'Fix the frobnicator', url: 'https://github.com/acme/widgets/issues/42', labels: ['bug'], key: 'acme/widgets#42' },
  { repo: 'acme/widgets', number: 4, title: 'Docs typo', key: 'acme/widgets#4' },
  { repo: 'acme/widgets', number: 7, title: 'Parked by operator', held: true },
  { repo: 'acme/other', number: 1, title: 'Other repo, never mirrored' },
];
const WORK = [
  { task_id: 't1', kind: 'issue', repo: 'acme/widgets', number: 9, title: 'Leased elsewhere', status: 'in-progress' },
  { task_id: 't2', kind: 'repo_activity', repo: 'acme/widgets', number: 10, title: 'Not an issue', status: 'in-progress' },
];

test('status mapping covers every hive state from the proposal', () => {
  const want = { queued: 'todo', leased: 'inprogress', review: 'inreview', merged: 'done', closed: 'done', parked: 'cancelled' };
  for (const [state, key] of Object.entries(want)) {
    assert.strictEqual(mirror.boardStatusKey(state), key, state);
    assert.ok(mirror.DEFAULT_STATUS_NAMES[key], `no board column name for ${key}`);
  }
  assert.throws(() => mirror.boardStatusKey('bogus'), /unknown hive work state/);
});

test('splitCommand honours quotes and never invokes a shell', () => {
  assert.deepStrictEqual(mirror.splitCommand('npx -y vibe-kanban@latest --mcp'), ['npx', '-y', 'vibe-kanban@latest', '--mcp']);
  assert.deepStrictEqual(mirror.splitCommand(`"/opt/my node/node" 'a b' $(x)`), ['/opt/my node/node', 'a b', '$(x)']);
  assert.throws(() => mirror.splitCommand('"open'), /unterminated quote/);
});

test('trailer is bracketed so #4 never matches #42', () => {
  assert.ok(!mirror.trailer('acme/widgets#42').includes(mirror.trailer('acme/widgets#4')));
  assert.ok(mirror.boardDescription({ key: 'acme/widgets#42', url: 'https://x/42' }).endsWith('[hive-work-key: acme/widgets#42]'));
});

test('readHiveItems filters to one repo and maps queue, hold and lease', async () => {
  const items = await mirror.readHiveItems({ hiveUrl: HIVE_URL, repo: 'ACME/Widgets', fetchImpl: hiveFetch({ queue: QUEUE, work: WORK }) });
  assert.deepStrictEqual(items.map((i) => [i.key, i.state]), [
    ['acme/widgets#4', 'queued'],
    ['acme/widgets#42', 'queued'],
    ['acme/widgets#7', 'parked'],
    ['acme/widgets#9', 'leased'],
  ]);
  assert.strictEqual(items.find((i) => i.key === 'acme/widgets#4').url, 'https://github.com/acme/widgets/issues/4');
});

test('a lease wins over the same item still listed in the queue', async () => {
  const items = await mirror.readHiveItems({
    hiveUrl: HIVE_URL,
    repo: REPO,
    fetchImpl: hiveFetch({ queue: [QUEUE[0]], work: [{ kind: 'issue', repo: 'acme/widgets', number: 42, title: 'Fix the frobnicator' }] }),
  });
  assert.strictEqual(items.length, 1);
  assert.strictEqual(items[0].state, 'leased');
  assert.strictEqual(items[0].url, 'https://github.com/acme/widgets/issues/42');
});

test('default off: no MCP command or project means no reads and exit 0', async () => {
  const fetchImpl = hiveFetch({ queue: QUEUE });
  const { code, lines } = await run(['--once'], { HIVE_DASHBOARD_URL: HIVE_URL, VIBE_KANBAN_REPO: REPO }, fetchImpl);
  assert.strictEqual(code, 0);
  assert.match(lines.join('\n'), /mirror is off/);
  assert.strictEqual(fetchImpl.calls.length, 0);
});

test('missing repo is a usage error once enabled', async () => {
  const board = newBoard();
  try {
    const { code, lines } = await run(['--once'], envFor(board, { VIBE_KANBAN_REPO: '' }), hiveFetch({}));
    assert.strictEqual(code, 2);
    assert.match(lines.join('\n'), /VIBE_KANBAN_REPO/);
  } finally {
    board.cleanup();
  }
});

test('--dry-run reads the board but changes nothing', async () => {
  const board = newBoard();
  try {
    const { code, lines } = await run(['--once', '--dry-run'], envFor(board), hiveFetch({ queue: QUEUE, work: WORK }));
    assert.strictEqual(code, 0, lines.join('\n'));
    const state = board.read();
    assert.strictEqual(state.issues.length, 0);
    const mutating = state.calls.filter((c) => !['list_tags', 'list_issues'].includes(c.name));
    assert.deepStrictEqual(mutating, []);
    assert.match(lines.join('\n'), /would create acme\/widgets#42 → To do/);
  } finally {
    board.cleanup();
  }
});

test('--once mirrors, tags, and a rerun is idempotent', async () => {
  const board = newBoard();
  try {
    const first = await run(['--once'], envFor(board), hiveFetch({ queue: QUEUE, work: WORK }));
    assert.strictEqual(first.code, 0, first.lines.join('\n'));
    let state = board.read();
    const byKey = Object.fromEntries(state.issues.map((i) => [i.description.match(/\[hive-work-key: ([^\]]+)\]/)[1], i]));
    assert.deepStrictEqual(Object.keys(byKey).sort(), ['acme/widgets#4', 'acme/widgets#42', 'acme/widgets#7', 'acme/widgets#9']);
    assert.strictEqual(byKey['acme/widgets#42'].status, 'To do');
    assert.strictEqual(byKey['acme/widgets#42'].title, 'Fix the frobnicator');
    assert.ok(byKey['acme/widgets#42'].description.startsWith('https://github.com/acme/widgets/issues/42'));
    assert.strictEqual(byKey['acme/widgets#7'].status, 'Cancelled');
    assert.strictEqual(byKey['acme/widgets#9'].status, 'In progress');
    assert.strictEqual(state.issueTags.length, 4);
    assert.ok(state.issueTags.every((t) => t.tag_id === 'tag-hive'));

    const callsBefore = state.calls.length;
    const second = await run(['--once'], envFor(board), hiveFetch({ queue: QUEUE, work: WORK }));
    assert.strictEqual(second.code, 0, second.lines.join('\n'));
    state = board.read();
    assert.strictEqual(state.issues.length, 4, 'rerun must not duplicate issues');
    const rerunCalls = state.calls.slice(callsBefore).map((c) => c.name);
    assert.ok(rerunCalls.every((n) => n === 'list_tags' || n === 'list_issues'), `rerun mutated: ${rerunCalls}`);
    assert.match(second.lines.join('\n'), /created 0, updated 0, unchanged 4, errors 0/);

    // #42 is leased now: exactly one status update, nothing created.
    const leased = await run(['--once'], envFor(board), hiveFetch({
      queue: QUEUE,
      work: [...WORK, { kind: 'issue', repo: 'acme/widgets', number: 42, title: 'Fix the frobnicator' }],
    }));
    assert.strictEqual(leased.code, 0, leased.lines.join('\n'));
    state = board.read();
    assert.strictEqual(state.issues.length, 4);
    assert.strictEqual(state.issues.find((i) => i.id === byKey['acme/widgets#42'].id).status, 'In progress');
    assert.match(leased.lines.join('\n'), /created 0, updated 1, unchanged 3, errors 0/);
  } finally {
    board.cleanup();
  }
});

test('no "hive" tag on the project: mirror still runs and warns', async () => {
  const board = newBoard({ tags: [] });
  try {
    const { code, lines } = await run(['--once'], envFor(board), hiveFetch({ queue: [QUEUE[0]] }));
    assert.strictEqual(code, 0, lines.join('\n'));
    assert.match(lines.join('\n'), /no "hive" tag/);
    const state = board.read();
    assert.strictEqual(state.issues.length, 1);
    assert.strictEqual(state.issueTags.length, 0);
  } finally {
    board.cleanup();
  }
});

test('a board-side tool error is reported and fails the --once exit code', async () => {
  const board = newBoard();
  try {
    const { code, lines } = await run(['--once'], envFor(board, { VIBE_KANBAN_STATUS_NAMES: '{"todo":"Someday"}' }), hiveFetch({ queue: [QUEUE[0]] }));
    assert.strictEqual(code, 1);
    assert.match(lines.join('\n'), /update_issue: Unknown status 'Someday'/);
  } finally {
    board.cleanup();
  }
});

test('VIBE_KANBAN_STATUS_NAMES rejects unknown keys', () => {
  assert.throws(() => mirror.parseArgs([], { VIBE_KANBAN_STATUS_NAMES: '{"later":"x"}' }), /unknown status key "later"/);
  assert.strictEqual(mirror.parseArgs([], { VIBE_KANBAN_STATUS_NAMES: '{"todo":"Backlog"}' }).statusNames.todo, 'Backlog');
});

(async () => {
  let failed = 0;
  for (const t of tests) {
    try {
      await t.fn();
      console.log(`ok - ${t.name}`);
    } catch (err) {
      failed++;
      console.log(`not ok - ${t.name}\n  ${err && err.stack ? err.stack.split('\n').join('\n  ') : err}`);
    }
  }
  console.log(`\n${tests.length - failed}/${tests.length} passed`);
  process.exit(failed ? 1 : 0);
})();
