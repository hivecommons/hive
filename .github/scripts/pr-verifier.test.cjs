'use strict';
// #18764: PR Verifier records PR Type / PR Issue with one completed create
// each, and fails the job only when a result cannot be recorded.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {CHECK_NAMES, record} = require('./pr-verifier.cjs');

const headSha = 'a'.repeat(40);
const pr = title => ({number: 7, title, head: {sha: headSha}});

function fakeCore() {
  const core = {failed: [], warnings: [], errors: [], infos: []};
  core.setFailed = message => core.failed.push(message);
  core.warning = message => core.warnings.push(message);
  core.error = message => core.errors.push(message);
  core.info = message => core.infos.push(message);
  return core;
}

// failures: name -> number of create calls for that name that throw.
function fakeGithub({failures = {}, record: mangle} = {}) {
  const calls = [];
  const remaining = {...failures};
  let id = 100;
  const checks = {
    create: async params => {
      calls.push({method: 'create', ...params});
      if (remaining[params.name] > 0) {
        remaining[params.name]--;
        throw Object.assign(new Error('Not Found'), {status: 404});
      }
      const data = {id: ++id, name: params.name, head_sha: params.head_sha,
        status: params.status, conclusion: params.conclusion};
      return {data: mangle ? mangle(data) : data};
    },
  };
  for (const forbidden of ['update', 'get', 'listForRef', 'listForSuite']) {
    checks[forbidden] = async () => { throw new Error(`checks.${forbidden} must not be called`); };
  }
  return {calls, github: {rest: {checks}, request: async () => { throw new Error('raw requests must not be made'); }}};
}

const run = (github, core, title) =>
  record({github, core, owner: 'hivecommons', repo: 'hive', pr: pr(title)});

test('check names are the required contexts', () => {
  assert.deepEqual(CHECK_NAMES, ['PR Type', 'PR Issue']);
});

test('a good title records two completed successes in one create each', async () => {
  const {calls, github} = fakeGithub();
  const core = fakeCore();
  const result = await run(github, core, '🐛 fix: stuck checks');
  assert.deepEqual(result, {recorded: ['PR Type', 'PR Issue'], unrecorded: []});
  assert.equal(calls.length, 2);
  assert.deepEqual(calls.map(c => c.name), ['PR Type', 'PR Issue']);
  for (const call of calls) {
    assert.equal(call.method, 'create');
    assert.equal(call.head_sha, headSha);
    assert.equal(call.status, 'completed');
    assert.equal(call.conclusion, 'success');
  }
  assert.equal(calls[0].output.title, 'PR Type in Title');
  assert.equal(calls[0].output.summary, 'Found 🐛 PR (bugfix)');
  assert.equal(calls[1].output.title, 'Issue/PR tag in PR title');
  assert.equal(calls[1].output.summary, 'Your PR title does not contain any Issue or PR tags');
  assert.equal(calls[1].output.text, undefined);
  assert.deepEqual(core.failed, []);
  assert.deepEqual(core.warnings, []);
});

test('a bad title is a completed failure on the check, advisory for the job', async () => {
  const {calls, github} = fakeGithub();
  const core = fakeCore();
  await run(github, core, '📝 proposal for #12');
  assert.deepEqual(calls.map(c => [c.name, c.status, c.conclusion]),
    [['PR Type', 'completed', 'failure'], ['PR Issue', 'completed', 'failure']]);
  assert.equal(calls[0].output.summary, 'No matching PR type indicator found in title.');
  assert.equal(calls[1].output.summary, 'Your PR has an Issue or PR number in the title.');
  assert.deepEqual(core.failed, []);
  assert.equal(core.warnings.length, 2);
});

test('one transient create error is retried with the real verdict', async () => {
  const {calls, github} = fakeGithub({failures: {'PR Type': 1}});
  const core = fakeCore();
  const result = await run(github, core, '✨ feature');
  assert.deepEqual(result.unrecorded, []);
  assert.deepEqual(calls.map(c => [c.name, c.conclusion]),
    [['PR Type', 'success'], ['PR Type', 'success'], ['PR Issue', 'success']]);
  assert.deepEqual(core.failed, []);
});

test('a result that cannot be recorded fails the job and posts a completed failure', async () => {
  const {calls, github} = fakeGithub({failures: {'PR Issue': 2}});
  const core = fakeCore();
  const result = await run(github, core, '✨ feature');
  assert.deepEqual(result, {recorded: ['PR Type'], unrecorded: ['PR Issue']});
  const fallback = calls.at(-1);
  assert.equal(fallback.name, 'PR Issue');
  assert.equal(fallback.status, 'completed');
  assert.equal(fallback.conclusion, 'failure');
  assert.match(fallback.output.summary, /could not record its "PR Issue" result/);
  assert.match(fallback.output.summary, /404 Not Found/);
  assert.equal(core.failed.length, 1);
  assert.match(core.failed[0], /could not record its result for PR Issue/);
});

test('the job still fails when even the fallback cannot be posted', async () => {
  const {github} = fakeGithub({failures: {'PR Type': 3, 'PR Issue': 3}});
  const core = fakeCore();
  const result = await run(github, core, '✨ feature');
  assert.deepEqual(result.unrecorded, ['PR Type', 'PR Issue']);
  assert.equal(core.failed.length, 1);
  assert.match(core.failed[0], /PR Type and PR Issue/);
  assert.ok(core.errors.some(e => /fallback failure run could not be posted/.test(e)));
});

test('a create GitHub answers with anything but the completed verdict is not a recorded result', async () => {
  const {github} = fakeGithub({record: data => ({...data, status: 'in_progress', conclusion: null})});
  const core = fakeCore();
  const result = await run(github, core, '✨ feature');
  assert.deepEqual(result.unrecorded, ['PR Type', 'PR Issue']);
  assert.equal(core.failed.length, 1);
});

test('an event without a head SHA or title fails instead of passing silently', async () => {
  const {calls, github} = fakeGithub();
  const core = fakeCore();
  await record({github, core, owner: 'o', repo: 'r', pr: {title: '✨ x', head: {}}});
  await record({github, core, owner: 'o', repo: 'r', pr: undefined});
  assert.equal(calls.length, 0);
  assert.equal(core.failed.length, 2);
});

test('the verifier never updates or looks up a check run', () => {
  const source = fs.readFileSync(path.join(__dirname, 'pr-verifier.cjs'), 'utf8');
  for (const forbidden of [/checks\.update/, /listForRef/, /PATCH/, /in_progress/, /status: 'queued'/]) {
    assert.doesNotMatch(source.replace(/^\s*\/\/.*$/gm, ''), forbidden);
  }
});
