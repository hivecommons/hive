import test from 'node:test';
import assert from 'node:assert/strict';
import { run } from './render-evidence-comment.mjs';
import { RENDER_EVIDENCE_MARKER } from './lib/render-evidence.mjs';

const HEAD = 'a'.repeat(40);
const env = { REPO: 'org/repo', RUN_ID: '42', HEAD_SHA: HEAD, PR_HINT: '', INDEX_PATH: 'index.json' };
const goodIndex = JSON.stringify({ schema: 1, pr: 7, head: HEAD, status: 'ok', reason: '',
  shots: [{ file: 'hive-overview-phone-dark.png', page: 'hive', section: 'overview', viewport: 'phone', theme: 'dark', bytes: 4096 }],
  skipped: [] });

// A gh double that records every call and answers by endpoint.
function fakeGH({ prHead = HEAD, existing = '' } = {}) {
  const calls = [];
  const gh = (args, input) => {
    calls.push({ args, input });
    const joined = args.join(' ');
    if (joined.includes('/pulls/')) return prHead + '\n';
    if (joined.includes('/artifacts')) return '99\n';
    if (joined.includes('/issues/7/comments') && !joined.includes('POST')) return existing;
    return '';
  };
  return { gh, calls };
}

test('first run posts one comment, with the sticky marker, and records a check', () => {
  const { gh, calls } = fakeGH();
  const out = run({ env, gh, readFile: () => goodIndex, log: () => {} });
  assert.equal(out.posted, true);
  const posts = calls.filter((c) => c.args.includes('POST') && c.args.join(' ').includes('/comments'));
  assert.equal(posts.length, 1);
  assert.ok(posts[0].input.startsWith(RENDER_EVIDENCE_MARKER));
  assert.ok(calls.some((c) => c.args.join(' ').includes('/check-runs')));
});

test('a later run updates the existing comment in place instead of adding another', () => {
  const { gh, calls } = fakeGH({ existing: '555\n' });
  run({ env, gh, readFile: () => goodIndex, log: () => {} });
  const patches = calls.filter((c) => c.args.includes('PATCH'));
  assert.equal(patches.length, 1);
  assert.ok(patches[0].args.join(' ').includes('/issues/comments/555'));
  assert.equal(calls.filter((c) => c.args.includes('POST') && c.args.join(' ').includes('/comments')).length, 0);
});

test('a PR number whose head is not the built commit is never commented on', () => {
  const { gh, calls } = fakeGH({ prHead: 'b'.repeat(40) });
  const out = run({ env, gh, readFile: () => goodIndex, log: () => {} });
  assert.equal(out.posted, false);
  assert.equal(calls.filter((c) => c.args.includes('POST') || c.args.includes('PATCH')).length, 0);
});

test('a missing or corrupt artifact degrades to a neutral comment and never throws', () => {
  const { gh } = fakeGH();
  assert.doesNotThrow(() => run({ env: { ...env, PR_HINT: '7' }, gh, readFile: () => { throw new Error('no file'); }, log: () => {} }));
  assert.doesNotThrow(() => run({ env: { ...env, PR_HINT: '7' }, gh, readFile: () => 'not json', log: () => {} }));
});

test('api failures are logged, not thrown', () => {
  const gh = () => { throw new Error('HTTP 403'); };
  assert.doesNotThrow(() => run({ env, gh, readFile: () => goodIndex, log: () => {} }));
});

test('a malformed workflow context posts nothing', () => {
  const { gh, calls } = fakeGH();
  const out = run({ env: { ...env, HEAD_SHA: 'nope' }, gh, readFile: () => goodIndex, log: () => {} });
  assert.equal(out.posted, false);
  assert.equal(calls.length, 0);
});
