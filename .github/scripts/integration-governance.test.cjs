'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const policy = require('./integration-governance.cjs');

const head = 'a'.repeat(40), commit = 'b'.repeat(40);
const subject = () => ({repo: 'hivecommons/hive', base: 'v5',
  members: [{pr: 7, head}], prefixes: [{commit, tree: 'c'.repeat(40)}]});
const member = () => ({number: 7, head: {sha: head, ref: 'feature', repo: {full_name: 'hivecommons/hive'}},
  base: {ref: 'v5', sha: 'd'.repeat(40), repo: {full_name: 'hivecommons/hive'}},
  user: {id: 42, login: 'author'}, draft: false, state: 'open', title: '✨ feature: exact subject',
  body: 'Fixes #6', labels: [{name: 'approved'}]});
const client = pr => ({rest: {pulls: {get: async () => ({data: pr})}}});

test('pinned pure verifier actually validates the immutable upstream rules', () => {
  assert.ok(policy.SOURCE.endsWith('012269a88fa4c034a0acf1ba84c26b195c0dbab4'));
  for (const title of ['✨ feature', '🐛 fix', '📖 docs', '🌱 infra', '⚠️ breaking', '🚀 release',
    ':bug: fix', ':running: legacy', 'WIP: [scope] ✨ feature', '🐛WIP: 📖 docs']) {
    assert.doesNotThrow(() => policy.verifyContents(title), title);
  }
  for (const title of ['', 'ordinary title', '📝 proposal', '✨ fix #123', ':bug: #12a and #13']) {
    assert.throws(() => policy.verifyContents(title), title);
  }
});

test('member snapshots never accept another head/repo/member or an unrelated merge', async () => {
  assert.equal((await policy.snapshot(client(member()), subject())).length, 1);
  const mutations = [
    pr => {pr.number++;}, pr => {pr.head.sha = 'e'.repeat(40);},
    pr => {pr.head.repo.full_name = 'other/fork';}, pr => {pr.base.ref = 'other';},
    pr => {pr.base.repo.full_name = 'other/repo';}, pr => {pr.draft = true;},
    pr => {pr.user.id = 0;}, pr => {pr.user.login = '';}, pr => {pr.state = 'closed';},
    pr => {pr.title = null;}, pr => {pr.labels = null;},
    pr => {pr.state = 'closed'; pr.merged = true; pr.merge_commit_sha = 'f'.repeat(40);},
  ];
  for (const mutate of mutations) {
    const pr = member(); mutate(pr);
    await assert.rejects(policy.snapshot(client(pr), subject()));
  }
  const duplicate = subject();
  duplicate.members.push({...duplicate.members[0]}); duplicate.prefixes.push({...duplicate.prefixes[0]});
  await assert.rejects(policy.snapshot(client(member()), duplicate));
  await assert.rejects(policy.snapshot(client(member()), {...subject(), members: []}));
  await assert.rejects(policy.snapshot(client(member()), {...subject(), prefixes: []}));
});

test('mutable governance is bound, but exact documented prefix promotion is not invalidated', async () => {
  const pr = member(), before = policy.digest([pr]);
  for (const change of [
    value => {value.title += ' changed';}, value => {value.body += ' changed';},
    value => {value.labels.push({name: 'no-changelog'});}, value => {value.head.ref = 'sync/v5';},
    value => {value.user.login = 'someone-else';}, value => {value.user.id++;},
    value => {value.draft = true;},
  ]) {
    const value = structuredClone(pr); change(value);
    assert.notEqual(policy.digest([value]), before);
  }
  pr.state = 'closed'; pr.merged = true; pr.merge_commit_sha = commit;
  pr.base.sha = 'f'.repeat(40); pr.updated_at = '2026-10-31T12:00:00Z';
  assert.equal(policy.digest(await policy.snapshot(client(pr), subject())), before);
});

test('unsupported or untrusted helper scope refuses instead of returning green', async () => {
  await assert.rejects(policy.verify('unknown', subject(), [member()]));
  await assert.rejects(policy.verify('contents', subject(), [member()]), /closure unavailable/);
});
