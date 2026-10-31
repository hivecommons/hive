'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const cp = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
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

test('real pinned helpers enforce each original member and reject missing signoff or fragment', () => {
  const root = process.cwd(), workspace = path.join(root, '.integration-governance-fixture');
  const helper = path.join(root, '.github/scripts/integration-governance.cjs');
  const env = {...process.env, GITHUB_TOKEN: '', GH_TOKEN: '', GITHUB_REPOSITORY: '', DCO_GITHUB_REPOSITORY: ''};
  const git = (...args) => cp.execFileSync('git', ['-c', 'core.hooksPath=/dev/null', ...args],
    {cwd: workspace, env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe']}).trim();
  const check = (kind, manifest, members, knownAuthor = true) => cp.execFileSync(process.execPath, ['-e',
    `const subject = ${JSON.stringify(manifest)};
     const github = {graphql: async (_, variables) => ({repository: {nameWithOwner: subject.repo,
       ...Object.fromEntries(Object.entries(variables).filter(([key]) => key.startsWith('oid')).map(([key, oid]) =>
         [key.replace('oid', 'c'), {oid, author: {user: ${knownAuthor ? "{login:'fixture'}" : "null"}}}]))}})};
     require(${JSON.stringify(helper)}).verify(${JSON.stringify(kind)}, subject, ${JSON.stringify(members)}, github).catch(error => {console.error(error); process.exitCode = 1;})`],
  {cwd: workspace, env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe']});
  assert.equal(fs.existsSync(workspace), false, 'fixture must not overwrite another workspace');
  fs.mkdirSync(workspace);
  try {
    fs.mkdirSync(path.join(workspace, '.github'));
    const scripts = cp.execFileSync('git', ['ls-files', '-z', 'src/scripts'], {cwd: root, encoding: 'utf8'});
    for (const file of scripts.split('\0').filter(Boolean)) {
      const source = path.join(root, file), target = path.join(workspace, file);
      fs.mkdirSync(path.dirname(target), {recursive: true});
      fs.copyFileSync(source, target);
      fs.chmodSync(target, fs.statSync(source).mode);
    }
    fs.writeFileSync(path.join(workspace, '.github/trusted'), 'trusted closure\n');
    git('init', '--initial-branch=base');
    git('config', 'user.name', 'Fixture Author');
    git('config', 'user.email', 'fixture@example.invalid');
    git('config', 'commit.gpgsign', 'false');
    git('add', '.'); git('commit', '-s', '-m', 'Initial trusted helpers');
    const base = git('rev-parse', 'HEAD'), workflowTree = git('rev-parse', 'HEAD:.github');
    git('checkout', '-b', 'feature');
    fs.writeFileSync(path.join(workspace, 'src/code.go'), 'package fixture\n');
    git('add', '.'); git('commit', '-s', '-m', 'Feature initially missing fragment');
    const missingFragment = git('rev-parse', 'HEAD');
    fs.mkdirSync(path.join(workspace, 'changelog.d'));
    fs.writeFileSync(path.join(workspace, 'changelog.d/added-fixture.md'), '- Actual fixture feature.\n');
    git('add', '.'); git('commit', '-s', '-m', 'Add own fragment');
    const memberHead = git('rev-parse', 'HEAD');
    git('checkout', 'base');
    git('merge', '--no-ff', 'feature', '-m', 'Candidate\n\nSigned-off-by: Fixture Author <fixture@example.invalid>');
    const manifest = {repo: 'fixture/repo', base: 'base', baseSHA: base, workflowTree,
      preparedBy: {sourceSHA: base}, members: [{pr: 7, head: memberHead}],
      prefixes: [{commit: git('rev-parse', 'HEAD')}]};
    const pr = {number: 7, head: {sha: memberHead, ref: 'feature'}, user: {login: 'fixture'}, labels: []};
    for (const kind of ['dco', 'attribution', 'changelog']) assert.doesNotThrow(() => check(kind, manifest, [pr]), kind);
    assert.throws(() => check('attribution', manifest, [pr], false), /Command failed/);
    const unsignedPrefix = structuredClone(manifest);
    unsignedPrefix.prefixes[0].commit = git('commit-tree', 'HEAD^{tree}', '-p', base, '-p', memberHead, '-m', 'Unsigned generated merge');
    assert.throws(() => check('dco', unsignedPrefix, [pr]));
    // The combined tree's fragment cannot satisfy another original member.
    assert.throws(() => check('changelog', manifest, [{...pr, head: {...pr.head, sha: missingFragment}}]));
    git('checkout', '-b', 'unsigned');
    fs.writeFileSync(path.join(workspace, 'src/unsigned.go'), 'package fixture\n');
    git('add', '.'); git('commit', '-m', 'Unsigned introduced commit');
    const bad = structuredClone(manifest);
    bad.prefixes[0].commit = git('rev-parse', 'HEAD');
    assert.throws(() => check('dco', bad, [pr]));
    // Changing the helper closure is not permission to execute a new checker.
    fs.appendFileSync(path.join(workspace, 'src/scripts/check-dco-trailers.sh'), '\n# different helper\n');
    git('add', '.'); git('commit', '-s', '-m', 'Different helper closure');
    assert.throws(() => check('dco', bad, [pr]));
    const forgedSource = structuredClone(bad);
    forgedSource.preparedBy.sourceSHA = git('rev-parse', 'HEAD');
    assert.throws(() => check('dco', forgedSource, [pr]));
  } finally { fs.rmSync(workspace, {recursive: true, force: true}); }
});
