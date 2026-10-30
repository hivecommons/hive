'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const {
  ACTIONS_APP_ID, DCO_APP_ID, POLICY_CHECKS, BUILD_CHECKS,
  PendingEvidence, membersForGroup, assertEligible, evidenceForHead, evaluate,
} = require('./native-queue-policy.cjs');

const sha = n => n.toString(16).padStart(40, '0');
const group = {
  base_sha: sha(1), head_sha: sha(4), base_ref: 'refs/heads/v5',
  head_ref: 'refs/heads/gh-readonly-queue/v5/pr-2-example',
};
const entries = [
  { id: 'one', position: 1, baseCommit: { oid: sha(1) }, headCommit: { oid: sha(3) },
    pullRequest: { number: 1, headRefOid: sha(11), baseRefName: 'v5' } },
  { id: 'two', position: 2, baseCommit: { oid: sha(3) }, headCommit: { oid: sha(4) },
    pullRequest: { number: 2, headRefOid: sha(12), baseRefName: 'v5' } },
];
const clone = x => structuredClone(x);
const eligible = n => ({
  number: n, state: 'open', draft: false, head: { sha: sha(10 + n) },
  base: { ref: 'v5' }, labels: [],
});

function checks(head, definitions = { ...POLICY_CHECKS, ...BUILD_CHECKS }) {
  return Object.entries(definitions).map(([name, source], index) => ({
    id: 100 + index, name, head_sha: head, status: 'completed', conclusion: 'success',
    app: { id: source ? ACTIONS_APP_ID : DCO_APP_ID },
    details_url: `https://github.com/acme/widget/actions/runs/${100 + index}/job/1`,
  }));
}

function fixture(eventName = 'merge_group') {
  const state = {
    queue: clone(entries), prs: [eligible(1), eligible(2)], seen: [], reviews: [],
    runs: new Map([1, 2].map(n => [sha(10 + n), checks(sha(10 + n))])),
    ancestry: 'ahead', ref: group.head_sha, queueReads: 0,
  };
  const github = {
    rest: {
      checks: { listForRef: 'checks' }, pulls: {
        get: async ({ pull_number: n }) => ({ data: clone(state.prs[n - 1]) }),
        listReviews: 'reviews',
      },
      actions: {
        getWorkflowRun: async ({ run_id: id }) => {
          const definition = [...Object.entries(POLICY_CHECKS), ...Object.entries(BUILD_CHECKS)][id - 100];
          const source = definition[1];
          return { data: {
            path: state.badWorkflow || `.github/workflows/${source[0]}`,
            event: source[1][0], head_sha: state.workflowHead,
          } };
        },
      },
      repos: { compareCommitsWithBasehead: async ({ basehead }) => {
        state.seen.push(basehead);
        return { data: { status: state.ancestry, merge_base_commit: { sha: basehead.split('...')[0] } } };
      } },
      git: { getRef: async () => ({ data: { object: { sha: state.ref } } }) },
    },
    paginate: async (method, args) => {
      if (method === 'checks') {
        state.workflowHead = args.ref;
        return clone(state.runs.get(args.ref));
      }
      return clone(state.reviews);
    },
    graphql: async () => {
      state.queueReads++;
      if (state.mutateOnReread && state.queueReads === 2) state.queue[0].pullRequest.headRefOid = sha(99);
      return { repository: { mergeQueue: { entries: {
        nodes: clone(state.queue), pageInfo: { hasNextPage: false },
      } } } };
    },
  };
  const context = {
    repo: { owner: 'acme', repo: 'widget' }, eventName,
    payload: eventName === 'merge_group' ? { action: 'checks_requested', merge_group: clone(group) } :
      { pull_request: clone(state.prs[0]) },
  };
  return { github, context, state };
}

test('all members, not merely the PR encoded in the group ref', () => {
  assert.deepEqual(membersForGroup(entries, group).map(e => e.pullRequest.number), [1, 2]);
});

test('missing, duplicate, reordered and changed-base membership fail closed', () => {
  for (const mutate of [
    es => es.shift(),
    es => { es[0].pullRequest.number = 2; },
    es => { es[1].baseCommit.oid = sha(99); },
    es => { es[0].pullRequest.baseRefName = 'v6'; },
    es => { es[1].headCommit = null; },
    es => { es[0].pullRequest.headRefOid = ''; },
  ]) {
    const es = clone(entries);
    mutate(es);
    assert.throws(() => membersForGroup(es, group));
  }
});

test('malformed group identity is rejected', () => {
  for (const change of [{ head_sha: '' }, { base_sha: '' }, { head_ref: 'refs/heads/v5' }, { base_ref: 'v5' }]) {
    assert.throws(() => membersForGroup(entries, { ...group, ...change }));
  }
});

test('every one of the eleven protections needs exact-head evidence', () => {
  const definitions = { ...POLICY_CHECKS, ...BUILD_CHECKS };
  assert.equal(Object.keys(definitions).length, 11);
  const runs = checks(sha(11));
  assert.equal(evidenceForHead(runs, sha(11), definitions).length, 11);
  for (const run of runs) {
    assert.throws(() => evidenceForHead(runs.filter(r => r.id !== run.id), sha(11), definitions), PendingEvidence);
    for (const change of [
      { status: 'in_progress' }, { conclusion: 'failure' }, { conclusion: 'neutral' },
      { app: { id: 999 } }, { head_sha: sha(99) },
    ]) {
      assert.throws(() => evidenceForHead(runs.map(r => r.id === run.id ? { ...r, ...change } : r), sha(11), definitions));
    }
  }
});

test('no Actions-generated DCO, stale success, or skipped policy proof', () => {
  const runs = checks(sha(11));
  const dco = runs.find(r => r.name === 'DCO');
  for (const change of [{ app: { id: ACTIONS_APP_ID } }, { conclusion: 'skipped' }, { status: 'queued' }]) {
    assert.throws(() => evidenceForHead([...runs, { ...dco, id: 999, ...change }], sha(11), POLICY_CHECKS));
  }
  const technical = runs.map(r => Object.hasOwn(BUILD_CHECKS, r.name) ? { ...r, conclusion: 'skipped' } : r);
  assert.equal(evidenceForHead(technical, sha(11), BUILD_CHECKS).length, 6);
});

test('draft, closed, held and moved-head/base PRs cannot pass', () => {
  for (const change of [
    { draft: true }, { state: 'closed' }, { head: { sha: sha(99) } }, { base: { ref: 'v6' } },
    ...['hold', 'needs-human', 'do-not-merge', 'do-not-merge/security', 'release-in-progress'].map(name => ({ labels: [{ name }] })),
  ]) assert.throws(() => assertEligible({ ...eligible(1), ...change }, sha(11), 'v5'));
});

test('group evaluation checks each member head, all evidence, and live synthetic ref', async () => {
  const f = fixture();
  assert.match(await evaluate(f), /all 2 member heads/);
  assert.deepEqual(f.state.seen, [`${sha(11)}...${sha(4)}`, `${sha(12)}...${sha(4)}`]);
  assert.equal(f.state.queueReads, 2);
});

test('PR-target admission consumes policy providers without group APIs', async () => {
  const f = fixture('pull_request_target');
  assert.match(await evaluate(f), /five existing policy providers/);
  assert.equal(f.state.queueReads, 0);
  assert.equal(f.state.seen.length, 0);
});

test('untrusted events and workflow provenance are rejected', async () => {
  const f = fixture('pull_request');
  await assert.rejects(evaluate(f), /Unsupported/);
  const g = fixture();
  g.state.badWorkflow = '.github/workflows/untrusted.yml';
  await assert.rejects(evaluate(g), /Unexpected workflow/);
});

test('earlier member failure blocks the whole group', async () => {
  const f = fixture();
  f.state.runs.get(sha(11)).find(r => r.name === 'DCO').conclusion = 'failure';
  await assert.rejects(evaluate(f), /DCO: failure/);
});

test('changed membership, missing ancestry and moved ref fail closed', async () => {
  for (const change of [
    { mutateOnReread: true }, { ancestry: 'diverged' }, { ref: sha(99) },
  ]) {
    const f = fixture();
    Object.assign(f.state, change);
    await assert.rejects(evaluate(f));
  }
});

test('changes requested, withdrawn and stale approvals block group validation', async () => {
  for (const review of [
    { state: 'CHANGES_REQUESTED', commit_id: sha(11) },
    { state: 'DISMISSED', commit_id: sha(11) },
    { state: 'APPROVED', commit_id: sha(99) },
  ]) {
    const f = fixture();
    f.state.reviews = [{ id: 1, user: { login: 'reviewer' }, ...review }];
    await assert.rejects(evaluate(f), /approval|changes requested/);
  }
});

test('API failure is never interpreted as an empty successful member list', async () => {
  const f = fixture();
  f.github.graphql = async () => { throw new Error('403 queue read forbidden'); };
  await assert.rejects(evaluate(f), /403/);
});
