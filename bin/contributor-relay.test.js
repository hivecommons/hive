// Tests for bin/contributor-relay.sh (JavaScript despite the .sh extension).
//
// Regression coverage for kubestellar/hive#2203 — "Contributor agent stuck in
// infinite crash loop after periodic CLI restart". Reported with a full source
// root-cause analysis by @castrojo.
//
// Run: node bin/contributor-relay.test.js

'use strict';

const assert = require('assert');
const Module = require('module');
const path = require('path');
const fs = require('fs');
const os = require('os');

// Set for the whole run, not just during module load: the relay checks it at
// CALL time in sleepMs() to skip its busy-wait, and the restart paths sleep for
// seconds at a time.
process.env.HIVE_RELAY_TEST_MODE = '1';

const RELAY_PATH = path.join(__dirname, 'contributor-relay.sh');

// ---------------------------------------------------------------------------
// Harness: load the relay with child_process/ws stubbed out, so no tmux, no
// bash and no WebSocket are ever touched.
// ---------------------------------------------------------------------------

function loadRelay({ backend = 'copilot', model = '', cliStates = ['ready'], procAlive = true } = {}) {
  const commands = [];
  const sent = [];
  let stateIdx = 0;
  // Guard against a runaway loop in the code under test eating all memory.
  const MAX_RECORDED_COMMANDS = 10000;

  const fakeExecSync = (cmd) => {
    if (commands.length < MAX_RECORDED_COMMANDS) commands.push(cmd);
    if (/backend_binary/.test(cmd)) return `${backend}\n`;
    if (/backend_perm_flag/.test(cmd)) return '--allow-all\n';
    if (/capture-pane/.test(cmd)) {
      const state = cliStates[Math.min(stateIdx++, cliStates.length - 1)];
      // Panes that getCLIState()/checkTmuxIdle() classify per backend.
      if (state === 'ready') return '/ commands for help\n';
      if (state === 'working') return '/ commands for help\nesc cancel\n';
      return 'dev@host:~$ \n';
    }
    if (/cmdline|ps -eo/.test(cmd)) {
      // The relay's liveness probe greps this for the backend name. When the
      // CLI is "dead" the pane is a bare shell — and crucially the string must
      // not contain any known backend name.
      return procAlive ? `${backend} --allow-all\n` : '/usr/bin/sh\n';
    }
    return '';
  };

  const stubs = {
    child_process: {
      execSync: fakeExecSync,
      execFile: () => {},
    },
    ws: class FakeWebSocket {
      static get OPEN() { return 1; }
      constructor() { this.readyState = 1; }
      on() {}
      send(payload) { sent.push(JSON.parse(payload)); }
      close() {}
      ping() {}
    },
  };

  const origResolve = Module._resolveFilename;
  const origLoad = Module._load;
  Module._load = function (request, parent, isMain) {
    if (Object.prototype.hasOwnProperty.call(stubs, request)) return stubs[request];
    return origLoad.apply(this, arguments);
  };

  const prevEnv = { ...process.env };
  process.env.HIVE_REGISTRATION_TOKEN = 'test-token';
  process.env.AGENT_BACKEND = backend;
  process.env.AGENT_MODEL = model;
  process.env.GOOSE_MODEL = '';
  process.env.HIVE_AGENT_SESSION = 'contributor';

  // Keep the relay's task-file write out of the real /tmp path used in prod.
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'relay-test-'));

  // node refuses to require a .sh file with the default extension handlers;
  // register .sh as JavaScript. This must happen BEFORE require.resolve(), and
  // the cache must be cleared on every load so each test gets a fresh module
  // wired to its own execSync stub.
  Module._extensions['.sh'] = Module._extensions['.js'];
  for (const key of Object.keys(require.cache)) {
    if (key.includes('contributor-relay.sh')) delete require.cache[key];
  }
  let relay;
  try {
    relay = require(RELAY_PATH);
  } finally {
    Module._load = origLoad;
    Module._resolveFilename = origResolve;
    process.env = prevEnv;
    process.env.HIVE_RELAY_TEST_MODE = '1';
  }

  const ws = new stubs.ws();
  relay.setWs(ws);
  relay.__commands = commands;
  relay.__sent = sent;
  relay.__tmpDir = tmpDir;
  relay.__tmuxSends = () => commands.filter(c => /send-keys/.test(c));
  return relay;
}

function teardown(relay) {
  try { relay.cleanup(); } catch (_) {}
}

const tests = [];
function test(name, fn) { tests.push([name, fn]); }

// ---------------------------------------------------------------------------
// Bug 1 — the relaunch command must keep the model flag, and must never pass
// --model to bob.
// ---------------------------------------------------------------------------

test('relaunch command includes --model for a model-taking backend', () => {
  const relay = loadRelay({ backend: 'copilot', model: 'gpt-5.6-luna' });
  try {
    const cmd = relay.buildLaunchCommand();
    assert.match(cmd, /--model gpt-5\.6-luna/, `expected model flag in launch command, got: ${cmd}`);
    assert.match(cmd, /copilot/);
    assert.match(cmd, /--allow-all/);
  } finally { teardown(relay); }
});

test('relaunchCLI() sends the model flag to tmux, not just the bare binary', () => {
  const relay = loadRelay({ backend: 'copilot', model: 'gpt-5.6-luna' });
  try {
    relay.relaunchCLI();
    const launch = relay.__tmuxSends().find(c => /copilot/.test(c));
    assert.ok(launch, 'no launch command was sent to tmux');
    assert.match(launch, /--model gpt-5\.6-luna/,
      `restart path dropped the model flag (issue #2203 bug 1): ${launch}`);
  } finally { teardown(relay); }
});

test('bob never receives --model even when AGENT_MODEL is set', () => {
  const relay = loadRelay({ backend: 'bob', model: 'some-model' });
  try {
    const cmd = relay.buildLaunchCommand();
    assert.ok(!/--model/.test(cmd),
      `--model is fatal for bob ("Cannot read properties of undefined (reading 'maxTokens')"), got: ${cmd}`);
  } finally { teardown(relay); }
});

test('amazonq and goose are also excluded from --model', () => {
  for (const backend of ['amazonq', 'goose']) {
    const relay = loadRelay({ backend, model: 'some-model' });
    try {
      assert.ok(!/--model/.test(relay.buildLaunchCommand()), `${backend} should not get --model`);
    } finally { teardown(relay); }
  }
});

// ---------------------------------------------------------------------------
// Bug 2 — a task prompt must never be typed into a pane that is not confirmed
// ready, or the literal keystrokes land on bash and wedge it in PS2.
// ---------------------------------------------------------------------------

const PROMPT_WITH_APOSTROPHES =
  "Work on issue foo/bar#421. Fork it with 'gh repo fork foo/bar --clone=false' first.";

test('task prompt is queued, not typed, while cliReady is false', () => {
  const relay = loadRelay({ backend: 'copilot' });
  try {
    relay.setCliReady(false);
    relay.setPendingTask(null);
    relay.tmuxSendKeys(PROMPT_WITH_APOSTROPHES);

    assert.strictEqual(relay.getPendingTask(), PROMPT_WITH_APOSTROPHES,
      'prompt should have been queued while the CLI was not ready');
    const literalSends = relay.__tmuxSends().filter(c => / -l /.test(c));
    assert.deepStrictEqual(literalSends, [],
      `no literal keystrokes may be sent to an unready pane (issue #2203 bug 2): ${literalSends}`);
  } finally { teardown(relay); }
});

test('queued prompt flushes once the CLI is confirmed ready', () => {
  const relay = loadRelay({ backend: 'copilot' });
  try {
    relay.setCliReady(false);
    relay.setPendingTask(null);
    relay.tmuxSendKeys(PROMPT_WITH_APOSTROPHES);
    const queued = relay.getPendingTask();
    assert.strictEqual(queued, PROMPT_WITH_APOSTROPHES, 'precondition: prompt is queued');

    const before = relay.__tmuxSends().length;
    relay.setCliReady(true);
    relay.setPendingTask(queued);
    relay.flushPendingTask();

    const literalSends = relay.__tmuxSends().slice(before).filter(c => / -l /.test(c));
    assert.ok(literalSends.some(c => c.includes('gh repo fork')),
      `prompt should be delivered once the CLI is ready; literal sends: ${JSON.stringify(literalSends)}`);
    assert.strictEqual(relay.getPendingTask(), null, 'queue should be drained after delivery');
  } finally { teardown(relay); }
});

test('task_assign queues rather than typing when the CLI is not ready', () => {
  const relay = loadRelay({ backend: 'copilot' });
  try {
    relay.setCliReady(false);
    relay.setPendingTask(null);
    relay.handleMessage(JSON.stringify({
      type: 'task_assign',
      task_id: 'ct-1',
      kind: 'issue',
      repo: 'foo/bar',
      number: 421,
      title: 'something',
      prompt: PROMPT_WITH_APOSTROPHES,
    }));

    assert.strictEqual(relay.getPendingTask(), PROMPT_WITH_APOSTROPHES);
    const literalSends = relay.__tmuxSends().filter(c => / -l /.test(c));
    assert.deepStrictEqual(literalSends, [], 'task_assign must not type into an unready pane');
  } finally { teardown(relay); }
});

test('relaunchCLI() leaves cliReady false until readiness is confirmed', () => {
  const relay = loadRelay({ backend: 'copilot', cliStates: ['starting'] });
  try {
    relay.setCliReady(true);
    relay.relaunchCLI();
    assert.strictEqual(relay.getCliReady(), false,
      'a restart must clear cliReady; it may only be set by the readiness classifier');
  } finally { teardown(relay); }
});

test('relaunch clears a wedged bash PS2 line before sending the command', () => {
  const relay = loadRelay({ backend: 'copilot' });
  try {
    relay.relaunchCLI();
    const sends = relay.__tmuxSends();
    const cIdx = sends.findIndex(c => /\bC-u\b/.test(c));
    const launchIdx = sends.findIndex(c => /copilot/.test(c));
    assert.ok(cIdx >= 0, 'expected a C-u to clear a possibly-wedged shell line');
    assert.ok(cIdx < launchIdx, 'the wedge recovery must run before the launch command');
  } finally { teardown(relay); }
});

// ---------------------------------------------------------------------------
// Bug 3 — bounded retries, permanent give-up, and the relay staying available.
// ---------------------------------------------------------------------------

function assignTask(relay, taskId, number = 421) {
  relay.handleMessage(JSON.stringify({
    type: 'task_assign',
    task_id: taskId,
    kind: 'issue',
    repo: 'foo/bar',
    number,
    title: 'crashy task',
    prompt: 'do the thing',
  }));
}

// Drive the crash path directly: assign, then let the progress tick observe a
// dead CLI. startProgressReporting() uses a long interval, so instead of
// waiting we re-enter via repeated assign/crash cycles using fake timers.
// Independent of the configured value: the cap must be a small finite number.
// Without this a regression that sets it to Infinity/MAX_SAFE_INTEGER would
// still satisfy every cap-relative assertion below while restoring the exact
// infinite loop #2203 reports.
const CAP_SANITY_LIMIT = 10;

test('the CLI-restart cap is a small finite number', () => {
  const relay = loadRelay({ backend: 'copilot' });
  try {
    const cap = relay.MAX_TASK_CLI_RESTARTS;
    assert.ok(Number.isInteger(cap) && cap >= 1 && cap <= CAP_SANITY_LIMIT,
      `MAX_TASK_CLI_RESTARTS must be a small finite integer to bound the retry loop, got: ${cap}`);
  } finally { teardown(relay); }
});

test('crash restarts are capped and end in a permanent failure', () => {
  const relay = loadRelay({ backend: 'copilot', procAlive: false });
  try {
    relay.setCliReady(true);
    const cap = relay.MAX_TASK_CLI_RESTARTS;
    assert.ok(cap <= CAP_SANITY_LIMIT, 'cap must be small enough to drive here');

    // Simulate the hub reassigning the same work item after each failure.
    for (let i = 0; i <= cap; i++) {
      assignTask(relay, `ct-421-${i}`);
      relay.__crashTick();
    }

    const failures = relay.__sent.filter(m => m.type === 'task_failed');
    assert.ok(failures.length >= cap + 1, `expected at least ${cap + 1} failures, got ${failures.length}`);

    const permanent = failures.filter(m => m.permanent === true);
    assert.ok(permanent.length >= 1,
      'after the retry cap the relay must report a PERMANENT failure so the hub reassigns elsewhere');
    assert.match(permanent[0].reason, /giving up/i);

    // And the non-permanent ones stop: no unbounded retrying.
    const transient = failures.filter(m => !m.permanent);
    assert.ok(transient.length <= cap,
      `retries must be bounded by MAX_TASK_CLI_RESTARTS=${cap}, saw ${transient.length}`);
  } finally { teardown(relay); }
});

test('a reassignment of a given-up task is rejected, not retried', () => {
  const relay = loadRelay({ backend: 'copilot', procAlive: false });
  try {
    relay.setCliReady(true);
    assert.ok(relay.MAX_TASK_CLI_RESTARTS <= CAP_SANITY_LIMIT, 'cap must be small enough to drive here');
    for (let i = 0; i <= relay.MAX_TASK_CLI_RESTARTS; i++) {
      assignTask(relay, `ct-421-${i}`);
      relay.__crashTick();
    }
    const before = relay.__sent.length;
    assignTask(relay, 'ct-421-again');

    assert.strictEqual(relay.getCurrentTask(), null,
      'a given-up work item must not be accepted again');
    const after = relay.__sent.slice(before);
    assert.ok(after.some(m => m.type === 'task_failed' && m.permanent),
      'reassignment of a given-up task should be refused with a permanent failure');
    assert.ok(after.some(m => m.type === 'ready'),
      'the relay must announce it is still ready for other work');
  } finally { teardown(relay); }
});

test('after give-up a DIFFERENT task is still accepted', () => {
  const relay = loadRelay({ backend: 'copilot', procAlive: false });
  try {
    relay.setCliReady(true);
    assert.ok(relay.MAX_TASK_CLI_RESTARTS <= CAP_SANITY_LIMIT, 'cap must be small enough to drive here');
    for (let i = 0; i <= relay.MAX_TASK_CLI_RESTARTS; i++) {
      assignTask(relay, `ct-421-${i}`);
      relay.__crashTick();
    }
    assert.strictEqual(relay.getCurrentTask(), null, 'precondition: no active task after give-up');

    relay.setCliReady(true);
    assignTask(relay, 'ct-999-0', 999);
    const task = relay.getCurrentTask();
    assert.ok(task, 'a poisoned task must not wedge the whole contributor');
    assert.strictEqual(task.number, 999);
  } finally { teardown(relay); }
});

test('restart backoff grows and is capped', () => {
  const relay = loadRelay({ backend: 'copilot' });
  try {
    const b1 = relay.restartBackoffMs(1);
    const b2 = relay.restartBackoffMs(2);
    const b3 = relay.restartBackoffMs(3);
    assert.ok(b2 > b1 && b3 > b2, `backoff must grow: ${b1}, ${b2}, ${b3}`);
    assert.strictEqual(relay.restartBackoffMs(50), relay.restartBackoffMs(60),
      'backoff must saturate at the cap');
  } finally { teardown(relay); }
});

// ---------------------------------------------------------------------------

let failed = 0;
// RELAY_TEST_ONLY=<substring> runs a single test, for debugging in isolation.
const only = process.env.RELAY_TEST_ONLY;
for (const [name, fn] of only ? tests.filter(([n]) => n.includes(only)) : tests) {
  try {
    fn();
    console.log(`ok   ${name}`);
  } catch (e) {
    failed++;
    console.error(`FAIL ${name}`);
    console.error(`     ${e.message}`);
  }
}
console.log(`\n${tests.length - failed}/${tests.length} passed`);
// waitForCLI() schedules polling timers that would otherwise keep the event
// loop alive well past the last assertion; exit explicitly.
process.exit(failed ? 1 : 0);
