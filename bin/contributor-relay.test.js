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

function loadRelay({ backend = 'copilot', backendBinary = null, backendPerm = '--allow-all', model = '', reasoningEffort = '', cliStates = ['ready'], procAlive = true, mode = 'interactive', execFileResult = null, statusFile = null, paneText = null, env = null, cliVersion = null } = {}) {
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
    if (/backend_perm_flag/.test(cmd)) return `${backendPerm}\n`;
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

// Live agy/Gemini pane after opening a PR. Newer builds no longer print
// "? for shortcuts" at rest: their idle chrome is a bare input prompt followed
// by the selected-model footer. The chrome below (both rules, the blank rows,
// and the right-aligned footer) is reproduced from a real
// `tmux capture-pane -p` of a finished turn; the PR line stays synthetic
// because a sibling test asserts URL extraction against it.
//
// The earlier version of this fixture omitted the rule that CLOSES the input
// box, and the footer padding, so it matched a regex that the real pane did
// not. That is how the wedge below shipped green.
const AGY_GEMINI_IDLE_PANE = [
  '● Bash(gh pr create --repo kubestellar/hive ...)',
  ...Array.from({ length: 20 }, (_, i) => `  completed test step ${i}`),
  '',
  '  • Opened https://github.com/foo/bar/pull/9 targeting v4.',
  '────────────────────────────────────────────',
  '>',
  '',
  '',
  '────────────────────────────────────────────',
  '                                        Gemini 3.7 Flash · high',
].join('\n');

// The same pane while the turn is still IN FLIGHT. agy replaces the idle hint
// with "esc to cancel" on the footer line, so this must NOT read as idle: a
// busy agent reported complete is the worse direction of this bug.
const AGY_GEMINI_WORKING_PANE = [
  '● Read(/home/dev/workspace/kubestellar/hive/.github/workflows/prune-ghcr.yml)',
  '● Edit(/home/dev/workspace/kubestellar/hive/.github/workflows/prune-ghcr.yml) (ctrl+o to expand)',
  '⣷  Editing files...',
  '└ Tip: Use /diff to view uncommitted changes in your workspace.',
  '────────────────────────────────────────────',
  '>',
  '',
  '',
  '────────────────────────────────────────────',
  'esc to cancel                           Gemini 3.7 Flash · high',
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
test('a relaunch cds somewhere resolvable before starting the CLI', () => {
  const relay = loadRelay({ backend: 'agy' });
  try {
    const launched = relay.launchCommandWithCwd('agy --dangerously-skip-permissions');
    assert.match(launched, /^cd .+ && agy --dangerously-skip-permissions$/,
      `relaunch must pin the cwd, got: ${launched}`);
    // With no HIVE_AGENT_CWD exported (an older entrypoint), the relay's own
    // cwd remains the fallback, so this keeps its previous behaviour.
    assert.ok(launched.includes(process.cwd()),
      `without HIVE_AGENT_CWD the cd target must be the relay's cwd: ${launched}`);
  } finally { teardown(relay); }
});

// In local mode the relay's own cwd IS the hive checkout `just contribute-hive`
// was run from — which is also a clone of the repo the agent is assigned to
// work on. Relaunching there puts the agent back in the one tree it must not
// adopt as its checkout, undoing the launch-side fix on the first stall
// recovery. Both entrypoints export HIVE_AGENT_CWD ($HOME) for this.
// The agent runs unattended with its backend's skip-permissions flag, so its
// cwd is where every relative `ls`, `grep -r` and relative write lands. On the
// host, $HOME holds .ssh, .gnupg and the contributor's own registration token
// (.config/hive/contributor.env). cwd is not a security boundary — the process
// runs as the user regardless — but the default blast radius should not be the
// user's home. Both entrypoints export a dedicated empty directory instead.
test('a relaunch does not root the agent at $HOME', () => {
  const home = process.env.HOME || '/home/nobody';
  const relay = loadRelay({
    backend: 'agy',
    env: { HIVE_AGENT_CWD: `${home}/.local/state/hive/agent-cwd` },
  });
  try {
    const launched = relay.launchCommandWithCwd('agy --dangerously-skip-permissions');
    const target = launched.replace(/^cd '?/, '').replace(/'? && .*$/, '');
    assert.notStrictEqual(target, home,
      `the agent must not be rooted at $HOME: ${launched}`);
    assert.ok(target.startsWith(home + '/'),
      `expected a directory beneath $HOME, got: ${target}`);
  } finally { teardown(relay); }
});

test('a relaunch prefers HIVE_AGENT_CWD over the relay cwd', () => {
  const relay = loadRelay({ backend: 'agy', env: { HIVE_AGENT_CWD: '/home/agent' } });
  try {
    const launched = relay.launchCommandWithCwd('agy --dangerously-skip-permissions');
    assert.match(launched, /^cd '?\/home\/agent'? && agy --dangerously-skip-permissions$/,
      `relaunch must cd into HIVE_AGENT_CWD, got: ${launched}`);
    assert.ok(!launched.includes(`cd ${process.cwd()} `),
      `relaunch must not land back in the relay's own checkout: ${launched}`);
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

// Regression for the wedge that shipped past the fixture above: the input box
// is closed by a second rule, so the gap between ">" and the footer is not pure
// whitespace. Live, this classified WORKING after the turn opened
// kubestellar/hive#4127, and the stall backstop failed the task 20 minutes
// later as `environment` — a shipped PR recorded as a failure.
test('agy idle pane with a closing rule under the input box is COMPLETE', () => {
  const relay = loadRelay({ backend: 'agy' });
  try {
    assert.strictEqual(
      relay.classifyTmuxPane(AGY_GEMINI_IDLE_PANE), relay.PANE_STATE_IDLE_COMPLETE,
      'the rule closing agy\'s input box must not hide the model footer');
  } finally { teardown(relay); }
});

// The opposite direction, and the one that must never regress: an in-flight
// turn renders "esc to cancel" on the footer line. Reporting THAT complete
// would hand the hub a half-done task and abandon real work.
// A FINISHED agy turn whose completion summary happens to contain one of the
// activity verbs. Reproduced from the pane of a task that opened
// kubestellar/hive#4181: the summary line says "...with writing
// HIVE_GITHUB_TOKEN to a local .env file". The verb is in the last 15 lines by
// construction, because it is the summary, so a bare word match reads a done
// turn as busy — and isWorking short-circuits before hasIdlePrompt is
// consulted, so the idle chrome below never gets a vote.
const AGY_DONE_SUMMARY_WITH_VERB = [
  '  I have completed work on issue kubestellar/hive#4179 and submitted pull request kubestellar/hive#4181.',
  '  ### Key Updates',
  '  • Docker Compose Quick Start: Updated commands across README.md and get-started.html.',
  '  • Environment file (.env): Replaced inline token export instructions with writing',
  '    HIVE_GITHUB_TOKEN to a local .env file, and added .env patterns to .gitignore.',
  '────────────────────────────────────────────',
  '>',
  '',
  '',
  '────────────────────────────────────────────',
  '                                        Gemini 3.7 Flash · high',
].join('\n');

// Neither marker present — an agy build whose chrome we do not recognise. The
// verb fallback must still apply here, so an unknown UI errs toward "busy"
// rather than reporting a working agent complete.
const AGY_UNKNOWN_CHROME_WORKING = [
  '● Edit(/home/dev/workspace/owner/repo/main.go)',
  '⣷  Editing files...',
  '  some unrecognised footer',
].join('\n');

test('a finished agy turn is COMPLETE even when its summary contains an activity verb', () => {
  const relay = loadRelay({ backend: 'agy' });
  try {
    assert.strictEqual(
      relay.classifyTmuxPane(AGY_DONE_SUMMARY_WITH_VERB), relay.PANE_STATE_IDLE_COMPLETE,
      'prose in a completion summary must not pin a finished pane to WORKING');
  } finally { teardown(relay); }
});

test('agy with no recognisable chrome still falls back to the verb heuristic', () => {
  const relay = loadRelay({ backend: 'agy' });
  try {
    assert.strictEqual(
      relay.classifyTmuxPane(AGY_UNKNOWN_CHROME_WORKING), relay.PANE_STATE_WORKING,
      'an unknown agy UI must err toward busy, never toward complete');
  } finally { teardown(relay); }
});

test('agy pane still working ("esc to cancel") is not COMPLETE', () => {
  const relay = loadRelay({ backend: 'agy' });
  try {
    assert.notStrictEqual(
      relay.classifyTmuxPane(AGY_GEMINI_WORKING_PANE), relay.PANE_STATE_IDLE_COMPLETE,
      'an in-flight agy turn must never be reported as a finished one');
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

test('a confirmed stall QUITS the live CLI before relaunching, so the launch command is never typed into it as a prompt', () => {
  // Reaching the stall path PROVES the CLI is alive: the `presence.isShell`
  // guard earlier in progressTick() returns before the completion check
  // whenever the pane has fallen back to a shell, so a confirmed stall is by
  // construction a pane still running the agent CLI.
  //
  // relaunchCLI() alone is calibrated for the opposite case. The single C-c in
  // recoverWedgedShell() clears a wedged bash PS2 prompt, but against a LIVE
  // claude/codex/agy it only cancels the current turn — the CLI stays up, and
  // the launch command that follows is delivered to it as a chat message. That
  // is the #2203 wedge with a shell command as the payload, so the quit has to
  // happen first and has to be the two-C-c sequence the memory-cleanup restart
  // path has used since #2596.
  const relay = loadRelay({
    backend: 'agy',
    paneText: 'a frozen pane with no idle prompt and nothing happening',
  });
  try {
    relay.setCliReady(true);
    assignTask(relay, 't-stall-quit');
    relay.__stallTick();
    relay.__agePaneStallClock(relay.PANE_STALL_TIMEOUT_MS + 1);
    relay.__stallTick();   // confirmation 1
    relay.__agePaneStallClock(relay.PANE_STALL_TIMEOUT_MS + 1);
    const before = relay.__tmuxSends().length;
    relay.__stallTick();   // confirmation 2 -> quit + relaunch + fail
    const sends = relay.__tmuxSends().slice(before);

    const launchIdx = sends.findIndex(c => /agy/.test(c));
    assert.ok(launchIdx >= 0, `expected the CLI to be relaunched: ${JSON.stringify(sends)}`);

    // At least the two quitLiveCLI() Ctrl-Cs must precede the launch command.
    // One is NOT enough and is the whole point of this test.
    const ctrlCsBeforeLaunch = sends.slice(0, launchIdx).filter(c => /C-c\s*$/.test(c)).length;
    assert.ok(ctrlCsBeforeLaunch >= 2,
      `a live CLI needs two Ctrl-Cs to exit before the relaunch is typed; saw ${ctrlCsBeforeLaunch} in ${JSON.stringify(sends)}`);

    // And the fix must not have cost the behaviour #4064 added.
    const failed = relay.__sent.filter(m => m.type === 'task_failed');
    assert.strictEqual(failed.length, 1, 'the task is still handed back after the quit+relaunch');
    assert.strictEqual(failed[0].failure_kind, 'environment');
  } finally { teardown(relay); }
});

test('quitLiveCLI sends two Ctrl-Cs and nothing else — a single one only cancels a turn', () => {
  const relay = loadRelay({ backend: 'agy' });
  try {
    const before = relay.__tmuxSends().length;
    relay.quitLiveCLI();
    const sends = relay.__tmuxSends().slice(before);
    assert.strictEqual(sends.length, 2, `expected exactly two sends, got ${JSON.stringify(sends)}`);
    assert.ok(sends.every(c => /C-c\s*$/.test(c)), `both must be Ctrl-C: ${JSON.stringify(sends)}`);
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

test('goose is also excluded from --model', () => {
  const relay = loadRelay({ backend: 'goose', model: 'some-model' });
  try {
    assert.ok(!/--model/.test(relay.buildLaunchCommand()), 'goose should not get --model');
  } finally { teardown(relay); }
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

test('auth_response reports NO effort for agy without a model — agy is given no --effort flag there', () => {
  // agy only receives --effort when it also receives --model (buildLaunchCommand
  // pairs them deliberately). Reporting a raw AGENT_REASONING_EFFORT in that case
  // would advertise to the dashboard an effort agy never actually applied.
  const relay = loadRelay({ env: { AGENT_BACKEND: 'agy', AGENT_REASONING_EFFORT: 'high' } });
  try {
    relay.handleMessage(JSON.stringify({ type: 'auth_challenge', seq: 1, nonce: 'n' }));
    const auth = relay.__sent.find(m => m.type === 'auth_response');
    assert.ok(auth, 'expected auth_response');
    assert.strictEqual(auth.reasoning_effort, undefined,
      'no --effort reaches agy without a model, so nothing should be reported');
  } finally { teardown(relay); }
});

test('the reported effort and the launched --effort come from ONE resolver', () => {
  // The effort now travels twice (command line + auth_response). These must not
  // be derived independently, or they drift -- the same failure the launch
  // command itself had in #2203 bug 1.
  const relay = loadRelay({ env: { AGENT_BACKEND: 'agy', AGENT_MODEL: 'gemini-3.7-flash', AGENT_REASONING_EFFORT: 'medium' } });
  try {
    const resolved = relay.effectiveReasoningEffort();
    assert.strictEqual(resolved, 'medium');
    relay.handleMessage(JSON.stringify({ type: 'auth_challenge', seq: 1, nonce: 'n' }));
    const auth = relay.__sent.find(m => m.type === 'auth_response');
    assert.strictEqual(auth.reasoning_effort, resolved, 'auth_response must report the resolved effort');
    assert.match(relay.buildLaunchCommand(), new RegExp('--effort ' + resolved),
      'the launch command must carry the SAME resolved effort');
  } finally { teardown(relay); }
});

test('an effort agy rejects falls back to the default, and that fallback is what gets reported', () => {
  // codex accepts "minimal"; agy does not, and agy silently drops --model when
  // paired with an effort it rejects. AGY_DEFAULT_EFFORT is what actually runs,
  // so that is what the dashboard must be told.
  const relay = loadRelay({ env: { AGENT_BACKEND: 'agy', AGENT_MODEL: 'gemini-3.7-flash', AGENT_REASONING_EFFORT: 'minimal' } });
  try {
    assert.strictEqual(relay.effectiveReasoningEffort(), 'low');
    relay.handleMessage(JSON.stringify({ type: 'auth_challenge', seq: 1, nonce: 'n' }));
    const auth = relay.__sent.find(m => m.type === 'auth_response');
    assert.strictEqual(auth.reasoning_effort, 'low',
      'reporting the raw env var here would claim an effort agy rejected');
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

test('a Codex auto-review denial is reported with its actionable diagnostic', () => {
  const err = new Error('codex failed'); err.code = 1;
  const relay = loadRelay({
    backend: 'codex',
    mode: 'headless',
    execFileResult: { err, stderr: 'approval automatic review timed out\n' },
  });
  try {
    assignHeadlessTask(relay);
    const failure = relay.__sent.find(m => m.type === 'task_failed');
    assert.ok(failure, 'an automatic-review failure must terminate the task');
    assert.match(failure.reason, /code 1.*automatic review timed out/i,
      'the redacted CLI diagnostic must reach Hive instead of an opaque exit code');
    assert.strictEqual(relay.__readHeadlessStatus().reason, failure.reason,
      'the probe status and hub failure must expose the same terminal reason');
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

test('Codex headless keeps auto-review bounded to the declared task workspace', () => {
  const workspace = '/tmp/hive-contributor-workspace';
  const perm = `--ask-for-approval on-request --sandbox workspace-write -c approvals_reviewer=auto_review --add-dir ${workspace}`;
  const relay = loadRelay({
    backend: 'codex',
    backendPerm: perm,
    mode: 'headless',
    env: { HIVE_WORKSPACE_DIR: workspace },
  });
  try {
    const built = relay.buildHeadlessArgv('ship the change');
    assert.deepStrictEqual(built.args.slice(0, 8), [
      '--ask-for-approval', 'on-request',
      '--sandbox', 'workspace-write',
      '-c', 'approvals_reviewer=auto_review',
      '--add-dir', workspace,
    ], `Codex unattended permission argv drifted: ${JSON.stringify(built.args)}`);
    assignHeadlessTask(relay);
    const call = relay.__execFileCalls[0];
    assert.strictEqual(call.opts.cwd, workspace,
      'the one-shot process cwd and its additional writable root must be the same task workspace');
    assert.ok(!built.args.includes('--dangerously-bypass-approvals-and-sandbox'),
      'automatic review must not disable the sandbox');
  } finally { teardown(relay); }
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

// A codex turn that finished and shipped a PR, reproduced from the pane of the
// task that opened kubestellar/hive#4259. Note what it does NOT contain:
// "completed", "done" or "finished". codex writes its summary in whatever words
// the work calls for, so gating completion on a prose word means a finished task
// is invisible whenever it reaches for different ones — the mirror of #4182,
// where agy's prose made a finished pane look busy.
const CODEX_SHIPPED_PR_IDLE_PANE = [
  '\u2022 Opened ready-for-review PR #4259 (https://github.com/kubestellar/hive/pull/4259).',
  '  - Conclusion: direct .kube reuse is not viable; native Quadlet units are recommended.',
  '  - Added the measured compatibility report and documentation index link.',
  '  - Commit c8ae4ddf includes a matching Signed-off-by trailer.',
  '  - Branch is pushed and clean.',
  '\u2500 Worked for 6m 22s \u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500',
  '\u203a Ask Codex to do anything',
  '  gpt-5.6-sol xhigh \u00b7 /home/dev/.local/state/hive/agent-cwd',
].join('\n');

test('a finished codex turn is COMPLETE even when its summary avoids completion words', () => {
  const relay = loadRelay({ backend: 'codex', paneText: CODEX_SHIPPED_PR_IDLE_PANE });
  try {
    assert.strictEqual(
      relay.classifyTmuxPane(CODEX_SHIPPED_PR_IDLE_PANE), relay.PANE_STATE_IDLE_COMPLETE,
      'a shipped-PR summary must not have to say "done" to count as finished');
  } finally { teardown(relay); }
});

// A FINISHED codex turn whose summary contains an activity verb. Same shape as
// the agy case #4182 fixed: the verb is in the summary the agent prints when it
// is DONE, so a bare word match reads a finished pane as busy. Captured markers
// show "esc to interrupt" is the only thing that distinguishes the two states —
// the "› Ask Codex to do anything" line is drawn while working too.
const CODEX_DONE_SUMMARY_WITH_VERB = [
  '\u2022 I finished the spike and pushed the branch.',
  '  - While running the contract tests I confirmed the selector is deterministic.',
  '  - Opened PR #4259 and verified DCO passes.',
  '\u2500 Worked for 6m 22s \u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500',
  '\u203a Ask Codex to do anything',
  '  gpt-5.6-sol xhigh \u00b7 /home/dev/.local/state/hive/agent-cwd',
].join('\n');

// The same pane mid-turn. codex renders its status row, which is the ONLY
// signal separating this from the pane above.
const CODEX_WORKING_STATUS_ROW = [
  '\u2022 Ran git status --short --branch',
  '\u2022 Working (46s \u2022 esc to interrupt)',
  '\u203a Ask Codex to do anything',
  '  gpt-5.6-sol xhigh \u00b7 /home/dev/.local/state/hive/agent-cwd',
].join('\n');

test('a finished codex turn is COMPLETE even when its summary contains an activity verb', () => {
  const relay = loadRelay({ backend: 'codex', paneText: CODEX_DONE_SUMMARY_WITH_VERB });
  try {
    assert.strictEqual(
      relay.classifyTmuxPane(CODEX_DONE_SUMMARY_WITH_VERB), relay.PANE_STATE_IDLE_COMPLETE,
      'prose in a completion summary must not pin a finished codex pane to WORKING');
  } finally { teardown(relay); }
});

test('codex mid-turn status row still reads as WORKING', () => {
  const relay = loadRelay({ backend: 'codex', paneText: CODEX_WORKING_STATUS_ROW });
  try {
    assert.strictEqual(
      relay.classifyTmuxPane(CODEX_WORKING_STATUS_ROW), relay.PANE_STATE_WORKING,
      'an in-flight codex turn must never be reported as finished');
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
// #4117 — auto-detect the running model from the CLI's own session transcript
// when AGENT_MODEL is unset. Precedence: AGENT_MODEL → detected → ''.
// ---------------------------------------------------------------------------

// Builds a claude-style transcript fixture: ~/.claude/projects/<hash>/x.jsonl
// with assistant turns recording message.model. Returns the projects dir and
// the session file path (so tests can append a later turn = /model switch).
function makeClaudeFixture(turns) {
  const scratchRoot = path.join(__dirname, '..', '.relay-test-tmp');
  fs.mkdirSync(scratchRoot, { recursive: true });
  const root = fs.mkdtempSync(path.join(scratchRoot, 'model-detect-'));
  const projDir = path.join(root, 'projects', '-home-dev-work');
  fs.mkdirSync(projDir, { recursive: true });
  const file = path.join(projDir, 'session-abc.jsonl');
  fs.writeFileSync(file, turns.map(t => JSON.stringify(t)).join('\n') + '\n');
  return { root, projectsDir: path.join(root, 'projects'), file };
}

function assistantTurn(model) {
  return { type: 'assistant', timestamp: new Date().toISOString(), message: { model, usage: { input_tokens: 1, output_tokens: 1 } } };
}

test('#4117: claude model is detected from the newest transcript when AGENT_MODEL is unset', () => {
  const fx = makeClaudeFixture([assistantTurn('claude-opus-5-20260101')]);
  const relay = loadRelay({ backend: 'claude', model: '', env: { HIVE_CLAUDE_PROJECTS_DIR: fx.projectsDir } });
  try {
    assert.strictEqual(relay.refreshDetectedModel(), 'claude-opus-5-20260101');
    assert.strictEqual(relay.effectiveModel(), 'claude-opus-5-20260101');
  } finally {
    teardown(relay);
    fs.rmSync(fx.root, { recursive: true, force: true });
  }
});

test('#4117: explicit AGENT_MODEL wins over a transcript recording a different model', () => {
  const fx = makeClaudeFixture([assistantTurn('claude-transcript-model')]);
  const relay = loadRelay({ backend: 'claude', model: 'my-explicit-model', env: { HIVE_CLAUDE_PROJECTS_DIR: fx.projectsDir } });
  try {
    assert.strictEqual(relay.refreshDetectedModel(), 'my-explicit-model');
    assert.strictEqual(relay.detectRunningModel(), '',
      'detection must not even run when AGENT_MODEL is set — explicit intent wins');
    // auth_response carries the explicit value, unconditionally.
    relay.handleMessage(JSON.stringify({ type: 'auth_challenge' }));
    const auth = relay.__sent.find(m => m.type === 'auth_response');
    assert.strictEqual(auth.model, 'my-explicit-model');
  } finally {
    teardown(relay);
    fs.rmSync(fx.root, { recursive: true, force: true });
  }
});

test('#4117: auth_response reports the detected model when AGENT_MODEL is unset', () => {
  const fx = makeClaudeFixture([assistantTurn('claude-sonnet-5-20260101')]);
  const relay = loadRelay({ backend: 'claude', model: '', env: { HIVE_CLAUDE_PROJECTS_DIR: fx.projectsDir } });
  try {
    relay.handleMessage(JSON.stringify({ type: 'auth_challenge' }));
    const auth = relay.__sent.find(m => m.type === 'auth_response');
    assert.strictEqual(auth.model, 'claude-sonnet-5-20260101');
  } finally {
    teardown(relay);
    fs.rmSync(fx.root, { recursive: true, force: true });
  }
});

test('#4117: a mid-session model switch is picked up by the progress tick and sent on task_progress', () => {
  const fx = makeClaudeFixture([assistantTurn('claude-sonnet-5-20260101')]);
  const relay = loadRelay({ backend: 'claude', model: '', cliStates: ['working'], env: { HIVE_CLAUDE_PROJECTS_DIR: fx.projectsDir } });
  try {
    assert.strictEqual(relay.refreshDetectedModel(), 'claude-sonnet-5-20260101');
    // The session switches models (`/model`): the CLI appends a turn served by
    // the NEW model to its own transcript.
    fs.appendFileSync(fx.file, JSON.stringify(assistantTurn('claude-opus-5-20260101')) + '\n');
    relay.setCurrentTask({ task_id: 'mt-1', task_gen: 3, kind: 'issue', repo: 'foo/bar', number: 1, title: 'x' });
    relay.__stallTick(); // one progress tick, grace period elapsed
    const prog = relay.__sent.filter(m => m.type === 'task_progress').pop();
    assert.ok(prog, 'the tick must send a task_progress');
    assert.strictEqual(prog.model, 'claude-opus-5-20260101',
      'task_progress must carry the model detected AFTER the mid-session switch');
    assert.strictEqual(relay.effectiveModel(), 'claude-opus-5-20260101');
  } finally {
    teardown(relay);
    fs.rmSync(fx.root, { recursive: true, force: true });
  }
});

test('#4117: synthetic placeholder turns are skipped in favor of the last real model', () => {
  const fx = makeClaudeFixture([assistantTurn('claude-opus-5-20260101'), assistantTurn('<synthetic>')]);
  const relay = loadRelay({ backend: 'claude', model: '', env: { HIVE_CLAUDE_PROJECTS_DIR: fx.projectsDir } });
  try {
    assert.strictEqual(relay.refreshDetectedModel(), 'claude-opus-5-20260101');
  } finally {
    teardown(relay);
    fs.rmSync(fx.root, { recursive: true, force: true });
  }
});

test('#4117: copilot model is detected from the newest events.jsonl', () => {
  const scratchRoot = path.join(__dirname, '..', '.relay-test-tmp');
  fs.mkdirSync(scratchRoot, { recursive: true });
  const root = fs.mkdtempSync(path.join(scratchRoot, 'model-detect-'));
  const sessDir = path.join(root, 'session-state', 'sess-1');
  fs.mkdirSync(sessDir, { recursive: true });
  fs.writeFileSync(path.join(sessDir, 'events.jsonl'), [
    JSON.stringify({ type: 'session.start', data: { sessionId: 'sess-1', selectedModel: 'gpt-5.4' } }),
    JSON.stringify({ type: 'tool.complete', data: { model: 'gpt-5.6-luna' } }),
  ].join('\n') + '\n');
  const relay = loadRelay({ backend: 'copilot', model: '', env: { HIVE_COPILOT_SESSIONS_DIR: path.join(root, 'session-state') } });
  try {
    assert.strictEqual(relay.refreshDetectedModel(), 'gpt-5.6-luna',
      'the LATEST model-bearing event must win, not the session.start value');
  } finally {
    teardown(relay);
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('#4117: bob model is detected from the last message of the newest chat recording', () => {
  const scratchRoot = path.join(__dirname, '..', '.relay-test-tmp');
  fs.mkdirSync(scratchRoot, { recursive: true });
  const root = fs.mkdtempSync(path.join(scratchRoot, 'model-detect-'));
  const chats = path.join(root, 'tmp', 'uuid-1', 'chats');
  fs.mkdirSync(chats, { recursive: true });
  fs.writeFileSync(path.join(chats, 'sess.json'), JSON.stringify({
    sessionId: 'sess',
    messages: [
      { type: 'user', content: 'hi' },
      { type: 'bob-shell', content: 'x', model: 'standard' },
      { type: 'bob-shell', content: 'y', model: 'premium' },
    ],
  }));
  const relay = loadRelay({ backend: 'bob', model: '', env: { HIVE_BOB_DIR: root } });
  try {
    assert.strictEqual(relay.refreshDetectedModel(), 'premium');
  } finally {
    teardown(relay);
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test('#4117: unsupported backends detect nothing and degrade to today\'s behavior', () => {
  // A claude transcript exists on disk, but codex has no scanner — detection
  // must not guess from another CLI's files.
  const fx = makeClaudeFixture([assistantTurn('claude-opus-5-20260101')]);
  for (const backend of ['codex', 'agy', 'goose', 'pi', 'aider', 'litellm']) {
    const relay = loadRelay({ backend, model: '', env: { HIVE_CLAUDE_PROJECTS_DIR: fx.projectsDir } });
    try {
      assert.strictEqual(relay.refreshDetectedModel(), '', `${backend} must not detect a model`);
      assert.deepStrictEqual(Object.keys(relay.progressModelFields()).filter(k => k === 'model'), [],
        `${backend} must not piggyback a model on task_progress`);
    } finally {
      teardown(relay);
    }
  }
  fs.rmSync(fx.root, { recursive: true, force: true });
});

test('#4117: detection failure (missing log root) is silent and reports no model', () => {
  const relay = loadRelay({ backend: 'claude', model: '', env: { HIVE_CLAUDE_PROJECTS_DIR: path.join(__dirname, 'does-not-exist-4117') } });
  try {
    assert.strictEqual(relay.refreshDetectedModel(), '');
    relay.handleMessage(JSON.stringify({ type: 'auth_challenge' }));
    const auth = relay.__sent.find(m => m.type === 'auth_response');
    assert.strictEqual(auth.model || '', '', 'auth_response must degrade to no model, exactly as before');
  } finally {
    teardown(relay);
  }
});

// ---------------------------------------------------------------------------
// kubestellar/hive#4267 — unit coverage for previously untested functions.
// ---------------------------------------------------------------------------

// A 36-char token body, the canonical length GitHub mints today.
const TOKEN_BODY = 'A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8';

test('#4267 redactTokens scrubs every GitHub token prefix', () => {
  const relay = loadRelay({});
  try {
    for (const prefix of ['gho_', 'ghp_', 'ghs_', 'ghu_', 'ghr_']) {
      const out = relay.redactTokens(`token=${prefix}${TOKEN_BODY} end`);
      assert.strictEqual(out, `token=${prefix}***REDACTED*** end`,
        `${prefix} token must be redacted, got: ${out}`);
    }
  } finally { teardown(relay); }
});

test('#4267 redactTokens scrubs tokens embedded in JSON and URLs', () => {
  const relay = loadRelay({});
  try {
    const json = `{"auth":"gho_${TOKEN_BODY}","other":1}`;
    assert.strictEqual(relay.redactTokens(json), `{"auth":"gho_***REDACTED***","other":1}`);
    const url = `https://x-access-token:ghs_${TOKEN_BODY}@github.com/o/r.git`;
    assert.strictEqual(relay.redactTokens(url), 'https://x-access-token:ghs_***REDACTED***@github.com/o/r.git');
  } finally { teardown(relay); }
});

test('#4267 redactTokens scrubs multiple tokens in one string', () => {
  const relay = loadRelay({});
  try {
    const out = relay.redactTokens(`a gho_${TOKEN_BODY} b ghp_${TOKEN_BODY} c gho_${TOKEN_BODY}`);
    assert.ok(!out.includes(TOKEN_BODY), `a token body survived: ${out}`);
    assert.strictEqual((out.match(/\*\*\*REDACTED\*\*\*/g) || []).length, 3);
  } finally { teardown(relay); }
});

test('#4267 redactTokens leaves token-free text untouched', () => {
  const relay = loadRelay({});
  try {
    for (const s of ['plain output', 'ghost_stories are fine', 'ghp_short', 'git push origin main', '']) {
      assert.strictEqual(relay.redactTokens(s), s, `must pass through unchanged: ${s}`);
    }
  } finally { teardown(relay); }
});

test('#4267 redactTokens scrubs the WHOLE body of a longer-than-36-char token', () => {
  // GitHub documents that token length may change. With an exact {36} bound a
  // 40-char token had its last 4 characters leaked after the REDACTED marker;
  // the bound is now {36,} so the entire alphanumeric run is scrubbed.
  const relay = loadRelay({});
  try {
    const long = TOKEN_BODY + 'Zz19';
    const out = relay.redactTokens(`log: gho_${long}.`);
    assert.strictEqual(out, 'log: gho_***REDACTED***.',
      `tail of a long token leaked: ${out}`);
  } finally { teardown(relay); }
});

test('#4267 redactTokens redacts a token even when glued to a preceding word', () => {
  const relay = loadRelay({});
  try {
    const out = relay.redactTokens(`x=Xgho_${TOKEN_BODY}`);
    assert.ok(!out.includes(TOKEN_BODY), `boundary-glued token leaked: ${out}`);
  } finally { teardown(relay); }
});

// --- paneLooksBlockedOnHuman -------------------------------------------------

test('#4267 blocked-on-human: trailing question mark on the last content line', () => {
  const relay = loadRelay({});
  try {
    assert.strictEqual(relay.paneLooksBlockedOnHuman(
      'I inspected the config.\nShould I also update the staging manifest?\n> '), true);
  } finally { teardown(relay); }
});

test('#4267 blocked-on-human: numbered menu with a choose keyword', () => {
  const relay = loadRelay({});
  try {
    const pane = [
      'Which option should I choose?',
      '1. Use the REST client',
      '2. Use the gRPC client',
      '❯ ',
    ].join('\n');
    assert.strictEqual(relay.paneLooksBlockedOnHuman(pane), true);
  } finally { teardown(relay); }
});

test('#4267 blocked-on-human: MCP elicitation form (lead-in + field structure)', () => {
  const relay = loadRelay({});
  try {
    const pane = [
      'I need the following information to proceed:',
      'Cluster name: [        ]',
      'Region: [        ]',
      '> Enter to send',
    ].join('\n');
    assert.strictEqual(relay.paneLooksBlockedOnHuman(pane), true);
    // Goose's own elicitation-timeout marker is sufficient alone.
    assert.strictEqual(relay.paneLooksBlockedOnHuman(
      'working...\nElicitation request timed out\n> '), true);
  } finally { teardown(relay); }
});

test('#4267 blocked-on-human: permission / confirmation prompts', () => {
  const relay = loadRelay({});
  try {
    for (const pane of [
      'About to run rm -rf build\nDo you want to allow this? (y/N)\n> ',
      'Do you trust this folder\n> ',
      'Allow this tool to edit the file\n> ',
      'Press Enter to continue\n',
      'Paste your API key here\n> ',
    ]) {
      assert.strictEqual(relay.paneLooksBlockedOnHuman(pane), true, `must look blocked: ${pane}`);
    }
  } finally { teardown(relay); }
});

test('#4267 blocked-on-human: ordinary build/test output is NOT blocked', () => {
  const relay = loadRelay({});
  try {
    for (const pane of [
      'Compiling module foo\nBuild succeeded in 12.3s\nAll 42 tests passed\n> ',
      'go build ./...\nok  pkg/dashboard  1.234s\n$ ',
      // A "label: value" line must not read as an elicitation form (#2844).
      'opened a PR: https://github.com/kubestellar/hive/pull/123\n> ',
      // A question mark mid-line is not a prompt.
      'Checked whether the flag applies? yes, and it is already set\ndone\n> ',
      '',
    ]) {
      assert.strictEqual(relay.paneLooksBlockedOnHuman(pane), false, `false positive on: ${pane}`);
    }
  } finally { teardown(relay); }
});

// --- paneStallConfirmed ------------------------------------------------------

test('#4267 paneStallConfirmed requires PANE_STALL_CONFIRM_TICKS consecutive stalled ticks', () => {
  const relay = loadRelay({});
  try {
    relay.resetPaneStallClock();
    const lines = ['same output', 'line two'];
    assert.strictEqual(relay.paneStallConfirmed(lines), false, 'first sight records the fingerprint');
    relay.__agePaneStallClock(relay.PANE_STALL_TIMEOUT_MS + 1);
    for (let i = 1; i < relay.PANE_STALL_CONFIRM_TICKS; i++) {
      assert.strictEqual(relay.paneStallConfirmed(lines), false, `tick ${i} must not confirm yet`);
      assert.strictEqual(relay.getStallConfirmCount(), i);
    }
    assert.strictEqual(relay.paneStallConfirmed(lines), true, 'the final tick confirms the stall');
  } finally { teardown(relay); }
});

test('#4267 paneStallConfirmed resets the count when new output appears', () => {
  const relay = loadRelay({});
  try {
    relay.resetPaneStallClock();
    relay.paneStallConfirmed(['v1']);
    relay.__agePaneStallClock(relay.PANE_STALL_TIMEOUT_MS + 1);
    relay.paneStallConfirmed(['v1']);
    assert.ok(relay.getStallConfirmCount() > 0, 'precondition: a confirm tick accrued');
    // New content: the CLI proved it is alive — full credit, count back to 0.
    assert.strictEqual(relay.paneStallConfirmed(['v2 fresh output']), false);
    assert.strictEqual(relay.getStallConfirmCount(), 0);
  } finally { teardown(relay); }
});

test('#4267 an empty pane capture never confirms a stall', () => {
  const relay = loadRelay({});
  try {
    relay.resetPaneStallClock();
    for (let i = 0; i < relay.PANE_STALL_CONFIRM_TICKS + 2; i++) {
      relay.paneStallConfirmed([]);
      relay.__agePaneStallClock(relay.PANE_STALL_TIMEOUT_MS + 1);
      assert.strictEqual(relay.paneStallConfirmed([]), false, 'empty capture is not agent evidence');
    }
  } finally { teardown(relay); }
});

test('#4267 a blocked-on-human pane also confirms as stalled (the two signals compose)', () => {
  const relay = loadRelay({});
  try {
    const pane = 'Do you want to allow this? (y/N)\n> ';
    assert.strictEqual(relay.paneLooksBlockedOnHuman(pane), true);
    relay.resetPaneStallClock();
    const lines = pane.split('\n');
    relay.paneStallConfirmed(lines);
    relay.__agePaneStallClock(relay.PANE_STALL_TIMEOUT_MS + 1);
    let confirmed = false;
    for (let i = 0; i < relay.PANE_STALL_CONFIRM_TICKS; i++) confirmed = relay.paneStallConfirmed(lines);
    assert.strictEqual(confirmed, true, 'an unanswered prompt is byte-identical and must stall out');
  } finally { teardown(relay); }
});

// --- detectNoWorkVerdict (semantics from #4265 / #3987) ----------------------

test('#4267 detectNoWorkVerdict extracts the verdict and reason', () => {
  const relay = loadRelay({});
  try {
    const v = relay.detectNoWorkVerdict(['some output', 'HIVE_VERDICT: no_work_needed — already merged in #123']);
    assert.deepStrictEqual(v, { verdict: 'no_work_needed', reason: 'already merged in #123' });
    // Codex bullet chrome and indentation are presentation, not content.
    const b = relay.detectNoWorkVerdict(['  • HIVE_VERDICT: no_work_needed - gated on maintainer decision']);
    assert.strictEqual(b.reason, 'gated on maintainer decision');
    // Case-insensitive, empty reason allowed.
    assert.strictEqual(relay.detectNoWorkVerdict(['hive_verdict: NO_WORK_NEEDED']).verdict, 'no_work_needed');
  } finally { teardown(relay); }
});

test('#4267 detectNoWorkVerdict scans newest-first so the final conclusion wins', () => {
  const relay = loadRelay({});
  try {
    const v = relay.detectNoWorkVerdict([
      'HIVE_VERDICT: no_work_needed — early wrong take',
      'more work happened',
      'HIVE_VERDICT: no_work_needed — final answer',
    ]);
    assert.strictEqual(v.reason, 'final answer');
  } finally { teardown(relay); }
});

test('#4267 detectNoWorkVerdict ignores prompt echoes and mid-line quotes', () => {
  const relay = loadRelay({});
  try {
    // A wrapped prompt instruction carries the literal "<short reason>" placeholder.
    assert.strictEqual(relay.detectNoWorkVerdict(['HIVE_VERDICT: no_work_needed — <short reason>']), null);
    // The marker quoted mid-sentence is the prompt talking, not the agent.
    assert.strictEqual(relay.detectNoWorkVerdict(
      ["...it prints a line of the exact form 'HIVE_VERDICT: no_work_needed — x'"]), null);
    // \b: a mangled sentinel must not match.
    assert.strictEqual(relay.detectNoWorkVerdict(['HIVE_VERDICT: no_work_neededX']), null);
    assert.strictEqual(relay.detectNoWorkVerdict([]), null);
    assert.strictEqual(relay.detectNoWorkVerdict('not-an-array'), null);
    assert.strictEqual(relay.detectNoWorkVerdict(['normal completion text']), null);
  } finally { teardown(relay); }
});

// --- resolveBackend ----------------------------------------------------------

test('#4267 resolveBackend maps the backend through backends.conf', () => {
  const relay = loadRelay({ backend: 'copilot' });
  try {
    const r = relay.resolveBackend();
    assert.deepStrictEqual(r, { cmd: 'copilot', perm: '--allow-all' });
    assert.strictEqual(relay.resolveBackend(), r, 'resolution must be cached');
  } finally { teardown(relay); }
});

test('#4267 resolveBackend follows a backend NAME mapped to a different BINARY', () => {
  const relay = loadRelay({ backend: 'litellm', backendBinary: 'claude' });
  try {
    assert.deepStrictEqual(relay.resolveBackend(), { cmd: 'claude', perm: '--allow-all' });
  } finally { teardown(relay); }
});

// --- injectGhToken -----------------------------------------------------------

test('#4267 injectGhToken writes the token cache with owner-only permissions', () => {
  const relay = loadRelay({});
  try {
    relay.injectGhToken(`gho_${TOKEN_BODY}`);
    assert.strictEqual(fs.readFileSync(relay.GH_TOKEN_CACHE, 'utf8'), `gho_${TOKEN_BODY}`);
    if (process.platform !== 'win32') {
      assert.strictEqual(fs.statSync(relay.GH_TOKEN_CACHE).mode & 0o777, 0o600,
        'token cache must be 0600');
    }
    // Creates missing parent directories.
    assert.ok(fs.existsSync(path.dirname(relay.GH_TOKEN_CACHE)));
  } finally { teardown(relay); }
});

test('#4267 injectGhToken must not throw when the cache path is unwritable', () => {
  // ENOTDIR: the parent "directory" is actually a regular file. A throw here
  // would crash handleMessage on every task_assign — a crash loop, not a
  // degraded mode.
  const scratchRoot = path.join(__dirname, '..', '.relay-test-tmp');
  fs.mkdirSync(scratchRoot, { recursive: true });
  const scratch = fs.mkdtempSync(path.join(scratchRoot, 'inject-'));
  const relay = loadRelay({ env: { HIVE_GH_TOKEN_CACHE: path.join(scratch, 'blocker', 'token') } });
  try {
    fs.writeFileSync(path.join(scratch, 'blocker'), 'i am a file, not a directory');
    assert.doesNotThrow(() => relay.injectGhToken(`gho_${TOKEN_BODY}`));
    assert.ok(!fs.existsSync(path.join(scratch, 'blocker', 'token')));
  } finally {
    teardown(relay);
    fs.rmSync(scratch, { recursive: true, force: true });
  }
});

// --- pure helpers ------------------------------------------------------------

test('#4267 shellQuote survives embedded single quotes', () => {
  const relay = loadRelay({});
  try {
    assert.strictEqual(relay.shellQuote('plain'), "'plain'");
    assert.strictEqual(relay.shellQuote(''), "''");
    assert.strictEqual(relay.shellQuote("it's"), "'it'\\''s'");
    assert.strictEqual(relay.shellQuote('a $VAR `cmd` "x"'), "'a $VAR `cmd` \"x\"'");
  } finally { teardown(relay); }
});

test('#4267 looksLikeModelName rejects placeholders and non-strings', () => {
  const relay = loadRelay({});
  try {
    assert.strictEqual(relay.looksLikeModelName('claude-sonnet-4.5'), true);
    assert.strictEqual(relay.looksLikeModelName('gpt-5.6-luna'), true);
    assert.strictEqual(relay.looksLikeModelName(''), false);
    assert.strictEqual(relay.looksLikeModelName('<synthetic>'), false);
    assert.strictEqual(relay.looksLikeModelName(null), false);
    assert.strictEqual(relay.looksLikeModelName(42), false);
  } finally { teardown(relay); }
});

test('#4267 parseProtocolVersion is strict MAJOR.MINOR', () => {
  const relay = loadRelay({});
  try {
    assert.deepStrictEqual(relay.parseProtocolVersion('1.0'), { major: 1, minor: 0 });
    assert.deepStrictEqual(relay.parseProtocolVersion(' 2.10 '), { major: 2, minor: 10 });
    for (const bad of ['1', '1.2.3', 'v1.0', '1.-2', 'banana', '', null, undefined, '1.0-rc1']) {
      assert.strictEqual(relay.parseProtocolVersion(bad), null, `must reject: ${bad}`);
    }
  } finally { teardown(relay); }
});

test('#4267 taskKey keys by repo#number with task_id fallback', () => {
  const relay = loadRelay({});
  try {
    assert.strictEqual(relay.taskKey({ repo: 'kubestellar/hive', number: 42 }), 'kubestellar/hive#42');
    assert.strictEqual(relay.taskKey({ task_id: 'abc-123' }), 'abc-123');
    assert.strictEqual(relay.taskKey(null), 'unknown');
    assert.strictEqual(relay.taskKey({}), 'unknown');
  } finally { teardown(relay); }
});

test('#4267 readFileTail / tailLinesReversed / newestByMtime file helpers', () => {
  const relay = loadRelay({});
  const filesRoot = path.join(__dirname, '..', '.relay-test-tmp');
  fs.mkdirSync(filesRoot, { recursive: true });
  const dir = fs.mkdtempSync(path.join(filesRoot, 'files-'));
  try {
    const f = path.join(dir, 'a.txt');
    fs.writeFileSync(f, 'HEAD-CUT-tail-part');
    assert.strictEqual(relay.readFileTail(f, 9), 'tail-part', 'must read only the last maxBytes');
    assert.strictEqual(relay.readFileTail(f, 1024), 'HEAD-CUT-tail-part', 'a large bound reads it all');

    const jl = path.join(dir, 'b.jsonl');
    fs.writeFileSync(jl, '{"cut mid-line\n{"n":1}\nnot json\n{"n":2}\n');
    assert.deepStrictEqual(relay.tailLinesReversed(jl), [{ n: 2 }, { n: 1 }],
      'newest first, unparseable lines skipped');

    const old = path.join(dir, 'old.txt');
    const fresh = path.join(dir, 'new.txt');
    fs.writeFileSync(old, 'x');
    fs.writeFileSync(fresh, 'y');
    const past = new Date(Date.now() - 60000);
    fs.utimesSync(old, past, past);
    assert.strictEqual(relay.newestByMtime([old, fresh, path.join(dir, 'missing')]), fresh);
    assert.strictEqual(relay.newestByMtime([]), null);
    assert.strictEqual(relay.newestByMtime([path.join(dir, 'missing')]), null);
  } finally {
    teardown(relay);
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test('#4267 nextSeq is a monotonic counter from 1', () => {
  const relay = loadRelay({});
  try {
    assert.strictEqual(relay.nextSeq(), 1);
    assert.strictEqual(relay.nextSeq(), 2);
    assert.strictEqual(relay.nextSeq(), 3);
  } finally { teardown(relay); }
});

test('#4267 modelFlagFor honours NO_MODEL_FLAG_BACKENDS', () => {
  const withModel = loadRelay({ backend: 'copilot', model: 'gpt-5.6-luna' });
  try {
    assert.strictEqual(withModel.modelFlagFor(), '--model gpt-5.6-luna');
  } finally { teardown(withModel); }
  const bob = loadRelay({ backend: 'bob', model: 'gpt-5.6-luna' });
  try {
    assert.strictEqual(bob.modelFlagFor(), '', 'bob takes no --model');
  } finally { teardown(bob); }
  const noModel = loadRelay({ backend: 'copilot', model: '' });
  try {
    assert.strictEqual(noModel.modelFlagFor(), '');
  } finally { teardown(noModel); }
});

test('#4267 sleepMs is a no-op under HIVE_RELAY_TEST_MODE', () => {
  const relay = loadRelay({});
  try {
    const start = Date.now();
    relay.sleepMs(5000);
    assert.ok(Date.now() - start < 500, 'test mode must skip the busy-wait');
  } finally { teardown(relay); }
});

test('#4267 detectPRURL prefers the task repo and falls back to the first URL', () => {
  const relay = loadRelay({});
  try {
    const lines = [
      'mentioned https://github.com/other/repo/pull/7 in passing',
      'Opened https://github.com/kubestellar/hive/pull/4267 for review',
    ];
    assert.strictEqual(relay.detectPRURL(lines, 'kubestellar/hive'),
      'https://github.com/kubestellar/hive/pull/4267');
    assert.strictEqual(relay.detectPRURL(lines, 'nomatch/repo'),
      'https://github.com/other/repo/pull/7', 'fall back to the first PR URL seen');
    assert.strictEqual(relay.detectPRURL(['no urls here'], 'kubestellar/hive'), '');
    assert.strictEqual(relay.detectPRURL([], 'kubestellar/hive'), '');
    assert.strictEqual(relay.detectPRURL(null, 'kubestellar/hive'), '');
    // An issue URL is not a PR URL.
    assert.strictEqual(relay.detectPRURL(['https://github.com/kubestellar/hive/issues/9'], 'kubestellar/hive'), '');
  } finally { teardown(relay); }
});

test('#4267 isGivenUp remembers a give-up and expires it after GIVE_UP_MEMORY_MS', () => {
  const relay = loadRelay({});
  try {
    assert.strictEqual(relay.isGivenUp('kubestellar/hive#1'), false, 'unknown key');
    relay.__setGivenUp('kubestellar/hive#1', Date.now());
    assert.strictEqual(relay.isGivenUp('kubestellar/hive#1'), true, 'fresh give-up');
    relay.__setGivenUp('kubestellar/hive#2', Date.now() - relay.GIVE_UP_MEMORY_MS - 1);
    assert.strictEqual(relay.isGivenUp('kubestellar/hive#2'), false, 'stale give-up expires');
    assert.strictEqual(relay.isGivenUp('kubestellar/hive#2'), false, 'and stays pruned');
  } finally { teardown(relay); }
});

test('#4267 recentPaneLines trims, drops blanks and bounds the window', () => {
  const relay = loadRelay({});
  try {
    assert.deepStrictEqual(relay.recentPaneLines('  a  \n\n   \nb\n c \n'), ['a', 'b', 'c']);
    const many = Array.from({ length: 20 }, (_, i) => `line${i}`).join('\n');
    assert.deepStrictEqual(relay.recentPaneLines(many),
      Array.from({ length: 12 }, (_, i) => `line${i + 8}`), 'default window is the last 12 lines');
    assert.deepStrictEqual(relay.recentPaneLines(many, 2), ['line18', 'line19']);
    assert.deepStrictEqual(relay.recentPaneLines(''), []);
  } finally { teardown(relay); }
});

// --- priority-3 extras -------------------------------------------------------

test('#4267 sendTo only writes to an OPEN socket and tolerates a missing hub', () => {
  const relay = loadRelay({});
  try {
    const hub = relay.getHubs()[0];
    relay.sendTo(hub, { type: 'probe', seq: 1 });
    assert.ok(relay.__sent.some(m => m.type === 'probe'), 'OPEN socket must receive the message');
    hub.ws.readyState = 3; // CLOSED
    relay.sendTo(hub, { type: 'dropped' });
    assert.ok(!relay.__sent.some(m => m.type === 'dropped'), 'closed socket must be skipped');
    assert.doesNotThrow(() => relay.sendTo(null, { type: 'x' }));
    assert.doesNotThrow(() => relay.sendTo({}, { type: 'x' }));
  } finally { teardown(relay); }
});

test('#4267 tmuxSendEnters presses Enter exactly ENTER_COUNT times', () => {
  const relay = loadRelay({});
  try {
    const before = relay.__tmuxSends().length;
    relay.tmuxSendEnters();
    const enters = relay.__tmuxSends().slice(before).filter(c => /send-keys .*Enter/.test(c));
    assert.strictEqual(enters.length, 3);
  } finally { teardown(relay); }
});

test('#4267 warnOnProtocolDrift warns once per hub and stays silent when current', () => {
  const relay = loadRelay({});
  const warnings = [];
  const origWarn = console.warn;
  console.warn = (...a) => warnings.push(a.join(' '));
  try {
    const self = relay.parseProtocolVersion(relay.RELAY_PROTOCOL_VERSION);
    const current = { name: 'h1' };
    relay.warnOnProtocolDrift(current, relay.RELAY_PROTOCOL_VERSION);
    relay.warnOnProtocolDrift({ name: 'h2' }, ''); // unknown: peer stated nothing
    assert.strictEqual(warnings.length, 0, 'current/unknown must not warn');

    const drifted = { name: 'h3' };
    relay.warnOnProtocolDrift(drifted, `${self.major + 1}.0`);
    assert.strictEqual(warnings.length, 1);
    assert.match(warnings[0], /incompatible/i);
    relay.warnOnProtocolDrift(drifted, `${self.major + 1}.0`);
    assert.strictEqual(warnings.length, 1, 'the warning is once per hub');

    relay.warnOnProtocolDrift({ name: 'h4' }, 'banana');
    assert.strictEqual(warnings.length, 2);
    assert.match(warnings[1], /malformed/i);
  } finally {
    console.warn = origWarn;
    teardown(relay);
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
