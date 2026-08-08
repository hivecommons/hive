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

function loadRelay({ backend = 'copilot', model = '', cliStates = ['ready'], procAlive = true, mode = 'interactive', execFileResult = null, statusFile = null, paneText = null } = {}) {
  const commands = [];
  const sent = [];
  // Records every execFile (headless one-shot) invocation: { bin, args, opts }.
  const execFileCalls = [];
  let stateIdx = 0;
  // Guard against a runaway loop in the code under test eating all memory.
  const MAX_RECORDED_COMMANDS = 10000;

  const fakeExecSync = (cmd) => {
    if (commands.length < MAX_RECORDED_COMMANDS) commands.push(cmd);
    if (/backend_binary/.test(cmd)) return `${backend}\n`;
    if (/backend_perm_flag/.test(cmd)) return '--allow-all\n';
    if (/capture-pane/.test(cmd)) {
      // paneText, when given, is returned verbatim — for tests that need a
      // REAL pane rendering (e.g. a codex modal menu) rather than one of the
      // three synthetic states below.
      if (paneText !== null) return paneText;
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

  // Headless mode drives the CLI through execFile (one-shot), not tmux. The stub
  // records the invocation and synchronously fires the completion callback with
  // the outcome the test asked for (default: exit 0). It returns a fake child
  // with a kill() so revoke/shutdown paths can be exercised.
  const fakeExecFile = (bin, args, opts, cb) => {
    const callback = typeof opts === 'function' ? opts : cb;
    execFileCalls.push({ bin, args, opts: typeof opts === 'function' ? {} : opts });
    const child = { killed: false, kill() { this.killed = true; } };
    const r = execFileResult || {};
    if (callback) {
      // Mirror execFile's async contract closely enough for the relay's logic:
      // callback(err, stdout, stderr).
      callback(r.err || null, r.stdout || '', r.stderr || '');
    }
    return child;
  };

  const stubs = {
    child_process: {
      execSync: fakeExecSync,
      execFile: fakeExecFile,
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
  process.env.CONTRIBUTOR_MODE = mode;

  // Keep the relay's task-file write out of the real /tmp path used in prod.
  const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'relay-test-'));
  // Point the headless status file at the test's tmp dir so probe writes don't
  // clobber a real one and can be asserted on.
  const headlessStatusFile = statusFile || path.join(tmpDir, 'headless-status.json');
  process.env.HIVE_HEADLESS_STATUS_FILE = headlessStatusFile;

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
  relay.__execFileCalls = execFileCalls;
  relay.__headlessStatusFile = headlessStatusFile;
  relay.__readHeadlessStatus = () => {
    try { return JSON.parse(fs.readFileSync(headlessStatusFile, 'utf8')); } catch (_) { return null; }
  };
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
// Issue #2596 — the periodic memory-cleanup restart must fire ONCE per
// threshold crossing, then deliver the next task. Before the fix, the #2203
// readiness guard re-entered tmuxSendKeys() at the same, unchanged
// tasksCompletedCount, the "count % RESET_EVERY_N === 0" predicate stayed true,
// and a non-claude CLI restarted forever after task 3, never delivering task 4.
// ---------------------------------------------------------------------------

// Number of completed tasks that triggers the memory-cleanup restart. Mirrors
// RESET_EVERY_N in the relay; the loop only manifests at a multiple of it.
const RESET_EVERY_N = 3;

// Count the queued-and-then-flushed prompt through the full re-entry cycle the
// same way production does: tmuxSendKeys() queues + relaunches, the readiness
// callback flushes, and flushPendingTask() calls tmuxSendKeys() again. A fixed
// relay restarts once and delivers; the buggy relay restarts on every re-entry.
function cycleReadyAndFlush(relay, maxCycles) {
  // Each iteration models "CLI came back ready -> flush the queued prompt".
  // If flushPendingTask() drains the queue and delivers, we're done. If the
  // restart predicate re-fires it re-queues the same prompt, and we loop.
  for (let i = 0; i < maxCycles; i++) {
    if (!relay.getPendingTask()) return i; // delivered, queue drained
    relay.setCliReady(true);
    relay.flushPendingTask();
  }
  return maxCycles; // never drained within the budget -> infinite loop
}

test('non-claude periodic reset fires once at count 3 and then delivers the next task', () => {
  const relay = loadRelay({ backend: 'goose' });
  try {
    relay.setCliReady(true);
    relay.setPendingTask(null);
    relay.setTasksCompletedCount(RESET_EVERY_N); // just finished task 3
    relay.setLastResetAtCount(-1);

    const before = relay.__tmuxSends().length;
    const NEXT_PROMPT = 'Work on the next task foo/bar#4.';

    // First delivery attempt of task 4: the restart predicate is true, so this
    // queues the prompt, relaunches, and returns without typing it.
    relay.tmuxSendKeys(NEXT_PROMPT);
    assert.strictEqual(relay.getPendingTask(), NEXT_PROMPT,
      'the restart should queue the next prompt for the readiness callback');

    // Drive the readiness-callback re-entry. A finite budget: if the reset is
    // not one-shot this never drains and we hit the ceiling.
    const REENTRY_BUDGET = 20;
    const cycles = cycleReadyAndFlush(relay, REENTRY_BUDGET);
    assert.ok(cycles < REENTRY_BUDGET,
      `the periodic reset re-triggered indefinitely (issue #2596): the queued task was never delivered within ${REENTRY_BUDGET} readiness cycles`);

    // Exactly one memory-cleanup restart happened for this crossing: the latch
    // records the serviced count so re-entry cannot restart again.
    assert.strictEqual(relay.getLastResetAtCount(), RESET_EVERY_N,
      'the reset must latch the count it serviced so it does not re-fire');

    // The next task was actually delivered as literal keystrokes.
    const literalSends = relay.__tmuxSends().slice(before).filter(c => / -l /.test(c));
    assert.ok(literalSends.some(c => c.includes('foo/bar#4')),
      `task 4 must be delivered after the single reset; literal sends: ${JSON.stringify(literalSends)}`);
    assert.strictEqual(relay.getPendingTask(), null, 'queue must be drained once the task is delivered');
  } finally { teardown(relay); }
});

test('the periodic reset does not re-fire while the completed-task count is unchanged', () => {
  const relay = loadRelay({ backend: 'goose' });
  try {
    relay.setCliReady(true);
    relay.setPendingTask(null);
    relay.setTasksCompletedCount(RESET_EVERY_N);
    relay.setLastResetAtCount(RESET_EVERY_N); // reset already serviced for count 3

    const before = relay.__commands.filter(c => /send-keys/.test(c) && /\bC-c\b/.test(c)).length;
    relay.tmuxSendKeys('Deliver me directly, no restart.');

    const after = relay.__commands.filter(c => /send-keys/.test(c) && /\bC-c\b/.test(c)).length;
    assert.strictEqual(after, before,
      'no additional CLI restart may happen once the reset is latched for the current count');
    const literalSends = relay.__tmuxSends().filter(c => / -l /.test(c));
    assert.ok(literalSends.some(c => c.includes('Deliver me directly')),
      'the prompt must be delivered directly when the reset is already latched');
  } finally { teardown(relay); }
});

test('a later crossing (count 6) resets again after count 3 was latched', () => {
  const relay = loadRelay({ backend: 'goose' });
  try {
    relay.setCliReady(true);
    relay.setPendingTask(null);
    relay.setTasksCompletedCount(2 * RESET_EVERY_N); // count 6 — a new crossing
    relay.setLastResetAtCount(RESET_EVERY_N);        // last reset was at count 3

    relay.tmuxSendKeys('Task at the second crossing.');
    assert.strictEqual(relay.getLastResetAtCount(), 2 * RESET_EVERY_N,
      'a genuinely new threshold crossing must trigger the periodic reset again');
  } finally { teardown(relay); }
});

test('claude backend never takes the periodic-reset path', () => {
  const relay = loadRelay({ backend: 'claude' });
  try {
    relay.setCliReady(true);
    relay.setPendingTask(null);
    relay.setTasksCompletedCount(RESET_EVERY_N);
    relay.setLastResetAtCount(-1);

    const beforeCc = relay.__commands.filter(c => /send-keys/.test(c) && /\bC-c\b/.test(c)).length;
    relay.tmuxSendKeys('Claude task, no memory-cleanup restart.');
    const afterCc = relay.__commands.filter(c => /send-keys/.test(c) && /\bC-c\b/.test(c)).length;
    assert.strictEqual(afterCc, beforeCc, 'the periodic CLI restart must never apply to the claude backend');
    assert.strictEqual(relay.getLastResetAtCount(), -1,
      'claude must not touch the reset latch');
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
// kubestellar/hive#2538 — headless (non-interactive) delivery mode.
//
// A task must reach the backend CLI through a one-shot invocation (execFile),
// never tmux send-keys; the exit status must drive task_complete / task_failed;
// mode selection must be honoured; an error must be reported, never hung on.
// ---------------------------------------------------------------------------

function assignHeadlessTask(relay, overrides = {}) {
  relay.handleMessage(JSON.stringify(Object.assign({
    type: 'task_assign',
    task_id: 'ct-h-1',
    kind: 'issue',
    repo: 'foo/bar',
    number: 7,
    title: 'headless task',
    prompt: "Work on foo/bar#7. Fork with 'gh repo fork foo/bar' first.",
  }, overrides)));
}

test('interactive is the default mode; headless is opt-in via CONTRIBUTOR_MODE', () => {
  const def = loadRelay({ backend: 'claude' });
  try {
    assert.strictEqual(def.CONTRIBUTOR_MODE, def.MODE_INTERACTIVE,
      'absent CONTRIBUTOR_MODE must select the interactive path');
  } finally { teardown(def); }

  const head = loadRelay({ backend: 'claude', mode: 'headless' });
  try {
    assert.strictEqual(head.CONTRIBUTOR_MODE, head.MODE_HEADLESS,
      'CONTRIBUTOR_MODE=headless must select the headless path');
  } finally { teardown(head); }
});

test('headless dispatch runs a one-shot execFile and never types into tmux', () => {
  const relay = loadRelay({ backend: 'claude', mode: 'headless' });
  try {
    assignHeadlessTask(relay);

    // The prompt went to a one-shot CLI invocation...
    assert.strictEqual(relay.__execFileCalls.length, 1, 'expected exactly one one-shot invocation');
    const call = relay.__execFileCalls[0];
    assert.strictEqual(call.bin, 'claude', 'claude backend should run the claude binary');
    assert.ok(call.args.includes('-p'), `claude headless must use print mode: ${JSON.stringify(call.args)}`);
    assert.ok(call.args[call.args.length - 1].includes('foo/bar#7'),
      'the prompt must be the trailing argv element (passed to execFile, never shell-quoted)');

    // ...and NOT into a tmux pane. No literal send-keys on the headless path.
    const literalSends = relay.__tmuxSends().filter(c => / -l /.test(c));
    assert.deepStrictEqual(literalSends, [], `headless mode must not send-keys into tmux: ${literalSends}`);
  } finally { teardown(relay); }
});

test('a successful headless run reports task_complete then ready, and status=done', () => {
  const relay = loadRelay({ backend: 'claude', mode: 'headless', execFileResult: { stdout: 'opened https://github.com/foo/bar/pull/9\n' } });
  try {
    assignHeadlessTask(relay);
    const complete = relay.__sent.find(m => m.type === 'task_complete');
    assert.ok(complete, 'exit 0 must report task_complete');
    assert.strictEqual(complete.task_id, 'ct-h-1');
    assert.strictEqual(complete.pr_url, 'https://github.com/foo/bar/pull/9',
      'a PR URL in the captured output should be reported');
    assert.ok(relay.__sent.some(m => m.type === 'ready'), 'a completed headless task must free the relay for more work');
    assert.strictEqual(relay.getCurrentTask(), null, 'currentTask must clear on completion');

    const status = relay.__readHeadlessStatus();
    assert.ok(status, 'a headless status file must be written for probes');
    assert.strictEqual(status.state, relay.HEADLESS_STATE_WAITING,
      'after completion the runner returns to the waiting state');
  } finally { teardown(relay); }
});

test('a failing headless run reports task_failed rather than hanging', () => {
  const err = new Error('boom'); err.code = 2;
  const relay = loadRelay({ backend: 'copilot', mode: 'headless', execFileResult: { err, stderr: 'fatal: something\n' } });
  try {
    assignHeadlessTask(relay);
    const failure = relay.__sent.find(m => m.type === 'task_failed');
    assert.ok(failure, 'a non-zero exit must report task_failed (never a silent stall)');
    assert.match(failure.reason, /exited with error|code 2/i);
    assert.ok(relay.__sent.some(m => m.type === 'ready'), 'the relay must stay available after a failed headless task');
    assert.strictEqual(relay.getCurrentTask(), null, 'currentTask must clear on failure');

    const status = relay.__readHeadlessStatus();
    assert.strictEqual(status.state, relay.HEADLESS_STATE_FAILED, 'status must record the failure for a probe');
  } finally { teardown(relay); }
});

test('a headless timeout kill is reported as a failure, not a completion', () => {
  const err = new Error('timeout'); err.killed = true; err.signal = 'SIGKILL';
  const relay = loadRelay({ backend: 'claude', mode: 'headless', execFileResult: { err } });
  try {
    assignHeadlessTask(relay);
    const failure = relay.__sent.find(m => m.type === 'task_failed');
    assert.ok(failure, 'a timed-out headless child must report task_failed');
    assert.match(failure.reason, /exceeded.*min|killed/i);
    assert.ok(!relay.__sent.some(m => m.type === 'task_complete'), 'a killed task must never look completed');
  } finally { teardown(relay); }
});

test('headless refuses an unsupported backend loudly instead of stalling', () => {
  const relay = loadRelay({ backend: 'goose', mode: 'headless' });
  try {
    assert.strictEqual(relay.headlessSupportsBackend(), false, 'goose has no one-shot mode');
    assignHeadlessTask(relay);
    // No CLI was ever spawned...
    assert.strictEqual(relay.__execFileCalls.length, 0, 'an unsupported backend must not spawn a CLI');
    // ...and the task was failed permanently rather than left hanging.
    const failure = relay.__sent.find(m => m.type === 'task_failed');
    assert.ok(failure, 'an unsupported backend must fail the task, not silently accept it');
    assert.strictEqual(failure.permanent, true, 'no other contributor with this backend can run it either');
    assert.match(failure.reason, /no headless|non-interactive/i);
  } finally { teardown(relay); }
});

test('buildHeadlessArgv maps each supported backend to its one-shot invocation', () => {
  const claude = loadRelay({ backend: 'claude', mode: 'headless' });
  try {
    const a = claude.buildHeadlessArgv('do the thing');
    assert.strictEqual(a.bin, 'claude');
    assert.ok(a.args.includes('-p') && a.args[a.args.length - 1] === 'do the thing');
  } finally { teardown(claude); }

  const codex = loadRelay({ backend: 'codex', mode: 'headless' });
  try {
    const a = codex.buildHeadlessArgv('do the thing');
    assert.strictEqual(a.bin, 'codex');
    assert.ok(a.args.includes('exec') && a.args[a.args.length - 1] === 'do the thing',
      `codex must use 'exec' one-shot: ${JSON.stringify(a.args)}`);
  } finally { teardown(codex); }

  const goose = loadRelay({ backend: 'goose', mode: 'headless' });
  try {
    assert.strictEqual(goose.buildHeadlessArgv('x'), null, 'unsupported backend has no argv');
  } finally { teardown(goose); }
});

test('interactive mode still delivers via tmux send-keys (unchanged default path)', () => {
  const relay = loadRelay({ backend: 'copilot' }); // default interactive
  try {
    relay.setCliReady(true);
    assignHeadlessTask(relay); // reuse the task shape; mode is interactive here
    // Interactive path uses tmux, not execFile.
    assert.strictEqual(relay.__execFileCalls.length, 0, 'interactive mode must not use the one-shot runner');
    const literalSends = relay.__tmuxSends().filter(c => / -l /.test(c));
    assert.ok(literalSends.some(c => c.includes('foo/bar#7')),
      'interactive mode must still type the prompt into tmux');
  } finally { teardown(relay); }
});

// ---------------------------------------------------------------------------
// Unresponsive-backend recovery: blocking modal prompts must be classified and
// dismissed with the RIGHT key, and a CLI that never reaches its prompt must
// hand its task back instead of silently holding it.
// ---------------------------------------------------------------------------

// Real captures from a running ghcr.io/kubestellar/hive-contributor container.
// Note both keep codex's banner chrome ("OpenAI Codex (v0.146.0)", the "›"
// input marker) on screen BEHIND the modal — which is exactly why the ready
// patterns have to be checked after the modal patterns, not before.
const CODEX_TRUST_PANE = [
  'dev@codex-contributor:~/workspace$ codex --dangerously-bypass-approvals-and-sandbox',
  '> You are in /home/dev/workspace',
  '',
  '  Do you trust the contents of this directory? Working with untrusted contents comes with higher risk of prompt injection.',
  '',
  '› 1. Yes, continue',
  '  2. No, quit',
  '',
  '  Press enter to continue',
  '╭─────────────────────────────────────────────────────╮',
  '│ >_ OpenAI Codex (v0.146.0)                          │',
  '╰─────────────────────────────────────────────────────╯',
].join('\n');

const CODEX_UPDATE_PANE = [
  'dev@codex-contributor:~/workspace$ codex --dangerously-bypass-approvals-and-sandbox',
  '  ✨ Update available! 0.146.0 -> 0.147.0',
  '  Release notes: https://github.com/openai/codex/releases/latest',
  '› 1. Update now (runs `npm install -g @openai/codex`)',
  '  2. Skip',
  '  3. Skip until next version',
  '  Press enter to continue',
].join('\n');

test('codex trust prompt is classified as onboarding, not ready, despite the banner behind it', () => {
  const relay = loadRelay({ backend: 'codex', paneText: CODEX_TRUST_PANE });
  try {
    assert.strictEqual(relay.getCLIState(), 'onboarding',
      'a pane blocked on the trust menu must not be reported ready — a task prompt typed there is swallowed by the menu');
  } finally { teardown(relay); }
});

test('codex update nudge is classified as onboarding, not ready', () => {
  const relay = loadRelay({ backend: 'codex', paneText: CODEX_UPDATE_PANE });
  try {
    assert.strictEqual(relay.getCLIState(), 'onboarding');
  } finally { teardown(relay); }
});

test('numbered menus get an explicit selection, not a bare Enter', () => {
  const relay = loadRelay({ backend: 'codex', paneText: CODEX_TRUST_PANE });
  try {
    // "1. Yes, continue" — Enter alone just re-renders the menu forever.
    assert.strictEqual(relay.blockingPromptKey(CODEX_TRUST_PANE), '1');
    // "3. Skip until next version" — deliberately NOT "1. Update now", which
    // would run npm install -g inside the container.
    assert.strictEqual(relay.blockingPromptKey(CODEX_UPDATE_PANE), '3');
    // A plain yes/no confirm still takes a bare Enter.
    assert.strictEqual(relay.blockingPromptKey('Do you trust this folder? (y/n)'), null);
  } finally { teardown(relay); }
});

test('a CLI that never becomes ready hands its task BACK to the hub instead of silently holding it', () => {
  const relay = loadRelay({ backend: 'copilot', cliStates: ['starting'] });
  try {
    relay.setCliReady(false);
    relay.setCurrentTask({ task_id: 'ct-stuck-1', kind: 'issue', repo: 'foo/bar', number: 5, title: 'stuck' });
    relay.setPendingTask('a queued prompt');
    relay.__sent.length = 0;

    // What armCLIReadyWait's catch path does on CLI_READY_TIMEOUT_MS.
    relay.failCurrentTask('CLI never became ready: CLI did not become ready within timeout', { skipReady: true });

    const failures = relay.__sent.filter(m => m.type === 'task_failed');
    assert.strictEqual(failures.length, 1, 'the hub must be told, or the slot is held until the hub times out');
    assert.strictEqual(failures[0].task_id, 'ct-stuck-1');
    assert.match(failures[0].reason, /never became ready/);
    assert.strictEqual(relay.getCurrentTask(), null, 'task must be released locally too');
  } finally { teardown(relay); }
});

test('handing back a task from a wedged CLI does NOT re-advertise ready', () => {
  const relay = loadRelay({ backend: 'copilot', cliStates: ['starting'] });
  try {
    relay.setCurrentTask({ task_id: 'ct-stuck-2', kind: 'issue', repo: 'foo/bar', number: 6, title: 'stuck' });
    relay.__sent.length = 0;
    relay.failCurrentTask('CLI never became ready', { skipReady: true });
    assert.strictEqual(relay.__sent.filter(m => m.type === 'ready').length, 0,
      'claiming to be free while the CLI is still wedged would pull in another unrunnable task every timeout window');
  } finally { teardown(relay); }
});

test('reconnect while wedged also withholds ready until CLI recovery', () => {
  const relay = loadRelay({ backend: 'copilot', cliStates: ['starting'] });
  try {
    relay.setCliReady(false);
    relay.setCliReadyFailed(true);
    relay.__sent.length = 0;
    relay.handleMessage(JSON.stringify({ type: 'auth_ok', contributor_id: 'c1', trust_tier: 'trusted' }));
    assert.strictEqual(relay.__sent.filter(m => m.type === 'ready').length, 0,
      'auth_ok must not bypass skipReady after a readiness failure; recovery re-advertises ready');
  } finally { teardown(relay); }
});

test('an ordinary task failure still re-advertises ready (skipReady is opt-in)', () => {
  const relay = loadRelay({ backend: 'copilot' });
  try {
    relay.setCurrentTask({ task_id: 'ct-normal', kind: 'issue', repo: 'foo/bar', number: 7, title: 'x' });
    relay.__sent.length = 0;
    relay.failCurrentTask('some ordinary failure');
    assert.strictEqual(relay.__sent.filter(m => m.type === 'ready').length, 1,
      'the pre-existing failure path must be unchanged');
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
