'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const now = Date.parse('2026-10-30T12:10:00Z');
const context = { URL, esc: value => String(value).replace(/[&<>"']/g, c => ({
  '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
})[c]) };
vm.createContext(context);
vm.runInContext(fs.readFileSync(path.join(__dirname,
  '../../src/pkg/dashboard/static-src/js/tracker-native-queue.js'), 'utf8'), context);
const row = { kind: 'pr', repo: 'o/r', number: 7, head: 'member-head', base_ref: 'v5' };
const queue = () => ({
  provider: 'github-native', repo: 'o/r', base: 'v5', state: 'observed', complete: true,
  observed_at: '2026-10-30T12:09:00Z', fresh_for_s: 360,
  entries: [{ id: 'entry', position: 3, pr: 7, head: 'member-head', state: 'AWAITING_CHECKS', enqueued_at: '2026-10-30T12:00:00Z' }],
  candidates: [], outcomes: [],
});
const render = (q, items = [row], page = items, repo = '') =>
  context.stageTrackerNativeQueueHtml([q], items, page, now, repo);

test('real queue position, state and provider age never invent pipeline progress', () => {
  const q = queue();
  const before = JSON.stringify([q, row]);
  const html = render(q);
  assert.match(html, /value="3"/);
  assert.match(html, /AWAITING_CHECKS/);
  assert.match(html, /queued 10m/);
  assert.match(html, /on this page/);
  assert.match(html, /Enqueue is not testing or merge completion/);
  assert.doesNotMatch(html, /data-ham-key|[0-9]+%|ETA [0-9]/);
  assert.equal(JSON.stringify([q, row]), before);
});

test('unknown, stale, unavailable and partial snapshots cannot claim empty current success', () => {
  const q = queue();
  q.entries = [];
  q.complete = false;
  q.stale = true;
  q.state = 'unavailable';
  const html = render(q);
  assert.match(html, /STALE/);
  assert.match(html, /coverage incomplete/);
  assert.match(html, /does not prove an empty queue/);
  assert.match(render({ ...queue(), observed_at: '2026-10-30T10:00:00Z' }), /STALE/);
});

test('unknown member mapping never trusts a FIFO member list or PR-head green', () => {
  const q = queue();
  q.entries = [];
  q.candidates = [{ sha: 'synthetic-only', membership: 'unknown', members: [{ pr: 999, head: 'unproved' }], ci: 'unknown', checks: [] }];
  const html = render(q);
  assert.match(html, /Member mapping unknown/);
  assert.match(html, /synthetic-only/);
  assert.doesNotMatch(html, /#999|unproved/);
  assert.match(html, /never copied from PR heads/);
});

test('parallel candidate cards retain exact members and exact candidate CI drilldown', () => {
  const q = queue();
  q.candidates = ['candidate-one', 'candidate-two'].map((sha, i) => ({
    sha, base_sha: 'base-sha', tree: 'tree-' + i, membership: 'proven',
    members: [{ pr: 7, head: row.head }], ci: i ? 'failed' : 'pending', checks_known: true,
    checks: [{ name: 'test-' + i, status: i ? 'completed' : 'in_progress', conclusion: i ? 'failure' : '', url: 'https://github.com/o/r/actions/runs/' + i }],
  }));
  const html = render(q, [row], []);
  assert.equal((html.match(/<article class="nq-candidate">/g) || []).length, 2);
  assert.match(html, /Candidate required CI: pending/);
  assert.match(html, /Candidate required CI: failed/);
  assert.match(html, /outside current page\/filters/);
  assert.match(html, /https:\/\/github.com\/o\/r\/actions\/runs\/1/);
  assert.match(render(q, [{ ...row, head: 'new-head' }]), /outside tracker snapshot or head changed/);
});

test('repo filters, unknown age, bounded outcomes and safe links remain explicit', () => {
  const q = queue();
  assert.equal(render(q, [], [], 'other/repo'), '');
  assert.equal(context.stageTrackerNativeAge('invalid', now), 'age unknown');
  q.outcomes = [
    { kind: 'entry-removed', pr: 7, head: 'old', source: 'snapshot-difference', observed_at: q.observed_at },
    { kind: 'merged', pr: 8, merge_sha: 'actual-merge', source: 'signed-merged-webhook', observed_at: q.observed_at },
  ];
  const html = render(q);
  assert.match(html, /entry-removed/);
  assert.match(html, /actual-merge/);
  assert.match(html, /Removal\/destroyed is not proof/);
  assert.match(html, /gaps after restart/);
  assert.equal(context.stageTrackerNativeLink('javascript:invalid', 'check'), 'check');
});

test('DOM synchronization only updates its own rail and preserves linked pipeline trees', () => {
  const root = { hidden: true };
  const requested = [];
  context.document = { getElementById: id => { requested.push(id); return root; } };
  context.setIfChanged = (element, html) => { assert.equal(element, root); root.html = html; };
  const linked = { kind: 'issue', repo: 'o/r', number: 42, linked_prs: [{ ...row }] };
  const data = { integration_queues: [queue()], items: [linked] };
  const page = [{ item: linked }];
  const before = JSON.stringify([data, page]);
  context.stageTrackerNativeQueueSync(data, page, now, 'O/R');
  assert.equal(root.hidden, false);
  assert.match(root.html, /on this page/);
  assert.deepEqual(requested, ['activity-monitor-native-queue']);
  assert.equal(JSON.stringify([data, page]), before);
  context.stageTrackerNativeQueueSync({}, [], now, '');
  assert.equal(root.hidden, true);
});
