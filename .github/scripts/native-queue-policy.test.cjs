'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const {
  ACTIONS_APP_ID, DCO_APP_ID, POLICY_CHECKS, BUILD_CHECKS,
  PendingEvidence, membersForGroup, assertEligible, evidenceForHead, closedNoop, evaluate,
  isTransientAPIError, isCosmeticLabelFlip, replayLastVerdict,
  ProviderUnavailable, ProviderVerdict, PROVIDER_VERDICT_NOTE, FORWARD_MERGE, FORWARD_MERGE_HEAD, PROW,
  DCO_ENUMERATION_SUMMARY_PREFIX,
  DCO_ENUMERATION_SUMMARY_FALLBACK,
  DCO_PROVIDER_UNAVAILABLE_TEXT,
  DCO_PROVIDER_RUNBOOK,
  PR_EVENTS, LEGACY_VERIFIER_EVENTS, HIVE_STATUS_CHECKS, HIVE_CREATOR_ENV, POLICY_CHECK_SOURCE,
  hiveCreatorLogin, assertHiveStatus, assertEvidence,
} = require('./native-queue-policy.cjs');

// Hermetic: the workflow sets this from a repository variable, and the cases
// that need the hive provider configured set it themselves (withCreator).
delete process.env[HIVE_CREATOR_ENV];

const sha = n => n.toString(16).padStart(40, '0');
// App 1861 check run 125732155592 on #16378, head 1989d20aa8d0d92cce761786ac05825aef1d9d3a.
const unavailableSummary = DCO_ENUMERATION_SUMMARY_PREFIX + '\n\n' +
  'GitHub returned 250 commits from the REST API, so the app attempted to retrieve the full list through GraphQL before evaluating the pull request. ' +
  DCO_ENUMERATION_SUMMARY_FALLBACK + '\n\nHTTP status: unknown\nGitHub request ID: unavailable';
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
    check_suite: { id: 10000 + index },
    details_url: `https://github.com/acme/widget/actions/runs/${1000 + index}/job/1`,
  }));
}

function fixture(eventName = 'merge_group') {
  const state = {
    queue: clone(entries), prs: [eligible(1), eligible(2)], seen: [], reviews: [],
    runs: new Map([1, 2].map(n => [sha(10 + n), checks(sha(10 + n))])),
    ancestry: 'ahead', ref: group.head_sha, queueReads: 0, prReads: 0, reviewReads: 0,
    statuses: [], statusReads: 0,
  };
  const github = {
    rest: {
      checks: { listForRef: 'checks' }, pulls: {
        get: async ({ pull_number: n }) => {
          state.prReads++;
          if (state.prReads === state.holdOnPRRead) state.prs[n - 1].labels = [{ name: 'hold' }];
          return { data: clone(state.prs[n - 1]) };
        },
        listReviews: 'reviews',
      },
      actions: {
        listJobsForWorkflowRun: 'jobs',
        getWorkflowRun: async ({ run_id: id }) => {
          const definition = [...Object.entries(POLICY_CHECKS), ...Object.entries(BUILD_CHECKS)][id - 1000];
          const source = definition[1];
          return { data: {
            id, check_suite_id: id + 9000,
            path: state.badWorkflow || `.github/workflows/${source[0]}`,
            event: definition[0] === 'verify PR contents' && state.verifierEvent ? state.verifierEvent : source[1][0],
            head_sha: state.workflowHead,
          } };
        },
      },
      repos: { listCommitStatusesForRef: 'statuses', compareCommitsWithBasehead: async ({ basehead }) => {
        state.seen.push(basehead);
        return { data: { status: state.ancestry, merge_base_commit: { sha: basehead.split('...')[0] } } };
      } },
      git: { getRef: async () => ({ data: { object: { sha: state.ref } } }) },
    },
    paginate: async (method, args) => {
      if (method === 'jobs') {
        const index = args.run_id - 1000;
        const [name] = [...Object.entries(POLICY_CHECKS), ...Object.entries(BUILD_CHECKS)][index];
        return [{
          id: 100000 + index, run_id: args.run_id, name, head_sha: state.workflowHead,
          check_run_url: `https://api.github.com/repos/acme/widget/check-runs/${100 + index}`,
          status: 'completed', conclusion: 'success',
        }];
      }
      if (method === 'statuses') {
        state.statusReads++;
        return clone(state.statuses);
      }
      if (method === 'checks') {
        state.workflowHead = args.ref;
        return clone(state.runs.get(args.ref));
      }
      state.reviewReads++;
      if (state.reviewReads === state.dismissOnReviewRead) {
        state.reviews = [{ id: 2, user: { login: 'reviewer' }, state: 'DISMISSED', commit_id: sha(11) }];
      }
      return clone(state.reviews);
    },
    graphql: async (_query, args) => {
      state.queueReads++;
      if (state.mutateOnReread && state.queueReads === 2) state.queue[0].pullRequest.headRefOid = sha(99);
      const start = Number(args.cursor || 0);
      const end = state.pageSize ? start + state.pageSize : state.queue.length;
      return { repository: { mergeQueue: { entries: {
        nodes: clone(state.queue.slice(start, end)),
        pageInfo: { hasNextPage: end < state.queue.length, endCursor: String(end) },
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

test('provider enumeration UNKNOWN blocks both admission paths without retry or substituted proof', async () => {
  for (const eventName of ['pull_request', 'pull_request_target', 'merge_group']) {
    const f = fixture(eventName);
    const runs = f.state.runs.get(sha(11));
    const dco = runs.find(r => r.name === 'DCO');
    Object.assign(dco, { conclusion: 'failure', output: { summary: unavailableSummary } });
    // Neither an older authoritative success nor a lowercase status-like
    // check can replace the latest failed authoritative run.
    runs.push({ ...dco, id: 1, conclusion: 'success' },
      { ...dco, id: 999, name: 'dco', conclusion: 'success', app: { id: 999 } });
    await assert.rejects(evaluate(f), error => {
      assert.ok(error instanceof ProviderUnavailable);
      assert.ok(!(error instanceof PendingEvidence));
      assert.match(error.message, /verdict UNKNOWN.*App 1861/);
      assert.ok(error.message.startsWith(DCO_PROVIDER_UNAVAILABLE_TEXT));
      assert.ok(error.message.includes(DCO_PROVIDER_RUNBOOK));
      assert.match(error.message, /Admission remains blocked/);
      assert.match(error.message, /No PR source change is requested/);
      return true;
    });
    dco.conclusion = 'success';
    assert.match(await evaluate(f), /policy providers succeeded|validated all/);
  }
});

test('UNKNOWN reporting requires trusted exact-head completed DCO and the specific enumeration diagnostic', () => {
  const dco = checks(sha(11), { DCO: null })[0];
  const unknown = { ...dco, conclusion: 'failure', output: { summary: unavailableSummary } };
  for (const change of [
    { app: { id: ACTIONS_APP_ID } }, { head_sha: sha(99) }, { status: 'in_progress' },
    { output: undefined }, { output: { summary: 'The sign-off is missing.' } },
    { output: { summary: 'DCO verdict is unknown.' } }, { conclusion: 'action_required' },
  ]) {
    assert.throws(() => evidenceForHead([{ ...unknown, ...change }], sha(11), { DCO: null }),
      error => !(error instanceof ProviderUnavailable));
  }
  assert.throws(() => evidenceForHead([{ ...dco, conclusion: 'failure' }], sha(11), { DCO: null }),
    error => !(error instanceof ProviderUnavailable) && error instanceof ProviderVerdict &&
      error.message.startsWith(`DCO: failure on ${sha(11)} `));
});

for (const prEvent of PR_EVENTS) test('a red provider is named as a per-PR verdict, not a shared CI incident (#18280)' + ` [${prEvent}]`, async () => {
  for (const [name, conclusion] of [
    ['DCO', 'action_required'], ['changelog-fragment-guard', 'failure'],
    ['verify PR contents', 'cancelled'], ['Sign-off survives the squash', 'timed_out'],
  ]) {
    const f = fixture(prEvent);
    f.state.runs.get(sha(11)).find(r => r.name === name).conclusion = conclusion;
    await assert.rejects(evaluate(f), error => {
      assert.ok(error instanceof ProviderVerdict, `${name} must be a ProviderVerdict`);
      assert.ok(!(error instanceof PendingEvidence), 'a completed verdict is not worth waiting on');
      assert.equal(error.provider, name);
      assert.equal(error.conclusion, conclusion);
      assert.ok(error.message.startsWith(`${name}: ${conclusion} on ${sha(11)} (`), error.message);
      assert.ok(error.message.includes(PROVIDER_VERDICT_NOTE), error.message);
      return true;
    });
  }
  // A path-scoped skip of a technical job is still not a verdict.
  const runs = checks(sha(11), BUILD_CHECKS).map(r => ({ ...r, conclusion: 'skipped' }));
  assert.doesNotThrow(() => evidenceForHead(runs, sha(11), BUILD_CHECKS));
});

// #17030: v6-topup.yml's PR whose probot DCO rejects a commit already on v5.
function forwardMergeFixture() {
  const f = fixture('pull_request_target');
  Object.assign(f.state.prs[0], {
    head: { sha: sha(11), ref: 'sync/v5-to-v6', repo: { full_name: 'acme/widget' } },
    base: { ref: 'v6' }, user: { ...FORWARD_MERGE.author },
  });
  f.context.payload.pull_request = clone(f.state.prs[0]);
  f.state.runs.get(sha(11)).find(r => r.name === 'DCO').conclusion = 'action_required';
  // sha(201) is the v5 commit probot rejects; sha(11) is the top-up merge of
  // v5 (sha(201)) onto v6 (sha(300)).
  f.state.commits = [
    { sha: sha(201), parents: [{ sha: sha(200) }] },
    { sha: sha(11), parents: [{ sha: sha(300) }, { sha: sha(201) }] },
  ];
  f.state.reachable = new Set([`v5...${sha(201)}`, `v6...${sha(300)}`]);
  f.state.statuses = [
    { id: 1, context: 'dco', state: 'success', creator: { login: PROW.login, id: PROW.id } },
  ];
  f.github.rest.pulls.listCommits = 'commits';
  f.github.rest.repos.listCommitStatusesForRef = 'statuses';
  f.github.rest.repos.compareCommitsWithBasehead = async ({ basehead }) => {
    f.state.seen.push(basehead);
    return { data: { status: f.state.reachable.has(basehead) ? 'behind' : 'diverged' } };
  };
  const basePaginate = f.github.paginate;
  f.github.paginate = async (method, args) => {
    if (method === 'commits') return clone(f.state.commits);
    if (method === 'statuses') return clone(f.state.statuses);
    return basePaginate(method, args);
  };
  return f;
}

test('v5-reachable sync/v5-to-v6 forward-merge passes on prow dco without probot DCO', async () => {
  const f = forwardMergeFixture();
  assert.match(await evaluate(f),
    /five existing policy providers succeeded.*DCO path: prow dco for v5-reachable sync\/v5-to-v6 forward-merge/);
  assert.deepEqual(f.state.seen, [`v5...${sha(201)}`, `v6...${sha(300)}`, `v5...${sha(201)}`]);
});

test('one commit unreachable from v5 keeps the forward-merge on probot DCO', async () => {
  const unreachable = forwardMergeFixture();
  unreachable.state.reachable.delete(`v5...${sha(201)}`);
  await assert.rejects(evaluate(unreachable), /DCO: action_required.*probot DCO.*not reachable from v5/);
  const extra = forwardMergeFixture();
  extra.state.commits.splice(1, 0, { sha: sha(202), parents: [{ sha: sha(201) }] });
  await assert.rejects(evaluate(extra), new RegExp(`DCO: action_required.*${sha(202)} is not reachable from v5`));
  for (const parents of [[{ sha: sha(201) }], [{ sha: sha(301) }, { sha: sha(201) }], [{ sha: sha(300) }, { sha: sha(202) }]]) {
    const notMerge = forwardMergeFixture();
    notMerge.state.commits[1].parents = parents;
    await assert.rejects(evaluate(notMerge), /DCO: action_required.*is not a merge of v5 into v6/);
  }
  const truncated = forwardMergeFixture();
  truncated.state.commits = Array.from({ length: 250 }, (_, i) => ({ sha: sha(1000 + i), parents: [] }));
  await assert.rejects(evaluate(truncated), /DCO: action_required.*commit list unverifiable/);
  // Probot DCO succeeding still admits through the unchanged path.
  unreachable.state.runs.get(sha(11)).find(r => r.name === 'DCO').conclusion = 'success';
  assert.match(await evaluate(unreachable), /DCO path: probot DCO \(App 1861\); sync\/v5-to-v6 fallback/);
});

// #18931: a squash or rebase queue never contains the PR's own head commit, so
// a forward-merge PR in such a group is refused by name.
test('a forward-merge PR whose head is not in the tested group is refused as a merge-method violation', async () => {
  for (const ref of ['sync/v5-to-v6', 'sync/v5-to-v6-2026-11-18', 'sync/v4-to-v5', 'topup/v5-to-v6-202611171235']) {
    const f = fixture('merge_group');
    f.state.prs[0].head = { ...f.state.prs[0].head, ref };
    f.state.ancestry = 'diverged';
    await assert.rejects(evaluate(f), error => {
      assert.match(error.message, /must land with a merge commit, never a squash or rebase/);
      assert.ok(error.message.includes(ref), error.message);
      return true;
    });
    // Contained head (the merge-commit method): the guard stays out of the way.
    const ok = fixture('merge_group');
    ok.state.prs[0].head = { ...ok.state.prs[0].head, ref };
    await evaluate(ok);
  }
  // Any other branch keeps the generic message: the guard is scoped to forward-merges.
  const other = fixture('merge_group');
  other.state.prs[0].head = { ...other.state.prs[0].head, ref: 'feature/sync-things' };
  other.state.ancestry = 'diverged';
  await assert.rejects(evaluate(other), /current PR head is not contained in the tested group/);
  for (const ref of ['sync/v5-to-v6', 'topup/x', 'sync/v4-to-v5-3']) assert.ok(FORWARD_MERGE_HEAD.test(ref), ref);
  for (const ref of ['feature/topup/x', 'fix/sync/v5-to-v6', 'sync-v5-to-v6', 'topup']) assert.ok(!FORWARD_MERGE_HEAD.test(ref), ref);
});

test('another head branch, base, fork or author keeps requiring probot DCO', async () => {
  for (const change of [
    pr => { pr.head.ref = 'sync/v5-to-v6-extra'; },
    pr => { pr.head.repo.full_name = 'fork/widget'; },
    pr => { pr.user = { login: 'clubanderson', id: 1, type: 'User' }; },
    pr => { pr.user.id = 1; },
    pr => { pr.user.login = 'hivecommons-hive[bot]'; },
  ]) {
    const f = forwardMergeFixture();
    change(f.state.prs[0]);
    await assert.rejects(evaluate(f), error => error.message === `DCO: action_required on ${sha(11)} (${PROVIDER_VERDICT_NOTE})`);
    assert.equal(f.state.seen.length, 0);
  }
  const v5 = forwardMergeFixture();
  v5.state.prs[0].base.ref = 'v5';
  v5.context.payload.pull_request.base.ref = 'v5';
  await assert.rejects(evaluate(v5), error => error.message === `DCO: action_required on ${sha(11)} (${PROVIDER_VERDICT_NOTE})`);
});

test('forward-merge path fails without a successful prow-created dco status', async () => {
  const failed = forwardMergeFixture();
  failed.state.statuses.push({ id: 2, context: 'dco', state: 'failure', creator: { login: PROW.login, id: PROW.id } });
  await assert.rejects(evaluate(failed), error => error instanceof ProviderVerdict &&
    /prow dco: failure on .*per-PR provider verdict.*DCO path: prow dco/.test(error.message));
  // Any workflow token can post a `dco` status; it never stands in for prow.
  const forged = forwardMergeFixture();
  forged.state.statuses = [{ id: 3, context: 'dco', state: 'success', creator: { login: 'github-actions[bot]', id: 41898282 } }];
  await assert.rejects(evaluate(forged), error => error instanceof PendingEvidence && /Missing prow dco/.test(error.message));
  const pending = forwardMergeFixture();
  pending.state.statuses[0].state = 'pending';
  await assert.rejects(evaluate(pending), error => error instanceof PendingEvidence);
});

test('draft, closed, held and moved-head/base PRs cannot pass', () => {
  for (const change of [
    { draft: true }, { state: 'closed' }, { head: { sha: sha(99) } }, { base: { ref: 'v6' } },
    ...['hold', 'needs-human', 'do-not-merge', 'do-not-merge/security', 'release-in-progress'].map(name => ({ labels: [{ name }] })),
  ]) assert.throws(() => assertEligible({ ...eligible(1), ...change }, sha(11), 'v5'));
});

test('ineligibility names the tripped condition, not a generic verdict (#18128)', () => {
  const expect = (change, pattern) =>
    assert.throws(() => assertEligible({ ...eligible(1), ...change }, sha(11), 'v5'), pattern);
  expect({ draft: true }, /#1 is a draft/);
  expect({ state: 'closed' }, /#1 is closed, not open/);
  expect({ head: { sha: sha(99) } }, new RegExp(`#1 head moved to ${sha(99)} \\(event head ${sha(11)}\\)`));
  expect({ base: { ref: 'v6' } }, /#1 base moved to v6 \(event base v5\)/);
  expect({ labels: [{ name: 'size/L' }, { name: 'needs-human' }, { name: 'hold' }] },
    /#1 is held by label needs-human, hold/);
  expect({ labels: [{ name: 'do-not-merge/work-in-progress' }] }, /PR state, not a CI failure/);
  assert.doesNotThrow(() => assertEligible({ ...eligible(1), labels: [{ name: 'stage/070-fixing' }] }, sha(11), 'v5'));
});

test('group evaluation checks each member head, all evidence, and live synthetic ref', async () => {
  const f = fixture();
  assert.match(await evaluate(f), /all 2 member heads/);
  assert.deepEqual(f.state.seen, [`${sha(11)}...${sha(4)}`, `${sha(12)}...${sha(4)}`]);
  assert.equal(f.state.queueReads, 2);
});

test('membership lookup consumes every page up to this group, not pending successors', async () => {
  const f = fixture();
  f.state.pageSize = 1;
  f.state.queue.push({ id: 'later', position: 3, headCommit: null, pullRequest: null });
  assert.match(await evaluate(f), /all 2 member heads/);
  assert.equal(f.state.queueReads, 4);
});

for (const prEvent of PR_EVENTS) test('PR-target admission consumes policy providers without group APIs' + ` [${prEvent}]`, async () => {
  const f = fixture(prEvent);
  assert.match(await evaluate(f), /five existing policy providers/);
  assert.equal(f.state.queueReads, 0);
  assert.equal(f.state.seen.length, 0);
});

for (const prEvent of PR_EVENTS) test('closed PR on pull_request_target passes as a no-op and skips evidence lookup' + ` [${prEvent}]`, async () => {
  const f = fixture(prEvent);
  f.state.prs[0].state = 'closed';
  let lookups = 0;
  const paginate = f.github.paginate;
  f.github.paginate = async (...args) => { lookups++; return paginate(...args); };
  assert.match(await evaluate(f), new RegExp(closedNoop(f.state.prs[0])));
  assert.equal(lookups, 0);
});

for (const prEvent of PR_EVENTS) test('PR closed during evidence wait passes as a no-op' + ` [${prEvent}]`, async () => {
  const f = fixture(prEvent);
  const get = f.github.rest.pulls.get;
  let reads = 0;
  f.github.rest.pulls.get = async args => {
    const res = await get(args);
    if (++reads === 2) res.data.state = 'closed';
    return res;
  };
  assert.match(await evaluate(f), new RegExp(closedNoop(f.state.prs[0])));
});

for (const prEvent of PR_EVENTS) test('a 5xx from the API is retried as pending evidence, never read as a verdict' + ` [${prEvent}]`, async () => {
  const apiError = status => Object.assign(new Error(`GET /check-runs - ${status}`), { status });
  for (const eventName of [prEvent, 'merge_group']) {
    const f = fixture(eventName);
    const paginate = f.github.paginate;
    let calls = 0;
    f.github.paginate = async (method, args) => {
      if (method === 'checks' && ++calls === 1) throw apiError(500);
      return paginate(method, args);
    };
    await assert.rejects(evaluate(f), error =>
      error instanceof PendingEvidence && /GitHub API 500/.test(error.message) && error.cause?.status === 500);
    // The retry is the workflow loop's job: the next evaluate succeeds on its own.
    assert.match(await evaluate(f), /policy providers succeeded|validated all/);
  }
  // Only the server side is transient: a 4xx or a non-HTTP failure stays a hard failure.
  for (const failure of [apiError(404), apiError(403), new Error('socket hang up')]) {
    const f = fixture(prEvent);
    f.github.paginate = async () => { throw failure; };
    await assert.rejects(evaluate(f), error => !(error instanceof PendingEvidence) && error === failure);
  }
  assert.equal(isTransientAPIError({ status: 502 }), true);
  assert.equal(isTransientAPIError({ status: '503' }), true);
  assert.equal(isTransientAPIError({ status: 429 }), false);
  assert.equal(isTransientAPIError({ status: 600 }), false);
  assert.equal(isTransientAPIError(undefined), false);
});

for (const prEvent of PR_EVENTS) test('open PR with moved head still fails closed on pull_request_target' + ` [${prEvent}]`, async () => {
  const f = fixture(prEvent);
  f.state.prs[0].head.sha = sha(99);
  await assert.rejects(evaluate(f), /#1 head moved to/);
});

test('untrusted events and workflow provenance are rejected', async () => {
  for (const eventName of ['push', 'workflow_dispatch', 'issue_comment', 'workflow_run']) {
    await assert.rejects(evaluate(fixture(eventName)), /Unsupported/);
  }
  const g = fixture();
  g.state.badWorkflow = '.github/workflows/untrusted.yml';
  await assert.rejects(evaluate(g), /Unexpected workflow/);
});

// feedback.yml's `gate` job is skipped on a PR closed unmerged. Adopting a PR
// at the same head closes the original after opening the copy, so that skip
// lands newer than docker.yml's `gate` on the copy's head.
const FEEDBACK_RUN = 8000;
function feedbackGateFixture(eventName, { dockerGate = true, feedback = {} } = {}) {
  const f = fixture(eventName);
  const runs = f.state.runs.get(sha(11));
  const docker = runs.find(r => r.name === 'gate');
  runs.push({ ...docker, id: 900, conclusion: 'skipped', check_suite: { id: 17000 },
    details_url: `https://github.com/acme/widget/actions/runs/${FEEDBACK_RUN}/job/1`, ...feedback });
  if (!dockerGate) runs.splice(runs.indexOf(docker), 1);
  f.state.feedbackJobReads = 0;
  f.state.workflowRunReads = 0;
  const getWorkflowRun = f.github.rest.actions.getWorkflowRun;
  f.github.rest.actions.getWorkflowRun = async args => {
    f.state.workflowRunReads++;
    return args.run_id === FEEDBACK_RUN ? { data: {
      id: FEEDBACK_RUN, check_suite_id: 17000, path: '.github/workflows/feedback.yml',
      event: 'pull_request', head_sha: sha(11),
    } } : getWorkflowRun(args);
  };
  const paginate = f.github.paginate;
  f.github.paginate = async (method, args) => {
    if (method !== 'jobs' || args.run_id !== FEEDBACK_RUN) return paginate(method, args);
    f.state.feedbackJobReads++;
    return [{
      id: 180000, run_id: FEEDBACK_RUN, name: 'gate', head_sha: sha(11),
      check_run_url: 'https://api.github.com/repos/acme/widget/check-runs/900',
      status: 'completed', conclusion: 'skipped',
    }];
  };
  return f;
}

const rejectsAsVerdict = (promise, provider, conclusion) => assert.rejects(promise, error => {
  assert.ok(error instanceof ProviderVerdict, error.message);
  assert.equal(error.provider, provider);
  assert.equal(error.conclusion, conclusion);
  return true;
});

test('a newer skipped gate from feedback.yml is passed over for docker.yml gate', async () => {
  for (const eventName of [...PR_EVENTS, 'merge_group']) {
    const f = feedbackGateFixture(eventName);
    assert.match(await evaluate(f), /policy providers succeeded|validated all/, eventName);
    assert.equal(f.state.feedbackJobReads, 0, 'a passed-over run is never bound');
  }
});

test('the newest docker.yml gate still decides: a skipped one is a verdict over an older success', async () => {
  for (const withFeedback of [false, true]) {
    const f = withFeedback ? feedbackGateFixture('pull_request') : fixture('pull_request');
    const runs = f.state.runs.get(sha(11));
    const docker = runs.find(r => r.name === 'gate' && r.id === 100);
    runs.push({ ...docker, id: 50 });
    docker.conclusion = 'skipped';
    await rejectsAsVerdict(evaluate(f), 'gate', 'skipped');
  }
});

test('a newer docker.yml gate on an event gate does not accept is judged as before, never passed over', async () => {
  for (const [conclusion, expect] of [['skipped', 'verdict'], ['success', /Unexpected workflow\/event\/head for gate/]]) {
    const f = fixture('pull_request');
    const runs = f.state.runs.get(sha(11));
    const docker = runs.find(r => r.name === 'gate');
    runs.push({ ...docker, id: 901, conclusion, check_suite: { id: 17001 },
      details_url: 'https://github.com/acme/widget/actions/runs/8001/job/1' });
    const getWorkflowRun = f.github.rest.actions.getWorkflowRun;
    f.github.rest.actions.getWorkflowRun = async args => (args.run_id === 8001 ? { data: {
      id: 8001, check_suite_id: 17001, path: '.github/workflows/docker.yml',
      event: 'workflow_dispatch', head_sha: sha(11),
    } } : getWorkflowRun(args));
    if (expect === 'verdict') await rejectsAsVerdict(evaluate(f), 'gate', 'skipped');
    else await assert.rejects(evaluate(f), expect);
  }
});

test('a newer feedback.yml gate success is not passed over and stays a provenance failure', async () => {
  const f = feedbackGateFixture('pull_request', { feedback: { conclusion: 'success' } });
  await assert.rejects(evaluate(f), /Unexpected workflow\/event\/head for gate on #1/);
});

test('only a feedback.yml gate on the head is a provenance failure, never a skipped verdict', async () => {
  for (const eventName of [...PR_EVENTS, 'merge_group']) {
    const f = feedbackGateFixture(eventName, { dockerGate: false });
    await assert.rejects(evaluate(f), error => {
      assert.ok(!(error instanceof ProviderVerdict), error.message);
      assert.ok(!(error instanceof PendingEvidence), error.message);
      assert.match(error.message, /Unexpected workflow\/event\/head for gate on #1/);
      return true;
    });
  }
});

test('a newer gate that is not provably another suite-bound workflow stops the scan and is judged as before', async () => {
  // Another App: rejected as an untrusted provider, as today.
  await assert.rejects(evaluate(feedbackGateFixture('pull_request', { feedback: { app: { id: 999 } } })),
    /Untrusted provider or wrong head for gate/);
  // A details_url that is not this repository's Actions run, or a check suite
  // the workflow run does not own: the skip is read as the verdict, as today.
  for (const feedback of [
    { details_url: 'not a url' },
    { details_url: `https://example.com/acme/widget/actions/runs/${FEEDBACK_RUN}/job/1` },
    { check_suite: { id: 99999 } },
    { check_suite: null },
  ]) {
    await rejectsAsVerdict(evaluate(feedbackGateFixture('pull_request', { feedback })), 'gate', 'skipped');
  }
});

test('a newer pending feedback.yml gate is pending evidence with no workflow-run read', async () => {
  for (const status of ['queued', 'in_progress']) {
    const f = feedbackGateFixture('pull_request', { feedback: { status, conclusion: null } });
    await assert.rejects(evaluate(f), error => error instanceof PendingEvidence && /gate is pending/.test(error.message));
    assert.equal(f.state.workflowRunReads, 0);
  }
});

for (const prEvent of PR_EVENTS) test('copied expected-run URL/name cannot substitute a foreign check suite or check identity' + ` [${prEvent}]`, async () => {
  for (const change of [
    { id: 999, check_suite: { id: 99999 } },
    { id: 999 }, // Even a claimed matching suite cannot replace actual job membership.
    { check_suite: null },
  ]) {
    const f = fixture(prEvent);
    const gate = f.state.runs.get(sha(11)).find(r => r.name === 'gate');
    Object.assign(gate, change);
    await assert.rejects(evaluate(f), /suite\/job identity mismatch/);
  }
});

for (const prEvent of PR_EVENTS) test('expected workflow suite alone does not accept a wrong job head, name or conclusion' + ` [${prEvent}]`, async () => {
  for (const change of [
    { head_sha: sha(99) }, { name: 'another job' }, { conclusion: 'failure' },
    { run_id: 99999 }, { status: 'in_progress' },
  ]) {
    const f = fixture(prEvent);
    const paginate = f.github.paginate;
    f.github.paginate = async (method, args) => {
      const results = await paginate(method, args);
      return method === 'jobs' ? results.map(job => ({ ...job, ...change })) : results;
    };
    await assert.rejects(evaluate(f), /suite\/job identity mismatch/);
  }
});

for (const prEvent of PR_EVENTS) test('Dependabot PR-target events require an actual successful verifier, never a skipped proof' + ` [${prEvent}]`, async () => {
  for (const conclusion of ['success', 'skipped', 'failure']) {
    const f = fixture(prEvent);
    f.context.actor = 'dependabot[bot]';
    f.context.payload.sender = { login: 'dependabot[bot]', type: 'Bot' };
    f.context.payload.pull_request.user = clone(f.context.payload.sender);
    f.state.prs[0].user = clone(f.context.payload.sender);
    f.state.runs.get(sha(11)).find(r => r.name === 'verify PR contents').conclusion = conclusion;
    if (conclusion === 'success') {
      assert.match(await evaluate(f), /five existing policy providers/);
    } else {
      await assert.rejects(evaluate(f), /verify PR contents/);
    }
  }
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

for (const prEvent of PR_EVENTS) test('late hold or approval withdrawal is caught by the final re-read' + ` [${prEvent}]`, async () => {
  for (const change of [{ holdOnPRRead: 3 }, { dismissOnReviewRead: 3 }]) {
    const f = fixture();
    Object.assign(f.state, change);
    await assert.rejects(evaluate(f), /held|approval/);
  }
  const pr = fixture(prEvent);
  pr.state.holdOnPRRead = 2;
  await assert.rejects(evaluate(pr), /held/);
});

test('API failure is never interpreted as an empty successful member list', async () => {
  const f = fixture();
  f.github.graphql = async () => { throw new Error('403 queue read forbidden'); };
  await assert.rejects(evaluate(f), /403/);
});

test('isCosmeticLabelFlip is true only for unread labels on label events', () => {
  const ctx = (eventName, action, label) => ({ eventName, payload: { action, ...(label && { label: { name: label } }) } });
  for (const eventName of PR_EVENTS) {
    assert.equal(isCosmeticLabelFlip(ctx(eventName, 'labeled', 'size/L')), true);
    assert.equal(isCosmeticLabelFlip(ctx(eventName, 'unlabeled', 'size/L')), true);
    assert.equal(isCosmeticLabelFlip(ctx(eventName, 'labeled', 'hold')), false);
  }
  assert.equal(isCosmeticLabelFlip(ctx('workflow_dispatch', 'labeled', 'size/L')), false);
  assert.equal(isCosmeticLabelFlip(ctx('pull_request_target', 'labeled', 'size/L')), true);
  assert.equal(isCosmeticLabelFlip(ctx('pull_request_target', 'unlabeled', 'size/L')), true);
  assert.equal(isCosmeticLabelFlip(ctx('pull_request_target', 'labeled', 'hold')), false);
  assert.equal(isCosmeticLabelFlip(ctx('pull_request_target', 'labeled', 'do-not-merge/foo')), false);
  assert.equal(isCosmeticLabelFlip(ctx('pull_request_target', 'synchronize', 'size/L')), false);
  assert.equal(isCosmeticLabelFlip(ctx('merge_group', 'checks_requested')), false);
  assert.equal(isCosmeticLabelFlip(ctx('pull_request_target', 'labeled')), false);
});

function replayFixture(priorRuns, labels = [], workflowOverride = {}, prEvent = 'pull_request_target') {
  const f = fixture(prEvent);
  f.state.prs[0].labels = labels;
  const head = sha(11);
  f.state.listed = [];
  const runs = clone(priorRuns).map(r => ({
    name: 'native-queue-policy', head_sha: head, app: { id: ACTIONS_APP_ID },
    check_suite: { id: 5000 + r.id },
    details_url: `https://github.com/acme/widget/actions/runs/${3000 + r.id}/job/1`, ...r,
  }));
  const baseGetWorkflowRun = f.github.rest.actions.getWorkflowRun;
  const basePaginate = f.github.paginate;
  f.github.rest.actions.getWorkflowRun = async ({ run_id: id }) => (id < 3000 ? baseGetWorkflowRun({ run_id: id }) : { data: {
    id, check_suite_id: id + 2000, path: '.github/workflows/native-queue-policy.yml',
    event: prEvent, head_sha: head, ...workflowOverride,
  } });
  f.github.paginate = async (method, args) => {
    // Policy provider evidence comes from the base fixture (all green).
    if (method === 'jobs' && args.run_id < 3000) return basePaginate(method, args);
    if (method === 'checks' && !args.check_name) return basePaginate(method, args);
    if (method === 'jobs') {
      const run = runs.find(r => 3000 + r.id === args.run_id);
      if (!run) return [];
      return [{
        id: 200000 + run.id, run_id: args.run_id, name: run.name, head_sha: head,
        check_run_url: `https://api.github.com/repos/acme/widget/check-runs/${run.id}`,
        status: 'completed', conclusion: run.conclusion,
      }];
    }
    assert.equal(method, 'checks');
    f.state.listed.push(args);
    return clone(runs);
  };
  return f;
}

test('cosmetic flip replays the latest completed success on the same head', async () => {
  const f = replayFixture([
    { id: 5, status: 'completed', conclusion: 'failure' },
    { id: 7, status: 'completed', conclusion: 'success' },
    { id: 9, status: 'in_progress', conclusion: null },
  ]);
  assert.match(await replayLastVerdict(f), /run 7 \(success\)/);
  assert.equal(f.state.listed[0].ref, sha(11));
});

test('cosmetic flip does not replay a success when a policy provider re-ran red on the head', async () => {
  for (const name of Object.keys(POLICY_CHECKS)) {
    const f = replayFixture([{ id: 7, status: 'completed', conclusion: 'success' }]);
    f.state.runs.get(sha(11)).find(r => r.name === name).conclusion = 'failure';
    await assert.rejects(replayLastVerdict(f), new RegExp(`${name}: failure`));
  }
});

test('cosmetic flip falls through to the full wait while a policy provider is re-running or missing', async () => {
  // PendingEvidence is not a verdict: returning null sends the caller to the
  // polling wait instead of failing the check with no retry.
  for (const name of Object.keys(POLICY_CHECKS)) {
    for (const change of [{ status: 'queued', conclusion: null }, { status: 'in_progress', conclusion: null }]) {
      const f = replayFixture([{ id: 7, status: 'completed', conclusion: 'success' }]);
      Object.assign(f.state.runs.get(sha(11)).find(r => r.name === name), change);
      assert.equal(await replayLastVerdict(f), null);
    }
    const f = replayFixture([{ id: 7, status: 'completed', conclusion: 'success' }]);
    const runs = f.state.runs.get(sha(11));
    runs.splice(runs.findIndex(r => r.name === name), 1);
    assert.equal(await replayLastVerdict(f), null);
  }
});

test('cosmetic flip falls through to the full wait when the DCO provider reports UNKNOWN', async () => {
  // The full wait re-raises ProviderUnavailable once and renders the runbook
  // job summary; failing here would skip that operator-facing surface.
  const f = replayFixture([{ id: 7, status: 'completed', conclusion: 'success' }]);
  Object.assign(f.state.runs.get(sha(11)).find(r => r.name === 'DCO'),
    { conclusion: 'failure', output: { summary: unavailableSummary } });
  assert.equal(await replayLastVerdict(f), null);
});

test('cosmetic flip has nothing to replay without a prior success and falls through', async () => {
  // A failed, cancelled (timed-out) or absent prior run must not be pinned
  // onto every later stage-label flip: null sends the caller to the full wait.
  assert.equal(await replayLastVerdict(replayFixture([
    { id: 5, status: 'completed', conclusion: 'success' },
    { id: 7, status: 'completed', conclusion: 'failure' },
  ])), null);
  assert.equal(await replayLastVerdict(replayFixture([
    { id: 5, status: 'completed', conclusion: 'success' },
    { id: 7, status: 'completed', conclusion: 'cancelled' },
  ])), null);
  assert.equal(await replayLastVerdict(replayFixture([
    { id: 9, status: 'in_progress', conclusion: null },
  ])), null);
  assert.equal(await replayLastVerdict(replayFixture([])), null);
  const own = replayFixture([{ id: 8, status: 'completed', conclusion: 'success' }]);
  assert.equal(await replayLastVerdict({ ...own, currentCheckRunId: 8 }), null);
});

test('cosmetic flip still honours a live hold even after a prior success', async () => {
  const f = replayFixture([{ id: 7, status: 'completed', conclusion: 'success' }], [{ name: 'hold' }]);
  await assert.rejects(replayLastVerdict(f), /held/);
});

test('replay ignores runs with a different name, head_sha or app id', async () => {
  for (const change of [
    { name: 'other-check' }, { head_sha: sha(99) }, { app: { id: DCO_APP_ID } }, { app: undefined },
  ]) {
    assert.equal(
      await replayLastVerdict(replayFixture([{ id: 7, status: 'completed', conclusion: 'success', ...change }])),
      null);
  }
});

test('replay rejects a same-named success run from another workflow', async () => {
  const run = { id: 7, status: 'completed', conclusion: 'success' };
  for (const override of [
    { path: '.github/workflows/forged.yml' },
    { event: 'workflow_dispatch' },
    { event: 'push' },
    { head_sha: sha(99) },
    { check_suite_id: 1 },
  ]) {
    await assert.rejects(replayLastVerdict(replayFixture([run], [], override)), /Unexpected workflow|identity mismatch/);
  }
  await assert.rejects(replayLastVerdict(replayFixture([
    { ...run, details_url: 'https://example.com/acme/widget/actions/runs/3007/job/1' },
  ])), /Invalid Actions provenance/);
  await assert.rejects(replayLastVerdict(replayFixture([
    { ...run, details_url: 'https://github.com/acme/widget/actions/runs/3008/job/1' },
  ])), /Unexpected workflow|identity mismatch/);
});

// --- hive-posted commit status as evidence for `verify PR contents` ---------

const HIVE_LOGIN = 'hive-gate[bot]';
const VERIFY = 'verify PR contents';
const repo = { owner: 'acme', repo: 'widget' };
const status = (id, state, over = {}) => ({
  id, context: VERIFY, state, creator: { login: HIVE_LOGIN, id: 4242 }, ...over,
});
const statusGithub = statuses => ({
  rest: { repos: { listCommitStatusesForRef: 'statuses' } },
  paginate: async (method, args) => {
    assert.equal(method, 'statuses');
    assert.equal(args.ref, sha(11));
    return clone(statuses);
  },
});

async function withCreator(login, fn) {
  const saved = process.env[HIVE_CREATOR_ENV];
  if (login === undefined) delete process.env[HIVE_CREATOR_ENV]; else process.env[HIVE_CREATOR_ENV] = login;
  try {
    return await fn();
  } finally {
    if (saved === undefined) delete process.env[HIVE_CREATOR_ENV]; else process.env[HIVE_CREATOR_ENV] = saved;
  }
}

const removeRun = (f, name) => {
  const runs = f.state.runs.get(sha(11));
  runs.splice(runs.findIndex(r => r.name === name), 1);
};

test('legacy evidence is only widened: other contexts and the original verifier event are unchanged', () => {
  assert.deepEqual(POLICY_CHECKS[VERIFY], ['pr-verifier.yml', ['pull_request', 'pull_request_target', 'workflow_dispatch']]);
  assert.deepEqual(LEGACY_VERIFIER_EVENTS, ['pull_request', 'pull_request_target', 'workflow_dispatch']);
  assert.ok(POLICY_CHECKS[VERIFY][1].includes('pull_request_target'));
  assert.deepEqual(POLICY_CHECK_SOURCE, ['native-queue-policy.yml', ['pull_request', 'pull_request_target']]);
  assert.deepEqual(PR_EVENTS, ['pull_request', 'pull_request_target']);
  assert.deepEqual(POLICY_CHECKS.gate, ['docker.yml', ['push', 'pull_request']]);
  assert.equal(POLICY_CHECKS.DCO, null);
  assert.deepEqual(POLICY_CHECKS['changelog-fragment-guard'], ['changelog-fragment-guard.yml', ['pull_request']]);
  assert.deepEqual(POLICY_CHECKS['Sign-off survives the squash'], ['dco-squash-attribution.yml', ['pull_request']]);
  assert.deepEqual([...HIVE_STATUS_CHECKS], [VERIFY]);
});

test('legacy verifier evidence is accepted from pull_request, pull_request_target and workflow_dispatch', async () => {
  await withCreator(undefined, async () => {
    for (const verifierEvent of LEGACY_VERIFIER_EVENTS) {
      for (const prEvent of PR_EVENTS) {
        const f = fixture(prEvent);
        f.state.verifierEvent = verifierEvent;
        assert.match(await evaluate(f), /five existing policy providers/, `${verifierEvent} under ${prEvent}`);
      }
    }
    // Any other event for that workflow is still a provenance failure.
    for (const verifierEvent of ['push', 'schedule', 'issue_comment', 'merge_group']) {
      const f = fixture('pull_request');
      f.state.verifierEvent = verifierEvent;
      await assert.rejects(evaluate(f), /Unexpected workflow/);
    }
  });
});

test('hive status: only the configured creator counts, newest wins', async () => {
  const pass = (statuses, name = VERIFY) => assertHiveStatus(statusGithub(statuses), repo, sha(11), name, HIVE_LOGIN);
  await pass([status(1, 'success')]);
  // Older failure, newer success: newest wins.
  await pass([status(1, 'failure'), status(2, 'success')]);
  await pass([status(2, 'success'), status(1, 'error')]);
  // Newer failure beats an older success.
  await assert.rejects(pass([status(1, 'success'), status(2, 'failure')]), ProviderVerdict);
  // A newer pending status supersedes an older success: not yet evidence.
  await assert.rejects(pass([status(1, 'success'), status(2, 'pending')]), PendingEvidence);
  // Login comparison is case-insensitive (GitHub logins are).
  await assertHiveStatus(statusGithub([status(1, 'success', { creator: { login: 'HIVE-GATE[bot]' } })]),
    repo, sha(11), VERIFY, HIVE_LOGIN);
});

test('hive status: a spoofed creator is ignored, whatever its state or recency', async () => {
  const forged = (id, state) => status(id, state, { creator: { login: 'github-actions[bot]', id: 41898282 } });
  const run = statuses => assertHiveStatus(statusGithub(statuses), repo, sha(11), VERIFY, HIVE_LOGIN);
  // A forged success never satisfies the gate.
  await assert.rejects(run([forged(9, 'success')]), PendingEvidence);
  await assert.rejects(run([status(1, 'success', { creator: undefined }), status(2, 'success', { creator: {} })]), PendingEvidence);
  await assert.rejects(run([status(1, 'success', { creator: { login: 'hive-gate' } })]), PendingEvidence);
  await assert.rejects(run([status(1, 'success', { creator: { login: 'hive-gate[bot]x' } })]), PendingEvidence);
  // A newer forged success does not mask the genuine failure.
  await assert.rejects(run([status(1, 'failure'), forged(9, 'success')]), ProviderVerdict);
  // A newer forged failure does not mask the genuine success.
  await run([status(1, 'success'), forged(9, 'failure')]);
  // A genuine success for another context is not evidence for this one.
  await assert.rejects(run([status(1, 'success', { context: 'DCO' })]), PendingEvidence);
});

test('hive status: missing and pending are pending evidence, failure and error are verdicts', async () => {
  const run = statuses => assertHiveStatus(statusGithub(statuses), repo, sha(11), VERIFY, HIVE_LOGIN);
  await assert.rejects(run([]), error => error instanceof PendingEvidence && /Missing hive verify PR contents/.test(error.message));
  await assert.rejects(run([status(1, 'pending')]), PendingEvidence);
  for (const state of ['failure', 'error']) {
    await assert.rejects(run([status(1, state)]), error => {
      assert.ok(error instanceof ProviderVerdict);
      assert.ok(!(error instanceof PendingEvidence));
      assert.equal(error.conclusion, state);
      assert.match(error.message, new RegExp(`hive verify PR contents status: ${state} on ${sha(11)}`));
      return true;
    });
  }
});

test('hive status: an unset or blank variable means not configured, never any creator', async () => {
  for (const value of [undefined, '', '   ']) {
    await withCreator(value, async () => {
      assert.equal(hiveCreatorLogin(), '');
      const github = statusGithub([status(1, 'success'), status(2, 'success', { creator: { login: 'anyone' } })]);
      await assert.rejects(assertHiveStatus(github, repo, sha(11), VERIFY),
        error => !(error instanceof PendingEvidence) && !(error instanceof ProviderVerdict) && /not configured/.test(error.message));
    });
  }
  await withCreator(` ${HIVE_LOGIN} `, async () => {
    assert.equal(hiveCreatorLogin(), HIVE_LOGIN);
    await assertHiveStatus(statusGithub([status(1, 'success')]), repo, sha(11), VERIFY);
  });
});

test('gate without the variable ignores hive statuses entirely and behaves as before', async () => {
  await withCreator(undefined, async () => {
    for (const prEvent of PR_EVENTS) {
      // A missing legacy verifier stays pending, even with a hive success on the head.
      const f = fixture(prEvent);
      removeRun(f, VERIFY);
      f.state.statuses = [status(1, 'success')];
      await assert.rejects(evaluate(f), error => error instanceof PendingEvidence && /Missing verify PR contents/.test(error.message));
      assert.equal(f.state.statusReads, 0);
      // A red legacy verifier stays a verdict.
      const g = fixture(prEvent);
      g.state.runs.get(sha(11)).find(r => r.name === VERIFY).conclusion = 'failure';
      g.state.statuses = [status(1, 'success')];
      await assert.rejects(evaluate(g), ProviderVerdict);
      assert.equal(g.state.statusReads, 0);
    }
  });
});

test('gate with the variable accepts either provider for verify PR contents', async () => {
  await withCreator(HIVE_LOGIN, async () => {
    for (const prEvent of PR_EVENTS) {
      // Legacy green: accepted exactly as before, without consulting statuses.
      const legacy = fixture(prEvent);
      assert.match(await evaluate(legacy), /five existing policy providers/);
      assert.doesNotMatch(await evaluate(fixture(prEvent)), /hive status/);
      assert.equal(legacy.state.statusReads, 0);
      // Hive only: no verifier run at all.
      const hive = fixture(prEvent);
      removeRun(hive, VERIFY);
      hive.state.statuses = [status(1, 'success')];
      assert.match(await evaluate(hive), /five existing policy providers.*hive status accepted for verify PR contents/);
      // Legacy red but the hive status is green: either provider suffices.
      const mixed = fixture(prEvent);
      mixed.state.runs.get(sha(11)).find(r => r.name === VERIFY).conclusion = 'failure';
      mixed.state.statuses = [status(1, 'success')];
      assert.match(await evaluate(mixed), /hive status accepted/);
    }
  });
});

test('gate with the variable: a forged status never satisfies it, missing waits, failure is a verdict', async () => {
  await withCreator(HIVE_LOGIN, async () => {
    for (const prEvent of PR_EVENTS) {
      const forged = fixture(prEvent);
      removeRun(forged, VERIFY);
      forged.state.statuses = [status(1, 'success', { creator: { login: 'github-actions[bot]', id: 41898282 } })];
      await assert.rejects(evaluate(forged), PendingEvidence);

      const missing = fixture(prEvent);
      removeRun(missing, VERIFY);
      await assert.rejects(evaluate(missing), error => error instanceof PendingEvidence && /Missing verify PR contents/.test(error.message));

      const pending = fixture(prEvent);
      removeRun(pending, VERIFY);
      pending.state.statuses = [status(1, 'pending')];
      await assert.rejects(evaluate(pending), PendingEvidence);

      const failed = fixture(prEvent);
      removeRun(failed, VERIFY);
      failed.state.statuses = [status(1, 'failure')];
      await assert.rejects(evaluate(failed), error => error instanceof ProviderVerdict && error.provider === 'hive verify PR contents status');

      // A legacy verdict is not hidden behind a pending hive status.
      const legacyRed = fixture(prEvent);
      legacyRed.state.runs.get(sha(11)).find(r => r.name === VERIFY).conclusion = 'failure';
      await assert.rejects(evaluate(legacyRed), error => error instanceof ProviderVerdict && error.provider === VERIFY);

      // A legacy provenance failure stays a hard failure when the hive status is absent.
      const untrusted = fixture(prEvent);
      untrusted.state.verifierEvent = 'push';
      await assert.rejects(evaluate(untrusted), error => !(error instanceof PendingEvidence) && /Unexpected workflow/.test(error.message));
    }
  });
});

test('a hive status satisfies only verify PR contents, never another required context', async () => {
  await withCreator(HIVE_LOGIN, async () => {
    for (const name of Object.keys(POLICY_CHECKS).filter(n => n !== VERIFY)) {
      const f = fixture('pull_request');
      removeRun(f, name);
      f.state.statuses = [status(1, 'success', { context: name })];
      await assert.rejects(evaluate(f), PendingEvidence, name);
    }
  });
});

test('merge-group evidence accepts either provider for verify PR contents per member head', async () => {
  await withCreator(HIVE_LOGIN, async () => {
    const f = fixture();
    for (const n of [1, 2]) {
      const runs = f.state.runs.get(sha(10 + n));
      runs.splice(runs.findIndex(r => r.name === VERIFY), 1);
    }
    // The verifier is gone on both heads: statuses for the heads decide.
    f.github.paginate = (base => async (method, args) => (method === 'statuses'
      ? [status(1, 'success')] : base(method, args)))(f.github.paginate);
    assert.match(await evaluate(f), /validated all 2 member heads.*hive status accepted for verify PR contents/);
    // A forged status leaves the group pending.
    const g = fixture();
    for (const n of [1, 2]) {
      const runs = g.state.runs.get(sha(10 + n));
      runs.splice(runs.findIndex(r => r.name === VERIFY), 1);
    }
    g.github.paginate = (base => async (method, args) => (method === 'statuses'
      ? [status(1, 'success', { creator: { login: 'mallory' } })] : base(method, args)))(g.github.paginate);
    await assert.rejects(evaluate(g), PendingEvidence);
  });
});

test('forward-merge DCO path still works with the hive provider configured', async () => {
  await withCreator(HIVE_LOGIN, async () => {
    const f = forwardMergeFixture();
    assert.match(await assertEvidence(f.github, f.context.repo, 1, sha(11), POLICY_CHECKS, f.state.prs[0]), /prow dco/);
  });
});

test('cosmetic-flip replay accepts a prior native-queue-policy success from either PR event', async () => {
  for (const prEvent of PR_EVENTS) {
    for (const runEvent of PR_EVENTS) {
      const f = replayFixture([{ id: 7, status: 'completed', conclusion: 'success' }], [], { event: runEvent }, prEvent);
      assert.match(await replayLastVerdict(f), /run 7 \(success\)/, `${runEvent} run under ${prEvent}`);
    }
  }
});

test('forward-merge fixture runs under the plain pull_request event too', async () => {
  const f = forwardMergeFixture();
  f.context.eventName = 'pull_request';
  assert.match(await evaluate(f), /prow dco/);
});
