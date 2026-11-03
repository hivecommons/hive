'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { diagnose } = require('./diagnose-dco-provider.cjs');
const sha = n => n.toString(16).padStart(40, '0');

function fixture() {
  const state = {
    pr: { head: { sha: sha(251) }, base: { sha: sha(999) }, commits: 251 },
    // Commit 251 must survive enumeration, including its invalid sign-off.
    commits: Array.from({ length: 251 }, (_, i) => ({ oid: sha(i + 1), message: i === 250 ? 'unsigned' : 'signed' })),
    calls: [], reads: 0,
  };
  const api = async (args, input) => {
    state.calls.push({ args, input });
    if (args[1] === 'graphql') {
      const start = Number(input.variables.cursor || 0);
      const end = Math.min(start + 100, state.commits.length);
      const result = { data: { repository: { pullRequest: {
        headRefOid: state.pr.head.sha,
        commits: {
          totalCount: state.pr.commits, nodes: state.commits.slice(start, end).map(commit => ({ commit })),
          pageInfo: { hasNextPage: end < state.commits.length, endCursor: String(end) },
        },
      } } } };
      state.mutate?.(result, start);
      return result;
    }
    if (args[1].includes('/commits?')) {
      return [state.commits.slice(0, 100), state.commits.slice(100, 200), state.commits.slice(200, 250)]
        .map(page => page.map(commit => ({ sha: commit.oid })));
    }
    state.reads++;
    const pr = structuredClone(state.pr);
    if (state.reads === 2) state.changeFinal?.(pr);
    return pr;
  };
  return { state, api };
}
const options = { owner: 'acme', repo: 'widget', number: 1 };

test('full history beyond the REST ceiling is retained with both parent selections', async () => {
  for (const parentLimit of [100, 2]) {
    const { state, api } = fixture();
    const result = await diagnose({ ...options, parentLimit }, api);
    assert.equal(result.restCount, 250);
    assert.equal(result.graphQLCount, 251);
    assert.equal(result.commits[250].message, 'unsigned');
    const queries = state.calls.filter(c => c.input);
    assert.deepEqual(queries.map(c => c.input.variables.cursor), [null, '100', '200']);
    assert.ok(queries.every(c => c.input.variables.parentLimit === parentLimit));
    assert.ok(queries.every(c => c.input.query.startsWith('query ') && !c.input.query.includes('mutation')));
    assert.ok(state.calls.every(c => c.args[0] === 'api' &&
      (c.args[1] === 'graphql' || c.args[1].startsWith('repos/acme/widget/pulls/1'))));
  }
});

test('partial data, omitted tail, duplicates, changed head/count and nonadvancing pages stay UNKNOWN', async () => {
  const mutations = [
    r => { r.errors = [{ message: 'provider unavailable' }]; },
    r => { r.data.repository.pullRequest.headRefOid = sha(500); },
    r => { r.data.repository.pullRequest.commits.totalCount = 250; },
    r => { r.data.repository.pullRequest.commits.pageInfo.hasNextPage = false; },
    (r, start) => { if (start) r.data.repository.pullRequest.commits.nodes[0].commit = { oid: sha(1) }; },
    r => { r.data.repository.pullRequest.commits.pageInfo.endCursor = '100'; },
    r => { r.data.repository.pullRequest.commits.pageInfo = null; },
  ];
  for (const mutate of mutations) {
    const { state, api } = fixture();
    state.mutate = mutate;
    await assert.rejects(diagnose(options, api));
  }
  for (const changeFinal of [
    pr => { pr.head.sha = sha(500); }, pr => { pr.base.sha = sha(500); }, pr => { pr.commits++; },
  ]) {
    const { state, api } = fixture();
    state.changeFinal = changeFinal;
    await assert.rejects(diagnose(options, api), /changed during diagnosis/);
  }
});

test('transport failures and malformed invocation do not produce an enumeration result', async () => {
  await assert.rejects(diagnose(options, async () => { throw new Error('403 forbidden'); }), /403 forbidden/);
  await assert.rejects(diagnose({ ...options, parentLimit: 1 }), /Usage/);
  await assert.rejects(diagnose({ ...options, owner: '../other' }), /Usage/);
});
