// Tests for bin/vibe-kanban-mirror.js (hivecommons/hive#10641, Phases 0 and 1).
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
    shadowPath: path.join(dir, 'shadow.jsonl'),
    // Simulates a developer acting on the board between mirror passes.
    edit: (fn) => {
      const state = JSON.parse(fs.readFileSync(statePath, 'utf8'));
      fn(state);
      fs.writeFileSync(statePath, JSON.stringify(state));
    },
    cleanup: () => fs.rmSync(dir, { recursive: true, force: true }),
  };
}

function readShadow(board) {
  if (!fs.existsSync(board.shadowPath)) return [];
  return fs.readFileSync(board.shadowPath, 'utf8').trim().split('\n').filter(Boolean).map((l) => JSON.parse(l));
}

function issueFor(state, key) {
  return state.issues.find((i) => (i.description || '').includes(`[hive-work-key: ${key}]`));
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

test('observePickup maps board pick-ups to extwork states and ignores non-queued items', () => {
  const item = { key: 'acme/widgets#42', state: 'queued' };
  const issue = (extra) => ({ id: 'i1', simple_id: 'VK-1', status: 'To do', ...extra });
  assert.strictEqual(mirror.observePickup({ item, issue: issue(), projectId: PROJECT_ID }), null);
  assert.strictEqual(mirror.observePickup({ item, issue: issue({ status: 'Backlog' }), projectId: PROJECT_ID }), null);
  const running = mirror.observePickup({ item, issue: issue({ status: 'in progress' }), projectId: PROJECT_ID });
  assert.strictEqual(running.action, mirror.EVENT_SHADOW_OBSERVED);
  assert.strictEqual(running.action, 'ext_work_shadow_observed');
  assert.strictEqual(running.state, 'running');
  assert.strictEqual(running.execution_key, `vibe-kanban:${PROJECT_ID}/i1`);
  assert.strictEqual(running.fields.external_start, false);
  assert.strictEqual(running.fields.work_key, 'acme/widgets#42');
  assert.strictEqual(running.fields.simple_id, 'VK-1');
  assert.strictEqual(mirror.observePickup({ item, issue: issue({ status: 'In review' }), projectId: PROJECT_ID }).state, 'waiting');
  assert.strictEqual(mirror.observePickup({ item, issue: issue({ status: 'Done' }), projectId: PROJECT_ID }).state, 'terminal');
  const prOpen = mirror.observePickup({ item, issue: issue({ latest_pr_url: 'https://github.com/acme/widgets/pull/5', latest_pr_status: 'open' }), projectId: PROJECT_ID });
  assert.strictEqual(prOpen.state, 'waiting');
  assert.strictEqual(prOpen.fields.latest_pr_url, 'https://github.com/acme/widgets/pull/5');
  assert.strictEqual(mirror.observePickup({ item, issue: issue({ status: 'In progress', latest_pr_url: 'u', latest_pr_status: 'merged' }), projectId: PROJECT_ID }).state, 'terminal');
  for (const state of ['leased', 'parked']) {
    assert.strictEqual(mirror.observePickup({ item: { ...item, state }, issue: issue({ status: 'In progress' }), projectId: PROJECT_ID }), null, state);
  }
  const renamed = { ...mirror.DEFAULT_STATUS_NAMES, inprogress: 'Doing' };
  assert.strictEqual(mirror.observePickup({ item, issue: issue({ status: 'doing' }), projectId: PROJECT_ID, statusNames: renamed }).state, 'running');
  assert.strictEqual(mirror.statusKeyForName('', renamed), '');
});

test('ShadowLog records a fact once, again on change, and remembers across restarts', () => {
  const board = newBoard();
  try {
    const now = () => new Date('2026-10-06T00:00:00Z');
    const ev = mirror.observePickup({ item: { key: 'acme/widgets#42', state: 'queued' }, issue: { id: 'i1', status: 'In progress' }, projectId: PROJECT_ID });
    const log1 = new mirror.ShadowLog(board.shadowPath, { now });
    assert.strictEqual(log1.record(ev), true);
    assert.strictEqual(log1.record(ev), false);
    const log2 = new mirror.ShadowLog(board.shadowPath, { now });
    assert.strictEqual(log2.isNew(ev), false, 'a restart must not re-record the same fact');
    const review = { ...ev, state: 'waiting', fields: { ...ev.fields, board_status: 'In review' } };
    assert.strictEqual(log2.record(review), true);
    const lines = readShadow(board);
    assert.deepStrictEqual(lines.map((l) => l.state), ['running', 'waiting']);
    assert.strictEqual(lines[0].observed_at, '2026-10-06T00:00:00.000Z');
  } finally {
    board.cleanup();
  }
});

test('shadow observation: a board pick-up is recorded, kept on the board, and never dispatched', async () => {
  const board = newBoard();
  try {
    const env = envFor(board, { VIBE_KANBAN_SHADOW_LOG: board.shadowPath });
    const first = await run(['--once'], env, hiveFetch({ queue: QUEUE, work: WORK }));
    assert.strictEqual(first.code, 0, first.lines.join('\n'));
    assert.match(first.lines.join('\n'), /errors 0, shadow pick-ups 0/);
    assert.deepStrictEqual(readShadow(board), []);

    // A developer starts a workspace on #42 (card moves to In progress) and
    // moves the operator-parked #7 too; only the queued item is a pick-up.
    board.edit((state) => {
      issueFor(state, 'acme/widgets#42').status = 'In progress';
      issueFor(state, 'acme/widgets#7').status = 'In progress';
    });
    let state = board.read();
    const callsBefore = state.calls.length;
    const second = await run(['--once'], env, hiveFetch({ queue: QUEUE, work: WORK }));
    assert.strictEqual(second.code, 0, second.lines.join('\n'));
    assert.match(second.lines.join('\n'), /recorded shadow pick-up acme\/widgets#42 \S+ running/);
    assert.match(second.lines.join('\n'), /created 0, updated 1, unchanged 3, errors 0, shadow pick-ups 1/);
    state = board.read();
    assert.strictEqual(issueFor(state, 'acme/widgets#42').status, 'In progress', 'pick-up must stay on the board');
    assert.strictEqual(issueFor(state, 'acme/widgets#7').status, 'Cancelled', 'parked item drifts back');
    const secondCalls = state.calls.slice(callsBefore).map((c) => c.name);
    assert.ok(!secondCalls.includes('start_workspace') && !secondCalls.includes('create_issue'), `unexpected calls: ${secondCalls}`);
    let shadow = readShadow(board);
    assert.strictEqual(shadow.length, 1);
    assert.strictEqual(shadow[0].action, 'ext_work_shadow_observed');
    assert.strictEqual(shadow[0].fields.work_key, 'acme/widgets#42');
    assert.strictEqual(shadow[0].fields.external_start, false);
    assert.strictEqual(shadow[0].fields.board_issue_id, issueFor(state, 'acme/widgets#42').id);

    // Same board state: nothing new is recorded or written.
    const third = await run(['--once'], env, hiveFetch({ queue: QUEUE, work: WORK }));
    assert.match(third.lines.join('\n'), /created 0, updated 0, unchanged 4, errors 0, shadow pick-ups 0/);
    assert.strictEqual(readShadow(board).length, 1);

    // A PR is linked and merged: the execution is observed as terminal.
    board.edit((s) => {
      const i = issueFor(s, 'acme/widgets#42');
      i.latest_pr_url = 'https://github.com/acme/widgets/pull/50';
      i.latest_pr_status = 'merged';
    });
    const dry = await run(['--once', '--dry-run'], env, hiveFetch({ queue: QUEUE, work: WORK }));
    assert.match(dry.lines.join('\n'), /would record shadow pick-up acme\/widgets#42 \S+ terminal/);
    assert.strictEqual(readShadow(board).length, 1, '--dry-run must not write the shadow log');
    await run(['--once'], env, hiveFetch({ queue: QUEUE, work: WORK }));
    shadow = readShadow(board);
    assert.deepStrictEqual(shadow.map((e) => e.state), ['running', 'terminal']);
    assert.strictEqual(shadow[1].fields.latest_pr_url, 'https://github.com/acme/widgets/pull/50');

    // Once hive leases the item itself, it is hive's execution, not a shadow one.
    const leased = await run(['--once'], env, hiveFetch({
      queue: QUEUE,
      work: [...WORK, { task_id: 't42', kind: 'issue', repo: 'acme/widgets', number: 42, title: 'Fix the frobnicator' }],
    }));
    assert.match(leased.lines.join('\n'), /shadow pick-ups 0/);
    assert.strictEqual(readShadow(board).length, 2);
  } finally {
    board.cleanup();
  }
});

test('without a shadow log the Phase 0 behaviour is unchanged: a moved card drifts back', async () => {
  const board = newBoard();
  try {
    await run(['--once'], envFor(board), hiveFetch({ queue: [QUEUE[0]] }));
    board.edit((state) => { issueFor(state, 'acme/widgets#42').status = 'In progress'; });
    const { lines } = await run(['--once'], envFor(board), hiveFetch({ queue: [QUEUE[0]] }));
    assert.doesNotMatch(lines.join('\n'), /shadow/);
    assert.strictEqual(issueFor(board.read(), 'acme/widgets#42').status, 'To do');
    assert.ok(!fs.existsSync(board.shadowPath));
  } finally {
    board.cleanup();
  }
});

test('--shadow-log flag is parsed', () => {
  assert.strictEqual(mirror.parseArgs(['--shadow-log', 'x.jsonl'], {}).shadowLog, 'x.jsonl');
  assert.strictEqual(mirror.parseArgs([], { VIBE_KANBAN_SHADOW_LOG: 'y.jsonl' }).shadowLog, 'y.jsonl');
  assert.throws(() => mirror.parseArgs(['--shadow-log'], {}), /needs a value/);
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
