'use strict';

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const {execFileSync} = require('node:child_process');
const test = require('node:test');
const {canonical, digest, makeSubject, parseRequest, prepare} = require('./integration-materialize.cjs');

const oid = value => value.repeat(40);
const clock = new Date('2026-10-31T01:00:00.000Z');
const PARENT_EPOCH = 1793400000;

function fixture(remote = new Map()) {
  const policyText = JSON.stringify({
    schema: 1, enabled: true, authority: 'forge-single-attempt-v1', actorIDs: ['42'],
    scopes: [{repo: 'owner/repo', base: 'v5'}], maxMembers: 8,
    protectedPaths: ['.github', 'src/scripts'],
    committer: {name: 'integration-bot', email: 'bot@example.invalid'},
  });
  const request = {
    schema: 1, protocol: 'integration-prefix-v1', engine: 'hive',
    repo: 'owner/repo', base: 'v5', baseSHA: oid('1'),
    policy: digest(policyText), generation: 1, createdAt: clock.toISOString(),
    members: [{pr: 10, head: oid('2')}, {pr: 11, head: oid('3')}],
  };
  const context = {
    serverUrl: 'https://github.com', apiUrl: 'https://api.github.com',
    repo: {owner: 'owner', repo: 'repo'}, sha: oid('9'),
    ref: 'refs/heads/v5', eventName: 'workflow_dispatch',
    payload: {repository: {default_branch: 'v5'}},
  };
  const calls = [];
  let commits = 0;
  const git = (args, env = {}) => {
    calls.push({type: 'git', args, env});
    const operation = args.includes('commit-tree') ? 'commit-tree' : args[0];
    switch (operation) {
    case 'ls-tree': return `040000 tree ${oid('a')}\t${args.at(-1)}`;
    case 'rev-parse': return oid('a');
    case '--version': return 'git version 2.48.1';
    case 'show': return String(PARENT_EPOCH + (args.at(-1) === oid('5') ? 1 : 0));
    case 'merge-tree': return commits ? oid('8') : oid('7');
    case 'commit-tree': return ++commits === 1 ? oid('5') : oid('6');
    case 'fetch':
    case 'push':
    case 'check-ref-format': return '';
    default: throw new Error(`unexpected Git operation ${operation}`);
    }
  };
  const github = {rest: {
    git: {
      getRef: async ({ref}) => {
        if (ref === 'heads/v5') return {data: {object: {sha: request.baseSHA}}};
        if (remote.has(ref)) return {data: {object: {sha: remote.get(ref)}}};
        const error = new Error('ref absent'); error.status = 404; throw error;
      },
      createCommit: async input => ({data: {sha: crypto.createHash('sha1').update(input.message).digest('hex')}}),
      createRef: async ({ref, sha}) => {
        const name = ref.replace(/^refs\//, '');
        if (remote.has(name)) { const error = new Error('ref exists'); error.status = 422; throw error; }
        remote.set(name, sha);
        return {status: 201, data: {ref, object: {sha}}};
      },
    },
    pulls: {get: async ({pull_number}) => {
      const member = request.members.find(item => item.pr === pull_number);
      return {data: {state: 'open', draft: false,
        head: {sha: member.head, repo: {full_name: 'owner/repo'}},
        base: {ref: 'v5', repo: {full_name: 'owner/repo'}}}};
    }},
    repos: {compareCommitsWithBasehead: async input => {
      calls.push({type: 'compare', input});
      return {data: {merge_base_commit: {sha: oid('4')}}};
    }},
    actions: {createWorkflowDispatch: async input => {
      calls.push({type: 'dispatch', input});
      return {status: 204};
    }},
  }};
  return {
    policyText, request, context, calls, git, github,
    args() { return {github, context, requestText: JSON.stringify(request),
      policyText: this.policyText, actorID: '42', execution: {runID: '100', attempt: '1'},
      git: this.git, now: clock}; },
  };
}

test('ordered prefixes dispatch as soon as published, without candidate checkout/base writes', async () => {
  const f = fixture();
  const result = await prepare(f.args());
  assert.equal(result.failure, null);
  assert.equal(result.prepared.length, 2);
  const [first, second] = result.prepared;
  assert.deepEqual(first.subject.members, f.request.members.slice(0, 1));
  assert.deepEqual(second.subject.members, f.request.members);
  assert.equal(second.subject.prefixes[0].commit, first.subject.prefixes[0].commit);
  assert.equal(first.publication, 'accepted');
  assert.equal(second.dispatch, 'accepted');
  const calls = f.calls;
  const firstDispatch = calls.findIndex(call => call.type === 'dispatch');
  const secondMerge = calls.findIndex(call => call.type === 'git' &&
    call.args[0] === 'merge-tree' && call.args.includes(oid('3')));
  assert.ok(firstDispatch < secondMerge, 'CI must overlap later materialization');
  const comparisons = calls.filter(call => call.type === 'compare');
  assert.equal(comparisons[0].input.basehead, `${oid('1')}...${oid('2')}`);
  assert.equal(comparisons[1].input.basehead, `${oid('5')}...${oid('3')}`);
  let candidateNumber = 0;
  for (const call of calls.filter(call => call.type === 'git')) {
    assert.ok(!call.args.includes('checkout'));
    assert.ok(!call.args.includes('--force'));
    if (call.args[0] === 'push') {
      assert.match(call.args.at(-1), /^[a-f0-9]{40}:refs\/tags\/hive-integration\/[a-f0-9]{64}$/);
    }
    if (call.args.includes('commit-tree')) {
      const index = call.args.indexOf('commit-tree');
      assert.equal(call.args[index + 2], '-p');
      assert.equal(call.args[index + 4], '-p');
      assert.match(call.args.at(-1), /Signed-off-by: integration-bot <bot@example.invalid>/);
      assert.equal(call.env.GIT_COMMITTER_DATE, new Date((PARENT_EPOCH + ++candidateNumber) * 1000).toISOString());
    }
  }
});

test('unchanged subject identity does not depend on observer/materializer source instance', () => {
  const f = fixture(), prefixes = [{commit: oid('5'), tree: oid('7')}];
  const first = makeSubject(f.request, f.request.members.slice(0, 1), prefixes, oid('a'), {sourceSHA: oid('9')});
  const replay = makeSubject(f.request, f.request.members.slice(0, 1), prefixes, oid('a'), {sourceSHA: oid('b')});
  assert.equal(first.id, replay.id);
  const changed = makeSubject(f.request, [f.request.members[1]], prefixes, oid('a'), {});
  assert.notEqual(first.id, changed.id, 'base+A+B evidence cannot become base+B evidence');
  assert.equal(canonical({z: 1, a: ['x', {b: true}]}), '{"a":["x",{"b":true}],"z":1}');
});

test('independent hives share one exact-subject dispatch owner; no TTL steal or rerun', async () => {
  const remote = new Map(), first = fixture(remote), second = fixture(remote);
  const original = await prepare(first.args());
  assert.equal(original.failure, null);
  const args = second.args();
  args.execution = {runID: '200', attempt: '1'};
  second.request.generation = 2;
  second.request.createdAt = new Date(clock.getTime() + 120000).toISOString();
  args.requestText = JSON.stringify(second.request);
  args.now = new Date(second.request.createdAt);
  const observed = await prepare(args);
  assert.equal(observed.failure, null);
  assert.deepEqual(observed.prepared.map(item => item.subject.id), original.prepared.map(item => item.subject.id));
  assert.ok(observed.prepared.every(item => item.dispatch === 'observer'));
  assert.ok(!second.calls.some(call => call.type === 'dispatch'));
  assert.equal(remote.size, 2);
});

test('default disabled policy, changed actor/scope/source and stale request cannot start Git effects', async () => {
  const cases = [
    args => { args.policyText = args.policyText.replace('"enabled":true', '"enabled":false'); },
    args => { args.actorID = '43'; },
    args => { args.context.ref = 'refs/heads/untrusted'; },
    args => { args.context.eventName = 'pull_request'; },
    args => { args.now = new Date(clock.getTime() + 16 * 60 * 1000); },
  ];
  for (const modify of cases) {
    const f = fixture(), args = f.args();
    modify(args);
    await assert.rejects(prepare(args));
    assert.equal(f.calls.length, 0);
  }
});

test('changed base/member and policy closure never publish or dispatch candidate work', async () => {
  for (const reason of ['base', 'member', 'closure']) {
    const f = fixture();
    if (reason === 'base') f.github.rest.git.getRef = async () => ({data: {object: {sha: oid('b')}}});
    if (reason === 'member') f.github.rest.pulls.get = async () => ({data: {state: 'closed'}});
    if (reason === 'closure') {
      const original = f.git;
      f.git = (args, env) => args[0] === 'ls-tree' && args[1] !== f.context.sha ?
        '120000 blob changed\t.github' : original(args, env);
    }
    let result;
    try { result = await prepare(f.args()); } catch (error) {
      assert.equal(reason, 'base');
      assert.equal(error.reason, 'base_changed');
    }
    if (result) assert.ok(result.failure);
    assert.ok(!f.calls.some(call => call.type === 'dispatch' ||
      (call.type === 'git' && call.args[0] === 'push')));
  }
});

test('a later textual conflict retains exact earlier published/accepted prefix', async () => {
  const f = fixture(), original = f.git;
  f.git = (args, env) => {
    if (args[0] === 'merge-tree' && args.includes(oid('3'))) {
      const error = new Error('conflict');
      error.status = 1;
      throw error;
    }
    return original(args, env);
  };
  const result = await prepare(f.args());
  assert.equal(result.failure.reason, 'materialization_conflict');
  assert.equal(result.prepared.length, 1);
  assert.equal(result.prepared[0].dispatch, 'accepted');
  assert.equal(f.calls.filter(call => call.type === 'dispatch').length, 1);
});

test('ambiguous publication/dispatch is retained for reconciliation, never blindly retried', async () => {
  for (const operation of ['push', 'dispatch']) {
    const f = fixture(), original = f.git;
    let attempts = 0;
    if (operation === 'push') f.git = (args, env) => {
      if (args[0] === 'push') { attempts++; throw new Error('lost acknowledgement'); }
      return original(args, env);
    };
    else f.github.rest.actions.createWorkflowDispatch = async () => {
      attempts++;
      throw new Error('lost acknowledgement');
    };
    const result = await prepare(f.args());
    assert.ok(result.failure);
    assert.equal(result.prepared.length, 1);
    assert.equal(attempts, 1);
    assert.equal(result.prepared[0][operation === 'push' ? 'publication' : 'dispatch'], 'unresolved');
  }
});

test('malformed, extra, duplicate, zero and oversized request identities fail closed', () => {
  const f = fixture();
  for (const raw of [
    'null', '[]', '{}', '{broken', ' '.repeat(33 * 1024),
    JSON.stringify({...f.request, injected: 'not a supported field'}),
    JSON.stringify({...f.request, baseSHA: oid('0')}),
    JSON.stringify({...f.request, members: [f.request.members[0], f.request.members[0]]}),
    JSON.stringify({...f.request, members: [f.request.members[0], {pr: 12, head: f.request.members[0].head}]}),
    JSON.stringify({...f.request, members: [{pr: 1, head: oid('2'), extra: true}]}),
  ]) assert.throws(() => parseRequest(raw));
});

test('create-ref lost acknowledgement resolves only the exact own pre-effect claim', async () => {
  const f = fixture(), createRef = f.github.rest.git.createRef;
  f.github.rest.git.createRef = async input => {
    await createRef(input);
    throw new Error('lost create-ref acknowledgement');
  };
  const result = await prepare(f.args());
  assert.equal(result.failure, null);
  assert.ok(result.prepared.every(item => item.ownership.state === 'claimed'));
  assert.equal(f.calls.filter(call => call.type === 'dispatch').length, 2);
});

test('foreign winners and unreadable/malformed claim acknowledgements never authorize dispatch', async () => {
  for (const reason of ['foreign', 'read_unavailable', 'ack_malformed']) {
    const remote = new Map(), f = fixture(remote);
    if (reason === 'foreign') f.github.rest.git.createRef = async ({ref}) => {
      remote.set(ref.replace(/^refs\//, ''), oid('b'));
      const error = new Error('another writer won'); error.status = 422; throw error;
    };
    if (reason === 'read_unavailable') {
      const getRef = f.github.rest.git.getRef;
      f.github.rest.git.getRef = async input => {
        if (input.ref === 'heads/v5') return getRef(input);
        const error = new Error('unreadable'); error.status = 503; throw error;
      };
    }
    if (reason === 'ack_malformed') f.github.rest.git.createRef = async () => ({status: 201, data: {}});
    const result = await prepare(f.args());
    assert.ok(!f.calls.some(call => call.type === 'dispatch'));
    if (reason === 'foreign') {
      assert.equal(result.failure, null);
      assert.ok(result.prepared.every(item => item.dispatch === 'observer'));
    } else {
      assert.equal(result.failure.reason, 'dispatch_authority_unknown');
      assert.equal(result.prepared.length, 1);
    }
  }
});

test('real Git plumbing produces exact deterministic prefixes and retains an actual earlier prefix on conflict', async () => {
  const directory = path.join(process.cwd(), `.integration-contract-${crypto.randomBytes(16).toString('hex')}`);
  fs.mkdirSync(directory);
  const environment = {
    ...process.env, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_SYSTEM: '/dev/null',
    GIT_AUTHOR_NAME: 'fixture', GIT_COMMITTER_NAME: 'fixture',
    GIT_AUTHOR_EMAIL: 'fixture@example.invalid', GIT_COMMITTER_EMAIL: 'fixture@example.invalid',
    GIT_AUTHOR_DATE: clock.toISOString(), GIT_COMMITTER_DATE: clock.toISOString(),
    GIT_TERMINAL_PROMPT: '0',
  };
  const git = (args, env = {}) => execFileSync('git',
    ['-c', 'core.hooksPath=/dev/null', '--literal-pathspecs', ...args],
    {cwd: directory, env: {...environment, ...env}, encoding: 'utf8',
      timeout: 30000, maxBuffer: 1024 * 1024, stdio: ['ignore', 'pipe', 'pipe']}).trim();
  const write = (file, value) => fs.writeFileSync(path.join(directory, file), `${value}\n`);
  try {
    git(['init', '--quiet']);
    for (const folder of ['.github', 'src/scripts', 'app']) fs.mkdirSync(path.join(directory, folder), {recursive: true});
    write('.github/policy', 'trusted');
    write('src/scripts/guard.sh', 'trusted');
    write('app/a.txt', 'base A');
    write('app/b.txt', 'base B');
    git(['add', '.']); git(['commit', '--quiet', '-m', 'base']);
    const base = git(['rev-parse', 'HEAD']);
    write('app/a.txt', 'author A');
    git(['add', '.']); git(['commit', '--quiet', '-m', 'head A']);
    const headA = git(['rev-parse', 'HEAD']);
    git(['checkout', '--quiet', '--detach', base]);
    write('app/b.txt', 'author B');
    git(['add', '.']); git(['commit', '--quiet', '-m', 'head B']);
    const headB = git(['rev-parse', 'HEAD']);
    git(['checkout', '--quiet', '--detach', base]);
    write('app/a.txt', 'conflicting author C');
    git(['add', '.']); git(['commit', '--quiet', '-m', 'head C']);
    const headC = git(['rev-parse', 'HEAD']);
    git(['checkout', '--quiet', '--detach', base]);
    const f = fixture();
    f.request.baseSHA = base;
    f.request.members = [{pr: 10, head: headA}, {pr: 11, head: headB}];
    f.context.sha = base;
    f.git = (args, env) => {
      f.calls.push({type: 'git', args});
      if (args[0] === 'fetch' || args[0] === 'push') return '';
      return git(args, env);
    };
    f.github.rest.repos.compareCommitsWithBasehead = async ({basehead}) => ({
      data: {merge_base_commit: {sha: git(['merge-base', ...basehead.split('...')])}},
    });
    const result = await prepare(f.args());
    assert.equal(result.failure, null);
    const [first, second] = result.prepared;
    const a = first.subject.prefixes.at(-1), b = second.subject.prefixes.at(-1);
    assert.equal(git(['show', '-s', '--format=%P', a.commit]), `${base} ${headA}`);
    assert.equal(git(['show', '-s', '--format=%P', b.commit]), `${a.commit} ${headB}`);
    assert.equal(git(['rev-parse', `${b.commit}^{tree}`]), b.tree);
    assert.equal(git(['show', `${b.commit}:app/a.txt`]), 'author A');
    assert.equal(git(['show', `${b.commit}:app/b.txt`]), 'author B');
    const replay = await prepare(f.args());
    assert.deepEqual(replay.prepared.map(item => item.subject.id), result.prepared.map(item => item.subject.id));
    assert.ok(replay.prepared.every(item => item.dispatch === 'observer'));
    f.request.members[1].head = headC;
    const conflict = await prepare(f.args());
    assert.equal(conflict.failure.reason, 'materialization_conflict');
    assert.equal(conflict.prepared.length, 1);
    assert.equal(conflict.prepared[0].subject.id, first.subject.id);
  } finally {
    fs.rmSync(directory, {recursive: true});
  }
});
