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

// Set for the whole run, not just during module load: the relay checks it at
// CALL time in sleepMs() to skip its busy-wait, and the restart paths sleep for
// seconds at a time.
process.env.HIVE_RELAY_TEST_MODE = '1';

const RELAY_PATH = path.join(__dirname, 'contributor-relay.sh');

// ---------------------------------------------------------------------------
// Harness: load the relay with child_process/ws stubbed out, so no tmux, no
// bash and no WebSocket are ever touched.
// ---------------------------------------------------------------------------

function loadRelay({ backend = 'copilot', model = '', cliStates = ['ready'], procAlive = true, mode = 'interactive', execFileResult = null, statusFile = null, env = {} } = {}) {
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
      const state = cliStates[Math.min(stateIdx++, cliStates.length - 1)];
      // Panes that getCLIState()/checkTmuxIdle() classify per backend.
      if (state === 'ready') return '/ commands for help\n';
      if (state === 'working') return '/ commands for help\nesc cancel\n';
      if (typeof state === 'string' && state.includes('\n')) return state;
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
  Object.assign(process.env, env);

  // Keep the relay's task-file/token writes out of the real prod paths.
  const scratchRoot = path.join(__dirname, '..', '.relay-test-tmp');
  fs.mkdirSync(scratchRoot, { recursive: true });
  const tmpDir = fs.mkdtempSync(path.join(scratchRoot, 'relay-test-'));
  process.env.HIVE_TASK_FILE = path.join(tmpDir, 'contributor-task.json');
  process.env.HIVE_GH_TOKEN_CACHE = path.join(tmpDir, 'gh-token.cache');
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
  try { fs.rmSync(relay.__tmpDir, { recursive: true, force: true }); } catch (_) {}
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
// kubestellar/hive#2844 — interactive completion detection must distinguish a
// finished turn from a backend prompt that is waiting for human input.
// ---------------------------------------------------------------------------

test('interactive pane classifier distinguishes complete, blocked, and working states', () => {
  const relay = loadRelay({ backend: 'goose' });
  try {
    const fixtures = [
      {
        name: 'finished turn',
        pane: 'Implemented the fix and opened a PR.\n> Enter to send\n',
        want: relay.PANE_STATE_IDLE_COMPLETE,
      },
      {
        name: 'finished turn with numbered summary',
        pane: 'Completed:\n1. Added tests\n2. Ran validation\n> Enter to send\n',
        want: relay.PANE_STATE_IDLE_COMPLETE,
      },
      {
        name: 'finished turn mentioning permission error',
        pane: 'Fixed the permission error and added regression coverage.\n> Enter to send\n',
        want: relay.PANE_STATE_IDLE_COMPLETE,
      },
      {
        name: 'finished turn after answered confirmation',
        pane: 'Continue with this command? [y/n]\ny\nDone.\n> Enter to send\n',
        want: relay.PANE_STATE_IDLE_COMPLETE,
      },
      {
        name: 'question with trailing question mark',
        pane: 'Should I open a pull request for this change?\n> \n',
        want: relay.PANE_STATE_BLOCKED_ON_HUMAN,
      },
      {
        name: 'numbered option menu',
        pane: 'Choose how to proceed:\n1. Open a PR\n2. File a follow-up issue\n> \n',
        want: relay.PANE_STATE_BLOCKED_ON_HUMAN,
      },
      {
        name: 'yes/no confirmation',
        pane: 'Continue with these changes? [y/n]\n> \n',
        want: relay.PANE_STATE_BLOCKED_ON_HUMAN,
      },
      {
        name: 'permission prompt',
        pane: 'Allow command to run?\n[y/n]\n> \n',
        want: relay.PANE_STATE_BLOCKED_ON_HUMAN,
      },
      {
        name: 'permission prompt with working verb',
        pane: 'Approve running this command? [y/n]\n> \n',
        want: relay.PANE_STATE_BLOCKED_ON_HUMAN,
      },
      {
        name: 'permission prompt without idle input chrome',
        pane: 'Bypass Permissions mode\n❯ 1. No, exit\n  2. Yes, I accept\nEnter to confirm\n',
        want: relay.PANE_STATE_BLOCKED_ON_HUMAN,
      },
      {
        name: 'actively working',
        pane: 'calling tool github.create_pull_request\n> Enter to send\n',
        want: relay.PANE_STATE_WORKING,
      },
    ];

    for (const tc of fixtures) {
      assert.strictEqual(relay.classifyTmuxPane(tc.pane), tc.want, tc.name);
    }
  } finally { teardown(relay); }
});

test('claude bypass-permissions idle footer is not itself a blocked prompt', () => {
  const relay = loadRelay({ backend: 'claude' });
  try {
    const pane = '✻ Worked for 1s\n❯ \n  ⏵⏵ bypass permissions on (shift+tab to cycle)\n';
    assert.strictEqual(relay.classifyTmuxPane(pane), relay.PANE_STATE_IDLE_COMPLETE);
  } finally { teardown(relay); }
});

test('blocked interactive panes report attention instead of task_complete', () => {
  const blockedPane = 'Should I open a pull request for this change?\n> \n';
  const relay = loadRelay({ backend: 'goose', cliStates: [blockedPane, blockedPane] });
  try {
    relay.setCliReady(true);
    assignTask(relay, 'ct-blocked');
    relay.__crashTick();

    assert.ok(!relay.__sent.some(m => m.type === 'task_complete'),
      'blocked panes must never be reported as completed');
    const progress = relay.__sent.find(m => m.type === 'task_progress' && m.status === 'blocked_on_human');
    assert.ok(progress, `expected blocked_on_human progress, got: ${JSON.stringify(relay.__sent)}`);
    assert.strictEqual(progress.attention, true, 'blocked status must request human attention');
    assert.ok(relay.getCurrentTask(), 'the task must remain active while waiting for a human');
  } finally { teardown(relay); }
});

// ---------------------------------------------------------------------------
// Multi-hub (kubestellar/hive#multi-hive) — one relay/CLI session subscribed
// to more than one hub via comma-separated HIVE_HUB/HIVE_REGISTRATION_TOKEN.
// ---------------------------------------------------------------------------

const MULTI_HUB_ENV = {
  HIVE_HUB: 'wss://hub-a.example/contribute,wss://hub-b.example/contribute',
  HIVE_REGISTRATION_TOKEN: 'tok-a,tok-b',
};

function attachHubSinks(relay) {
  const hubs = relay.getHubs();
  const sentA = [], sentB = [];
  hubs[0].ws = { readyState: 1, send: p => sentA.push(JSON.parse(p)) };
  hubs[1].ws = { readyState: 1, send: p => sentB.push(JSON.parse(p)) };
  return { hubs, sentA, sentB };
}

function withImmediateTimers(fn) {
  const origSetTimeout = global.setTimeout;
  global.setTimeout = (cb) => { cb(); return 0; };
  try { fn(); } finally { global.setTimeout = origSetTimeout; }
}

test('HIVE_HUB/HIVE_REGISTRATION_TOKEN comma lists parse into one hub per entry, matched by position', () => {
  const relay = loadRelay({ env: MULTI_HUB_ENV });
  try {
    const hubs = relay.getHubs();
    assert.strictEqual(hubs.length, 2);
    assert.ok(hubs[0].url.includes('hub-a.example'));
    assert.ok(hubs[1].url.includes('hub-b.example'));
    assert.strictEqual(hubs[0].regToken, 'tok-a');
    assert.strictEqual(hubs[1].regToken, 'tok-b');
  } finally { teardown(relay); }
});

test('only the active hub is sent ready on auth_ok; the other waits its turn', () => {
  const relay = loadRelay({ env: MULTI_HUB_ENV });
  try {
    const { hubs, sentA, sentB } = attachHubSinks(relay);

    relay.handleMessage(JSON.stringify({ type: 'auth_ok', contributor_id: 'c1', trust_tier: 'contributor' }), hubs[0]);
    assert.deepStrictEqual(sentA.map(m => m.type), ['ready']);
    assert.deepStrictEqual(sentB.map(m => m.type), []);

    relay.handleMessage(JSON.stringify({ type: 'auth_ok', contributor_id: 'c1', trust_tier: 'contributor' }), hubs[1]);
    assert.deepStrictEqual(sentB.map(m => m.type), [], 'non-active hub stays silent on its own auth_ok');
  } finally { teardown(relay); }
});

test('auth_failed on the active hub advances polling to an already-authenticated hub', () => {
  const relay = loadRelay({ env: MULTI_HUB_ENV });
  try {
    const { hubs, sentA, sentB } = attachHubSinks(relay);

    relay.handleMessage(JSON.stringify({ type: 'auth_ok', contributor_id: 'c1', trust_tier: 'contributor' }), hubs[1]);
    assert.deepStrictEqual(sentB.map(m => m.type), [], 'non-active hub waits before the active hub fails');

    relay.handleMessage(JSON.stringify({ type: 'auth_failed', reason: 'bad token' }), hubs[0]);
    assert.deepStrictEqual(sentA.map(m => m.type), []);
    assert.deepStrictEqual(sentB.map(m => m.type), ['ready'], 'polling moved to the authenticated remaining hub');
  } finally { teardown(relay); }
});

test('a task_assign is remembered by hub, and a rejection while busy is routed to the ASKING hub', () => {
  const relay = loadRelay({ env: MULTI_HUB_ENV });
  try {
    const { hubs, sentA, sentB } = attachHubSinks(relay);
    withImmediateTimers(() => {
      relay.handleMessage(JSON.stringify({ type: 'task_unavailable', reason: 'no_work' }), hubs[0]);
    });

    relay.handleMessage(JSON.stringify({ type: 'task_assign', task_id: 't1', kind: 'issue', repo: 'foo/bar', number: 1, title: 'x' }), hubs[1]);
    const task = relay.getCurrentTask();
    assert.strictEqual(task._hub, hubs[1], 'currentTask remembers which hub assigned it');
    assert.ok(sentB.some(m => m.type === 'task_accepted'), 'task_accepted went to the assigning hub');
    assert.strictEqual(sentA.filter(m => m.type === 'task_accepted').length, 0);

    sentA.length = 0;
    relay.handleMessage(JSON.stringify({ type: 'task_assign', task_id: 't2', kind: 'issue', repo: 'foo/bar', number: 2, title: 'y' }), hubs[0]);
    assert.ok(sentA.some(m => m.type === 'task_failed' && m.reason === 'Already has active task'),
      'busy-rejection went to the hub that just asked, not silently dropped or misrouted to the active-task hub');
    assert.strictEqual(sentB.filter(m => m.type === 'task_failed').length, 0);
  } finally { teardown(relay); }
});

test('an idle non-active hub cannot assign work until the poll slot reaches it', () => {
  const relay = loadRelay({ env: MULTI_HUB_ENV });
  try {
    const { hubs, sentB } = attachHubSinks(relay);

    relay.handleMessage(JSON.stringify({ type: 'task_assign', task_id: 't2', kind: 'issue', repo: 'foo/bar', number: 2, title: 'y' }), hubs[1]);

    assert.strictEqual(relay.getCurrentTask(), null);
    assert.ok(sentB.some(m => m.type === 'task_failed' && m.reason === 'Hub is not the active polling slot'),
      'unexpected assignment must be rejected back to the hub that sent it');
  } finally { teardown(relay); }
});

test('token_refresh, task_revoke, and blocked progress only affect the hub that owns the active task', () => {
  const blockedPane = 'Should I open a pull request for this change?\n> \n';
  const relay = loadRelay({ backend: 'goose', cliStates: [blockedPane, blockedPane], env: MULTI_HUB_ENV });
  try {
    const { hubs, sentA, sentB } = attachHubSinks(relay);

    relay.handleMessage(JSON.stringify({ type: 'task_assign', task_id: 't1', kind: 'issue', repo: 'foo/bar', number: 1, title: 'x' }), hubs[0]);
    const tokenPath = path.join(relay.__tmpDir, 'gh-token.cache');

    relay.handleMessage(JSON.stringify({ type: 'token_refresh', github_token: 'hub-b-token' }), hubs[1]);
    assert.strictEqual(fs.existsSync(tokenPath), false, 'non-owning hub must not overwrite the active task token');

    relay.__crashTick();
    assert.ok(sentA.some(m => m.type === 'task_progress' && m.status === 'blocked_on_human'),
      'blocked_on_human progress must go to the owning hub');
    assert.strictEqual(sentB.filter(m => m.type === 'task_progress').length, 0,
      'blocked_on_human progress must not leak to the non-owning hub');

    relay.handleMessage(JSON.stringify({ type: 'task_revoke', task_id: 't1', reason: 'wrong hub' }), hubs[1]);
    assert.strictEqual(relay.getCurrentTask().task_id, 't1', 'non-owning hub must not revoke the active task');

    relay.handleMessage(JSON.stringify({ type: 'token_refresh', github_token: 'hub-a-token' }), hubs[0]);
    assert.strictEqual(fs.readFileSync(tokenPath, 'utf8'), 'hub-a-token');

    relay.handleMessage(JSON.stringify({ type: 'task_revoke', task_id: 't1', reason: 'owner revoke' }), hubs[0]);
    assert.strictEqual(relay.getCurrentTask(), null);
    assert.ok(sentA.some(m => m.type === 'ready'), 'owning hub is asked for work after its revoke');
    assert.strictEqual(sentB.filter(m => m.type === 'ready').length, 0);
  } finally { teardown(relay); }
});

test('task_unavailable on the active hub rotates the poll slot to the next hub', () => {
  const relay = loadRelay({ env: MULTI_HUB_ENV });
  try {
    const { hubs, sentA, sentB } = attachHubSinks(relay);

    relay.handleMessage(JSON.stringify({ type: 'auth_ok', contributor_id: 'c1', trust_tier: 'contributor' }), hubs[0]);
    assert.deepStrictEqual(sentA.map(m => m.type), ['ready']);

    withImmediateTimers(() => {
      relay.handleMessage(JSON.stringify({ type: 'task_unavailable', reason: 'no_work' }), hubs[0]);
    });
    assert.deepStrictEqual(sentB.map(m => m.type), ['ready'], 'rotation sent ready to hub B, not hub A again');
  } finally { teardown(relay); }
});

test('currentTask stays JSON-serializable after task_assign attaches its owning hub', () => {
  const relay = loadRelay({ env: MULTI_HUB_ENV });
  try {
    const { hubs } = attachHubSinks(relay);
    hubs[0].heartbeatInterval = setInterval(() => {}, 999999);
    hubs[1].reconnectTimer = setTimeout(() => {}, 999999);
    try {
      withImmediateTimers(() => {
        relay.handleMessage(JSON.stringify({ type: 'task_unavailable', reason: 'no_work' }), hubs[0]);
      });
      relay.handleMessage(JSON.stringify({ type: 'task_assign', task_id: 't1', kind: 'issue', repo: 'foo/bar', number: 1, title: 'x' }), hubs[1]);
      assert.ok(relay.getCurrentTask(), 'task was accepted before serializing');
      assert.doesNotThrow(() => JSON.stringify(relay.getCurrentTask()), 'currentTask must serialize even with its _hub set');
    } finally {
      clearInterval(hubs[0].heartbeatInterval);
      clearTimeout(hubs[1].reconnectTimer);
    }
  } finally { teardown(relay); }
});

test('a currentTask with no recorded hub still reaches the hub', () => {
  const relay = loadRelay({ env: MULTI_HUB_ENV });
  try {
    const { sentA } = attachHubSinks(relay);

    relay.setCurrentTask({ task_id: 'pr-review-1', kind: 'review', repo: 'foo/bar', number: 0, title: 'Review open PRs' });
    relay.failCurrentTask('done reviewing');

    assert.ok(sentA.some(m => m.type === 'task_failed' && m.task_id === 'pr-review-1'),
      'frames for a hubless currentTask must fall back to the active hub, not be dropped');
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
  // bob drives an interactive TUI with no known one-shot entry point. (goose
  // used to be the example here, but it gained one — see the table test below.)
  const relay = loadRelay({ backend: 'bob', mode: 'headless' });
  try {
    assert.strictEqual(relay.headlessSupportsBackend(), false, 'bob has no one-shot mode');
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

// Enumerates every backend the relay reasons about, so adding one forces an
// explicit decision about its headless invocation rather than silently
// inheriting "unsupported". `tail` is the exact trailing argv (one-shot tokens
// + prompt); null means the backend has no non-interactive entry point.
test('buildHeadlessArgv maps each supported backend to its one-shot invocation', () => {
  const PROMPT = 'do the thing';
  for (const tc of [
    { backend: 'claude', tail: ['-p', PROMPT] },
    { backend: 'litellm', tail: ['-p', PROMPT] },
    { backend: 'copilot', tail: ['-p', PROMPT] },
    { backend: 'codex', tail: ['exec', PROMPT] },
    // goose needs its `run` sub-command AND -t (whose VALUE is the prompt) —
    // two leading tokens, unlike every other entry (#2828).
    { backend: 'goose', tail: ['run', '--no-session', '-t', PROMPT] },
    // Interactive-TUI backends with no known one-shot entry point.
    { backend: 'bob', tail: null },
    { backend: 'agy', tail: null },
    { backend: 'pi', tail: null },
  ]) {
    const relay = loadRelay({ backend: tc.backend, mode: 'headless' });
    try {
      const got = relay.buildHeadlessArgv(PROMPT);
      if (tc.tail === null) {
        assert.strictEqual(got, null, `${tc.backend} has no one-shot mode, so no argv`);
        assert.strictEqual(relay.headlessSupportsBackend(), false,
          `${tc.backend} must not report headless support`);
        continue;
      }
      assert.ok(got, `${tc.backend} must build a headless argv`);
      assert.strictEqual(got.bin, tc.backend, `${tc.backend} should run its own binary`);
      assert.deepStrictEqual(got.args.slice(-tc.tail.length), tc.tail,
        `${tc.backend} one-shot argv wrong: ${JSON.stringify(got.args)}`);
      assert.strictEqual(got.args[got.args.length - 1], PROMPT,
        `${tc.backend} must pass the prompt as the final distinct argv element`);
      assert.strictEqual(relay.headlessSupportsBackend(), true,
        `${tc.backend} must report headless support`);
    } finally { teardown(relay); }
  }
});

test('goose headless passes the prompt as -t\'s value and skips --model', () => {
  // goose is in NO_MODEL_FLAG_BACKENDS (it selects its model via GOOSE_MODEL),
  // so a configured MODEL must not leak in as --model and break the argv.
  const relay = loadRelay({ backend: 'goose', mode: 'headless', model: 'some-model' });
  try {
    const a = relay.buildHeadlessArgv("it's a prompt with quotes");
    assert.ok(!a.args.includes('--model'),
      `goose must not get --model: ${JSON.stringify(a.args)}`);
    assert.deepStrictEqual(a.args.slice(-4),
      ['run', '--no-session', '-t', "it's a prompt with quotes"],
      'the prompt is -t\'s value, passed verbatim as its own argv element');
  } finally { teardown(relay); }
});

test('goose runs headless end-to-end and reports completion on exit 0', () => {
  const relay = loadRelay({ backend: 'goose', mode: 'headless' });
  try {
    assignHeadlessTask(relay);
    assert.strictEqual(relay.__execFileCalls.length, 1, 'expected one one-shot invocation');
    const call = relay.__execFileCalls[0];
    assert.strictEqual(call.bin, 'goose');
    assert.ok(call.args.includes('run') && call.args.includes('-t'),
      `goose headless must use 'run' with -t: ${JSON.stringify(call.args)}`);
    assert.ok(call.args[call.args.length - 1].includes('foo/bar#7'),
      'the task prompt must be the trailing argv element');
    // Headless completion is the child's exit code, not scraped output.
    assert.ok(relay.__sent.find(m => m.type === 'task_complete'),
      'a successful goose one-shot run must report task_complete');
    assert.ok(!relay.__tmuxSends().some(c => / -l /.test(c)),
      'headless goose must never type into tmux');
  } finally { teardown(relay); }
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
