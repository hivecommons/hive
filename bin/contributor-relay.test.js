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

function loadRelay({ backend = 'copilot', backendBinary = null, model = '', reasoningEffort = '', cliStates = ['ready'], procAlive = true, mode = 'interactive', execFileResult = null, statusFile = null, paneText = null, env = null, cliVersion = null } = {}) {
  const commands = [];
  const sent = [];
  // Records every execFile (headless one-shot) invocation: { bin, args, opts }.
  const execFileCalls = [];
  let stateIdx = 0;
  // Guard against a runaway loop in the code under test eating all memory.
  const MAX_RECORDED_COMMANDS = 10000;

  const fakeExecSync = (cmd) => {
    if (commands.length < MAX_RECORDED_COMMANDS) commands.push(cmd);
    // backendBinary lets a test model backends.conf mapping a backend NAME to a
    // different BINARY (litellm → claude); it defaults to the identity mapping
    // every other backend has.
    if (/backend_binary/.test(cmd)) return `${backendBinary || backend}\n`;
    if (/backend_perm_flag/.test(cmd)) return '--allow-all\n';
    if (/capture-pane/.test(cmd)) {
      // paneText, when given, is returned verbatim — for tests that need a
      // REAL pane rendering (e.g. a codex modal menu) rather than one of the
      // three synthetic states below. A function is called fresh each capture
      // (tests that need the pane to CHANGE partway through, e.g. a late
      // completion arriving after a stall confirmation tick).
      if (typeof paneText === 'function') return paneText();
      if (paneText !== null) return paneText;
      const state = cliStates[Math.min(stateIdx++, cliStates.length - 1)];
      // Panes that getCLIState()/checkTmuxIdle() classify per backend.
      if (state === 'ready') return '/ commands for help\n';
      if (state === 'working') return '/ commands for help\nesc cancel\n';
      if (typeof state === 'string' && state.includes('\n')) return state;
      return 'dev@host:~$ \n';
    }
    if (/display-message/.test(cmd)) {
      // The relay asks the PANE what it is running (pane_current_command).
      // procAlive:false models a CLI that exited and left the pane at a shell.
      return procAlive ? `${backend}\n` : 'bash\n';
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

  // The capability probe (`<cli> --version`, kubestellar/hive#2547) is the only
  // execFileSync caller. `cliVersion` is what the CLI "prints"; an Error instance
  // makes the probe throw, standing in for an absent binary, an unsupported flag
  // or a timeout kill — every one of which must leave the field simply absent.
  const execFileSyncCalls = [];
  const fakeExecFileSync = (bin, args, opts) => {
    execFileSyncCalls.push({ bin, args, opts });
    if (cliVersion instanceof Error) throw cliVersion;
    if (cliVersion === null) throw new Error('spawnSync ENOENT');
    return cliVersion;
  };

  const stubs = {
    child_process: {
      execSync: fakeExecSync,
      execFile: fakeExecFile,
      execFileSync: fakeExecFileSync,
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
  process.env.AGENT_REASONING_EFFORT = reasoningEffort;
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
  process.env.HIVE_TASK_FILE = path.join(tmpDir, 'contributor-task.json');
  process.env.HIVE_GH_TOKEN_CACHE = path.join(tmpDir, 'gh-token.cache');
  if (env) Object.assign(process.env, env);

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
  relay.__execFileSyncCalls = execFileSyncCalls;
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

// --- agy pane classification: stale narration must not pin WORKING ---------
//
// Verbatim shape of a real wedged pane (kubestellar/hive): agy had finished the
// turn and printed its no_work_needed verdict, and was sitting at its idle
// prompt. One line of narration left over from the PREVIOUS task — "I am
// running the pkg/agent tests…" — kept the whole-pane isWorking scan true, so
// the relay never reported completion, kept renewing the hub's task lease, and
// the contributor had to Ctrl-C to get any further work.
const AGY_WEDGED_PANE = [
  'Edit(~/hive/src/pkg/agent/manager_test.go)',
  'Bash(cd src && go test ./pkg/agent)',
  '',
  'I am running the pkg/agent tests with the shortened temp directory path to verify they now pass locally as well.',
  // The turn continues for a while after that line — in the pane this fixture
  // came from it sat 36 rows above the bottom, far outside any sane tail.
  ...Array.from({ length: 20 }, (_, i) => `  ok  github.com/kubestellar/hive/pkg/thing${i}  0.0${i}s`),
  '',
  '  HIVE_VERDICT: no_work_needed — standing living document tracker, not an actionable task',
  '────────────────────────────────────────────',
  '>',
  '────────────────────────────────────────────',
  '? for shortcuts',
].join('\n');

// Live agy/Gemini pane after opening kubestellar/hive#4079. Newer builds no
// longer print "? for shortcuts" at rest: their idle chrome is a bare input
// prompt followed by the selected-model footer.
const AGY_GEMINI_IDLE_PANE = [
  '● Bash(gh pr create --repo kubestellar/hive ...)',
  ...Array.from({ length: 20 }, (_, i) => `  completed test step ${i}`),
  '',
  '  • Opened https://github.com/foo/bar/pull/9 targeting v4.',
  '────────────────────────────────────────────',
  '>',
  '',
  'Gemini 3.7 Flash · high',
].join('\n');

// --- CLI liveness: ask the PANE, not the process table --------------------
//
// The old probe substring-matched the whole process table for the backend's
// name, OR'd with every other CLI's name unconditionally. Observed live: the
// relay's own launcher (`just contribute-hive agy local`) and its tmux session
// (`hive-agy-5b4f`) both contain "agy", and any box running Claude Code matched
// 'claude' whatever the backend was — so a dead CLI was never detected, was
// never relaunched, cliReady stayed latched, and the hub's task prompt was typed
// into a bare shell.
// --- Relaunch must pin the working directory --------------------------------
//
// A long-lived tmux server can hand every pane it forks a cwd that no longer
// exists (observed: a nested clone's v2/pkg/agent, orphaned when the repo
// renamed v2/ -> src/). The shell reports "shell-init: error retrieving current
// directory" and a backend needing a resolvable cwd dies shortly after its
// first task — agy exits 2 that way. The Justfile pins the cwd for the first
// launch; a relaunch that dropped the cd would silently undo it.
test('a relaunch cds into the relay cwd before starting the CLI', () => {
  const relay = loadRelay({ backend: 'agy' });
  try {
    const launched = relay.launchCommandWithCwd('agy --dangerously-skip-permissions');
    assert.match(launched, /^cd .+ && agy --dangerously-skip-permissions$/,
      `relaunch must pin the cwd, got: ${launched}`);
    assert.ok(launched.includes(process.cwd()),
      `the cd target must be the relay's own cwd: ${launched}`);
  } finally { teardown(relay); }
});

test('relaunchCLI sends the cd-prefixed command to tmux', () => {
  const relay = loadRelay({ backend: 'agy' });
  try {
    const before = relay.__tmuxSends().length;
    relay.relaunchCLI();
    const sends = relay.__tmuxSends().slice(before);
    assert.ok(sends.some(c => /cd .+ && /.test(c)),
      `the relaunch typed into the pane must carry the cd: ${JSON.stringify(sends)}`);
  } finally { teardown(relay); }
});

test('a shell in the pane is only a death after consecutive confirmations', () => {
  const relay = loadRelay({ backend: 'agy', procAlive: false });
  try {
    assert.strictEqual(relay.cliProcessLooksGone(), false,
      'one shell reading may just be a foreground tool call — never restart on it');
    assert.strictEqual(relay.cliProcessLooksGone(), true,
      `${relay.CLI_GONE_CONFIRMATIONS} consecutive shell readings mean the CLI really left`);
  } finally { teardown(relay); }
});

test('a pane running the CLI is never reported gone, and clears the count', () => {
  const relay = loadRelay({ backend: 'agy', procAlive: true });
  try {
    assert.strictEqual(relay.cliProcessLooksGone(), false);
    assert.strictEqual(relay.cliProcessLooksGone(), false,
      'a live CLI must never accumulate toward a death, however long it runs');
  } finally { teardown(relay); }
});

test('a task prompt is never typed into a pane that is running a shell', () => {
  // The latch says ready — as it did in production, because nothing had
  // detected the CLI leaving — but the pane is a shell. The prompt must be
  // queued and the CLI relaunched, NOT typed at the shell.
  const relay = loadRelay({ backend: 'agy', procAlive: false });
  try {
    relay.setCliReady(true);
    const PROMPT = "You are a contributor to the kubestellar/hive hive. Work on issue #4030.";
    const before = relay.__tmuxSends().length;
    relay.tmuxSendKeys(PROMPT);

    assert.strictEqual(relay.getPendingTask(), PROMPT,
      'the prompt must be queued for the readiness callback, not typed into the shell');
    assert.strictEqual(relay.getCliReady(), false,
      'a stale readiness latch must be dropped once the pane is seen to be a shell');
    const sends = relay.__tmuxSends().slice(before);
    assert.ok(!sends.some(c => c.includes('contributor to the kubestellar/hive hive')),
      `the prompt text must never reach the pane: ${JSON.stringify(sends)}`);
    assert.ok(sends.some(c => /agy/.test(c)),
      `the CLI must be relaunched so the queued prompt has somewhere to go: ${JSON.stringify(sends)}`);
  } finally { teardown(relay); }
});

test('agy at its idle prompt is COMPLETE even with stale narration on screen', () => {
  const relay = loadRelay({ backend: 'agy' });
  try {
    assert.strictEqual(
      relay.classifyTmuxPane(AGY_WEDGED_PANE), relay.PANE_STATE_IDLE_COMPLETE,
      'a finished agy turn must not read as working just because an older line says "running"');
  } finally { teardown(relay); }
});

test('agy Gemini footer with a bare prompt is COMPLETE', () => {
  const relay = loadRelay({ backend: 'agy' });
  try {
    assert.strictEqual(
      relay.classifyTmuxPane(AGY_GEMINI_IDLE_PANE), relay.PANE_STATE_IDLE_COMPLETE,
      'a finished current agy/Gemini pane must not remain working because its old footer changed');
  } finally { teardown(relay); }
});

test('agy Gemini idle pane reports its visible PR as task_complete', () => {
  const relay = loadRelay({ backend: 'agy', paneText: AGY_GEMINI_IDLE_PANE });
  try {
    assignTask(relay, 'ct-agy-gemini-idle');
    relay.__crashTick();
    const complete = relay.__sent.find(m => m.type === 'task_complete');
    assert.ok(complete, 'the live agy/Gemini pane shape must complete the task');
    assert.strictEqual(complete.pr_url, 'https://github.com/foo/bar/pull/9');
  } finally { teardown(relay); }
});

test('agy still reads as WORKING while activity is in the tail', () => {
  const relay = loadRelay({ backend: 'agy' });
  try {
    const busy = [
      'HIVE_VERDICT: no_work_needed — an older, finished turn',
      '',
      'Bash(go build ./...)',
      'Reading src/pkg/agent/manager.go',
    ].join('\n');
    assert.strictEqual(
      relay.classifyTmuxPane(busy), relay.PANE_STATE_WORKING,
      'agy with fresh activity at the bottom of the pane must stay WORKING');
  } finally { teardown(relay); }
});

// --- Pane stall backstop ---------------------------------------------------
//
// The hub's wedged-worker reclaim only catches a relay that goes SILENT; one
// that keeps reporting "working" renews the lease forever. These pin the
// relay-side half: an unchanging pane eventually stops claiming progress.
test('an unchanging pane trips the stall backstop; any change resets it', () => {
  const relay = loadRelay({ backend: 'agy' });
  try {
    const past = () => relay.__agePaneStallClock(relay.PANE_STALL_TIMEOUT_MS + 1);
    assert.strictEqual(relay.paneStalled(['same']), false, 'first sighting only records the fingerprint');
    past();
    assert.strictEqual(relay.paneStalled(['same']), true, 'unchanged pane past the timeout is stalled');
    assert.strictEqual(relay.paneStalled(['different']), false, 'a changed pane restarts the clock');
    past();
    assert.strictEqual(relay.paneStalled(['different']), true);
    // An empty capture is a missing pane, not a stalled agent.
    assert.strictEqual(relay.paneStalled([]), false);
    past();
    assert.strictEqual(relay.paneStalled([]), false, 'an empty capture must never trip the backstop');
  } finally { teardown(relay); }
});

test('a stalled pane is NOT failed on the first confirmation tick', () => {
  // Observed live: a task can cross PANE_STALL_TIMEOUT_MS while the CLI is
  // blocked on a slow network call (a `gh pr create` round trip), then print
  // its real completion — with a real PR link — moments later. The relay must
  // not act on the very first tick that crosses the timeout; it needs to give
  // the CLI PANE_STALL_CONFIRM_TICKS chances to prove it was about to finish.
  const relay = loadRelay({
    backend: 'agy',
    paneText: 'a frozen pane with no idle prompt and nothing happening',
  });
  try {
    relay.setCliReady(true);
    assignTask(relay, 't-stall');
    relay.__stallTick();   // records the fingerprint, reports working
    relay.__agePaneStallClock(relay.PANE_STALL_TIMEOUT_MS + 1);
    relay.__stallTick();   // first tick past the timeout -> confirmation 1, not a failure yet
    assert.strictEqual(relay.__sent.filter(m => m.type === 'task_failed').length, 0,
      'the first tick past the stall timeout must not fail the task on its own');
    assert.strictEqual(relay.getStallConfirmCount(), 1);
    assert.strictEqual(relay.getCurrentTask() && relay.getCurrentTask().task_id, 't-stall',
      'the task must still be held while confirmation is pending');
  } finally { teardown(relay); }
});

test('a pane that recovers between stall ticks is NOT failed, and the confirm count resets', () => {
  let capture = 'a frozen pane with no idle prompt and nothing happening';
  const relay = loadRelay({ backend: 'agy', paneText: () => capture });
  try {
    relay.setCliReady(true);
    assignTask(relay, 't-recover');
    relay.__stallTick();
    relay.__agePaneStallClock(relay.PANE_STALL_TIMEOUT_MS + 1);
    relay.__stallTick();   // confirmation 1
    assert.strictEqual(relay.getStallConfirmCount(), 1);
    // New output appears -- the CLI was never actually stuck.
    capture = 'a frozen pane with no idle prompt and nothing happening, plus fresh output this time';
    relay.__agePaneStallClock(relay.PANE_STALL_TIMEOUT_MS);
    relay.__stallTick();
    assert.strictEqual(relay.getStallConfirmCount(), 0,
      'any new pane content must reset the confirm count, not just delay the verdict');
    assert.strictEqual(relay.__sent.filter(m => m.type === 'task_failed').length, 0);
  } finally { teardown(relay); }
});

test('a stalled pane hands the task back as an environment failure once confirmed, and relaunches the CLI', () => {
  const relay = loadRelay({
    backend: 'agy',
    paneText: 'a frozen pane with no idle prompt and nothing happening',
  });
  try {
    relay.setCliReady(true);
    assignTask(relay, 't-stall');
    relay.__stallTick();
    relay.__agePaneStallClock(relay.PANE_STALL_TIMEOUT_MS + 1);
    relay.__stallTick();   // confirmation 1 — not yet failed (see the test above)
    relay.__agePaneStallClock(relay.PANE_STALL_TIMEOUT_MS + 1);
    const before = relay.__tmuxSends().length;
    relay.__stallTick();   // confirmation 2 (== PANE_STALL_CONFIRM_TICKS) -> give the task back
    const failed = relay.__sent.filter(m => m.type === 'task_failed');
    assert.strictEqual(failed.length, 1, `expected one task_failed, got ${JSON.stringify(relay.__sent.map(m => m.type))}`);
    assert.strictEqual(failed[0].failure_kind, 'environment',
      'a frozen pane says nothing about the WORK, so the failure is the environment kind');
    assert.match(failed[0].reason, /no pane activity/);
    assert.match(failed[0].reason, new RegExp(String(relay.PANE_STALL_CONFIRM_TICKS)),
      'the failure reason should name how many checks confirmed it, for anyone reading the log');
    assert.strictEqual(relay.getCurrentTask(), null, 'the relay must let go of the task, not keep renewing its lease');
    // The CLI is relaunched so the NEXT task cannot land its prompt on top of
    // whatever the abandoned turn is still doing in the background.
    const sends = relay.__tmuxSends().slice(before);
    assert.ok(sends.some(c => /agy/.test(c)),
      `a confirmed stall must relaunch the CLI: ${JSON.stringify(sends)}`);
  } finally { teardown(relay); }
});

test('a pane that reaches real IDLE_COMPLETE between stall ticks is reported as a normal completion, PR and all', () => {
  // The exact live scenario: paneText starts frozen (mid stall), then -- before
  // the SECOND confirmation tick -- the CLI's real completion appears, agy back
  // at its idle prompt with a PR link in the output. checkTmuxPaneState() must
  // win over the stall path on that tick, so the task is reported completed
  // with the PR, not failed.
  let capture = 'a frozen pane with no idle prompt and nothing happening';
  const relay = loadRelay({ backend: 'agy', paneText: () => capture });
  try {
    relay.setCliReady(true);
    assignTask(relay, 't-late-finish');
    relay.__stallTick();
    relay.__agePaneStallClock(relay.PANE_STALL_TIMEOUT_MS + 1);
    relay.__stallTick();   // confirmation 1
    assert.strictEqual(relay.getStallConfirmCount(), 1);
    // The slow network call the pane was blocked on finally returns.
    capture = 'Pull request opened: foo/bar#4061 https://github.com/foo/bar/pull/4061\n? for shortcuts';
    relay.__agePaneStallClock(relay.PANE_STALL_TIMEOUT_MS + 1);
    relay.__stallTick();
    const completed = relay.__sent.filter(m => m.type === 'task_complete');
    assert.strictEqual(completed.length, 1,
      `late completion must be reported as completed, not failed: ${JSON.stringify(relay.__sent.map(m => m.type))}`);
    assert.strictEqual(completed[0].pr_url, 'https://github.com/foo/bar/pull/4061',
      'the PR that actually landed must be credited, not lost to the stall path');
    assert.strictEqual(relay.__sent.filter(m => m.type === 'task_failed').length, 0);
  } finally { teardown(relay); }
});

test('agy pairs --effort with --model, and omits it when no model is set', () => {
  // agy warns "--model <m> requires --effort (available: low, medium, high)"
  // and then IGNORES the model, so the two flags must travel together. Mirrors
  // agyDefaultEffort ("low") in the hub-side launcher.
  const withModel = loadRelay({ backend: 'agy', model: 'gemini-3.6-flash-high' });
  try {
    const cmd = withModel.buildLaunchCommand();
    assert.match(cmd, /--model gemini-3\.6-flash-high/, `expected model flag, got: ${cmd}`);
    assert.match(cmd, /--effort low/, `agy dropped the required --effort: ${cmd}`);
  } finally { teardown(withModel); }

  const noModel = loadRelay({ backend: 'agy' });
  try {
    const cmd = noModel.buildLaunchCommand();
    assert.ok(!/--effort/.test(cmd), `--effort belongs only with --model, got: ${cmd}`);
  } finally { teardown(noModel); }
});

test('agy effort honors AGENT_REASONING_EFFORT but rejects values agy cannot take', () => {
  // codex's vocabulary is wider than agy's ("minimal" is valid for codex only).
  // Forwarding an unknown token would make agy reject the pairing and drop the
  // model again, so anything outside low|medium|high falls back to the default.
  const good = loadRelay({ backend: 'agy', model: 'm', reasoningEffort: 'high' });
  try {
    assert.match(good.buildLaunchCommand(), /--effort high/);
  } finally { teardown(good); }

  const bogus = loadRelay({ backend: 'agy', model: 'm', reasoningEffort: 'minimal' });
  try {
    const cmd = bogus.buildLaunchCommand();
    assert.match(cmd, /--effort low/, `unknown effort must fall back to low, got: ${cmd}`);
    assert.ok(!/minimal/.test(cmd), `agy must not receive codex-only effort values: ${cmd}`);
  } finally { teardown(bogus); }
});

test('agy headless argv carries the same --model/--effort pairing', () => {
  const relay = loadRelay({ backend: 'agy', mode: 'headless', model: 'gemini-3.6-flash-high' });
  try {
    const a = relay.buildHeadlessArgv('review this');
    const i = a.args.indexOf('--effort');
    assert.ok(i >= 0, `headless agy dropped --effort: ${JSON.stringify(a.args)}`);
    assert.strictEqual(a.args[i + 1], 'low');
    assert.ok(a.args.includes('--model'), `headless agy dropped --model: ${JSON.stringify(a.args)}`);
    // The prompt still has to be the final, distinct element after -p.
    assert.deepStrictEqual(a.args.slice(-2), ['-p', 'review this']);
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

test('auth_response includes optional HIVE_AGENT_ROLE', () => {
  const relay = loadRelay({ env: { HIVE_AGENT_ROLE: 'scanner' } });
  try {
    relay.handleMessage(JSON.stringify({ type: 'auth_challenge', seq: 1, nonce: 'n' }));
    const auth = relay.__sent.find(m => m.type === 'auth_response');
    assert.ok(auth, 'expected auth_response');
    assert.strictEqual(auth.role, 'scanner');
  } finally { teardown(relay); }
});

test('auth_response includes AGENT_REASONING_EFFORT when set', () => {
  const relay = loadRelay({ env: { AGENT_BACKEND: 'codex', AGENT_MODEL: 'gpt-5.6-terra', AGENT_REASONING_EFFORT: 'high' } });
  try {
    relay.handleMessage(JSON.stringify({ type: 'auth_challenge', seq: 1, nonce: 'n' }));
    const auth = relay.__sent.find(m => m.type === 'auth_response');
    assert.ok(auth, 'expected auth_response');
    assert.strictEqual(auth.reasoning_effort, 'high');
  } finally { teardown(relay); }
});

test('auth_response defaults reasoning_effort to low for agy with model', () => {
  const relay = loadRelay({ env: { AGENT_BACKEND: 'agy', AGENT_MODEL: 'gemini-3.7-flash' } });
  try {
    relay.handleMessage(JSON.stringify({ type: 'auth_challenge', seq: 1, nonce: 'n' }));
    const auth = relay.__sent.find(m => m.type === 'auth_response');
    assert.ok(auth, 'expected auth_response');
    assert.strictEqual(auth.reasoning_effort, 'low');
  } finally { teardown(relay); }
});

test('auth_response omits reasoning_effort when empty and not agy-with-model', () => {
  const relay = loadRelay({ env: { AGENT_BACKEND: 'claude', AGENT_MODEL: 'claude-sonnet-5' } });
  try {
    relay.handleMessage(JSON.stringify({ type: 'auth_challenge', seq: 1, nonce: 'n' }));
    const auth = relay.__sent.find(m => m.type === 'auth_response');
    assert.ok(auth, 'expected auth_response');
    assert.strictEqual(auth.reasoning_effort, undefined);
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
    // TWO ticks per assignment: a CLI death is only declared once the pane has
    // read as a shell on consecutive checks, so a single transient reading
    // cannot restart a CLI that is merely running a foreground tool call.
    for (let i = 0; i <= cap; i++) {
      assignTask(relay, `ct-421-${i}`);
      relay.__crashTick();
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
      // Two ticks: a death needs consecutive shell readings to be confirmed.
      relay.__crashTick();
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
      // Two ticks: a death needs consecutive shell readings to be confirmed.
      relay.__crashTick();
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
      // kubestellar/hive#2844 — MCP elicitation forms. Each of these leaves the
      // pane at goose's idle chrome ("> " / "> Enter to send") with no working
      // word, so the pre-fix classifier called them IDLE_COMPLETE and the relay
      // reported the unanswered form as a finished task.
      {
        name: 'elicitation form ending in a bare > input line',
        pane: 'Extension needs some information to proceed:\n\n  Project name: my-service\n  Environment:  production\n  Region:       us-east-1\n\n> \n',
        want: relay.PANE_STATE_BLOCKED_ON_HUMAN,
      },
      {
        name: 'elicitation form with bracketed fields and no > at all',
        pane: 'Extension needs some information to proceed:\n\n  Project name: [                    ]\n  Environment:  [ production        ]\n\n  [ Submit ]   [ Cancel ]\n',
        want: relay.PANE_STATE_BLOCKED_ON_HUMAN,
      },
      {
        name: 'elicitation form while goose "> Enter to send" hint is still on screen',
        pane: 'Please fill in the deployment details below:\n\n  Namespace: default\n  Replicas:  3\n\n> Enter to send\n',
        want: relay.PANE_STATE_BLOCKED_ON_HUMAN,
      },
      {
        name: 'goose elicitation timeout marker',
        pane: 'Elicitation request timed out or failed\n> Enter to send\n',
        want: relay.PANE_STATE_BLOCKED_ON_HUMAN,
      },
    ];

    for (const tc of fixtures) {
      assert.strictEqual(relay.classifyTmuxPane(tc.pane), tc.want, tc.name);
    }
  } finally { teardown(relay); }
});

test('elicitation-form detection does not false-positive on ordinary finished output (#2844)', () => {
  const relay = loadRelay({ backend: 'goose' });
  try {
    // Finished panes that happen to contain form-ish shapes or words like
    // "provide"/"continue" mid-sentence, or a "label: value" line, must stay
    // COMPLETE. The elicitation matcher requires BOTH an explicit request-for-
    // input lead-in AND a form structure, so none of these should trip it — the
    // same bare-substring lesson as the /login false-positive fix.
    const finished = [
      'Implemented the fix and opened a PR: https://github.com/x/y/pull/1\n\ngoose is ready\n> Enter to send\n',
      'Done.\nFiles changed: 3\nTests: passing\n> Enter to send\n',
      'I updated the docs to provide the following context for readers.\n> Enter to send\n',
      'The build will continue to run in CI. All done.\n> Enter to send\n',
      'Refactored the parser [see commit abc123]. Complete.\n> Enter to send\n',
    ];
    for (const pane of finished) {
      assert.strictEqual(
        relay.classifyTmuxPane(pane), relay.PANE_STATE_IDLE_COMPLETE,
        `finished pane wrongly classified as blocked/working: ${JSON.stringify(pane)}`);
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

test('goose elicitation form is reported as blocked, never as task_complete (#2844)', () => {
  // The exact scenario Jorge reported: an MCP elicitation form left the pane at
  // goose's "> Enter to send" chrome, and the relay sent task_complete for work
  // that had not happened. It must now go out as attention/blocked instead.
  const formPane = 'Extension needs some information to proceed:\n\n  Project name: my-service\n  Region:       us-east-1\n\n> Enter to send\n';
  const relay = loadRelay({ backend: 'goose', cliStates: [formPane, formPane] });
  try {
    relay.setCliReady(true);
    assignTask(relay, 'ct-elicit');
    relay.__crashTick();

    assert.ok(!relay.__sent.some(m => m.type === 'task_complete'),
      'an unanswered elicitation form must never be reported as completed');
    const progress = relay.__sent.find(m => m.type === 'task_progress' && m.status === 'blocked_on_human');
    assert.ok(progress, `expected blocked_on_human progress, got: ${JSON.stringify(relay.__sent)}`);
    assert.strictEqual(progress.attention, true, 'blocked status must request human attention');
    assert.ok(relay.getCurrentTask(), 'the task must remain active while the form is unanswered');
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

test('hub notice messages are logged for operators', () => {
  const relay = loadRelay();
  const lines = [];
  const oldLog = console.log;
  console.log = (msg) => { lines.push(String(msg)); };
  try {
    relay.handleMessage(JSON.stringify({ type: 'notice', message: 'role assigned: scanner — your next task will be scanner work' }));
    assert.ok(lines.some(l => l.includes('role assigned: scanner')), 'notice message was not logged');
  } finally {
    console.log = oldLog;
    teardown(relay);
  }
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
    // agy -p "<prompt>" — Antigravity's print mode. Headless CAPABILITY only:
    // agy still cannot sign in inside a pod (interactive Google OAuth, no
    // API-key mode), which is why the k8s manifest generator keeps warning.
    { backend: 'agy', tail: ['-p', PROMPT] },
    // Interactive-TUI backends with no known one-shot entry point.
    { backend: 'bob', tail: null },
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

test('codex headless transports model and reasoning effort without affecting other backends', () => {
  const relay = loadRelay({
    backend: 'codex',
    mode: 'headless',
    model: 'gpt-5.6-luna',
    reasoningEffort: 'low',
  });
  try {
    const a = relay.buildHeadlessArgv('review this');
    assert.ok(a.args.includes('--model'), `codex must receive --model: ${JSON.stringify(a.args)}`);
    assert.ok(a.args.includes('gpt-5.6-luna'), `codex must receive the configured model: ${JSON.stringify(a.args)}`);
    assert.ok(a.args.includes('-c'), `codex must receive a config override: ${JSON.stringify(a.args)}`);
    assert.ok(a.args.includes('model_reasoning_effort="low"'), `codex must receive the configured effort: ${JSON.stringify(a.args)}`);
    assert.deepStrictEqual(a.args.slice(-2), ['exec', 'review this'],
      'codex one-shot mode and prompt must remain at the tail');
  } finally { teardown(relay); }

  const goose = loadRelay({
    backend: 'goose',
    mode: 'headless',
    model: 'some-model',
    reasoningEffort: 'low',
  });
  try {
    const a = goose.buildHeadlessArgv('review this');
    assert.ok(!a.args.includes('--model'), `goose must still skip --model: ${JSON.stringify(a.args)}`);
    assert.ok(!a.args.includes('model_reasoning_effort="low"'), `goose must not inherit codex effort: ${JSON.stringify(a.args)}`);
  } finally { teardown(goose); }
});

test('codex interactive launch transports reasoning effort only for codex', () => {
  const codex = loadRelay({
    backend: 'codex',
    model: 'gpt-5.6-luna',
    reasoningEffort: 'low',
  });
  try {
    const cmd = codex.buildLaunchCommand();
    assert.match(cmd, /--model gpt-5\.6-luna/);
    assert.match(cmd, /-c 'model_reasoning_effort="low"'/);
  } finally { teardown(codex); }

  const copilot = loadRelay({
    backend: 'copilot',
    model: 'gpt-5.6-luna',
    reasoningEffort: 'low',
  });
  try {
    const cmd = copilot.buildLaunchCommand();
    assert.match(cmd, /--model gpt-5\.6-luna/);
    assert.doesNotMatch(cmd, /model_reasoning_effort/);
  } finally { teardown(copilot); }
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


// Verbatim capture of a genuinely READY codex pane from a running
// ghcr.io/kubestellar/hive-contributor container. Note what it does NOT
// contain: no "codex>", no line ending in ">", and the banner says "OpenAI
// Codex", not "Codex CLI". The pre-fix patterns matched none of it, so this
// pane classified as 'starting' forever.
const CODEX_READY_PANE = [
  'dev@codex-contributor:~/workspace$ codex --dangerously-bypass-approvals-and-sandbox',
  '\u256d\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u256e',
  '\u2502 >_ OpenAI Codex (v0.146.0)                          \u2502',
  '\u2502 model:       gpt-5.6-luna medium   /model to change \u2502',
  '\u2502 directory:   ~/workspace                            \u2502',
  '\u2570\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u256f',
  '  Tip: Use /rename to rename your threads for easier thread resuming.',
  '\u203a Run /review on my current changes',
  '  gpt-5.6-luna medium \u00b7 ~/workspace',
  '', '', '',
].join('\n');

const CODEX_TRUST_PANE = [
  'dev@codex-contributor:~/workspace$ codex --dangerously-bypass-approvals-and-sandbox',
  '> You are in /home/dev/workspace',
  '',
  '  Do you trust the contents of this directory? Working with untrusted contents comes with higher risk of prompt injection.',
  '',
  '\u203a 1. Yes, continue',
  '  2. No, quit',
  '',
  '  Press enter to continue',
  '\u256d\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u256e',
  '\u2502 >_ OpenAI Codex (v0.146.0)                          \u2502',
  '\u2570\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u256f',
].join('\n');

const CODEX_UPDATE_PANE = [
  'dev@codex-contributor:~/workspace$ codex --dangerously-bypass-approvals-and-sandbox',
  '  ✨ Update available! 0.146.0 -> 0.147.0',
  '  Release notes: https://github.com/openai/codex/releases/latest',
  '\u203a 1. Update now (runs `npm install -g @openai/codex`)',
  '  2. Skip',
  '  3. Skip until next version',
  '  Press enter to continue',
].join('\n');

const CODEX_COMPLETED_NO_WORK_PANE = [
  '• Running GH_TOKEN=... gh issue view 4065 --repo kubestellar/hive',
  '',
  // Codex may leave many old tool rows above the completed turn.
  ...Array.from({ length: 20 }, (_, i) => `  checked upstream evidence ${i}`),
  '',
  // Codex prefixes completed assistant output with this bullet in tmux.
  '• HIVE_VERDICT: no_work_needed — upstream PR #4066 already implements issue #4065.',
  '─ Worked for 1m 59s ─',
  '',
  '› ',
].join('\n');

test('a ready codex pane is classified ready (regression: > vs \u203a, and "OpenAI Codex" not "Codex CLI")', () => {
  const relay = loadRelay({ backend: 'codex', cliStates: [CODEX_READY_PANE] });
  try {
    assert.strictEqual(relay.getCLIState(), 'ready',
      'codex readiness was never detected, so every task was queued and handed back at timeout — the backend could not run a single task');
  } finally { teardown(relay); }
});

test('the modal panes still win over the ready marker they also draw', () => {
  // Both modals render '\u203a' too; modal classification runs first, so a
  // blocked pane must NOT be reported ready by the widened pattern.
  for (const pane of [CODEX_TRUST_PANE, CODEX_UPDATE_PANE]) {
    const relay = loadRelay({ backend: 'codex', cliStates: [pane] });
    try {
      assert.strictEqual(relay.getCLIState(), 'onboarding');
    } finally { teardown(relay); }
  }
});

test('codex numbered startup menus get explicit safe selections', () => {
  const relay = loadRelay({ backend: 'codex' });
  try {
    assert.strictEqual(relay.blockingPromptKey(CODEX_TRUST_PANE), '1');
    assert.strictEqual(relay.blockingPromptKey(CODEX_UPDATE_PANE), '3');
    assert.strictEqual(relay.blockingPromptKey('Do you trust this folder? (y/n)'), null);
  } finally { teardown(relay); }
});

test('codex no-work verdict is COMPLETE despite stale activity in scrollback', () => {
  const relay = loadRelay({ backend: 'codex' });
  try {
    assert.strictEqual(
      relay.classifyTmuxPane(CODEX_COMPLETED_NO_WORK_PANE), relay.PANE_STATE_IDLE_COMPLETE,
      'an old Codex Running row must not keep a completed no-work turn in WORKING');
  } finally { teardown(relay); }
});

test('a bullet-prefixed Codex no-work verdict is reported as task_complete', () => {
  const relay = loadRelay({ backend: 'codex', paneText: CODEX_COMPLETED_NO_WORK_PANE });
  try {
    assignTask(relay, 'ct-codex-no-work');
    relay.__crashTick();
    const complete = relay.__sent.find(m => m.type === 'task_complete');
    assert.ok(complete, 'the live Codex pane shape must complete the task rather than remain working');
    assert.strictEqual(complete.verdict, 'no_work_needed');
  } finally { teardown(relay); }
});

test('codex still reads as WORKING while activity is in the tail', () => {
  const relay = loadRelay({ backend: 'codex' });
  try {
    const busy = [
      'HIVE_VERDICT: no_work_needed — an older, finished turn',
      '',
      '› ',
      '• Running gh issue view 4066 --repo kubestellar/hive',
    ].join('\n');
    assert.strictEqual(
      relay.classifyTmuxPane(busy), relay.PANE_STATE_WORKING,
      'recent Codex activity must still take precedence over an older verdict');
  } finally { teardown(relay); }
});

test('a pane with long diff in tail (no activity verbs) but completion_marker=true stays WORKING', () => {
  // Regression for the tail-scope fix: narrowing the activity scan to the tail
  // must not flip a mid-turn pane to COMPLETE just because the scrollback holds
  // a completion word. Work is still ongoing here — codex is streaming a diff,
  // so the tail carries neither an activity verb nor the '›' idle prompt.
  const relay = loadRelay({ backend: 'codex' });
  try {
    const midTurn = [
      '• Running git diff --stat',
      '  done reading upstream evidence',
      '',
      ...Array.from({ length: 20 }, (_, i) => `+  const line${i} = compute(${i});`),
    ].join('\n');
    assert.strictEqual(
      relay.classifyTmuxPane(midTurn), relay.PANE_STATE_WORKING,
      'a mid-turn codex pane must not complete just because "done" sits in the scrollback');
  } finally { teardown(relay); }
});

test('codex status indicators ("Working", "esc to interrupt") count as in-flight', () => {
  const relay = loadRelay({ backend: 'codex' });
  try {
    for (const status of ['• Working (12s • esc to interrupt)', '  esc to interrupt']) {
      const pane = [
        'HIVE_VERDICT: no_work_needed — an older, finished turn',
        '› ',
        status,
      ].join('\n');
      assert.strictEqual(
        relay.classifyTmuxPane(pane), relay.PANE_STATE_WORKING,
        `codex status row ${JSON.stringify(status)} must read as WORKING`);
    }
  } finally { teardown(relay); }
});

// ---------------------------------------------------------------------------
// Unresponsive-backend recovery: blocking modal prompts must be classified and
// dismissed with the RIGHT key, and a CLI that never reaches its prompt must
// hand its task back instead of silently holding it.
// ---------------------------------------------------------------------------

// NOTE (v2→v4 sync): v2 and v4 each added a codex-pane test block with its own
// CODEX_TRUST_PANE / CODEX_UPDATE_PANE constants (semantically identical — the
// only difference is '›' escapes vs the literal '›'). The v4 definitions
// above are kept as the single source of truth; the v2 redeclarations here were
// dropped in the sync merge, and the v2 tests below reuse the v4 constants. Both
// keep codex's banner chrome ("OpenAI Codex (v0.146.0)", the "›" input marker) on
// screen BEHIND the modal — which is exactly why the ready patterns have to be
// checked after the modal patterns, not before.

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

// Multi-hub (kubestellar/hive#multi-hive) — one relay/CLI session subscribed
// to more than one hub via comma-separated HIVE_HUB/HIVE_REGISTRATION_TOKEN.
// ---------------------------------------------------------------------------

test('HIVE_HUB/HIVE_REGISTRATION_TOKEN comma lists parse into one hub per entry, matched by position', () => {
  const relay = loadRelay({ env: {
    HIVE_HUB: 'wss://hub-a.example/contribute,wss://hub-b.example/contribute',
    HIVE_REGISTRATION_TOKEN: 'tok-a,tok-b',
  } });
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
  const relay = loadRelay({ env: {
    HIVE_HUB: 'wss://hub-a.example/contribute,wss://hub-b.example/contribute',
    HIVE_REGISTRATION_TOKEN: 'tok-a,tok-b',
  } });
  try {
    const hubs = relay.getHubs();
    const sentA = [], sentB = [];
    hubs[0].ws = { readyState: 1, send: p => sentA.push(JSON.parse(p)) };
    hubs[1].ws = { readyState: 1, send: p => sentB.push(JSON.parse(p)) };

    relay.handleMessage(JSON.stringify({ type: 'auth_ok', contributor_id: 'c1', trust_tier: 'contributor' }), hubs[0]);
    assert.deepStrictEqual(sentA.map(m => m.type), ['ready']);
    assert.deepStrictEqual(sentB.map(m => m.type), []);

    relay.handleMessage(JSON.stringify({ type: 'auth_ok', contributor_id: 'c1', trust_tier: 'contributor' }), hubs[1]);
    assert.deepStrictEqual(sentB.map(m => m.type), [], 'non-active hub stays silent on its own auth_ok');
  } finally { teardown(relay); }
});

test('auth_failed on the active hub advances polling to an already-authenticated hub', () => {
  const relay = loadRelay({ env: {
    HIVE_HUB: 'wss://hub-a.example/contribute,wss://hub-b.example/contribute',
    HIVE_REGISTRATION_TOKEN: 'tok-a,tok-b',
  } });
  try {
    const hubs = relay.getHubs();
    const sentA = [], sentB = [];
    hubs[0].ws = { readyState: 1, send: p => sentA.push(JSON.parse(p)) };
    hubs[1].ws = { readyState: 1, send: p => sentB.push(JSON.parse(p)) };

    relay.handleMessage(JSON.stringify({ type: 'auth_ok', contributor_id: 'c1', trust_tier: 'contributor' }), hubs[1]);
    assert.deepStrictEqual(sentB.map(m => m.type), [], 'non-active hub waits before the active hub fails');

    relay.handleMessage(JSON.stringify({ type: 'auth_failed', reason: 'bad token' }), hubs[0]);
    assert.deepStrictEqual(sentA.map(m => m.type), []);
    assert.deepStrictEqual(sentB.map(m => m.type), ['ready'], 'polling moved to the authenticated remaining hub');
  } finally { teardown(relay); }
});

test('a task_assign is remembered by hub, and a rejection while busy is routed to the ASKING hub', () => {
  const relay = loadRelay({ env: {
    HIVE_HUB: 'wss://hub-a.example/contribute,wss://hub-b.example/contribute',
    HIVE_REGISTRATION_TOKEN: 'tok-a,tok-b',
  } });
  try {
    const hubs = relay.getHubs();
    const sentA = [], sentB = [];
    hubs[0].ws = { readyState: 1, send: p => sentA.push(JSON.parse(p)) };
    hubs[1].ws = { readyState: 1, send: p => sentB.push(JSON.parse(p)) };

    const origSetTimeout = global.setTimeout;
    global.setTimeout = (fn) => { fn(); return 0; };
    try {
      relay.handleMessage(JSON.stringify({ type: 'task_unavailable', reason: 'no_work' }), hubs[0]);
    } finally {
      global.setTimeout = origSetTimeout;
    }

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
  const relay = loadRelay({ env: {
    HIVE_HUB: 'wss://hub-a.example/contribute,wss://hub-b.example/contribute',
    HIVE_REGISTRATION_TOKEN: 'tok-a,tok-b',
  } });
  try {
    const hubs = relay.getHubs();
    const sentB = [];
    hubs[0].ws = { readyState: 1, send: () => {} };
    hubs[1].ws = { readyState: 1, send: p => sentB.push(JSON.parse(p)) };

    relay.handleMessage(JSON.stringify({ type: 'task_assign', task_id: 't2', kind: 'issue', repo: 'foo/bar', number: 2, title: 'y' }), hubs[1]);

    assert.strictEqual(relay.getCurrentTask(), null);
    assert.ok(sentB.some(m => m.type === 'task_failed' && m.reason === 'Hub is not the active polling slot'),
      'unexpected assignment must be rejected back to the hub that sent it');
  } finally { teardown(relay); }
});

test('token_refresh and task_revoke only affect the hub that owns the active task', () => {
  const relay = loadRelay({ env: {
    HIVE_HUB: 'wss://hub-a.example/contribute,wss://hub-b.example/contribute',
    HIVE_REGISTRATION_TOKEN: 'tok-a,tok-b',
  } });
  try {
    const hubs = relay.getHubs();
    const sentA = [], sentB = [];
    hubs[0].ws = { readyState: 1, send: p => sentA.push(JSON.parse(p)) };
    hubs[1].ws = { readyState: 1, send: p => sentB.push(JSON.parse(p)) };

    relay.handleMessage(JSON.stringify({ type: 'task_assign', task_id: 't1', kind: 'issue', repo: 'foo/bar', number: 1, title: 'x' }), hubs[0]);
    const tokenPath = path.join(relay.__tmpDir, 'gh-token.cache');

    relay.handleMessage(JSON.stringify({ type: 'token_refresh', github_token: 'hub-b-token' }), hubs[1]);
    assert.strictEqual(fs.existsSync(tokenPath), false, 'non-owning hub must not overwrite the active task token');

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
  const relay = loadRelay({ env: {
    HIVE_HUB: 'wss://hub-a.example/contribute,wss://hub-b.example/contribute',
    HIVE_REGISTRATION_TOKEN: 'tok-a,tok-b',
  } });
  try {
    const hubs = relay.getHubs();
    const sentA = [], sentB = [];
    hubs[0].ws = { readyState: 1, send: p => sentA.push(JSON.parse(p)) };
    hubs[1].ws = { readyState: 1, send: p => sentB.push(JSON.parse(p)) };

    relay.handleMessage(JSON.stringify({ type: 'auth_ok', contributor_id: 'c1', trust_tier: 'contributor' }), hubs[0]);
    assert.deepStrictEqual(sentA.map(m => m.type), ['ready']);

    // task_unavailable's retry delay is a raw setTimeout (not the relay's
    // test-mode-aware sleepMs), so run it synchronously here rather than
    // waiting out the real 30s TASK_UNAVAILABLE_RETRY_MS.
    const origSetTimeout = global.setTimeout;
    global.setTimeout = (fn) => { fn(); return 0; };
    try {
      relay.handleMessage(JSON.stringify({ type: 'task_unavailable', reason: 'no_work' }), hubs[0]);
    } finally {
      global.setTimeout = origSetTimeout;
    }
    assert.deepStrictEqual(sentB.map(m => m.type), ['ready'], 'rotation sent ready to hub B, not hub A again');
  } finally { teardown(relay); }
});

test('currentTask stays JSON-serializable after task_assign attaches its owning hub (regression: circular Timeout handles)', () => {
  const relay = loadRelay({ env: {
    HIVE_HUB: 'wss://hub-a.example/contribute,wss://hub-b.example/contribute',
    HIVE_REGISTRATION_TOKEN: 'tok-a,tok-b',
  } });
  try {
    const hubs = relay.getHubs();
    hubs[0].ws = { readyState: 1, send: () => {} };
    hubs[1].ws = { readyState: 1, send: () => {} };
    // Real per-hub state (heartbeatInterval/reconnectTimer are live Timeout
    // objects once connected) is what made JSON.stringify(currentTask) throw
    // "Converting circular structure to JSON" the first time this shipped —
    // a plain unit test with bare {ws} stubs didn't catch it because it never
    // populated these fields. Set them for real here.
    hubs[0].heartbeatInterval = setInterval(() => {}, 999999);
    hubs[1].reconnectTimer = setTimeout(() => {}, 999999);
    try {
      const origSetTimeout = global.setTimeout;
      global.setTimeout = (fn) => { fn(); return 0; };
      try {
        relay.handleMessage(JSON.stringify({ type: 'task_unavailable', reason: 'no_work' }), hubs[0]);
      } finally {
        global.setTimeout = origSetTimeout;
      }
      relay.handleMessage(JSON.stringify({ type: 'task_assign', task_id: 't1', kind: 'issue', repo: 'foo/bar', number: 1, title: 'x' }), hubs[1]);
      assert.ok(relay.getCurrentTask(), 'task was accepted before serializing');
      assert.doesNotThrow(() => JSON.stringify(relay.getCurrentTask()), 'currentTask must serialize even with its _hub set');
    } finally {
      clearInterval(hubs[0].heartbeatInterval);
      clearTimeout(hubs[1].reconnectTimer);
    }
  } finally { teardown(relay); }
});

test('a currentTask with no recorded hub still reaches the hub (regression: synthetic pr-review task)', () => {
  const relay = loadRelay({ env: {
    HIVE_HUB: 'wss://hub-a.example/contribute,wss://hub-b.example/contribute',
    HIVE_REGISTRATION_TOKEN: 'tok-a,tok-b',
  } });
  try {
    const hubs = relay.getHubs();
    const sentA = [];
    hubs[0].ws = { readyState: 1, send: p => sentA.push(JSON.parse(p)) };
    hubs[1].ws = { readyState: 1, send: () => {} };

    // The pr-review task the relay builds for itself after every
    // PR_REVIEW_EVERY_N completions is assembled locally and never carries a
    // _hub. Routing strictly on currentTask._hub sent its frames to
    // `undefined`, so the hub saw the contributor go silent mid-review.
    relay.setCurrentTask({ task_id: 'pr-review-1', kind: 'review', repo: 'foo/bar', number: 0, title: 'Review open PRs' });
    relay.failCurrentTask('done reviewing');

    assert.ok(sentA.length > 0,
      'frames for a hubless currentTask must fall back to the active hub, not be dropped');
    assert.ok(sentA.some(m => m.type === 'task_failed' && m.task_id === 'pr-review-1'));
  } finally { teardown(relay); }
});

// NOTE (v2->v4 sync): v2 added a second copy of CODEX_READY_PANE and repeats of
// the "ready codex pane" / "modal panes still win" tests here. Those constants
// and tests are already defined once in the v4 codex block earlier in this file,
// so the redeclarations were dropped in the sync merge to keep a single source of
// truth; the unique N20 /tmp-cleanup test below is preserved.

// N20 (CWE-20): the /tmp cleanup's second `find` must parenthesize its -o group.
//
// `-type f -user dev -name '*.out' -o -name '*.html' -mmin +60 -exec rm -f`
// parses as (-type f AND -user dev AND -name '*.out') OR (-name '*.html' AND
// -mmin +60 AND -exec rm), because -o binds looser than the implicit -a. The
// right branch drops BOTH -type f and -user dev, so ANY owner's /tmp/*.html
// older than 60 minutes was deleted — root's included, directories included.
// The left branch carries no -exec, so the *.out cleanup never ran at all.
//
// This asserts on the SOURCE rather than a live run: the command is built as a
// template literal, and the bug is in shell-operator precedence, so what matters
// is the exact string handed to the shell.
test('the /tmp cleanup find scopes its -o group (N20: no cross-owner *.html deletion)', () => {
  const src = fs.readFileSync(RELAY_PATH, 'utf8');
  // Match the execSync CODE line, not the comment above it that quotes the
  // vulnerable form for explanation.
  const line = src.split('\n').find((l) => l.includes('execSync(') && l.includes("-name '*.out'"));
  assert.ok(line, 'could not find the /tmp cleanup command');

  // The escaped parens must be present around the -o alternation. In the
  // template literal these are written \\( / \\) so the SHELL receives \( / \).
  // In the source the escapes are written \\( / \\) (two chars: backslash,
  // paren) so the template literal yields \( / \) for the shell. Match that
  // literally via indexOf rather than fighting a third layer of regex escaping.
  assert.ok(
    line.includes("-type f -user dev \\\\( -name '*.out' -o -name '*.html' \\\\) -mmin +60"),
    'the -o group is not parenthesized — any owner\'s /tmp/*.html would be deleted:\n' + line
  );

  // Guard the specific broken form, so a future edit cannot silently reintroduce
  // it by dropping the escapes.
  assert.ok(
    !line.includes("-user dev -name '*.out' -o -name"),
    'the unparenthesized (vulnerable) form is back:\n' + line
  );
});

// ---------------------------------------------------------------------------
// Capability declaration — agent CLI version (kubestellar/hive#2547, DECLARE).
//
// The hub schema, src/docs/contributor-relay.md and the Operations row ("cli
// 1.2.3") all carried agent_cli_version, but the relay never sent it, so the
// column was blank for every connected client. These pin the probe AND — more
// importantly — that failing to probe stays a first-class outcome: an omitted
// field is what every relay written before this change reports, and nothing may
// treat that silence as a defect.
// ---------------------------------------------------------------------------

test('the relay declares the agent CLI version it probed', () => {
  const relay = loadRelay({ backend: 'copilot', cliVersion: '0.0.352\n' });
  try {
    const caps = relay.detectCapabilities();
    assert.strictEqual(caps.agent_cli_version, '0.0.352');

    const [call] = relay.__execFileSyncCalls;
    assert.ok(call, 'no version probe was made');
    assert.deepStrictEqual(call.args, ['--version']);
    // stdin closed so a CLI that mistakes --version for a launch gets EOF
    // instead of waiting on a terminal nobody is at; a timeout so a wedged
    // binary costs seconds rather than the handshake.
    assert.deepStrictEqual(call.opts.stdio, ['ignore', 'pipe', 'ignore']);
    assert.ok(call.opts.timeout > 0 && call.opts.timeout <= 10000,
      `probe timeout should be short and present, got ${call.opts.timeout}`);
  } finally { teardown(relay); }
});

test('the probed version is the binary backends.conf maps the backend to', () => {
  // litellm runs the claude binary. Probing "litellm --version" would report the
  // version of a CLI that never runs the work — usually nothing at all, since no
  // such binary exists — so the probe has to go through the same resolution the
  // launch path uses.
  const relay = loadRelay({ backend: 'litellm', backendBinary: 'claude', cliVersion: '2.0.14 (Claude Code)' });
  try {
    const caps = relay.detectCapabilities();
    const [call] = relay.__execFileSyncCalls;
    assert.strictEqual(call.bin, 'claude',
      'the probe must run the resolved backend binary, not the backend name');
    assert.strictEqual(caps.agent_cli_version, '2.0.14 (Claude Code)');
  } finally { teardown(relay); }
});

test('a CLI that cannot be probed simply declares nothing', () => {
  for (const failure of [null, new Error('ETIMEDOUT'), new Error('Unknown flag: --version')]) {
    const relay = loadRelay({ backend: 'copilot', cliVersion: failure });
    try {
      const caps = relay.detectCapabilities();
      assert.ok(!('agent_cli_version' in caps),
        `a failed probe must omit the field entirely, got ${JSON.stringify(caps)}`);
      // The rest of the declaration still stands — one unprobeable field must
      // not cost the others.
      assert.strictEqual(caps.os, process.platform);
      assert.ok(caps.container_runtime, 'container runtime should still be declared');
    } finally { teardown(relay); }
  }
});

test('a CLI banner is reduced to one short printable line before it is declared', () => {
  const cases = [
    // Real shape: version first, update nudge after. Keep the version.
    ['1.4.2\nA new release is available!\n', '1.4.2'],
    // Leading blank lines and padding.
    ['\n\n   3.1.0   \n', '3.1.0'],
    // Escape sequences from a colourising CLI.
    ['\x1b[32m2.0.1\x1b[0m', '[32m2.0.1 [0m'],
    // Nothing usable reads as no declaration at all.
    ['\n \n', ''],
  ];
  const relay = loadRelay({ backend: 'copilot' });
  try {
    for (const [raw, want] of cases) {
      assert.strictEqual(relay.sanitizeDeclaredValue(raw), want,
        `sanitizeDeclaredValue(${JSON.stringify(raw)})`);
    }
    // Bounded: the hub bounds it again on receipt, but a relay should not be
    // shipping a novel in a display field either.
    const long = relay.sanitizeDeclaredValue('v'.repeat(500));
    assert.ok(long.length <= 64, `declared version not bounded: ${long.length} chars`);
  } finally { teardown(relay); }
});

test('auth_response carries the declared capabilities, and omits nothing else', () => {
  const relay = loadRelay({ backend: 'copilot', model: 'gpt-5.6-luna', cliVersion: '0.0.352' });
  try {
    const hubs = relay.getHubs();
    const sent = [];
    hubs[0].ws = { readyState: 1, send: p => sent.push(JSON.parse(p)) };

    relay.handleMessage(JSON.stringify({ type: 'auth_challenge' }), hubs[0]);

    const auth = sent.find(m => m.type === 'auth_response');
    assert.ok(auth, 'no auth_response was sent');
    assert.strictEqual(auth.cli_backend, 'copilot');
    assert.ok(auth.capabilities, 'auth_response carried no capabilities object');
    assert.strictEqual(auth.capabilities.agent_cli_version, '0.0.352');
    assert.strictEqual(auth.capabilities.os, process.platform);
    assert.strictEqual(auth.capabilities.arch, process.arch);
  } finally { teardown(relay); }
});

test('capabilities are probed once and cached, not re-probed per hub handshake', () => {
  const relay = loadRelay({ backend: 'copilot', cliVersion: '0.0.352' });
  try {
    relay.detectCapabilities();
    relay.detectCapabilities();
    relay.detectCapabilities();
    assert.strictEqual(relay.__execFileSyncCalls.length, 1,
      'the CLI version probe must run once per process, not once per handshake');
  } finally { teardown(relay); }
});

// ---------------------------------------------------------------------------
// Peer-protocol compatibility (kubestellar/hive#2547).
//
// #2567 gave both sides a version to STATE; neither side COMPARED them, so the
// issue's original complaint stood: "the only way to learn that an old relay is
// talking to a new hub is to watch it misbehave." These cover the relay half of
// the detection, and — more importantly — that detecting a mismatch NEVER
// changes what the relay does.
// ---------------------------------------------------------------------------

test('protocol verdicts: peer version is classified against our own', () => {
  const relay = loadRelay();
  try {
    const self = relay.RELAY_PROTOCOL_VERSION;
    const [maj, min] = self.split('.').map(Number);
    const c = (peer) => relay.classifyPeerProtocol(peer, self);

    // An unversioned hub is 'unknown', NEVER a fault: that is what every
    // deployment predating the versioned handshake answers with.
    assert.strictEqual(c(undefined), 'unknown');
    assert.strictEqual(c(''), 'unknown');
    assert.strictEqual(c('   '), 'unknown');

    assert.strictEqual(c(self), 'current');
    assert.strictEqual(c(`${maj}.${min + 1}`), 'newer');
    assert.strictEqual(c(`${maj + 1}.${min}`), 'incompatible');
    if (min > 0) assert.strictEqual(c(`${maj}.${min - 1}`), 'older');

    // Strict MAJOR.MINOR — an unrecognised shape is reported as unparseable
    // rather than coerced into a confident, wrong comparison.
    for (const bad of ['1', '1.2.3', 'v1.2', '1.x', 'nonsense']) {
      assert.strictEqual(c(bad), 'malformed', `${bad} should be malformed`);
    }
  } finally { teardown(relay); }
});

test('protocol drift is reported once per hub and never changes relay behaviour', () => {
  const relay = loadRelay();
  const origWarn = console.warn;
  const warnings = [];
  console.warn = (m) => warnings.push(String(m));
  try {
    const hub = relay.getHubs()[0];
    const self = relay.RELAY_PROTOCOL_VERSION;
    const [maj, min] = self.split('.').map(Number);
    const wsBefore = hub.ws;
    const authBefore = hub.authenticated;

    // Matching and unversioned hubs are SILENT: a healthy connection and an old
    // hub both stay quiet, so the warning means something when it does appear.
    relay.warnOnProtocolDrift(hub, self);
    relay.warnOnProtocolDrift(hub, undefined);
    assert.strictEqual(warnings.length, 0, `expected silence, got: ${warnings.join(' | ')}`);

    // A real mismatch is reported once, names BOTH versions (the verdict alone
    // is ambiguous about which side is behind), and says it is advisory.
    relay.warnOnProtocolDrift(hub, `${maj + 1}.${min}`);
    assert.strictEqual(warnings.length, 1, 'expected exactly one warning');
    assert.ok(/incompatible/.test(warnings[0]), warnings[0]);
    assert.ok(warnings[0].includes(`${maj + 1}.${min}`) && warnings[0].includes(self),
      `warning must name both versions: ${warnings[0]}`);
    assert.ok(/advisory/.test(warnings[0]), `warning must say it is advisory: ${warnings[0]}`);

    // Reconnect loops must not repeat it.
    relay.warnOnProtocolDrift(hub, `${maj + 1}.${min}`);
    assert.strictEqual(warnings.length, 1, 'drift warning repeated on a second call');

    // And nothing about the connection changed: a mismatched peer keeps working
    // exactly as before. #2547 is explicit that compatibility is carried by the
    // defaults, because there is no negotiation to carry it.
    assert.strictEqual(hub.authFailed, false, 'a protocol mismatch must not fail auth');
    assert.strictEqual(hub.authenticated, authBefore, 'a protocol mismatch must not change auth state');
    assert.strictEqual(hub.ws, wsBefore, 'a protocol mismatch must not drop the socket');
  } finally {
    console.warn = origWarn;
    teardown(relay);
  }
});

test('the relay declares the same protocol version the hub speaks', () => {
  // The hub and this relay ship from the same tree. That was previously only a
  // comment, and it drifted (#2600 shipped both at 1.1; #2671 bumped the hub to
  // 1.2 and left the relay at 1.1). Pinned from both sides — the Go half is
  // TestRelayProtocolVersionMatchesHub.
  const goSrc = fs.readFileSync(
    path.join(__dirname, '..', 'src', 'pkg', 'dashboard', 'contribute_protocol.go'), 'utf8');
  const m = /const contributorProtocolVersion = "([^"]*)"/.exec(goSrc);
  assert.ok(m, 'could not find contributorProtocolVersion in contribute_protocol.go');
  const relay = loadRelay();
  try {
    assert.strictEqual(relay.RELAY_PROTOCOL_VERSION, m[1],
      'bin/contributor-relay.sh and the hub must declare the same contributor-protocol version; ' +
      'bump both in the same PR');
  } finally { teardown(relay); }
});


// ---------------------------------------------------------------------------
// kubestellar/hive#1861 / #3842 (audit N14) — the relay must never target the
// hub's full-privilege installation-token cache, and a failed token write must
// degrade, not crash the relay mid-assignment.
// ---------------------------------------------------------------------------

test('default token cache path is never the hub shared gh-app-token.cache', () => {
  // Empty override forces the relay's built-in default (|| is falsy on '').
  const relay = loadRelay({ env: { HIVE_GH_TOKEN_CACHE: '' } });
  try {
    assert.ok(relay.GH_TOKEN_CACHE, 'expected GH_TOKEN_CACHE exported in test mode');
    assert.notStrictEqual(path.basename(relay.GH_TOKEN_CACHE), 'gh-app-token.cache',
      'the relay defaulted its token write path to the hub\'s full-privilege ' +
      `installation-token cache: ${relay.GH_TOKEN_CACHE} — a relay on a hive ` +
      'host would clobber it (as root) or crash on EACCES (as anyone else)');
  } finally { teardown(relay); }
});

test('task_assign with an unwritable token cache path does not crash the relay', () => {
  // Parent "directory" is a regular file, so mkdirSync and writeFileSync both
  // fail no matter what uid the test runs as.
  const scratchRoot = path.join(__dirname, '..', '.relay-test-tmp');
  fs.mkdirSync(scratchRoot, { recursive: true });
  const tmp = fs.mkdtempSync(path.join(scratchRoot, 'relay-badcache-'));
  const fileAsDir = path.join(tmp, 'blocker');
  fs.writeFileSync(fileAsDir, 'not a directory');
  const relay = loadRelay({ env: { HIVE_GH_TOKEN_CACHE: path.join(fileAsDir, 'token.cache') } });
  try {
    relay.setCliReady(true);
    relay.handleMessage(JSON.stringify({
      type: 'task_assign',
      task_id: 'tok-1',
      kind: 'issue',
      repo: 'foo/bar',
      number: 7,
      title: 'token write failure must degrade',
      prompt: 'do the thing',
      github_token: 'scoped-task-token',
    }));
    const accepted = relay.__sent.find(m => m.type === 'task_accepted');
    assert.ok(accepted,
      'task_assign must survive an unwritable token cache and still accept the task');
  } finally {
    teardown(relay);
    try { fs.rmSync(tmp, { recursive: true, force: true }); } catch (_) {}
  }
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
