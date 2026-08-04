#!/usr/bin/env node
// contributor-relay.sh — ClankeR, the contributor relay: the WebSocket client
// that connects a contributor agent to the Hive hub.
//
// Handles: authentication, task receipt, GitHub token injection, result reporting,
// heartbeat, and reconnection with exponential backoff.
//
// Environment:
//   HIVE_HUB              — WebSocket URL (wss://host:port/contribute)
//   HIVE_REGISTRATION_TOKEN — contributor's registration token
//   AGENT_BACKEND          — CLI backend name (claude, copilot, gemini, etc.)
//   AGENT_MODEL            — model override (optional)
//   HIVE_AGENT_SESSION     — tmux session name for the agent (default: contributor)

'use strict';

const WebSocket = require('ws');
const { execSync, execFile } = require('child_process');
const fs = require('fs');
const path = require('path');

const rawHub = process.env.HIVE_HUB || 'wss://hive.kubestellar.io:3001/contribute';
const HUB_URL = rawHub.replace(/\/contribute\/?$/, '/api/contribute/ws');
const REG_TOKEN = process.env.HIVE_REGISTRATION_TOKEN;
const BACKEND = process.env.AGENT_BACKEND || 'claude';
const MODEL = process.env.AGENT_MODEL || process.env.GOOSE_MODEL || '';
const TMUX_SESSION = process.env.HIVE_AGENT_SESSION || 'contributor';
const GH_TOKEN_CACHE = fs.existsSync('/var/run/hive-metrics')
  ? '/var/run/hive-metrics/gh-app-token.cache'
  : '/tmp/hive-gh-token.cache';
const TASK_FILE = '/tmp/contributor-task.json';

const TMUX_TAIL_LINES = 15;
const HEARTBEAT_INTERVAL_MS = 30000;
const HEARTBEAT_TIMEOUT_MS = 90000;
const PROGRESS_REPORT_INTERVAL_MS = 120000;
const MAX_RECONNECT_DELAY_MS = 60000;
const BASE_RECONNECT_DELAY_MS = 1000;
const TOKEN_REFRESH_MARGIN_MS = 300000;
const MAX_TASK_DURATION_MS = 1800000;
const NETWORK_ERROR_RETRY_DELAY_MS = 5000;
// After the hub sends an explicit task_unavailable negative-ack (no admissible
// work, a disabled tier, a concurrency limit, or a token-mint failure — see
// kubestellar/hive#2436), wait before re-asking so we neither hang forever
// (the old silent-nil behaviour) nor busy-loop the hub.
const TASK_UNAVAILABLE_RETRY_MS = 30000;

// RELAY_PROTOCOL_VERSION is the contributor-protocol version this relay speaks
// (kubestellar/hive#2567). It is DECLARED to the hub in auth_response (additive,
// optional — an older hub simply ignores it) and the hub advertises its own
// version + capability set back on auth_ok. Keep in step with
// contributorProtocolVersion in v2/pkg/dashboard/contribute_protocol.go.
const RELAY_PROTOCOL_VERSION = '1.1';

// Per-task CLI-crash retry budget. Issue #2203: a task whose CLI kept dying was
// reassigned by the hub and failed identically forever (5+ times in ~20min),
// starving that hub task slot. After MAX_TASK_CLI_RESTARTS crash-restarts for
// the SAME repo#number, the relay gives up on that task permanently and tells
// the hub so it can be reassigned elsewhere.
const MAX_TASK_CLI_RESTARTS = 3;
// Backoff before each successive restart of the same task: 5s, 10s, 20s.
const TASK_RESTART_BASE_BACKOFF_MS = 5000;
const TASK_RESTART_MAX_BACKOFF_MS = 60000;
// How long a permanently-given-up task stays on the deny list. Long enough
// that the hub does not immediately hand the same poison task back, short
// enough that a transient environment fault eventually clears.
const GIVE_UP_MEMORY_MS = 3600000;

if (!REG_TOKEN) {
  console.error('FATAL: HIVE_REGISTRATION_TOKEN not set. Run `just contribute-register` first.');
  process.exit(1);
}

let ws = null;
let seq = 0;
let reconnectDelay = BASE_RECONNECT_DELAY_MS;
let heartbeatInterval = null;
let lastPong = Date.now();
let currentTask = null;
let progressInterval = null;
let tokenExpiresAt = null;

function nextSeq() { return ++seq; }

function send(msg) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify(msg));
  }
}

function injectGhToken(token) {
  const dir = path.dirname(GH_TOKEN_CACHE);
  try { fs.mkdirSync(dir, { recursive: true }); } catch (_) {}
  fs.writeFileSync(GH_TOKEN_CACHE, token, { mode: 0o600 });
}

const CLI_READY_POLL_MS = 2000;
const CLI_READY_TIMEOUT_MS = 600000;
const CONTAINER_NAME = process.env.HIVE_CONTAINER_NAME || 'hive-contributor';

// detectCapabilities builds the OPTIONAL, client-declared capability object the
// relay reports in auth_response (kubestellar/hive#2547, declare half). Every
// entry is a cheap, honest self-report the hub STORES + SURFACES read-only and
// NEVER routes/gates on. It is best-effort: any probe that throws is simply
// omitted, so a constrained environment still authenticates unchanged. Computed
// once at startup and cached.
let cachedCapabilities = null;
function detectCapabilities() {
  if (cachedCapabilities) return cachedCapabilities;
  const caps = {
    os: process.platform,
    arch: process.arch,
    relay_protocol_version: RELAY_PROTOCOL_VERSION,
  };
  // Container runtime: prefer docker, then podman, else none. `command -v` is a
  // cheap presence check; failure just means the runtime is absent.
  let runtime = 'none';
  for (const rt of ['docker', 'podman']) {
    try {
      execSync(`command -v ${rt}`, { stdio: 'ignore' });
      runtime = rt;
      break;
    } catch (_) { /* not installed */ }
  }
  caps.container_runtime = runtime;
  // Credential type: the KIND of GitHub credential the relay authenticates with
  // (never the credential itself). App-token cache present → "app"; an explicit
  // GH_TOKEN/GITHUB_TOKEN in the environment → "pat"; otherwise leave unset.
  try {
    if (fs.existsSync(GH_TOKEN_CACHE)) {
      caps.credential_type = 'app';
    } else if (process.env.GH_TOKEN || process.env.GITHUB_TOKEN) {
      caps.credential_type = 'pat';
    }
  } catch (_) { /* ignore */ }
  cachedCapabilities = caps;
  return caps;
}

// Backends that must NOT be given --model, mirroring contributor-agent.sh.
// amazonq/goose take their model from config/env. bob is excluded because
// --model is actively FATAL for it: bob auto-selects its own model and passing
// one leaves its model config undefined, so every prompt dies with
// "Cannot read properties of undefined (reading 'maxTokens')" (bobshell 1.0.6).
const NO_MODEL_FLAG_BACKENDS = ['amazonq', 'goose', 'bob'];

// Single source of truth for the CLI launch command (issue #2203, bug 1).
// contributor-agent.sh builds "$CMD $PERM_FLAG $MODEL_FLAG" for the FIRST
// launch; every restart path in this file previously rebuilt only "$CMD $PERM"
// inline, silently dropping the resolved model for the rest of the container's
// life. Build it once here and reuse it everywhere so the paths cannot drift.
let cachedLaunchCommand = null;

function buildLaunchCommand() {
  if (cachedLaunchCommand) return cachedLaunchCommand;
  const confPaths = ['/usr/local/etc/hive/backends.conf', path.join(process.cwd(), 'config/backends.conf')];
  const confPath = confPaths.find(p => fs.existsSync(p)) || confPaths[0];
  let cmd = BACKEND;
  let perm = '';
  try {
    cmd = execSync(`bash -c 'source ${confPath} 2>/dev/null; backend_binary ${BACKEND}'`, { encoding: 'utf8', timeout: 15000 }).trim() || BACKEND;
    perm = execSync(`bash -c 'source ${confPath} 2>/dev/null; backend_perm_flag ${BACKEND}'`, { encoding: 'utf8', timeout: 15000 }).trim();
  } catch (e) {
    console.error(`Could not resolve backend flags from ${confPath}: ${e.message}`);
  }
  const modelFlag = MODEL && !NO_MODEL_FLAG_BACKENDS.includes(BACKEND) ? `--model ${MODEL}` : '';
  cachedLaunchCommand = [cmd, perm, modelFlag].filter(Boolean).join(' ');
  return cachedLaunchCommand;
}

// A tmux pane can be left in bash's PS2 continuation state ("> ") when task
// text is typed into a bare shell — the prompt contains literal apostrophes
// (e.g. 'gh repo fork ... --clone=false'), so bash's readline sees an
// unbalanced quote and swallows everything typed afterwards, including the
// relaunch command (issue #2203). Clear the line before relaunching.
function recoverWedgedShell() {
  try {
    execSync(`tmux send-keys -t ${TMUX_SESSION} C-c`, { timeout: 15000 });
    execSync(`tmux send-keys -t ${TMUX_SESSION} C-u`, { timeout: 15000 });
    execSync(`tmux send-keys -t ${TMUX_SESSION} Enter`, { timeout: 15000 });
  } catch (_) {}
}

function getCLIState() {
  try {
    const output = execSync(
      `tmux capture-pane -t ${TMUX_SESSION} -p 2>/dev/null`,
      { encoding: 'utf8', timeout: 15000 }
    );
    const text = output.toString();
    if (BACKEND === 'claude') {
      if (/Not logged in|Please run \/login/.test(text)) return 'needs-login';
      if (/bypass permissions|Welcome back|Try "how does|medium.*effort|@gmail\.com|@.*\.com.*Organization/.test(text)) return 'ready';
      if (/Choose the text style|trust this folder/.test(text)) return 'onboarding';
    } else if (BACKEND === 'copilot') {
      if (/copilot login|gh auth login/.test(text)) return 'needs-login';
      if (/Confirm folder trust|trust the files|Do you trust/.test(text)) return 'onboarding';
      if (/\/ commands.*help/.test(text)) return 'ready';
    } else if (BACKEND === 'gemini') {
      if (/not authenticated|login required/i.test(text)) return 'needs-login';
      if (/>\s*$|❯/.test(text)) return 'ready';
    } else if (BACKEND === 'goose') {
      if (/goose is ready|> Enter to send|>\s*$|goose>|G\s*>/.test(text)) return 'ready';
    } else if (BACKEND === 'bob') {
      // Order matters: the blocked states are checked FIRST because bob's
      // auth prompt contains the literal string "Bob-Shell" ("Enter Bob-Shell
      // API Key"). The former /Bob-Shell/ 'ready' test therefore reported a
      // bob stuck at the API-key prompt as READY, and the relay would dispatch
      // tasks into a pane that could never run them.
      if (/Enter Bob-Shell API Key|enter your Bob-Shell API key|Paste your API key here/i.test(text)) return 'needs-login';
      if (/Do you trust this folder|not trusted/i.test(text)) return 'onboarding';
      // Real prompt chrome of an authenticated, ready bob TUI. Matching the
      // status line ("Auto-approve:", "Tokens left:") or the boxed input hint
      // is far tighter than the old />\s*$/, which matched almost any pane
      // that happened to end in a '>' — including partially drawn frames.
      if (/Enter your prompt, \/ for commands|Auto-approve:|Tokens left:/.test(text)) return 'ready';
    } else if (BACKEND === 'codex') {
      if (/codex>|>\s*$|Codex CLI/.test(text)) return 'ready';
    } else if (BACKEND === 'pi') {
      if (/pi v\d|0\.0%|auto\)|\d+\.\d+%/.test(text)) return 'ready';
    } else {
      if (/>\s*$|❯|\$\s*$/.test(text)) return 'ready';
    }
    return 'starting';
  } catch (_) {
    return 'starting';
  }
}

function waitForCLI() {
  let loginMessageShown = false;
  return new Promise((resolve, reject) => {
    const start = Date.now();
    const check = () => {
      const state = getCLIState();
      if (state === 'ready') {
        console.log('CLI ready — accepting tasks');
        resolve();
      } else if (state === 'onboarding') {
        console.log('Auto-dismissing trust/onboarding dialog...');
        try { execSync(`tmux send-keys -t ${TMUX_SESSION} Enter`, { timeout: 15000 }); } catch (_) {}
        setTimeout(check, CLI_READY_POLL_MS);
      } else if (state === 'needs-login' && !loginMessageShown) {
        loginMessageShown = true;
        console.log('');
        console.log('╔══════════════════════════════════════════════════════════╗');
        console.log('║  Claude Code needs authentication.                      ║');
        console.log('║  In another terminal, run:                              ║');
        console.log(`║  docker exec -it ${CONTAINER_NAME} tmux attach -t ${TMUX_SESSION}`);
        console.log('║  Then type: /login                                      ║');
        console.log('║  Complete the login, then press Ctrl-B D to detach.     ║');
        console.log('║  Waiting for login to complete...                       ║');
        console.log('╚══════════════════════════════════════════════════════════╝');
        console.log('');
        setTimeout(check, CLI_READY_POLL_MS);
      } else if (Date.now() - start > CLI_READY_TIMEOUT_MS) {
        reject(new Error('CLI did not become ready within timeout'));
      } else {
        setTimeout(check, CLI_READY_POLL_MS);
      }
    };
    check();
  });
}

let cliReady = false;
let pendingTask = null;

waitForCLI().then(() => {
  cliReady = true;
  flushPendingTask();
}).catch(e => console.error(e.message));

const ENTER_COUNT = 3;
const ENTER_DELAY_MS = 300;

function sleepMs(ms) {
  // Tests drive the restart/backoff paths synchronously; a real busy-wait
  // would make the suite take minutes of wall clock for no added coverage.
  if (process.env.HIVE_RELAY_TEST_MODE === '1') return;
  const end = Date.now() + ms;
  while (Date.now() < end) {
    try { execSync(`sleep 0.1`, { timeout: 5000 }); } catch (_) {}
  }
}

function tmuxSendEnters() {
  for (let i = 0; i < ENTER_COUNT; i++) {
    execSync(`tmux send-keys -t ${TMUX_SESSION} Enter`, { timeout: 15000 });
    if (i < ENTER_COUNT - 1) sleepMs(ENTER_DELAY_MS);
  }
}

const CLEAR_CONTEXT_THRESHOLD_PCT = 70;

function checkContextUsage() {
  try {
    const output = execSync(
      `tmux capture-pane -t ${TMUX_SESSION} -p -S -3 2>/dev/null`,
      { encoding: 'utf8', timeout: 15000 }
    );
    const match = output.match(/ctx:(\d+)%|(\d+)% context/);
    return match ? parseInt(match[1] || match[2], 10) : 0;
  } catch (_) {
    return 0;
  }
}

function tmuxSendKeys(text) {
  // Hard gate (issue #2203, bug 2): `send-keys -l` types literal keystrokes
  // into whatever owns the pane. If the CLI is not confirmed ready, those
  // keystrokes land on bash, whose readline chokes on the apostrophes in the
  // prompt and drops the pane into PS2 continuation, wedging it permanently.
  // Queue instead; flushPendingTask() delivers it once readiness is confirmed.
  if (!cliReady) {
    console.log('CLI not ready — queuing task prompt instead of typing into the pane');
    pendingTask = text;
    return;
  }
  try {
    try {
      execSync(`find /tmp -maxdepth 1 -type d -user dev -not -name 'tmux-*' -not -name 'claude-*' -not -name 'node-*' -not -name '.' -mmin +60 -exec rm -rf {} + 2>/dev/null; find /tmp -maxdepth 1 -type f -user dev -name '*.out' -o -name '*.html' -mmin +60 -exec rm -f {} + 2>/dev/null`, { timeout: 15000 });
    } catch (_) {}
    const ctxPct = checkContextUsage();
    const RESET_EVERY_N = 3;
    const needsClaudeClear = BACKEND === 'claude' && ctxPct >= CLEAR_CONTEXT_THRESHOLD_PCT;
    const needsCliRestart = BACKEND !== 'claude' && tasksCompletedCount > 0 && tasksCompletedCount % RESET_EVERY_N === 0;
    if (needsClaudeClear) {
      console.log(`Context at ${ctxPct}% — sending /clear before next task`);
      execSync(`tmux send-keys -t ${TMUX_SESSION} Escape`, { timeout: 15000 });
      sleepMs(200);
      execSync(`tmux send-keys -t ${TMUX_SESSION} C-a`, { timeout: 15000 });
      execSync(`tmux send-keys -t ${TMUX_SESSION} C-k`, { timeout: 15000 });
      sleepMs(200);
      execSync(`tmux send-keys -t ${TMUX_SESSION} -l '/clear'`, { timeout: 15000 });
      sleepMs(200);
      tmuxSendEnters();
      sleepMs(3000);
    } else if (needsCliRestart) {
      console.log(`Restarting ${BACKEND} CLI for memory cleanup (task ${tasksCompletedCount})`);
      execSync(`tmux send-keys -t ${TMUX_SESSION} C-c`, { timeout: 15000 });
      sleepMs(1000);
      execSync(`tmux send-keys -t ${TMUX_SESSION} C-c`, { timeout: 15000 });
      sleepMs(2000);
      // Queue this prompt and hand delivery to the readiness callback.
      // Previously the restart set cliReady=false and then FELL THROUGH to the
      // send loop below, typing the prompt into a pane where the CLI had just
      // been Ctrl-C'd and had not come back — the exact sequence in #2203.
      pendingTask = text;
      cliReady = false;
      try {
        console.log(`CLI restarted: ${relaunchCLI()}`);
      } catch (e) {
        console.error('CLI restart failed:', e.message);
      }
      return;
    }
    const MAX_SEND_RETRIES = 3;
    const RETRY_DELAY_MS = 10000;
    let sent = false;
    for (let attempt = 1; attempt <= MAX_SEND_RETRIES; attempt++) {
      try {
        execSync(`tmux send-keys -t ${TMUX_SESSION} Escape`, { timeout: 15000 });
        sleepMs(200);
        execSync(`tmux send-keys -t ${TMUX_SESSION} C-a`, { timeout: 15000 });
        execSync(`tmux send-keys -t ${TMUX_SESSION} C-k`, { timeout: 15000 });
        sleepMs(200);
        execSync(`tmux send-keys -t ${TMUX_SESSION} -l ${shellQuote(text)}`, { timeout: 30000 });
        sleepMs(300);
        tmuxSendEnters();
        console.log('Task prompt sent to CLI');
        sent = true;
        break;
      } catch (e) {
        console.error(`tmux send-keys attempt ${attempt}/${MAX_SEND_RETRIES} failed: ${e.message}`);
      }
      if (!sent && attempt < MAX_SEND_RETRIES) {
        console.log(`Waiting ${RETRY_DELAY_MS/1000}s before retry...`);
        sleepMs(RETRY_DELAY_MS);
      }
    }
    if (!sent) console.error('All tmux send-keys attempts failed — task prompt lost');
  } catch (e) {
    console.error('tmux send-keys failed:', e.message);
  }
}

function shellQuote(s) {
  return "'" + s.replace(/'/g, "'\\''") + "'";
}

function redactTokens(text) {
  return text.replace(/gho_[A-Za-z0-9]{36}/g, 'gho_***REDACTED***')
    .replace(/ghp_[A-Za-z0-9]{36}/g, 'ghp_***REDACTED***')
    .replace(/ghs_[A-Za-z0-9]{36}/g, 'ghs_***REDACTED***');
}

function captureTmuxLines(n) {
  try {
    const output = execSync(
      `tmux capture-pane -t ${TMUX_SESSION} -p -S -${n} 2>/dev/null`,
      { encoding: 'utf8', timeout: 15000 }
    );
    return output.trim().split('\n').slice(-n).map(l => redactTokens(l));
  } catch (_) {
    return [];
  }
}

// Best-effort scan of the agent's recent output for a GitHub pull-request URL
// it opened for this task. Reported on task_complete as pr_url so the hub can
// tell "work shipped" from "agent merely went idle" and pick the right issue
// cooldown (kubestellar/hive#2393 item 7). This is intentionally best-effort:
// when no PR link is visible we return '' and the hub applies its short no-PR
// cooldown. When `repo` is known (owner/repo) we prefer a URL under that repo
// so an unrelated PR mentioned in passing does not get attributed to the task.
function detectPRURL(lines, repo) {
  if (!Array.isArray(lines) || lines.length === 0) return '';
  // Matches https://github.com/<owner>/<repo>/pull/<number>, capturing owner/repo.
  const PR_URL_RE = /https:\/\/github\.com\/([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)\/pull\/\d+/g;
  let repoMatch = '';
  let anyMatch = '';
  for (const line of lines) {
    let m;
    PR_URL_RE.lastIndex = 0;
    while ((m = PR_URL_RE.exec(line)) !== null) {
      const url = m[0];
      if (!anyMatch) anyMatch = url;
      if (repo && m[1] === repo) { repoMatch = url; break; }
    }
    if (repoMatch) break;
  }
  // Prefer a URL under the task's own repo; otherwise fall back to the first
  // PR URL seen (better an approximate audit trail than none).
  return repoMatch || anyMatch;
}

// True while a bob CLI process is alive. bob exits at the end of every turn,
// so "process gone" means the turn finished — see the bob branch of
// checkTmuxIdle(). Matches the launch command rather than the bare name so a
// stray "bob" substring elsewhere in the process table cannot mask an exit.
const BOB_PROCESS_PATTERN = 'bob --accept-license';

function bobIsRunning() {
  try {
    let procs;
    if (fs.existsSync('/proc')) {
      procs = execSync(
        `for p in /proc/[0-9]*/cmdline; do tr "\\0" " " < "$p" 2>/dev/null; echo; done`,
        { encoding: 'utf8', timeout: 15000 }
      );
    } else {
      procs = execSync('ps -eo command 2>/dev/null', { encoding: 'utf8', timeout: 15000 });
    }
    return procs.includes(BOB_PROCESS_PATTERN);
  } catch (_) {
    // Unknown -> assume still running, so a probe failure cannot fabricate a
    // completion for a task that is actually still in flight.
    return true;
  }
}

// Relaunch the backend CLI in the tmux session using the flags from
// backends.conf, the same way contributor-agent.sh first launched it.
function relaunchCLI() {
  const launchCmd = buildLaunchCommand();
  // The pane may be wedged in bash PS2 continuation; clear it or the relaunch
  // command is swallowed as more continuation text and never runs.
  recoverWedgedShell();
  execSync(`tmux send-keys -t ${TMUX_SESSION} ${shellQuote(launchCmd)} Enter`, { timeout: 15000 });
  // The CLI is NOT up yet. cliReady must stay false until the readiness
  // classifier positively confirms it, or a task prompt sent in the meantime
  // is typed as literal keystrokes into a bare shell (issue #2203, bug 2).
  cliReady = false;
  waitForCLI().then(() => {
    cliReady = true;
    flushPendingTask();
  }).catch(e => console.error(`CLI did not become ready after relaunch: ${e.message}`));
  return launchCmd;
}

function flushPendingTask() {
  if (!pendingTask) return;
  const t = pendingTask;
  pendingTask = null;
  tmuxSendKeys(t);
}

function checkTmuxIdle() {
  try {
    const output = execSync(
      `tmux capture-pane -t ${TMUX_SESSION} -p 2>/dev/null`,
      { encoding: 'utf8', timeout: 15000 }
    );
    const text = output.toString();
    let hasIdlePrompt, hasCompletionMarker, isWorking;

    if (BACKEND === 'claude') {
      const lastLines = text.split('\n').slice(-15).join('\n');
      hasIdlePrompt = /bypass permissions|shift\+tab to cycle/.test(text);
      hasCompletionMarker = /[✻✶✽] \S+ed for \d+[ms]|Honking|tokens\)/.test(text);
      isWorking = /─.*Bash\(|Reading|Editing|Writing|Searching/.test(lastLines) || /ing…/.test(lastLines);
    } else if (BACKEND === 'copilot') {
      hasIdlePrompt = /\/ commands.*help/.test(text);
      hasCompletionMarker = true;
      isWorking = /esc cancel/.test(text);
    } else if (BACKEND === 'gemini') {
      hasIdlePrompt = />\s*$|❯\s*$/.test(text);
      hasCompletionMarker = /completed|Done|finished/i.test(text);
      isWorking = /Thinking|Running|Searching/i.test(text);
    } else if (BACKEND === 'goose') {
      const hasNetworkError = /Network error:|Please resend your message|Could not connect/i.test(text);
      if (hasNetworkError && /> Enter to send/.test(text)) {
        console.log('Goose network error detected — pressing Enter to retry');
        try {
          execSync(`tmux send-keys -t ${TMUX_SESSION} Enter`, { timeout: 15000 });
        } catch (_) {}
        return false;
      }
      hasIdlePrompt = /goose is ready|> Enter to send|>\s*$|goose>|G\s*>/.test(text);
      hasCompletionMarker = true;
      isWorking = /working|running|executing|calling/i.test(text);
    } else if (BACKEND === 'bob') {
      // Matched against real bobshell 1.0.6 panes. Every part of the previous
      // classifier was wrong on real output, and each was independently fatal:
      //
      //   hasIdlePrompt /bob>|>\s*$/ NEVER matched. bob's idle prompt is drawn
      //     inside a box ("│ >   Enter your prompt, / for commands, …  │"), so
      //     the '>' is never at end-of-line and there is no "bob>" anywhere.
      //     Since idle is ANDed in, task_complete could never fire for bob:
      //     a finished task stayed "in progress" until the 30-min timeout, and
      //     the hub then heard task_failed for work bob had actually done.
      //
      //   isWorking /running|executing|thinking/i was permanently TRUE. bob
      //     prints a static banner "You are running Bob Shell in your home
      //     directory" and echoes its <thinking> block; both stay in the pane
      //     forever, so this never cleared even on a fully idle bob.
      //
      // Instead: reuse the same prompt chrome getCLIState() already verifies
      // for 'ready', and detect work via bob's live spinner line, which reads
      // "◡ <task title> (esc to cancel, 5s)" only while a turn is running.
      // hasCompletionMarker is true because bob has no completion token —
      // returning to the idle prompt with no spinner IS the completion signal
      // (same convention as the copilot/goose branches above).
      const BOB_IDLE_CHROME = /Enter your prompt, \/ for commands|Auto-approve:|Tokens left:/;
      const BOB_SPINNER = /\(esc to cancel/;
      // bob exits after finishing a turn ("Bob goes to sleep 💤"). Process
      // liveness is the only reliable completion signal: bob does not repaint
      // over its last frame on the way out, so a finished pane keeps a frozen
      // spinner line ("◡ <title> (esc to cancel, 5s)") and the "goes to sleep"
      // notice often scrolls out of the visible pane entirely. Screen-scraping
      // alone therefore reports a completed turn as still working. Conversely
      // the trailing shell prompt cannot stand in for "exited" either — it is
      // also present mid-turn, left over from before bob was launched.
      // Once bob has exited its chrome scrolls away, so an exited bob counts
      // as idle on its own: there is no prompt left to look for.
      const bobRunning = bobIsRunning();
      hasIdlePrompt = BOB_IDLE_CHROME.test(text) || !bobRunning;
      hasCompletionMarker = true;
      isWorking = bobRunning && BOB_SPINNER.test(text);
    } else if (BACKEND === 'codex') {
      hasIdlePrompt = /codex>|>\s*$/.test(text);
      hasCompletionMarker = /completed|done|finished/i.test(text);
      isWorking = /running|executing|thinking/i.test(text);
    } else if (BACKEND === 'pi') {
      hasIdlePrompt = /pi v\d|0\.0%|auto\)|\d+\.\d+%/.test(text);
      hasCompletionMarker = /completed|done|finished|tokens\)|\d+\.\d+%/i.test(text);
      isWorking = /Reading|Writing|Bash|Editing|thinking|running/i.test(text);
    } else {
      hasIdlePrompt = />\s*$|\$\s*$/.test(text);
      hasCompletionMarker = /completed|done|finished/i.test(text);
      isWorking = false;
    }
    return hasIdlePrompt && hasCompletionMarker && !isWorking;
  } catch (_) {
    return false;
  }
}

const TASK_GRACE_PERIOD_MS = 180000;
let taskAssignedAt = 0;
let tasksCompletedCount = 0;
const PR_REVIEW_EVERY_N = 5;
let taskTimeoutHandle = null;
let lastProgressTick = 0;

// Crash-restart bookkeeping, keyed by the underlying work item (repo#number)
// rather than task_id — the hub mints a NEW task_id on every reassignment of
// the same issue, so a task_id-keyed counter would reset each round and never
// reach the cap (issue #2203, bug 3).
const cliRestartCounts = new Map();
const givenUpTasks = new Map();

function taskKey(task) {
  return task && task.repo ? `${task.repo}#${task.number}` : (task && task.task_id) || 'unknown';
}

function isGivenUp(key) {
  const at = givenUpTasks.get(key);
  if (at === undefined) return false;
  if (Date.now() - at > GIVE_UP_MEMORY_MS) {
    givenUpTasks.delete(key);
    return false;
  }
  return true;
}

function restartBackoffMs(attempt) {
  return Math.min(TASK_RESTART_BASE_BACKOFF_MS * Math.pow(2, attempt - 1), TASK_RESTART_MAX_BACKOFF_MS);
}

function failCurrentTask(reason, opts) {
  if (!currentTask) return;
  const permanent = !!(opts && opts.permanent);
  const taskId = currentTask.task_id;
  const tmuxLines = captureTmuxLines(TMUX_TAIL_LINES);
  console.error(`Task ${taskId} failed${permanent ? ' permanently' : ''}: ${reason}`);
  send({ type: 'task_failed', seq: nextSeq(), task_id: taskId, reason, permanent, tmux_output: tmuxLines });
  currentTask = null;
  taskAssignedAt = 0;
  if (progressInterval) { clearInterval(progressInterval); progressInterval = null; }
  if (taskTimeoutHandle) { clearTimeout(taskTimeoutHandle); taskTimeoutHandle = null; }
  send({ type: 'ready', seq: nextSeq() });
}

function startProgressReporting() {
  if (progressInterval) clearInterval(progressInterval);
  if (taskTimeoutHandle) clearTimeout(taskTimeoutHandle);
  if (!taskAssignedAt) taskAssignedAt = Date.now();
  lastProgressTick = Date.now();

  taskTimeoutHandle = setTimeout(() => {
    if (currentTask) {
      failCurrentTask(`task exceeded max duration (${MAX_TASK_DURATION_MS / 60000}min)`);
    }
  }, MAX_TASK_DURATION_MS);

  progressInterval = setInterval(progressTick, PROGRESS_REPORT_INTERVAL_MS);
}

// One iteration of the progress/completion/crash-detection loop. Extracted from
// the setInterval body so it can be driven deterministically from tests.
function progressTick() {
  lastProgressTick = Date.now();
  if (!currentTask) return;
  if (Date.now() - taskAssignedAt < TASK_GRACE_PERIOD_MS) return;

  try {
    let procs = '';
    try {
      if (fs.existsSync('/proc')) {
        procs = execSync(`for p in /proc/[0-9]*/cmdline; do tr "\\0" " " < "$p" 2>/dev/null; done`, { encoding: 'utf8', timeout: 15000 });
      } else {
        procs = execSync(`ps -eo command 2>/dev/null`, { encoding: 'utf8', timeout: 15000 });
      }
    } catch (_) { procs = BACKEND; }
    const cliAlive = procs.includes(BACKEND) || procs.includes('claude') || procs.includes('copilot') || procs.includes('bob') || procs.includes('codex') || procs.includes('goose') || procs.includes('pi');
    // bob is not a persistent REPL: it exits at the end of every turn ("Bob
    // goes to sleep 💤"). For bob an exited process is the normal completion
    // signal, not a crash, so it must fall through to the checkTmuxIdle()
    // path below and be reported as task_complete. Treating it as a death
    // here reported finished work as task_failed on every single task.
    const cliExitIsNormal = BACKEND === 'bob';
    if (!cliAlive && !cliExitIsNormal) {
      const key = taskKey(currentTask);
      const attempt = (cliRestartCounts.get(key) || 0) + 1;
      cliRestartCounts.set(key, attempt);

      if (attempt > MAX_TASK_CLI_RESTARTS) {
        // Terminal give-up (issue #2203, bug 3). Do NOT relaunch on this
        // task's behalf again; report a permanent failure so the hub can
        // hand the work to a different contributor instead of looping.
        // The relay itself stays healthy and keeps accepting NEW tasks —
        // only this one work item is poisoned — so a single bad task can
        // never wedge the whole contributor.
        givenUpTasks.set(key, Date.now());
        cliRestartCounts.delete(key);
        failCurrentTask(
          `CLI process exited ${MAX_TASK_CLI_RESTARTS} times for ${key} — giving up on this task (relay still accepting other work)`,
          { permanent: true }
        );
        // Bring the CLI back so the next, different task can run.
        try { console.log(`CLI restarted: ${relaunchCLI()}`); } catch (e) { console.error('Failed to restart CLI:', e.message); }
        return;
      }

      const backoff = restartBackoffMs(attempt);
      console.error(`CLI process (${BACKEND}) died — restart ${attempt}/${MAX_TASK_CLI_RESTARTS} for ${key} after ${backoff / 1000}s backoff`);
      sleepMs(backoff);
      try {
        console.log(`CLI restarted: ${relaunchCLI()}`);
      } catch (e) {
        console.error('Failed to restart CLI:', e.message);
      }
      failCurrentTask('CLI process exited — restarted');
      return;
    }
  } catch (_) {}

  const idle = checkTmuxIdle();
  const tmuxLines = captureTmuxLines(TMUX_TAIL_LINES);
  if (idle) {
    console.log(`Task ${currentTask.task_id} completed — agent idle`);
    // Successful completion clears this work item's crash-retry budget.
    cliRestartCounts.delete(taskKey(currentTask));
    // Best-effort: report the PR the agent opened, if one is visible in its
    // recent output, so the hub can distinguish "shipped a PR" from "just went
    // idle" and pick the right issue cooldown (kubestellar/hive#2393 item 7).
    // Empty when no PR link is found — the hub then applies the short cooldown.
    const prURL = detectPRURL(tmuxLines, currentTask.repo);
    if (prURL) console.log(`Detected PR for ${currentTask.task_id}: ${prURL}`);
    send({ type: 'task_complete', seq: nextSeq(), task_id: currentTask.task_id, result: 'completed', summary: 'Agent returned to idle', tmux_output: tmuxLines, pr_url: prURL });
    // bob exits after each turn, so the pane is now a bare shell. Bring it
    // back up before the next task, or the prompt would be typed into bash
    // ("-bash: <prompt>: command not found") and silently lost.
    if (BACKEND === 'bob' && !bobIsRunning()) {
      try {
        // relaunchCLI() clears cliReady and re-arms the readiness callback,
        // which flushes any queued prompt once the CLI is confirmed up.
        console.log(`Relaunching bob for the next task: ${relaunchCLI()}`);
      } catch (e) {
        console.error('Failed to relaunch bob:', e.message);
      }
    }
    const completedRepo = currentTask.repo;
    currentTask = null;
    taskAssignedAt = 0;
    clearInterval(progressInterval);
    progressInterval = null;
    if (taskTimeoutHandle) { clearTimeout(taskTimeoutHandle); taskTimeoutHandle = null; }
    tasksCompletedCount++;
    if (tasksCompletedCount % PR_REVIEW_EVERY_N === 0) {
      console.log(`PR review cycle (${tasksCompletedCount} tasks completed) — checking open PRs`);
      currentTask = { task_id: `pr-review-${Date.now()}`, kind: 'review', repo: completedRepo, number: 0, title: 'Review open PRs for comments' };
      taskAssignedAt = Date.now();
      const reviewPrompt = `Check your open PRs on ${completedRepo} for review comments. ` +
        `Run 'GH_TOKEN=$GH_TOKEN gh pr list --repo ${completedRepo} --author @me --state open' to find them. ` +
        `For each PR with review comments, read the comments, address the feedback, push fixes, and respond. ` +
        `If no PRs have comments, just say "No PR comments to address."`;
      tmuxSendKeys(reviewPrompt);
      startProgressReporting();
    } else {
      send({ type: 'ready', seq: nextSeq() });
    }
  } else {
    send({ type: 'task_progress', seq: nextSeq(), task_id: currentTask.task_id, status: 'working', tmux_output: tmuxLines });
  }
}

function handleMessage(data) {
  let msg;
  try { msg = JSON.parse(data); } catch (_) { return; }

  switch (msg.type) {
    case 'auth_challenge':
      send({
        type: 'auth_response',
        seq: nextSeq(),
        registration_token: REG_TOKEN,
        cli_backend: BACKEND,
        model: MODEL,
        // #2547 declare half + #2567: additive, optional self-report of runtime
        // posture and protocol version. An older hub ignores these unknown fields.
        protocol_version: RELAY_PROTOCOL_VERSION,
        capabilities: detectCapabilities(),
      });
      break;

    case 'auth_ok':
      console.log(`Authenticated as ${msg.contributor_id} (tier: ${msg.trust_tier})`);
      // #2567: the hub advertises its protocol version + capability set here. We
      // log them (forward-compatible: unknown/absent fields are simply skipped)
      // so a newer relay can adapt to what the deployed server supports instead
      // of probing. No behaviour is gated on them today.
      if (msg.protocol_version || (msg.server_capabilities && msg.server_capabilities.length)) {
        console.log(`Hub protocol ${msg.protocol_version || 'unversioned'}; capabilities: ${(msg.server_capabilities || []).join(', ') || 'none'}`);
      }
      reconnectDelay = BASE_RECONNECT_DELAY_MS;
      if (!currentTask) {
        send({ type: 'ready', seq: nextSeq() });
      } else {
        console.log(`Reconnected while working on ${currentTask.repo}#${currentTask.number} — resuming`);
        send({ type: 'task_accepted', seq: nextSeq(), task_id: currentTask.task_id });
        send({ type: 'task_progress', seq: nextSeq(), task_id: currentTask.task_id, kind: currentTask.kind, repo: currentTask.repo, number: currentTask.number, title: currentTask.title, status: 'working' });
        startProgressReporting();
      }
      break;

    case 'auth_failed':
      console.error(`Authentication failed: ${msg.reason}`);
      if (msg.accepted_models && msg.accepted_models.length > 0) {
        console.error('\nThis hive accepts the following models:');
        msg.accepted_models.forEach(m => console.error('  - ' + m));
        console.error('\nSet your model: export AGENT_MODEL=<model>');
      }
      process.exit(1);
      break;

    case 'task_assign':
      if (currentTask) {
        console.log(`Rejecting task ${msg.repo}#${msg.number} — already working on ${currentTask.repo}#${currentTask.number}`);
        send({ type: 'task_failed', seq: nextSeq(), task_id: msg.task_id, reason: 'Already has active task' });
        break;
      }
      // A task we already gave up on permanently must not restart the loop if
      // the hub reassigns it anyway (issue #2203, bug 3). Reject it up front
      // and stay available for other work.
      if (isGivenUp(taskKey(msg))) {
        console.log(`Rejecting ${taskKey(msg)} — previously given up on after ${MAX_TASK_CLI_RESTARTS} CLI crashes`);
        send({ type: 'task_failed', seq: nextSeq(), task_id: msg.task_id, reason: `previously given up on after ${MAX_TASK_CLI_RESTARTS} CLI crashes`, permanent: true });
        send({ type: 'ready', seq: nextSeq() });
        break;
      }
      currentTask = msg;
      console.log(`Task assigned: ${msg.kind} ${msg.repo}#${msg.number} — ${msg.title}`);
      if (msg.github_token) {
        injectGhToken(msg.github_token);
        tokenExpiresAt = msg.token_expires_at ? new Date(msg.token_expires_at).getTime() : null;
      }
      fs.writeFileSync(TASK_FILE, JSON.stringify(msg, null, 2));
      send({ type: 'task_accepted', seq: nextSeq(), task_id: msg.task_id });
      const taskPrompt = msg.prompt || `Work on ${msg.kind} ${msg.repo}#${msg.number}: ${msg.title}`;
      // tmuxSendKeys() itself queues when the CLI is not confirmed ready, so
      // there is a single gate rather than two that can disagree.
      tmuxSendKeys(taskPrompt);
      startProgressReporting();
      break;

    case 'token_refresh':
      if (msg.github_token) {
        injectGhToken(msg.github_token);
        tokenExpiresAt = msg.token_expires_at ? new Date(msg.token_expires_at).getTime() : null;
        console.log('GitHub token refreshed');
      }
      break;

    case 'task_revoke':
      console.log(`Task revoked: ${msg.task_id} — ${msg.reason}`);
      currentTask = null;
      taskAssignedAt = 0;
      if (progressInterval) { clearInterval(progressInterval); progressInterval = null; }
      send({ type: 'ready', seq: nextSeq() });
      break;

    case 'task_unavailable':
      // #2436 finding 1/2/3: the hub explicitly declined to assign work and told
      // us why (reason: no_work / token_mint_failed / tier_disabled /
      // concurrency_limit). Surface the reason instead of hanging silently, then
      // re-ask after a delay so a transient condition (a freed slot, a fixed
      // installation permission) recovers on its own.
      console.log(`No task assigned — reason: ${msg.reason || 'unspecified'}; retrying in ${TASK_UNAVAILABLE_RETRY_MS / 1000}s`);
      setTimeout(() => {
        if (ws && ws.readyState === WebSocket.OPEN && !currentTask) {
          send({ type: 'ready', seq: nextSeq() });
        }
      }, TASK_UNAVAILABLE_RETRY_MS);
      break;

    case 'ping':
      send({ type: 'pong', seq: msg.seq });
      break;

    case 'pong':
      lastPong = Date.now();
      break;

    default:
      console.log('Unknown message type:', msg.type);
  }
}

let connectGeneration = 0;
let reconnectTimer = null;

function connect() {
  cleanup();
  if (reconnectTimer) { clearTimeout(reconnectTimer); reconnectTimer = null; }
  if (ws) { try { ws.removeAllListeners(); ws.terminate(); } catch (_) {} }
  const gen = ++connectGeneration;
  console.log(`Connecting to ${HUB_URL}...`);
  ws = new WebSocket(HUB_URL);

  ws.on('open', () => {
    if (gen !== connectGeneration) return;
    console.log('Connected to hub');
    reconnectDelay = BASE_RECONNECT_DELAY_MS;
    lastPong = Date.now();

    heartbeatInterval = setInterval(() => {
      if (gen !== connectGeneration) { clearInterval(heartbeatInterval); return; }
      if (Date.now() - lastPong > HEARTBEAT_TIMEOUT_MS) {
        console.error('Heartbeat timeout — reconnecting');
        ws.terminate();
        return;
      }
      send({ type: 'ping', seq: nextSeq() });
    }, HEARTBEAT_INTERVAL_MS);
  });

  ws.on('message', (data) => {
    if (gen !== connectGeneration) return;
    handleMessage(data.toString());
  });

  ws.on('close', () => {
    if (gen !== connectGeneration) return;
    console.log(`Connection closed. Reconnecting in ${reconnectDelay}ms...`);
    cleanup();
    reconnectTimer = setTimeout(connect, reconnectDelay);
    reconnectDelay = Math.min(reconnectDelay * 2, MAX_RECONNECT_DELAY_MS);
  });

  ws.on('error', (err) => {
    if (gen !== connectGeneration) return;
    console.error('WebSocket error:', err.message);
  });
}

function cleanup() {
  if (heartbeatInterval) { clearInterval(heartbeatInterval); heartbeatInterval = null; }
  if (progressInterval) { clearInterval(progressInterval); progressInterval = null; }
}

process.on('SIGTERM', () => { cleanup(); process.exit(0); });
process.on('SIGINT', () => { cleanup(); process.exit(0); });

// Test hook: when HIVE_RELAY_TEST_MODE=1 the relay exposes its internals and
// does NOT open a hub connection, so contributor-relay.test.js can drive the
// restart/queueing/give-up logic directly. Production runs never set this.
if (process.env.HIVE_RELAY_TEST_MODE === '1') {
  module.exports = {
    buildLaunchCommand,
    handleMessage,
    tmuxSendKeys,
    flushPendingTask,
    relaunchCLI,
    failCurrentTask,
    startProgressReporting,
    progressTick,
    // Run one progress tick with the grace period already elapsed.
    __crashTick: () => { taskAssignedAt = Date.now() - TASK_GRACE_PERIOD_MS - 1; progressTick(); },
    cleanup,
    restartBackoffMs,
    NO_MODEL_FLAG_BACKENDS,
    MAX_TASK_CLI_RESTARTS,
    setCliReady: (v) => { cliReady = v; },
    getCliReady: () => cliReady,
    getPendingTask: () => pendingTask,
    setPendingTask: (v) => { pendingTask = v; },
    getCurrentTask: () => currentTask,
    setWs: (w) => { ws = w; },
  };
} else {
  connect();
}
