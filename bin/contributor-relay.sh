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
  if (pendingTask) {
    const task = pendingTask;
    pendingTask = null;
    tmuxSendKeys(task);
  }
}).catch(e => console.error(e.message));

const ENTER_COUNT = 3;
const ENTER_DELAY_MS = 300;

function sleepMs(ms) {
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
      try {
        const confPaths2 = ['/usr/local/etc/hive/backends.conf', path.join(process.cwd(), 'config/backends.conf')];
        const confPath2 = confPaths2.find(p => fs.existsSync(p)) || confPaths2[0];
        const CMD = execSync(`bash -c 'source ${confPath2} 2>/dev/null; backend_binary ${BACKEND}'`, { encoding: 'utf8', timeout: 15000 }).trim() || BACKEND;
        const PERM = execSync(`bash -c 'source ${confPath2} 2>/dev/null; backend_perm_flag ${BACKEND}'`, { encoding: 'utf8', timeout: 15000 }).trim();
        execSync(`tmux send-keys -t ${TMUX_SESSION} '${CMD} ${PERM}' Enter`, { timeout: 15000 });
        cliReady = false;
        waitForCLI().then(() => { cliReady = true; if (pendingTask) { const t = pendingTask; pendingTask = null; tmuxSendKeys(t); } }).catch(() => {});
        sleepMs(10000);
      } catch (e) { console.error('CLI restart failed:', e.message); }
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

function failCurrentTask(reason) {
  if (!currentTask) return;
  const taskId = currentTask.task_id;
  const tmuxLines = captureTmuxLines(TMUX_TAIL_LINES);
  console.error(`Task ${taskId} failed: ${reason}`);
  send({ type: 'task_failed', seq: nextSeq(), task_id: taskId, reason, tmux_output: tmuxLines });
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

  progressInterval = setInterval(() => {
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
      if (!cliAlive) {
        console.error(`CLI process (${BACKEND}) died — restarting and reporting task as failed`);
        try {
          const confPaths = ['/usr/local/etc/hive/backends.conf', path.join(process.cwd(), 'config/backends.conf')];
          const confPath = confPaths.find(p => fs.existsSync(p)) || confPaths[0];
          const CMD = execSync(`bash -c 'source ${confPath} 2>/dev/null; backend_binary ${BACKEND}'`, { encoding: 'utf8', timeout: 15000 }).trim() || BACKEND;
          const PERM = execSync(`bash -c 'source ${confPath} 2>/dev/null; backend_perm_flag ${BACKEND}'`, { encoding: 'utf8', timeout: 15000 }).trim();
          execSync(`tmux send-keys -t ${TMUX_SESSION} '${CMD} ${PERM}' Enter`, { timeout: 15000 });
          console.log(`CLI restarted: ${CMD} ${PERM}`);
          cliReady = false;
          waitForCLI().then(() => { cliReady = true; }).catch(() => {});
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
      send({ type: 'task_complete', seq: nextSeq(), task_id: currentTask.task_id, result: 'completed', summary: 'Agent returned to idle', tmux_output: tmuxLines });
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
  }, PROGRESS_REPORT_INTERVAL_MS);
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
      });
      break;

    case 'auth_ok':
      console.log(`Authenticated as ${msg.contributor_id} (tier: ${msg.trust_tier})`);
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
      currentTask = msg;
      console.log(`Task assigned: ${msg.kind} ${msg.repo}#${msg.number} — ${msg.title}`);
      if (msg.github_token) {
        injectGhToken(msg.github_token);
        tokenExpiresAt = msg.token_expires_at ? new Date(msg.token_expires_at).getTime() : null;
      }
      fs.writeFileSync(TASK_FILE, JSON.stringify(msg, null, 2));
      send({ type: 'task_accepted', seq: nextSeq(), task_id: msg.task_id });
      const taskPrompt = msg.prompt || `Work on ${msg.kind} ${msg.repo}#${msg.number}: ${msg.title}`;
      if (cliReady) {
        tmuxSendKeys(taskPrompt);
      } else {
        console.log('CLI not ready yet — queuing task prompt');
        pendingTask = taskPrompt;
      }
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

connect();
