#!/usr/bin/env node
// contributor-relay.sh — ClankeR, the contributor relay: the WebSocket client
// that connects a contributor agent to the Hive hub.
//
// Handles: authentication, task receipt, GitHub token injection, result reporting,
// heartbeat, and reconnection with exponential backoff.
//
// Environment:
//   HIVE_HUB              — WebSocket URL (wss://host:port/contribute);
//                           comma-separated URLs subscribe to multiple hubs
//   HIVE_REGISTRATION_TOKEN — contributor's registration token; for multiple
//                           hubs, provide one comma-separated token per hub in
//                           the same order as HIVE_HUB
//   AGENT_BACKEND          — CLI backend name (claude, copilot, gemini, etc.)
//   AGENT_MODEL            — model override (optional). When unset, the relay
//                           auto-detects the running model from the CLI's own
//                           session transcript for claude/copilot/bob (#4117);
//                           other backends report no model, as before.
//   AGENT_REASONING_EFFORT — reasoning effort override (optional). Consumed by
//                           codex (-c model_reasoning_effort) and by agy
//                           (--effort low|medium|high); ignored elsewhere.
//   HIVE_AGENT_ROLE        — optional spoke agent role to claim (scanner,
//                           quality, outreach, etc.; hub-enforced)
//   HIVE_AGENT_SESSION     — tmux session name for the agent (default: contributor)

'use strict';

const WebSocket = require('ws');
const { execSync, execFile, execFileSync } = require('child_process');
const fs = require('fs');
const path = require('path');
const {
  parsePiModelSelection,
  redactPiCredentials,
  piReadiness,
} = require('./pi-backend.js');

const rawHub = process.env.HIVE_HUB || 'wss://hive.kubestellar.io:3001/contribute';
// Multi-hub (hivecommons/hive#multi-hive): HIVE_HUB and HIVE_REGISTRATION_TOKEN
// may each be a comma-separated list, one token per hub in the same order, so
// one relay/CLI session can hold work from more than one hive without running
// duplicate contributor processes. A single value of each (the common case)
// behaves exactly as before — this only ever adds hubs, never changes
// single-hub behaviour.
const rawHubList = rawHub.split(',').map(s => s.trim()).filter(Boolean);
const rawTokenList = (process.env.HIVE_REGISTRATION_TOKEN || '').split(',').map(s => s.trim()).filter(Boolean);
if (rawHubList.length > 1 && rawTokenList.length !== rawHubList.length) {
  console.error(`FATAL: HIVE_HUB lists ${rawHubList.length} hub(s) but HIVE_REGISTRATION_TOKEN lists ${rawTokenList.length} token(s) — need one registration token per hub, in the same order.`);
  process.exit(1);
}
const BACKEND = process.env.AGENT_BACKEND || 'claude';
// GOOSE_MODEL is a Goose-only compatibility input. Letting it fall back for Pi
// made a restart silently select a Goose model the initial Pi launcher never
// requested (#5039).
const MODEL = process.env.AGENT_MODEL || (BACKEND === 'goose' ? process.env.GOOSE_MODEL : '') || '';
const PI_SELECTION = parsePiModelSelection(MODEL);
// Process environment is immutable for a running container in normal use. Keep
// the startup view so readiness/redaction stays consistent across reconnects
// (and so a later test/process mutation cannot change the declared contract).
const PI_ENV = BACKEND === 'pi' ? { ...process.env } : {};
let piInvocationState = 'untested';
const REASONING_EFFORT = process.env.AGENT_REASONING_EFFORT || '';
const AGENT_ROLE = (process.env.HIVE_AGENT_ROLE || '').trim();
// HIVE_SESSION — optional session label (multi-session-per-account). One GitHub
// account has one contributor identity per hub, and the hub keys task
// leases/cooldowns/ownership on that identity, so two relays under the same
// account would collide on a single active-task slot. Declaring a distinct
// session gives each relay an independent session-scoped identity
// (ContributorID#session) on the hub while auth/tier stay per-account. Defaults
// to the backend name so the common case — one relay per CLI backend under one
// account — works with no extra config. Omitted only if explicitly emptied.
const AGENT_SESSION = (process.env.HIVE_SESSION !== undefined
  ? process.env.HIVE_SESSION
  : BACKEND).trim();
// Neutral directory both entrypoints launch the CLI from ($HOME). Used to pin
// the cwd on relaunch; see launchCommandWithCwd for why the relay's own cwd is
// the wrong answer in local mode.
const AGENT_CWD = (process.env.HIVE_AGENT_CWD || '').trim();
// AGENT_LAUNCH_CMD is the launch line resolved by the entrypoint that started
// the pane. Local mode uses stricter sandbox flags than container mode; a relay
// restart must reuse that exact posture instead of deriving container defaults.
const ENTRYPOINT_LAUNCH_CMD = (process.env.AGENT_LAUNCH_CMD || '').trim();
const TMUX_SESSION = process.env.HIVE_AGENT_SESSION || 'contributor';
// Where the hub-delivered, task-scoped token is written (injectGhToken). This
// deliberately does NOT default to /var/run/hive-metrics/gh-app-token.cache:
// that filename is the hub's FULL-privilege installation-token cache
// (bin/gh-app-token.sh, root-owned 0600 since audit H3). A relay started on a
// host that also runs hive-hub components (native install) would either
// clobber that cache with a short-lived repo-scoped token (relay running as
// root) or die on EACCES trying (any other uid — the write was uncaught), and
// detectCapabilities() would misreport the hub's own cache as this relay's
// credential. Distinct filename, same directory, so the contributor container
// (which owns /var/run/hive-metrics — src/Dockerfile.contributor) behaves as
// before. See hivecommons/hive#1861 / #3842 (audit N14).
const GH_TOKEN_CACHE = process.env.HIVE_GH_TOKEN_CACHE || (fs.existsSync('/var/run/hive-metrics')
  ? '/var/run/hive-metrics/contributor-gh-token.cache'
  : '/tmp/hive-gh-token.cache');
const TASK_FILE = process.env.HIVE_TASK_FILE || '/tmp/contributor-task.json';

// --- Delivery mode (hivecommons/hive#2538) -------------------------------
// The relay can deliver a task to the backend CLI in one of two ways:
//
//   interactive (default) — the legacy path: type the prompt into a live
//     tmux pane with `tmux send-keys` and scrape the pane for readiness,
//     progress and completion. Requires an attached-or-attachable TTY and is
//     unchanged by this feature.
//
//   headless — the non-interactive path added for #2538: drive the backend
//     CLI in a one-shot / print invocation (`claude -p`, `copilot -p`,
//     `codex exec`, …), capture its stdout/stderr, and report completion or a
//     REAL error back over the same WebSocket channel. No tmux, no pane
//     scraping, no waiting on an invisible prompt — so a K8s Job/Deployment
//     running this mode either runs to completion or fails loudly (the exact
//     "healthy-looking but stalled pod" failure #2538 warns about), and it
//     never needs a human to attach and type `/login`.
//
// This is opt-in and additive: absent/any-other value keeps the interactive
// path exactly as before. K8s manifests (#2549) and the credential boundary
// (#2537) are the explicit follow-ons and are NOT built here.
const MODE_INTERACTIVE = 'interactive';
const MODE_HEADLESS = 'headless';
const CONTRIBUTOR_MODE = process.env.CONTRIBUTOR_MODE === MODE_HEADLESS
  ? MODE_HEADLESS
  : MODE_INTERACTIVE;
// contributor-agent.sh creates and exports this before starting the relay. Pin
// it at process startup just like CONTRIBUTOR_MODE so a later environment
// mutation cannot make the one-shot CLI run outside the workspace that was
// granted to Codex with --add-dir.
const TASK_WORKSPACE_DIR = process.env.HIVE_WORKSPACE_DIR || process.cwd();

// Where the headless runner records its current lifecycle state as JSON, so a
// supervising process (or a future K8s liveness/readiness probe reading the
// file) can distinguish waiting / working / done / failed — instead of a pod
// that merely looks alive. Best-effort: a write failure never aborts a task.
const HEADLESS_STATUS_FILE = process.env.HIVE_HEADLESS_STATUS_FILE || '/tmp/contributor-headless-status.json';

// Coarse lifecycle states reported by the headless runner. Named so probes and
// logs agree on the vocabulary rather than matching free text.
const HEADLESS_STATE_WAITING = 'waiting'; // authenticated, no task in flight
const HEADLESS_STATE_WORKING = 'working'; // one-shot CLI invocation running
const HEADLESS_STATE_DONE = 'done';       // last task completed (exit 0)
const HEADLESS_STATE_FAILED = 'failed';   // last task failed (non-zero/spawn error)

// Interactive pane classifier states. Keep this vocabulary small and explicit:
// "not complete" splits into active work vs. human input needed so the relay
// never reports success for a turn that is actually sitting at a question.
const PANE_STATE_WORKING = 'WORKING';
const PANE_STATE_BLOCKED_ON_HUMAN = 'BLOCKED_ON_HUMAN';
const PANE_STATE_IDLE_COMPLETE = 'IDLE_COMPLETE';
// A retryable API failure left the CLI parked at its idle prompt with the
// response truncated (hivecommons/hive#5094). This is NOT completion and NOT a
// stall: the turn ended, but it ended in an error, and the same request can
// succeed on a retry.
const PANE_STATE_TRANSIENT_API_ERROR = 'TRANSIENT_API_ERROR';
// An API failure a retry CANNOT clear — an authorization refusal or an exhausted
// quota — left the CLI parked at its idle prompt. Also not completion: the turn
// ended having shipped nothing. Retrying it would loop the agent against a wall,
// so this is failed at once rather than nudged.
const PANE_STATE_FATAL_API_ERROR = 'FATAL_API_ERROR';
// An API failure matching NEITHER curated list ended the turn at the idle
// prompt (hivecommons/hive#5121). Still not completion: the turn shipped
// nothing. Nobody can say from a pattern table whether a retry clears it, so
// it takes the bounded transient path — if it was retryable the retry wins,
// and if not the budget runs out and the task is handed back as an honest
// environment failure. Either way, never a fabricated completion.
const PANE_STATE_UNKNOWN_API_ERROR = 'UNKNOWN_API_ERROR';

// ── Transient API-error recovery (hivecommons/hive#5094) ─────────────────────
//
// THE DEFECT: Claude Code prints a turn-duration summary ("✻ Cogitated for
// 9m 24s") whenever a turn ENDS — including when it ends in an API error — and
// classifyTmuxPane's claude branch matched exactly that line as its completion
// marker. An errored turn was therefore indistinguishable from a finished one,
// so the relay reported task_complete for work that shipped nothing. Observed
// live: issue #5061 was picked up at 11:46:38 and booked "completed" at
// 11:57:40 with no PR, its half-written work still uncommitted in the tree.
//
// These patterns mirror src/pkg/agent/manager.go's transientAPIErrorPatterns
// (#4697), which the hub's own fleet has used for this same error since. Keep
// the two lists in step. Membership is deliberately narrow: every entry must be
// an error where REPEATING THE SAME REQUEST CAN SUCCEED.
const TRANSIENT_API_ERROR_PATTERNS = [
  'connection lost mid-response',
  'connection error',
  'request timed out',
  'overloaded_error',
];
// 500/502/503/529 are retryable upstream failures. Whole tokens only, so a
// request id or token count under the same "API Error:" chrome cannot trip it.
const TRANSIENT_API_ERROR_STATUS_RE = /\b(?:500|502|503|529)\b/;
// Errors a retry CANNOT fix. Claude Code renders every API failure under the
// same "API Error:" prefix, so a substring match alone cannot tell an
// overloaded upstream from a refused one — these are re-checked separately and
// veto the retry, exactly as the hub path does via
// lineShowsUpstreamAuthorizationError / paneShowsQuotaExhausted. Nudging one of
// these loops the agent against a wall and burns tokens to no effect.
const UNRETRYABLE_API_ERROR_PATTERNS = [
  'not allowed to access model',
  'team not allowed to access',
  'exceeded your monthly quota',
  'used all your copilot free chat requests',
  'budget_exceeded',
  'budget has been exceeded',
  'provider spending limit reached',
  'refused the request on a spending limit',
  'gone over your budget allowance',
  'bobcoins',
];
// 403 is authorization, not authentication: the caller IS identified and is not
// permitted, so neither a retry nor a login changes anything (#4400).
const UNRETRYABLE_API_ERROR_STATUS_RE = /\bAPI Error: 403\b/i;
// The visible tail the error must appear in. Matching the whole pane would let
// an error the agent already recovered from read as current.
const TRANSIENT_API_ERROR_TAIL_LINES = 12;
// What we type. Short and free of shell metacharacters by construction — it is
// interpolated into a tmux send-keys command line below.
const TRANSIENT_API_ERROR_NUDGE_MESSAGE = 'try again';
// Bounded so a persistent upstream failure ends as an honest task failure
// rather than an infinite typing loop. Mirrors the hub's cap and cooldown.
const TRANSIENT_API_ERROR_MAX_NUDGES = 3;
const TRANSIENT_API_ERROR_NUDGE_COOLDOWN_MS = 90000;

// What the relay types at an unattended pane that stopped to ask a question
// (hivecommons/hive#5281). The task prompts already tell agents to decide for
// themselves — an agent that stops to ask is one that forgot, and a human
// watching would type exactly this line. When nobody is watching, nobody does.
//
// Letters, spaces and one comma, by construction: tmuxSendNudge interpolates
// this into a single-quoted `tmux send-keys -l '...'`, so a quote or a shell
// metacharacter here would be a command-injection shaped bug rather than a
// typo. There is a test pinning that.
const AUTONOMY_NUDGE_MESSAGE =
  'no human is available to answer, so proceed autonomously with your best judgment';

// Cap on captured child output kept in memory / sent to the hub, so a chatty
// CLI cannot grow the buffer without bound. The tail is what matters for an
// audit trail, mirroring TMUX_TAIL_LINES on the interactive path.
const HEADLESS_MAX_OUTPUT_BYTES = 1048576; // 1 MiB

const TMUX_TAIL_LINES = 15;
const HEARTBEAT_INTERVAL_MS = 30000;
const HEARTBEAT_TIMEOUT_MS = 90000;
const PROGRESS_REPORT_INTERVAL_MS = 120000;
const MAX_RECONNECT_DELAY_MS = 60000;
const BASE_RECONNECT_DELAY_MS = 1000;
const TOKEN_REFRESH_MARGIN_MS = 300000;
// MAX_TASK_DURATION_MS is a PROGRESS lease, not a wall-clock budget
// (hivecommons/hive#5321). It bounds how long a task may go without the relay
// observing forward progress; every tick that sees new pane output re-arms it
// from now. It is NOT "the longest a task may take".
//
// It used to be exactly that, and the result was a bug: the timer was armed
// once in startProgressReporting() and never re-armed, so a task was killed at
// a flat 30 minutes however hard the agent was working. Observed live on
// 2026-08-31 it killed an agent that had already committed and pushed and was
// blocked on a green `go test` run — the hub booked the task `failed` 57
// seconds before that task's PR (#5320) was opened, and returned the issue to
// the failure cooldown. Any task whose honest duration exceeds this bound was
// not slow, it was impossible.
//
// The hang case the wall was nominally there for is covered — better — by
// PANE_STALL_TIMEOUT_MS, which fails a frozen pane in 20 minutes and confirms
// the verdict over multiple ticks. What remains here is a coarser second
// opinion on the same question, kept because it is armed from the timer wheel
// rather than from the tick loop and so still fires if the tick loop itself
// dies.
const MAX_TASK_DURATION_MS = 1800000;

// ABSOLUTE_TASK_DEADLINE_MS is the backstop the progress lease deliberately
// does not provide: a ceiling on total elapsed time from task assignment,
// re-armed by nothing. A task that produces output forever (a retry loop
// redrawing a spinner is output) would otherwise hold its lease indefinitely.
//
// Set far above the working range — the point is to bound the pathological
// case, not to second-guess a long one. Crossing it is a statement about this
// runtime, not about the agent's work, so it is reported as an `environment`
// failure (see the failCurrentTask contract).
const ABSOLUTE_TASK_DEADLINE_MS = Number(process.env.HIVE_ABSOLUTE_TASK_DEADLINE_MS) || 4 * 60 * 60 * 1000;

// Hard ceiling on a single headless one-shot invocation (hivecommons/hive#2538).
// The interactive path has no pane to scrape for progress on the headless path,
// so a headless child cannot use the progress lease above: there is no
// equivalent signal. It gets the ABSOLUTE bound enforced directly on the
// process instead, so a wedged CLI is killed and reported failed rather than
// hanging the pod forever — and, per #5321, a long-but-live headless run is no
// longer killed at 30 minutes either.
const HEADLESS_TASK_TIMEOUT_MS = Number(process.env.HIVE_HEADLESS_TASK_TIMEOUT_MS) || ABSOLUTE_TASK_DEADLINE_MS;
const NETWORK_ERROR_RETRY_DELAY_MS = 5000;
// After the hub sends an explicit task_unavailable negative-ack (no admissible
// work, a disabled tier, a concurrency limit, or a token-mint failure — see
// hivecommons/hive#2436), wait before re-asking so we neither hang forever
// (the old silent-nil behaviour) nor busy-loop the hub.
const TASK_UNAVAILABLE_RETRY_MS = 30000;

// RELAY_PROTOCOL_VERSION is the contributor-protocol version this relay speaks
// (hivecommons/hive#2567). It is DECLARED to the hub in auth_response (additive,
// optional — an older hub simply ignores it) and the hub advertises its own
// version + capability set back on auth_ok.
//
// MUST equal contributorProtocolVersion in src/pkg/dashboard/contribute_protocol.go:
// the hub and this relay ship from the same tree, so they speak the same version
// by construction. That was previously only a comment, and it drifted — #2600
// shipped both at 1.1, #2671 bumped the hub to 1.2 for credential_after_accept
// (handled by the token_refresh case below) and left this at 1.1, so the relay
// under-declared itself for months with nothing to notice. It is now pinned by
// TestRelayProtocolVersionMatchesHub, which fails the build on the next drift.
const RELAY_PROTOCOL_VERSION = '1.2';

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

if (rawTokenList.length === 0) {
  console.error('FATAL: HIVE_REGISTRATION_TOKEN not set. Run `just contribute-register` first.');
  process.exit(1);
}

// One entry per hub, each owning its own connection/reconnect/heartbeat
// state. currentTask, cliReady and everything CLI-facing stay single global
// values below — there is exactly one CLI/tmux session, shared across
// whichever hub currently holds the active task or is being polled for work.
const hubs = rawHubList.map((url, i) => ({
  url: url.replace(/\/contribute\/?$/, '/api/contribute/ws'),
  regToken: rawTokenList[i] || rawTokenList[0],
  ws: null,
  reconnectDelay: BASE_RECONNECT_DELAY_MS,
  heartbeatInterval: null,
  lastPong: Date.now(),
  connectGeneration: 0,
  reconnectTimer: null,
  authenticated: false,
  authFailed: false,
  // #2547: set once we have reported a contributor-protocol difference with
  // this hub, so a reconnect loop does not repeat the same advisory line.
  protocolDriftReported: false,
}));
// Index into hubs[] of the hub we are currently soliciting work from (sent it
// the last 'ready'), or that owns currentTask. Round-robins forward on an
// explicit task_unavailable from the active hub; sticks with the same hub
// across a completed/failed/revoked task rather than switching eagerly, since
// task_unavailable is the only signal (hivecommons/hive#2436/#2546 — the hub
// always sends it, never stays silent) that a hub genuinely has no work.
let activeHubIndex = 0;

let seq = 0;
let currentTask = null;
let progressInterval = null;
let tokenExpiresAt = null;
// tokenRefreshFailedAt records when the hub last told us a mid-task re-mint
// FAILED (a token_refresh_failed, hivecommons/hive#5447). Null means "no known
// refresh problem"; a successful token_refresh clears it, because a fresh
// credential resolves the condition. It exists so the expiry warning below can
// distinguish "the hub is quiet and our clock may simply be off" from "the hub
// told us it could not renew this credential", which is the difference between a
// guess and a diagnosis.
let tokenRefreshFailedAt = null;

// TOKEN_EXPIRY_WARN_MS is how far ahead of expiry the relay starts warning. It
// is one full progress interval plus a margin, so a task that is about to lose
// push access says so at least one tick BEFORE the first push can fail, rather
// than reporting it afterwards.
const TOKEN_EXPIRY_WARN_MS = 5 * 60 * 1000;
// TOKEN_EXPIRY_WARN_INTERVAL_MS throttles the warning so a long task past expiry
// logs periodically instead of on every single progress tick.
const TOKEN_EXPIRY_WARN_INTERVAL_MS = 10 * 60 * 1000;
let lastTokenExpiryWarnAt = 0;

// tokenLifetimeStatus turns the hub-supplied token_expires_at into the relay's
// own read of its credential: how long is left, whether we are inside the warning
// window, and whether the hub has reported a failed renewal.
//
// It is PURE and clock-injectable so the expiry logic can be tested without
// waiting an hour, and it deliberately reports rather than decides — see
// warnOnTokenExpiry() for why this only ever warns.
function tokenLifetimeStatus(now = Date.now()) {
  if (!tokenExpiresAt) {
    return { known: false, expired: false, expiring: false, remainingMs: null, refreshFailed: tokenRefreshFailedAt !== null };
  }
  const remainingMs = tokenExpiresAt - now;
  return {
    known: true,
    expired: remainingMs <= 0,
    expiring: remainingMs <= TOKEN_EXPIRY_WARN_MS,
    remainingMs,
    refreshFailed: tokenRefreshFailedAt !== null,
  };
}

function formatDuration(ms) {
  const abs = Math.abs(ms);
  const mins = Math.floor(abs / 60000);
  const secs = Math.floor((abs % 60000) / 1000);
  return mins > 0 ? `${mins}m${secs}s` : `${secs}s`;
}

// warnOnTokenExpiry logs — and ONLY logs — when the task's credential is at or
// past its advertised expiry (hivecommons/hive#5447).
//
// It does NOT refuse the push, and that is deliberate. The relay's clock and the
// hub's are independent; tokenExpiresAt is the HUB's wall-clock stamp read on the
// relay's, so a machine with a few minutes of skew would refuse work on a
// perfectly valid credential. Refusing on a bad clock is strictly worse than
// today's behaviour, where the token simply works. The authority on whether a
// token is good remains GitHub's answer to the actual call; this turns the
// resulting failure from an unexplained auth error into a named, already-logged
// condition — which is the whole point of the issue.
//
// Throttled, and never touches the token itself.
function warnOnTokenExpiry(now = Date.now()) {
  const status = tokenLifetimeStatus(now);
  if (!status.known || !status.expiring) return null;
  if (now - lastTokenExpiryWarnAt < TOKEN_EXPIRY_WARN_INTERVAL_MS) return null;
  lastTokenExpiryWarnAt = now;
  const cause = status.refreshFailed
    ? ' — the hub reported that it could not renew this credential, so pushes may fail with a generic auth error'
    : '';
  const msg = status.expired
    ? `GitHub token expired ${formatDuration(status.remainingMs)} ago${cause}`
    : `GitHub token expires in ${formatDuration(status.remainingMs)}${cause}`;
  console.warn(msg);
  return msg;
}

function nextSeq() { return ++seq; }

function sendTo(hub, msg) {
  if (hub && hub.ws && hub.ws.readyState === WebSocket.OPEN) {
    hub.ws.send(JSON.stringify(msg));
  }
}

// send() targets whichever hub is relevant right now: the hub that owns the
// in-flight task, or (idle) the hub currently being polled for work. Every
// existing interactive/headless/progress/heartbeat call site keeps calling
// plain send(msg) unchanged — only the messages that must go to a SPECIFIC
// hub regardless of currentTask/activeHubIndex (auth handshake, rejecting a
// task from a hub that isn't getting the active slot, per-hub ping/pong) use
// sendTo() directly.
// The `|| hubs[activeHubIndex]` fallback is load-bearing, not defensive
// padding: not every currentTask comes from a task_assign. The synthetic
// pr-review task built after every PR_REVIEW_EVERY_N completions is assembled
// locally and has no _hub, so keying strictly off currentTask._hub sent its
// progress and completion frames to `undefined` — silently dropped, leaving
// the hub to watch the contributor go mute mid-review and time it out.
// Falling back to the active hub is also the correct target there: it is the
// hub whose task we just finished.
function send(msg) {
  sendTo((currentTask && currentTask._hub) || hubs[activeHubIndex], msg);
}

function currentTaskHub() {
  return (currentTask && currentTask._hub) || hubs[activeHubIndex];
}

function advanceActiveHub(fromHub) {
  const fromIndex = hubs.indexOf(fromHub);
  const start = fromIndex >= 0 ? fromIndex : activeHubIndex;
  for (let offset = 1; offset <= hubs.length; offset++) {
    const idx = (start + offset) % hubs.length;
    if (!hubs[idx].authFailed) {
      activeHubIndex = idx;
      return hubs[idx];
    }
  }
  return null;
}

function injectGhToken(token) {
  const dir = path.dirname(GH_TOKEN_CACHE);
  try { fs.mkdirSync(dir, { recursive: true }); } catch (_) {}
  // A failed write must never throw out of handleMessage: task_assign calls
  // this before task_accepted is sent, so an unwritable cache path (EACCES on
  // a root-owned directory, HIVE_GH_TOKEN_CACHE pointing somewhere bad) would
  // crash the relay on every assignment that carries a token — a crash loop,
  // not a degraded mode. The agent can still work with its own GH_TOKEN, so
  // log loudly and carry on.
  try {
    fs.writeFileSync(GH_TOKEN_CACHE, token, { mode: 0o600 });
  } catch (e) {
    console.error(`Failed to write GitHub token cache ${GH_TOKEN_CACHE}: ${e.message} — continuing without it`);
  }
}

const CLI_READY_POLL_MS = 2000;
const CLI_READY_TIMEOUT_MS = 600000;
const CONTAINER_NAME = process.env.HIVE_CONTAINER_NAME || 'hive-contributor';
// ATTACH_COMMAND is the paste-able command that puts a human on the CLI's tmux
// pane. It is computed once, here, because it is printed at the one moment a
// wrong answer really costs: the "needs authentication" banner fires when the
// agent is BLOCKED and a person must intervene, so a command that fails is
// worse than no command at all (hivecommons/hive#5145).
//
// Two facts the relay cannot infer and so is told:
//
//   * HIVE_CONTAINER_NAME is set ONLY by the container arm of the
//     `just contribute-hive` recipe. Local mode runs this relay directly on the
//     host, beside the tmux server it drives — there is no container to exec
//     into, and the hint is plain `tmux attach`, which is what the recipe's own
//     status line four lines earlier already says.
//   * HIVE_CONTAINER_RUNTIME carries the engine the recipe resolved. A
//     container cannot see its own launcher, so hardcoding "docker" handed
//     every podman operator a command that fails. It defaults to docker, so a
//     bare-docker launch prints exactly what it printed before.
const CONTAINER_RUNTIME = process.env.HIVE_CONTAINER_RUNTIME || 'docker';
const ATTACH_COMMAND = process.env.HIVE_CONTAINER_NAME
  ? `${CONTAINER_RUNTIME} exec -it ${CONTAINER_NAME} tmux attach -t ${TMUX_SESSION}`
  : `tmux attach -t ${TMUX_SESSION}`;

// detectCapabilities builds the OPTIONAL, client-declared capability object the
// relay reports in auth_response (hivecommons/hive#2547, declare half). Every
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
  // Agent CLI version: the hub schema, the operator docs and the Operations row
  // ("cli 1.2.3") have carried this field since the declare half shipped, but the
  // relay never populated it — so the one axis #2547's own evidence names first
  // ("an agent CLI old enough to lack a flag the prompt assumes") was the one an
  // operator could not see. Best-effort: omitted entirely when the probe fails.
  const cliVersion = detectAgentCLIVersion();
  if (cliVersion) caps.agent_cli_version = cliVersion;
  if (BACKEND === 'pi') Object.assign(caps, piReadiness(PI_SELECTION, !!cliVersion, piInvocationState, PI_ENV));
  cachedCapabilities = caps;
  return caps;
}

// CLI_VERSION_PROBE_TIMEOUT_MS bounds the `<cli> --version` probe. Generous
// enough for a Node/Python CLI's cold start, short enough that a wedged binary
// costs a couple of seconds rather than the handshake.
const CLI_VERSION_PROBE_TIMEOUT_MS = 3000;
// CLI_VERSION_MAX_LEN bounds what we are willing to REPORT. The value is another
// program's stdout, so it is arbitrary text; the hub bounds it again on receipt
// (ContributorCapabilities.Sanitized) because no hub should trust a client to
// have done this.
const CLI_VERSION_MAX_LEN = 64;

// detectAgentCLIVersion asks the agent CLI this relay drives for its version.
//
// Best-effort and deliberately quiet: any failure — binary absent, flag
// unsupported, CLI wedged, output unusable — yields '' and the field is simply
// omitted, which reads as "unknown" and is exactly what every relay written
// before this change reports. Declaring nothing must always remain a working
// answer (#2547: no default may read silence as incapacity).
//
// stdin is closed (`ignore`) so a CLI that mistakes --version for an interactive
// launch gets EOF and exits rather than waiting on a terminal nobody is at;
// stderr is discarded so a warning banner cannot end up declared as a version.
function detectAgentCLIVersion() {
  try {
    // resolveBackend() maps the backend NAME to its actual binary (litellm →
    // claude), and is the same resolution the launch path uses — so the version
    // reported is the version of the CLI that will really run the work.
    const bin = (resolveBackend().cmd || BACKEND).trim();
    if (!bin) return '';
    const out = execFileSync(bin, ['--version'], {
      encoding: 'utf8',
      timeout: CLI_VERSION_PROBE_TIMEOUT_MS,
      stdio: ['ignore', 'pipe', 'ignore'],
      killSignal: 'SIGKILL',
    });
    return sanitizeDeclaredValue(out);
  } catch (_) {
    // Nothing to log: an absent or unprobeable CLI is an ordinary, supported
    // state here, not a fault.
    return '';
  }
}

// sanitizeDeclaredValue reduces a CLI's version output to one short, printable
// line fit to declare. Takes the first non-empty line (CLIs append update
// nudges and banners), strips control characters, collapses whitespace, and
// truncates. The hub renders declarations into an operator row, so a multi-line
// or unbounded value would be its problem rather than ours.
function sanitizeDeclaredValue(raw) {
  if (typeof raw !== 'string') return '';
  const line = raw.split('\n').map(s => s.trim()).find(Boolean) || '';
  const clean = line.replace(/[\x00-\x1f\x7f]/g, ' ').replace(/\s+/g, ' ').trim();
  // Truncate by code POINT, not code unit, so a value carrying an astral
  // character is never cut into a lone surrogate on the way out.
  const points = Array.from(clean);
  return points.length > CLI_VERSION_MAX_LEN
    ? points.slice(0, CLI_VERSION_MAX_LEN).join('').trim()
    : clean;
}

// parseProtocolVersion mirrors the hub's parser (contribute_protocol_compat.go):
// strict "MAJOR.MINOR", both non-negative integers, nothing after the minor.
// Anything else returns null so an unrecognised shape is reported as unparseable
// rather than coerced into a confident, wrong comparison.
function parseProtocolVersion(v) {
  var m = /^\s*(\d+)\.(\d+)\s*$/.exec(String(v == null ? '' : v));
  if (!m) return null;
  return { major: parseInt(m[1], 10), minor: parseInt(m[2], 10) };
}

// classifyPeerProtocol compares a peer's declared version against ours and
// returns the same verdict vocabulary the hub uses, so the two sides describe a
// mismatch identically: 'unknown' | 'current' | 'older' | 'newer' |
// 'incompatible' | 'malformed'. 'unknown' (peer stated nothing) is the
// backward-compatible default and is never treated as a fault.
//
// The verdict always describes THE PEER, so the same 'older' means "the hub is
// older" here and "the client is older" on the hub side. That is deliberate —
// one vocabulary, each side reading it about the other — and every message
// built from it names both versions explicitly so it cannot be misread.
function classifyPeerProtocol(peer, self) {
  if (!peer || !String(peer).trim()) return 'unknown';
  var p = parseProtocolVersion(peer);
  if (!p) return 'malformed';
  var s = parseProtocolVersion(self);
  if (!s) return 'unknown';
  if (p.major !== s.major) return 'incompatible';
  if (p.minor < s.minor) return 'older';
  if (p.minor > s.minor) return 'newer';
  return 'current';
}

// warnOnProtocolDrift reports, once per hub for the life of this process, that
// the hub speaks a
// different contributor-protocol version than this relay (hivecommons/hive#2547).
// Purely informational: nothing below changes what we send, what we ask for, or
// whether we stay connected — a version is not a gate on either side. Silent when
// the versions agree or the hub is unversioned, so a healthy connection logs
// nothing extra and an old hub is not nagged about a field it never had.
function warnOnProtocolDrift(hub, hubVersion) {
  if (hub.protocolDriftReported) return;
  var verdict = classifyPeerProtocol(hubVersion, RELAY_PROTOCOL_VERSION);
  if (verdict === 'current' || verdict === 'unknown') return;
  hub.protocolDriftReported = true;
  var detail = {
    older: `hub ${hubVersion} is behind this relay ${RELAY_PROTOCOL_VERSION} — features this relay knows about may not be deployed there`,
    newer: `hub ${hubVersion} is ahead of this relay ${RELAY_PROTOCOL_VERSION} — the hub may support features this relay does not use yet`,
    incompatible: `hub ${hubVersion} differs from this relay ${RELAY_PROTOCOL_VERSION} in MAJOR version — the wire contract differs and behaviour is undefined; consider updating the relay`,
    malformed: `hub announced an unparseable protocol version; expected MAJOR.MINOR`,
  }[verdict];
  console.warn(`Protocol ${verdict}: ${detail}. Continuing normally — this is advisory and nothing is gated on it.`);
}

// Backends that must NOT be given --model, mirroring contributor-agent.sh.
// goose takes its model from config/env. bob is excluded because
// --model is actively FATAL for it: bob auto-selects its own model and passing
// one leaves its model config undefined, so every prompt dies with
// "Cannot read properties of undefined (reading 'maxTokens')" (bobshell 1.0.6).
const NO_MODEL_FLAG_BACKENDS = ['goose', 'bob'];

// agy (Google's Antigravity CLI) REQUIRES --effort whenever --model is given:
// without it agy warns "--model <m> requires --effort (available: low, medium,
// high)" and silently IGNORES the model, so the contributor's configured model
// never takes effect. AGY_DEFAULT_EFFORT mirrors agyDefaultEffort in the
// hub-side launcher (src/pkg/agent/manager.go) so a relay agent and a pod agent
// resolve the same effort. AGENT_REASONING_EFFORT can override it, but only
// with a value agy actually accepts — codex's vocabulary is wider (it takes
// "minimal"), and forwarding an unknown token here would make agy reject the
// pairing and drop the model again.
const AGY_DEFAULT_EFFORT = 'low';
const AGY_EFFORTS = ['low', 'medium', 'high'];
const agyEffort = AGY_EFFORTS.includes(REASONING_EFFORT) ? REASONING_EFFORT : AGY_DEFAULT_EFFORT;

// Single source of truth for the CLI launch command (issue #2203, bug 1).
// The entrypoint may export AGENT_LAUNCH_CMD with the exact command used for
// the FIRST launch. Prefer it so local mode keeps its sandbox/allowlist posture
// across restarts instead of rebuilding the more-permissive container default
// (#5652). Older entrypoints fall back to resolving backend flags here.
let cachedLaunchCommand = null;
let cachedBackendResolution = null;

// resolveBackend() returns the { cmd, perm } pair backends.conf maps this
// backend to (binary + permission flags). Shared by the interactive launch
// command and the headless argv builder so the two paths cannot drift on which
// binary/flags a backend uses. Result cached — the resolution is a couple of
// bash sub-shells and never changes for the life of the process.
function resolveBackend() {
  if (cachedBackendResolution) return cachedBackendResolution;
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
  cachedBackendResolution = { cmd, perm };
  return cachedBackendResolution;
}

// modelFlagFor reports the --model flag this backend actually receives, or ''
// when the backend takes no --model at all. Shared by the launch command and by
// effectiveReasoningEffort() below, which must agree on whether a model is in
// play — agy's effort is conditional on exactly that.
function modelFlagFor() {
  if (BACKEND === 'pi' && !PI_SELECTION.valid) throw new Error(PI_SELECTION.error);
  return MODEL && !NO_MODEL_FLAG_BACKENDS.includes(BACKEND) ? `--model ${MODEL}` : '';
}

function effectiveProvider() {
  return BACKEND === 'pi' && PI_SELECTION.valid ? PI_SELECTION.provider : '';
}

// Receipt fields are bounded selections, never credentials. Provider is
// transported canonically inside model; the separate field is evidence for
// local status/receipts, not another input or authority source.
function effectiveSelectionFields() {
  const out = { cli_backend: BACKEND };
  const model = effectiveModel();
  const provider = effectiveProvider();
  if (provider) out.provider = provider;
  if (model) out.model = model;
  return out;
}

function setPiInvocationState(state) {
  if (BACKEND !== 'pi') return;
  piInvocationState = state;
  if (cachedCapabilities) Object.assign(cachedCapabilities, piReadiness(PI_SELECTION, !!cachedCapabilities.agent_cli_version, state, PI_ENV));
}

// effectiveReasoningEffort is the SINGLE source of truth for the effort actually
// in effect for this launch — the value the CLI is really running with, not the
// value the contributor happened to export.
//
// It exists because the effort now travels twice: onto the command line here,
// and up to the hub in auth_response so the dashboard can show it (#4084).
// Deriving it independently in those two places is the same drift this file
// already warns about for the launch command itself (#2203 bug 1, the comment
// above cachedLaunchCommand), and it would misreport in two concrete ways:
//
//   - agy WITHOUT a model gets no --effort flag at all, so reporting a raw
//     AGENT_REASONING_EFFORT there advertises an effort agy never applied.
//   - agy WITH a model gets agyEffort, which falls back to AGY_DEFAULT_EFFORT
//     when AGENT_REASONING_EFFORT is unset or is a value agy rejects (codex's
//     vocabulary is wider), so the raw env var is the wrong answer there too.
//
// Returns '' when nothing is in effect; auth_response omits the field entirely
// in that case rather than sending an empty string.
function effectiveReasoningEffort() {
  // agy is the only backend whose effort is conditional on a model being passed.
  if (BACKEND === 'agy') return modelFlagFor() ? agyEffort : '';
  return REASONING_EFFORT || '';
}

// --- Model auto-detection from the CLI's own session transcript (#4117) ----
//
// AGENT_MODEL is optional and launch-time-only: most contributors never set it
// (Live Activity then shows just "via claude CLI"), and even a set value goes
// stale the moment the session switches models (`/model` in claude). For the
// backends whose CLIs keep a local session transcript that records which model
// served each turn — the same files src/pkg/tokens/*_scanner.go already reads
// for cost attribution — the relay can report the model ACTUALLY in use.
//
// Precedence is explicit and fixed: AGENT_MODEL if set (the contributor's
// intent overrides detection — e.g. a claude pointed at a LiteLLM proxy whose
// transcript records a spoofed name) → the model detected from the CLI's own
// transcript → '' (today's degrade, unchanged). Backends with no known local
// transcript format (codex, agy, goose, pi, aider, litellm, …) always take the
// last branch — no regression, no guess.
//
// Privacy: transcripts contain the task prompt and file contents. Detection
// reads only the TAIL bytes needed to find the latest turn's model field,
// extracts that single field, and never logs or transmits anything else.
const MODEL_DETECT_HOME = process.env.HOME || require('os').homedir() || '';
// Tail window per read. A transcript line is one JSON turn; 64 KiB comfortably
// covers the last few turns of every observed format without pulling a whole
// multi-megabyte session into memory.
const MODEL_DETECT_TAIL_BYTES = 65536;
const CLAUDE_PROJECTS_DIR = process.env.HIVE_CLAUDE_PROJECTS_DIR || path.join(MODEL_DETECT_HOME, '.claude', 'projects');
const COPILOT_SESSIONS_DIR = process.env.HIVE_COPILOT_SESSIONS_DIR || path.join(MODEL_DETECT_HOME, '.copilot', 'session-state');
const BOB_HOME_DIR = process.env.HIVE_BOB_DIR || path.join(MODEL_DETECT_HOME, '.bob');

// readFileTail returns at most the last maxBytes of a file as UTF-8, without
// reading the rest — the "minimal tail" privacy bound above.
function readFileTail(file, maxBytes) {
  const fd = fs.openSync(file, 'r');
  try {
    const size = fs.fstatSync(fd).size;
    const start = Math.max(0, size - maxBytes);
    const len = size - start;
    const buf = Buffer.alloc(len);
    fs.readSync(fd, buf, 0, len, start);
    return buf.toString('utf8');
  } finally {
    fs.closeSync(fd);
  }
}

// newestByMtime picks the most recently modified path from a list, or null.
function newestByMtime(files) {
  let best = null;
  let bestMtime = -1;
  for (const f of files) {
    try {
      const m = fs.statSync(f).mtimeMs;
      if (m > bestMtime) { bestMtime = m; best = f; }
    } catch (_) {}
  }
  return best;
}

// tailLinesReversed parses the tail of a JSONL file and yields each line's
// parsed JSON from NEWEST to oldest, skipping unparseable lines (the first
// tail line is usually a mid-line cut).
function tailLinesReversed(file) {
  const lines = readFileTail(file, MODEL_DETECT_TAIL_BYTES).split('\n');
  const out = [];
  for (let i = lines.length - 1; i >= 0; i--) {
    const line = lines[i].trim();
    if (!line) continue;
    try { out.push(JSON.parse(line)); } catch (_) {}
  }
  return out;
}

// looksLikeModelName rejects placeholder values some transcripts record for
// error/synthetic turns (claude logs "<synthetic>") — better no model than a
// confidently wrong one.
function looksLikeModelName(m) {
  return typeof m === 'string' && m !== '' && !m.startsWith('<');
}

// detectClaudeModel: newest ~/.claude/projects/*/*.jsonl, latest assistant
// turn's message.model (same source claude_scanner.go aggregates for cost).
function detectClaudeModel() {
  const files = [];
  for (const d of fs.readdirSync(CLAUDE_PROJECTS_DIR, { withFileTypes: true })) {
    if (!d.isDirectory()) continue;
    const dir = path.join(CLAUDE_PROJECTS_DIR, d.name);
    for (const f of fs.readdirSync(dir)) {
      if (f.endsWith('.jsonl')) files.push(path.join(dir, f));
    }
  }
  const newest = newestByMtime(files);
  if (!newest) return '';
  for (const obj of tailLinesReversed(newest)) {
    const m = obj && obj.message && obj.message.model;
    if (looksLikeModelName(m)) return m;
  }
  return '';
}

// detectCopilotModel: newest ~/.copilot/session-state/*/events.jsonl, latest
// event carrying a model field (session.start selectedModel, per-tool model,
// or shutdown currentModel — same fields copilot_scanner.go reads).
function detectCopilotModel() {
  const files = [];
  for (const d of fs.readdirSync(COPILOT_SESSIONS_DIR, { withFileTypes: true })) {
    if (!d.isDirectory()) continue;
    files.push(path.join(COPILOT_SESSIONS_DIR, d.name, 'events.jsonl'));
  }
  const newest = newestByMtime(files);
  if (!newest) return '';
  for (const obj of tailLinesReversed(newest)) {
    const data = (obj && obj.data) || {};
    const m = data.model || data.currentModel || data.selectedModel;
    if (looksLikeModelName(m)) return m;
  }
  return '';
}

// Bob session recordings are one JSON document, not JSONL, so a byte tail
// cannot be parsed. Cap what we are willing to read instead; sessions past
// this size just report no model rather than ballooning relay memory.
const BOB_MAX_SESSION_BYTES = 5242880; // 5 MiB
// detectBobModel: newest ~/.bob/tmp/*/chats/*.json, last message with a
// per-message model field (same shape bob_scanner.go reads).
function detectBobModel() {
  const files = [];
  const tmpDir = path.join(BOB_HOME_DIR, 'tmp');
  for (const d of fs.readdirSync(tmpDir, { withFileTypes: true })) {
    if (!d.isDirectory()) continue;
    const chats = path.join(tmpDir, d.name, 'chats');
    let entries;
    try { entries = fs.readdirSync(chats); } catch (_) { continue; }
    for (const f of entries) {
      if (f.endsWith('.json')) files.push(path.join(chats, f));
    }
  }
  const newest = newestByMtime(files);
  if (!newest) return '';
  if (fs.statSync(newest).size > BOB_MAX_SESSION_BYTES) return '';
  const session = JSON.parse(fs.readFileSync(newest, 'utf8'));
  const messages = Array.isArray(session && session.messages) ? session.messages : [];
  for (let i = messages.length - 1; i >= 0; i--) {
    if (looksLikeModelName(messages[i] && messages[i].model)) return messages[i].model;
  }
  return '';
}

const MODEL_DETECTORS = { claude: detectClaudeModel, copilot: detectCopilotModel, bob: detectBobModel };

// The last model detected from the transcript. Refreshed at auth and on every
// progress tick, so a mid-session `/model` switch is reflected within one
// PROGRESS_REPORT_INTERVAL_MS.
let detectedModel = '';

// detectRunningModel reads the transcript once and returns the model, or ''.
// Never throws; never runs at all when AGENT_MODEL is set (explicit intent
// wins, so there is nothing to detect) or the backend has no known transcript.
function detectRunningModel() {
  if (MODEL) return '';
  const detector = MODEL_DETECTORS[BACKEND];
  if (!detector) return '';
  try { return sanitizeDeclaredValue(detector() || ''); } catch (_) { return ''; }
}

// refreshDetectedModel re-detects and returns the model currently in effect
// under the fixed precedence (AGENT_MODEL → detected → '').
function refreshDetectedModel() {
  const m = detectRunningModel();
  if (m && m !== detectedModel) {
    detectedModel = m;
    console.log(`Detected running model from ${BACKEND} session transcript: ${m}`);
  }
  return effectiveModel();
}

// effectiveModel is the model counterpart of effectiveReasoningEffort(): the
// single source of truth for the model actually reported to the hub.
function effectiveModel() {
  return MODEL || detectedModel || '';
}

// progressModelFields returns the optional model/effort fields piggybacked on
// periodic task_progress reports, so the hub can track a mid-session model
// switch. Empty values are omitted entirely (an older hub ignores the fields).
function progressModelFields() {
  const out = {};
  const model = effectiveModel();
  const effort = effectiveReasoningEffort();
  if (model) out.model = model;
  if (effort) out.reasoning_effort = effort;
  return out;
}

function buildLaunchCommand() {
  if (cachedLaunchCommand) return cachedLaunchCommand;
  if (ENTRYPOINT_LAUNCH_CMD) {
    cachedLaunchCommand = ENTRYPOINT_LAUNCH_CMD;
    return cachedLaunchCommand;
  }
  const { cmd, perm } = resolveBackend();
  const modelFlag = modelFlagFor();
  const reasoningFlag = BACKEND === 'codex' && REASONING_EFFORT
    ? `-c 'model_reasoning_effort="${REASONING_EFFORT}"'`
    : '';
  // Paired with modelFlag, never on its own: agy without --model needs no
  // --effort, and passing one alone would be a flag agy has no model to apply.
  const agyEffortFlag = BACKEND === 'agy' && modelFlag ? `--effort ${effectiveReasoningEffort()}` : '';
  cachedLaunchCommand = [cmd, perm, modelFlag, reasoningFlag, agyEffortFlag].filter(Boolean).join(' ');
  return cachedLaunchCommand;
}

// --- Headless (non-interactive) one-shot dispatch (hivecommons/hive#2538) ---
//
// Backends whose CLI supports a one-shot / print invocation that takes the
// prompt on the command line, runs to completion, and EXITS with a meaningful
// status — the property the headless mode needs. Each entry says how to turn
// (binary, perm-flags, prompt) into an argv:
//
//   flag — the sub-command/flag(s) that select one-shot mode. Either a single
//          token, where the prompt follows as a bare positional
//          (`claude -p "<prompt>"`, `codex exec "<prompt>"`), or an array of
//          leading tokens when a sub-command AND a flag both precede the
//          prompt (`goose run --no-session -t "<prompt>"`). Either way the
//          prompt is appended as the final, distinct argv element.
//
// Backends NOT listed here have no known non-interactive entry point (bob /
// aider drive an interactive TUI), so headless mode refuses them LOUDLY at
// task time rather than silently stalling. Extending this table is how a
// future PR adds a backend once its headless invocation is verified.
const HEADLESS_BACKENDS = {
  // claude -p "<prompt>" — print mode: runs the prompt non-interactively and
  // exits. Same perm flags as the interactive launch (bypass permissions).
  claude: { flag: '-p' },
  // litellm is the claude binary pointed at a LiteLLM proxy, so the same
  // print-mode invocation applies.
  litellm: { flag: '-p' },
  // copilot -p "<prompt>" — non-interactive programmatic mode.
  copilot: { flag: '-p' },
  // codex exec "<prompt>" — Codex's non-interactive execution sub-command.
  // --skip-git-repo-check: exec refuses to run at all in a cwd that is not a
  // git repository ("Not inside a trusted directory..."), and the task
  // workspace root is exactly that — the agent clones INTO it as its first
  // act. Verified live against codex 0.146.0 via bin/test_backend_smoke.sh.
  codex: { flag: ['exec', '--skip-git-repo-check'] },
  // goose run --no-session -t "<prompt>" — goose's one-shot sub-command. The
  // bare `goose` binary drives the interactive TUI, but `goose run` is a
  // documented non-interactive entry point (#2828): `-t` takes the prompt as
  // its VALUE (not a trailing positional), and --no-session skips creating or
  // resuming a session file, which one-shot dispatch never needs. Verified
  // against goose 1.37.0 — the version src/Dockerfile pins via GOOSE_VERSION —
  // that `run`, `-t` and `--no-session` all exist and that a failed run exits
  // non-zero, which is the exit-code contract runHeadlessTask() relies on.
  goose: { flag: ['run', '--no-session', '-t'] },
  // pi --print --mode json <prompt> — Pi's bounded non-interactive entry point.
  // AGENT_MODEL is already the canonical provider/model token, so no separate
  // --provider input is needed (or allowed) and restart/headless stay identical.
  pi: { flag: ['--print', '--mode', 'json'] },
  // agy -p "<prompt>" — Antigravity's print mode ("Run a single prompt
  // non-interactively and print the response", `agy --help`). Verified against
  // agy 1.1.13: a print-mode run answers on stdout and exits 0, which is the
  // exit-code contract runHeadlessTask() relies on. NOTE this makes agy
  // headless-capable on a HOST only — agy's sign-in is an interactive Google
  // OAuth flow (browser URL + pasted code) with no API-key mode, and a fresh
  // container has nothing to inherit it from, which is why agy stays OUT of
  // K8S_HEADLESS_BACKENDS on the /contribute page and out of the contributor
  // image. The capability and the credential are separate questions.
  agy: { flag: '-p' },
  // opencode run "<prompt>" — opencode's one-shot headless invocation
  // (hivecommons/hive#4970). Unlike agy, opencode is the ONLY launch mode
  // this backend gets: there is no interactive-tmux wiring for it (see the
  // getCLIState()/classifyTmuxPane() backend lists below, which opencode
  // deliberately does not join), so it is only ever reached through
  // CONTRIBUTOR_MODE=headless. `opencode run` exits with a real status code
  // on completion, the exit-code contract runHeadlessTask() relies on.
  opencode: { flag: 'run' },
  // Kilo is OpenCode-derived but uses distinct credentials and config.
  kilo: { flag: 'run' },
};

// headlessSupportsBackend reports whether the configured backend has a known
// one-shot invocation. Used to fail fast at startup and per task.
function headlessSupportsBackend() {
  return Object.prototype.hasOwnProperty.call(HEADLESS_BACKENDS, BACKEND);
}

// buildHeadlessArgv turns a task prompt into the argv for a one-shot,
// non-interactive backend invocation: [binary, ...permFlags, ...modelFlag,
// ...oneShotFlags, prompt]. Returns null for an unsupported backend. Never
// shell-interpolates the prompt — it is passed as a distinct argv element to
// execFile, so apostrophes/quotes in the prompt (the exact #2203 wedge on the
// interactive path) cannot break anything here.
function buildHeadlessArgv(prompt) {
  const spec = HEADLESS_BACKENDS[BACKEND];
  if (!spec) return null;
  if (BACKEND === 'pi' && !PI_SELECTION.valid) throw new Error(PI_SELECTION.error);
  const { cmd, perm } = resolveBackend();
  const permArgs = perm ? perm.split(/\s+/).filter(Boolean) : [];
  const modelArgs = MODEL && !NO_MODEL_FLAG_BACKENDS.includes(BACKEND) ? ['--model', MODEL] : [];
  const reasoningArgs = BACKEND === 'codex' && REASONING_EFFORT ? ['-c', `model_reasoning_effort="${REASONING_EFFORT}"`] : [];
  // Same --model/--effort pairing the interactive launch enforces, so headless
  // agy honors the configured model instead of silently falling back.
  const agyEffortArgs = BACKEND === 'agy' && modelArgs.length ? ['--effort', agyEffort] : [];
  // spec.flag is a single token for most backends, or an array of leading
  // tokens for backends needing a sub-command plus a flag (goose). Normalize
  // to an array so both shapes spread the same way ahead of the prompt.
  const oneShotArgs = Array.isArray(spec.flag) ? spec.flag : [spec.flag];
  const args = [...permArgs, ...modelArgs, ...reasoningArgs, ...agyEffortArgs, ...oneShotArgs, prompt];
  return { bin: cmd, args };
}

// writeHeadlessStatus records the runner's coarse lifecycle state so a
// supervising process / K8s probe can read it. Best-effort: a failed write is
// logged-by-omission and never aborts the task.
function writeHeadlessStatus(state, extra) {
  const payload = Object.assign({
    mode: MODE_HEADLESS,
    backend: BACKEND,
    ...effectiveSelectionFields(),
    ...(BACKEND === 'pi' ? piReadiness(PI_SELECTION, !!detectCapabilities().agent_cli_version, piInvocationState, PI_ENV) : {}),
    state,
    updated_at: new Date().toISOString(),
  }, extra || {});
  try {
    fs.writeFileSync(HEADLESS_STATUS_FILE, JSON.stringify(payload, null, 2));
  } catch (_) { /* probe file is advisory; never fail a task on it */ }
  return payload;
}

// Reference to the in-flight headless child, so a revoke/shutdown can kill it.
let headlessChild = null;

// runHeadlessTask drives a single task to completion WITHOUT tmux: it spawns the
// one-shot CLI, captures (bounded) output, and on exit reports task_complete
// (exit 0) or task_failed (non-zero / spawn error / timeout) over the existing
// WebSocket channel — then announces `ready` for the next task. This is the
// headless analogue of the interactive progressTick() completion path.
function runHeadlessTask(task) {
  const prompt = task.prompt || `Work on ${task.kind} ${task.repo}#${task.number}: ${task.title}`;
  if (!headlessSupportsBackend()) {
    // No non-interactive entry point for this backend: fail LOUDLY rather than
    // stall. This is the #2538 guarantee — a headless run never waits silently.
    const reason = `backend '${BACKEND}' has no headless (non-interactive) mode; supported: ${Object.keys(HEADLESS_BACKENDS).join(', ')}`;
    console.error(`Headless dispatch refused: ${reason}`);
    writeHeadlessStatus(HEADLESS_STATE_FAILED, { task_id: task.task_id, reason });
    // environment: this relay's configured backend has no headless entry point;
    // the work item itself is unjudged.
    failCurrentTask(reason, { permanent: true, kind: 'environment' });
    return;
  }

  let built;
  try {
    built = buildHeadlessArgv(prompt);
  } catch (e) {
    const reason = e.message;
    writeHeadlessStatus(HEADLESS_STATE_FAILED, { task_id: task.task_id, task_gen: task.task_gen, result: 'failed', reason });
    failCurrentTask(reason, { permanent: true, kind: 'environment' });
    return;
  }
  const { bin, args } = built;
  console.log(`Headless: running ${bin} (one-shot) for ${task.repo}#${task.number}`);
  writeHeadlessStatus(HEADLESS_STATE_WORKING, { task_id: task.task_id, task_gen: task.task_gen, repo: task.repo, number: task.number, result: 'working' });
  send({ type: 'task_progress', seq: nextSeq(), task_id: task.task_id, task_gen: task.task_gen, kind: task.kind, repo: task.repo, number: task.number, title: task.title, status: 'working', ...effectiveSelectionFields() });

  let settled = false;
  const finish = (fn) => { if (settled) return; settled = true; fn(); };

  headlessChild = execFile(bin, args, {
    timeout: HEADLESS_TASK_TIMEOUT_MS,
    maxBuffer: HEADLESS_MAX_OUTPUT_BYTES,
    killSignal: 'SIGKILL',
    cwd: TASK_WORKSPACE_DIR,
  }, (err, stdout, stderr) => {
    headlessChild = null;
    // Tokens can appear in agent output; redact before the tail leaves the host.
    const outTail = redactTokens(String(stdout || '') + String(stderr || ''))
      .split('\n').slice(-TMUX_TAIL_LINES);
    // A revoke clears currentTask before killing the child. Ignore any callback
    // that arrives afterwards — including a raced exit 0 — so stale work cannot
    // emit completion after its assignment generation was fenced out.
    if (!currentTask || currentTask.task_id !== task.task_id || currentTask.task_gen !== task.task_gen) {
      writeHeadlessStatus(HEADLESS_STATE_WAITING, { revoked_task_id: task.task_id });
      return;
    }
    if (err) {
      // A non-zero exit, a spawn failure (ENOENT), or the timeout kill all land
      // here. err.killed && err.signal signals the timeout; report a real
      // failure either way so the hub can reassign — never a silent hang.
      const timedOut = err.killed === true;
      // Preserve one bounded, token-redacted diagnostic line. In particular,
      // Codex automatic-review denial/timeout is an expected terminal outcome
      // for an unattended run and must reach Hive as an actionable failure,
      // rather than being flattened to an opaque exit code.
      const diagnostic = outTail.map(line => line.trim()).filter(Boolean).slice(-1)[0];
      const diagnosticSuffix = diagnostic ? `: ${diagnostic.slice(0, 500)}` : '';
      const reason = timedOut
        ? `headless task exceeded ${HEADLESS_TASK_TIMEOUT_MS / 60000}min and was killed`
        : `headless CLI exited with error: ${err.code !== undefined ? `code ${err.code}` : err.message}${diagnosticSuffix}`;
      finish(() => {
        setPiInvocationState('failed');
        console.error(`Headless task ${task.task_id} failed: ${reason}`);
        writeHeadlessStatus(HEADLESS_STATE_FAILED, { task_id: task.task_id, task_gen: task.task_gen, result: 'failed', reason });
        failCurrentTask(reason, {
          permanent: false,
          kind: BACKEND === 'pi' ? 'environment' : undefined,
        });
      });
      return;
    }
    finish(() => {
      setPiInvocationState('succeeded');
      console.log(`Headless task ${task.task_id} completed (exit 0)`);
      const prURL = detectPRURL(outTail, task.repo);
      if (prURL) console.log(`Detected PR for ${task.task_id}: ${prURL}`);
      // #3987: only report a no_work_needed verdict when no PR was shipped —
      // a visible PR contradicts "nothing shippable" (the hub would override
      // the claim with "shipped" anyway).
      const noWork = prURL ? null : detectNoWorkVerdict(outTail);
      if (noWork) console.log(`Detected no_work_needed verdict for ${task.task_id}: ${noWork.reason || '(no reason)'}`);
      writeHeadlessStatus(HEADLESS_STATE_DONE, { task_id: task.task_id, task_gen: task.task_gen, result: 'completed', pr_url: prURL });
      // #5353: the one-shot child has already exited (this callback is its
      // exit), so there is no process to stop — but the task-scoped token it
      // was given stays valid for the rest of wsTokenTTL. Drop it with the
      // task, so a credential never outlives the assignment it belongs to.
      stopAgentForTaskExit();
      send({ type: 'task_complete', seq: nextSeq(), task_id: task.task_id, task_gen: task.task_gen, result: 'completed', summary: 'Headless one-shot invocation exited 0', tmux_output: outTail, pr_url: prURL, verdict: noWork ? noWork.verdict : undefined, verdict_reason: noWork ? noWork.reason : undefined, ...effectiveSelectionFields() });
      currentTask = null;
      taskAssignedAt = 0;
      tasksCompletedCount++;
      writeHeadlessStatus(HEADLESS_STATE_WAITING);
      send({ type: 'ready', seq: nextSeq() });
    });
  });
  // codex exec prints "Reading additional input from stdin..." and then blocks
  // on stdin-EOF even with the prompt already passed as an argv element; with
  // execFile's default piped stdio nothing ever closes that pipe, so a
  // headless codex task produced zero output and hung until the timeout
  // killed it (found live by bin/test_backend_smoke.sh). Close stdin for
  // every backend — a one-shot child has no interactive input coming.
  if (headlessChild && headlessChild.stdin) headlessChild.stdin.end();
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

// quitLiveCLI stops an agent CLI that is STILL RUNNING in the pane, so the
// pane falls back to a shell and a subsequent relaunch types its command at a
// shell prompt rather than into the CLI as a chat message.
//
// Why two Ctrl-Cs and not one: recoverWedgedShell() above sends a single C-c,
// which is right for its case (a DEAD CLI leaving a wedged bash PS2 prompt).
// For a LIVE agent CLI, one C-c only cancels the current turn — claude, codex
// and agy all stay running — so the relaunch command that follows is delivered
// to the CLI as a prompt. That is exactly the #2203 wedge shape. The second
// C-c, with the same delays the memory-cleanup restart path has used since
// #2596, is what actually exits the CLI.
//
// Best-effort by design: if tmux is unreachable the caller is already on a
// failure path, and a relaunch that lands badly is recovered by the
// armCLIReadyWait() contract rather than by anything here.
function quitLiveCLI() {
  try {
    execSync(`tmux send-keys -t ${TMUX_SESSION} C-c`, { timeout: 15000 });
    sleepMs(1000);
    execSync(`tmux send-keys -t ${TMUX_SESSION} C-c`, { timeout: 15000 });
    sleepMs(2000);
  } catch (_) {}
}

// capturePaneText returns the current pane contents, or "" if tmux can't be
// reached. Extracted so the readiness classifier and the blocking-prompt
// dismissal can look at the SAME text without capturing twice, and so the
// dismissal can see WHICH prompt is on screen rather than re-deriving it from
// a state enum that has already thrown that detail away.
// Shell names that mean the pane fell back to a prompt — i.e. whatever the
// relay launched is no longer the pane's foreground program.
const PANE_SHELL_COMMANDS = new Set(['bash', 'sh', 'zsh', 'fish', 'dash', 'ksh', 'ash', 'tcsh', 'csh']);

// How many consecutive shell readings (one per progress tick) are required
// before the CLI is declared gone. One is not enough: a tool call can briefly
// put a shell in the pane's foreground while the CLI is very much alive.
const CLI_GONE_CONFIRMATIONS = 2;
let consecutiveShellReadings = 0;

// paneForegroundCommand asks tmux what the pane is actually RUNNING. Empty when
// tmux cannot answer (session gone, tmux missing) — an unknown, never a death.
function paneForegroundCommand() {
  try {
    return execSync(
      `tmux display-message -p -t ${TMUX_SESSION} '#{pane_current_command}' 2>/dev/null`,
      { encoding: 'utf8', timeout: 15000 }
    ).toString().trim();
  } catch (_) {
    return '';
  }
}

// cliProcessLooksGone reports whether the agent CLI has left the pane.
//
// It replaces a substring scan of the WHOLE process table:
//
//   procs.includes(BACKEND) || procs.includes('claude') || procs.includes('copilot') || …
//
// which could not do this job. Two independent defects, both observed live:
//
//  1. The relay's own machinery carries the backend's name. For agy the
//     launcher (`just contribute-hive agy local`) and the tmux session itself
//     (`tmux attach -t hive-agy-5b4f`) both contain "agy", so the probe was
//     pinned alive no matter what happened to the CLI.
//  2. The other CLI names were OR'd in unconditionally, whatever BACKEND was.
//     Any contributor with Claude Code running — i.e. most of them — reported
//     a live CLI for every backend, forever.
//
// With the probe stuck true the relay never relaunched a dead CLI, cliReady
// stayed latched, and task prompts were typed into a bare shell: exactly the
// #2203 bug-2 wedge the send gate exists to prevent.
//
// The pane's own foreground command answers the real question, and it cannot be
// confused by anything outside the pane. Two consecutive readings are required
// so that a tool call which briefly fronts a shell does not read as a death —
// the expensive mistake, since it restarts a CLI that is working. A CLI that
// really exited leaves the pane at a prompt permanently, so it still trips on
// the following tick; the stall backstop in progressTick is the second net.
//
// Note the pane TEXT is deliberately not consulted: a CLI that dies leaves its
// last frame on screen, ready-chrome and all, so requiring that chrome to be
// gone would re-introduce exactly the blindness this replaces.
function probeCLIPresence() {
  const fg = paneForegroundCommand();
  const isShell = !!fg && PANE_SHELL_COMMANDS.has(fg);
  if (!isShell) {
    consecutiveShellReadings = 0;
  } else {
    consecutiveShellReadings++;
  }
  return { isShell, gone: isShell && consecutiveShellReadings >= CLI_GONE_CONFIRMATIONS };
}

function cliProcessLooksGone() {
  return probeCLIPresence().gone;
}

// paneIsRunningShell answers "is the pane at a prompt RIGHT NOW", without
// touching the confirmation counter. Used by the send gate, where one reading
// is enough: typing a prompt into a shell is never right, and the cost of
// waiting a tick when we are wrong is nil.
function paneIsRunningShell() {
  const fg = paneForegroundCommand();
  return !!fg && PANE_SHELL_COMMANDS.has(fg);
}

function capturePaneText() {
  try {
    return execSync(
      `tmux capture-pane -t ${TMUX_SESSION} -p 2>/dev/null`,
      { encoding: 'utf8', timeout: 15000 }
    ).toString();
  } catch (_) {
    return '';
  }
}

// blockingPromptKey returns the keystroke that dismisses whatever modal prompt
// is on screen, or null meaning "a bare Enter is the right answer".
//
// Some CLIs gate startup behind a NUMBERED menu rather than a yes/no confirm.
// For those a bare Enter does nothing useful — it just re-renders the menu — so
// the relay would "dismiss" in a loop until CLI_READY_TIMEOUT_MS and the task
// would be dropped. Each entry names the exact prompt it answers, so an
// unrelated menu is never blind-fired at.
function blockingPromptKey(text) {
  // codex: "Do you trust the contents of this directory?" → 1. Yes, continue
  if (/Do you trust the contents of this directory/.test(text)) return '1';
  // codex: "✨ Update available! x -> y" → 3. Skip until next version.
  // Deliberately NOT "1. Update now": that shells out to `npm install -g`
  // inside the container — slow, needs network, can fail half-way, and drifts
  // the CLI version out from under the image. "Skip until next version" also
  // persists, so this prompt stops coming back on every restart the way a
  // plain "Skip" would.
  if (/Update available!/.test(text) && /Skip until next version/.test(text)) return '3';
  const recent = paneTail(text, 15);
  // agy: "Terms of Service & Data Use" ends on a [Previous] [Done] button row
  // with focus on the CHECKBOX above it, where Enter toggles consent instead of
  // advancing ("enter Toggle"). A bare Enter therefore never leaves this page.
  // Down moves to the button row, Right selects [Done]; the caller appends
  // Enter. The other two steps (theme picker, folder trust) do advance on a
  // bare Enter and deliberately fall through to null.
  if (BACKEND === 'agy' && /Terms of Service & Data Use/.test(recent) && /\[(?:Previous|Back)\]\s+\[Done\]/.test(recent)) return 'Down Right';
  return null;
}

function getCLIState() {
  try {
    const text = capturePaneText();
    if (BACKEND === 'claude') {
      // Order matters, as it does for bob and codex below: the blocked states
      // are classified FIRST, so a pane sitting on a login or trust gate is
      // never reported ready by persistent chrome it happens to draw as well.
      if (/Not logged in|Please run \/login/.test(text)) return 'needs-login';
      if (/Choose the text style|trust this folder/.test(text)) return 'onboarding';
      // The first alternation below is startup-only: a welcome banner, the
      // account line printed just after login, the first-run tip. That made
      // claude readiness a one-shot property of the SPLASH SCREEN — and
      // cliReady is cleared on EVERY task exit (stopAgentForTaskExit), then
      // re-latched only from here. Once the splash had scrolled away, a
      // perfectly healthy idle pane matched none of these, so the latch never
      // re-latched: every task prompt after the first was queued instead of
      // typed, and each of those tasks was handed back at CLI_READY_TIMEOUT_MS
      // with "CLI never became ready" (hivecommons/hive#5156, seen again in
      // #5650). Recovery depended on a fresh splash, which needs the CLI to
      // actually exit — and quitLiveCLI()'s two C-c keystrokes routinely do not
      // end claude.
      //
      // The second alternation is the footer chrome a live claude draws at ALL
      // times, splash or not: the auto-mode indicator, the agents hint, the
      // shift+tab cycle hint, and the in-turn interrupt hint. It is the same
      // evidence classifyTmuxPane's claude hasIdlePrompt has always used — two
      // detectors reading one pane must not disagree about whether the CLI is
      // even there.
      //
      // Readiness asks "is the CLI up and past its gates", not "is it idle":
      // busy-vs-idle is classifyTmuxPane's job, and tmuxSendKeys separately
      // refuses to type into a pane whose foreground command is a shell. So
      // matching "esc to interrupt", which is drawn mid-turn, is correct here.
      if (/bypass permissions|Welcome back|Try "how does|medium.*effort|@gmail\.com|@.*\.com.*Organization/.test(text)) return 'ready';
      if (/⏵⏵|← for agents|shift\+tab to cycle|esc to interrupt/.test(text)) return 'ready';
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
      // Order matters, as it does for bob above: codex draws its version banner
      // and input chrome BEHIND a modal prompt, so the 'ready' patterns below
      // match a pane that is actually blocked on a menu. Classifying the modals
      // first is what stops the relay from typing a task prompt into a
      // "1. Yes, continue / 2. No, quit" list, where it is swallowed.
      if (/Do you trust the contents of this directory/.test(text)) return 'onboarding';
      if (/Update available!/.test(text) && /Skip until next version/.test(text)) return 'onboarding';
      // codex renders its input marker as '›' (U+203A), not '>', and its
      // banner reads "OpenAI Codex (vX.Y.Z)" — never the literal "Codex CLI".
      // The three original patterns therefore matched NOTHING a real codex
      // pane ever contains, so readiness was never detected: every task was
      // queued and then handed back at CLI_READY_TIMEOUT_MS, and an
      // interactive codex contributor could not run a single task. Matching
      // the marker and the real banner is what makes the backend usable.
      // Safe against the modals above: those are classified first and return
      // 'onboarding', so a menu that also draws '›' never reaches here.
      if (/codex>|›|OpenAI Codex|Codex CLI|>\s*$/.test(text)) return 'ready';
    } else if (BACKEND === 'pi') {
      if (/pi v\d|0\.0%|auto\)|\d+\.\d+%/.test(text)) return 'ready';
    } else if (BACKEND === 'agy') {
      // Antigravity gates first run behind login plus a three-step wizard, and
      // every agent that shares a $HOME can re-enter it whenever another agent
      // writes antigravity-cli/cache/onboarding.json mode 600. Check the
      // visible tail only: old task output may quote the wizard text, and a
      // stale quote must not make a live prompt look blocked.
      const recent = paneTail(text, 15);
      if (/not signed in|Select login method/i.test(recent)) return 'needs-login';
      if (/Choose your color scheme|Terms of Service & Data Use|Do you trust the contents|I trust this (?:folder|directory)|Welcome to (?:the )?Antigravity/i.test(recent)) return 'onboarding';
      // agy shows "? for shortcuts" at the bottom when its interactive prompt
      // is ready. The generic />\s*$/ fires too early during splash, and the
      // wizard's selection cursor is also ❯.
      if (/\? for shortcuts/.test(recent)) return 'ready';
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
        // A numbered menu needs its option typed before Enter; a yes/no confirm
        // takes a bare Enter. blockingPromptKey() tells the two apart from the
        // pane text, so this no longer loops uselessly on menu-shaped prompts.
        const key = blockingPromptKey(capturePaneText());
        console.log(`Auto-dismissing trust/onboarding dialog${key ? ` (selecting "${key}")` : ''}...`);
        try {
          if (key) execSync(`tmux send-keys -t ${TMUX_SESSION} ${key} Enter`, { timeout: 15000 });
          else execSync(`tmux send-keys -t ${TMUX_SESSION} Enter`, { timeout: 15000 });
        } catch (_) {}
        setTimeout(check, CLI_READY_POLL_MS);
      } else if (state === 'needs-login' && !loginMessageShown) {
        loginMessageShown = true;
        console.log('');
        console.log('╔══════════════════════════════════════════════════════════╗');
        console.log('║  Claude Code needs authentication.                      ║');
        console.log('║  In another terminal, run:                              ║');
        console.log(`║  ${ATTACH_COMMAND}`);
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
// True once a CLI-readiness wait has timed out and we handed its task back.
// Used so the eventual recovery re-advertises availability to the hub, which
// we deliberately withheld at failure time (see armCLIReadyWait).
let cliReadyFailed = false;
// Set only by an interactive revoke. The next ready is delayed until a fresh CLI is confirmed.
let readyAfterInteractiveRevoke = false;

// False until the CURRENT task's prompt actually reached the pane
// (hivecommons/hive#5650). tmuxSendKeys() queues rather than types whenever the
// CLI is not confirmed ready or the pane has fallen back to a shell, and a task
// whose prompt is still queued has told the agent nothing — so nothing on the
// pane is evidence about it. progressTick() consults this before judging.
let taskPromptDelivered = false;

// The HIVE_VERDICT: line already on the pane when the current task's prompt was
// typed into it, or null when the pane held none (hivecommons/hive#5650).
//
// The relay drives ONE long-lived CLI, so a new task starts against a pane that
// still shows the previous task's finished transcript — including its
// "HIVE_VERDICT: complete — ..." line. detectCompletionVerdict() has no notion
// of which task a verdict belongs to, so progressTick() read that line and
// booked the NEW task completed minutes after assigning it, with no PR and the
// issue untouched. Remembering the line that was already there is what makes
// the verdict per-task: a verdict byte-identical to the one present at delivery
// time is, by construction, not this task's statement.
let deliveredVerdictBaseline = null;

if (CONTRIBUTOR_MODE === MODE_HEADLESS) {
  // Headless mode has no tmux pane to scrape for readiness. Each task spawns
  // its own one-shot CLI process on demand, so there is nothing to "become
  // ready" — the relay is ready to accept work as soon as it authenticates.
  // Fail fast on an unsupported backend so a K8s pod reports a real error at
  // startup instead of accepting a task it can never run.
  cliReady = true;
  if (!headlessSupportsBackend()) {
    console.error(`FATAL: CONTRIBUTOR_MODE=headless but backend '${BACKEND}' has no non-interactive mode. Supported: ${Object.keys(HEADLESS_BACKENDS).join(', ')}`);
    writeHeadlessStatus(HEADLESS_STATE_FAILED, { reason: `unsupported headless backend: ${BACKEND}` });
    if (process.env.HIVE_RELAY_TEST_MODE !== '1') process.exit(1);
  } else {
    console.log(`Headless mode: backend '${BACKEND}' will run one-shot per task (no tmux).`);
    writeHeadlessStatus(HEADLESS_STATE_WAITING);
  }
} else {
  armCLIReadyWait();
}

// armCLIReadyWait waits for the CLI to reach its prompt and, crucially, does
// something sane when it never does.
//
// The old code was `.catch(e => console.error(e.message))`. That silently
// abandoned the task: cliReady stayed false, pendingTask kept holding the
// prompt, and the HUB WAS NEVER TOLD — so from the hub's side this contributor
// was still working on the issue, and the slot stayed held until the hub's own
// timeout eventually revoked it. Any cause of an unresponsive backend (an
// unrecognized modal prompt, a crashed pane, a half-finished login, a hung
// update) produced the same black hole.
//
// Now the task is handed straight back so another contributor can pick it up,
// and the relay keeps waiting rather than declaring itself available: it does
// NOT re-advertise 'ready' until the CLI genuinely reaches its prompt.
// Otherwise it would immediately accept another task it still cannot run and
// churn one task per timeout window forever.
function armCLIReadyWait() {
  const hadFailed = cliReadyFailed;
  const becameReadyAfterRevoke = readyAfterInteractiveRevoke;
  waitForCLI().then(() => {
    cliReady = true;
    cliReadyFailed = false;
    if (becameReadyAfterRevoke) {
      readyAfterInteractiveRevoke = false;
      send({ type: 'ready', seq: nextSeq() });
    }
    // Only re-advertise if we previously withdrew by failing a task; the normal
    // startup path is already advertised by the auth_ok handler.
    if (hadFailed) send({ type: 'ready', seq: nextSeq() });
    flushPendingTask();
  }).catch(e => {
    cliReadyFailed = true;
    console.error(e.message);
    // Drop the queued prompt first: if the CLI later recovers, flushing a
    // prompt for a task the hub has already reassigned would have this
    // contributor silently working on someone else's issue.
    pendingTask = null;
    if (currentTask) {
      // environment: the agent CLI never reached its prompt on this host.
      // skipCLI: this IS the relaunch path — armCLIReadyWait() re-arms itself
      // below and the pane already has a launch in flight. Quitting and
      // relaunching from here would nest a second launch inside the first
      // (#5353). The credential is still dropped by failCurrentTask.
      failCurrentTask(`CLI never became ready: ${e.message}`, { skipReady: true, skipCLI: true, kind: 'environment' });
    }
    // Keep waiting. The CLI may still come up (a slow login, an operator
    // attaching to clear a prompt we don't recognize), and when it does the
    // handler above re-advertises availability.
    armCLIReadyWait();
  });
}

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
  // Cleared up front and set again only by a send that actually happened: every
  // early return below leaves the agent WITHOUT this prompt, and progressTick()
  // must not judge a task in that state (hivecommons/hive#5650).
  taskPromptDelivered = false;
  // Hard gate (issue #2203, bug 2): `send-keys -l` types literal keystrokes
  // into whatever owns the pane. If the CLI is not confirmed ready, those
  // keystrokes land on bash, whose readline chokes on the apostrophes in the
  // prompt and drops the pane into PS2 continuation, wedging it permanently.
  // Queue instead; flushPendingTask() delivers it once readiness is confirmed.
  //
  // cliReady is a LATCH: set once the CLI is confirmed up, cleared only by a
  // relaunch. When the liveness probe could not tell the CLI apart from the
  // relay's own processes (see cliProcessLooksGone), a CLI that died was never
  // relaunched, the latch stayed true, and this gate waved the prompt straight
  // through into a bare shell — observed live, with the hub's task prompt
  // executing as shell commands. So re-confirm against the LIVE pane before
  // typing; the per-backend readiness patterns already exist in getCLIState().
  if (!cliReady) {
    console.log('CLI not ready — queuing task prompt instead of typing into the pane');
    pendingTask = text;
    return;
  }
  if (paneIsRunningShell()) {
    console.log(`Pane is at a shell prompt, not ${BACKEND} — queuing task prompt instead of typing it into the shell`);
    pendingTask = text;
    {
      // The latch was STALE: the CLI exited without the relay noticing. Drop it
      // and bring the CLI back, or the queued prompt has nothing to flush into.
      cliReady = false;
      try {
        console.log(`Relaunching ${BACKEND} after a stale readiness latch: ${relaunchCLI()}`);
      } catch (e) {
        console.error('Failed to relaunch after a stale readiness latch:', e.message);
      }
    }
    return;
  }
  try {
    try {
      // SECURITY (N20, CWE-20): the second find MUST parenthesize the -o group.
      // `-type f -user dev -name '*.out' -o -name '*.html' -mmin +60 -exec rm`
      // parses as (-type f AND -user dev AND -name '*.out') OR (-name '*.html'
      // AND -mmin +60 AND -exec rm) because -o binds looser than the implicit
      // -a. The right branch therefore drops BOTH -type f and -user dev, so ANY
      // owner's /tmp/*.html older than 60min was deleted — including root's, and
      // including directories. The left branch had no -exec, so the *.out
      // cleanup this line exists to perform never actually ran.
      execSync(`find /tmp -maxdepth 1 -type d -user dev -not -name 'tmux-*' -not -name 'claude-*' -not -name 'node-*' -not -name '.' -mmin +60 -exec rm -rf {} + 2>/dev/null; find /tmp -maxdepth 1 -type f -user dev \\( -name '*.out' -o -name '*.html' \\) -mmin +60 -exec rm -f {} + 2>/dev/null`, { timeout: 15000 });
    } catch (_) {}
    const ctxPct = checkContextUsage();
    const RESET_EVERY_N = 3;
    const needsClaudeClear = BACKEND === 'claude' && ctxPct >= CLEAR_CONTEXT_THRESHOLD_PCT;
    // Fire the periodic memory-cleanup restart at most ONCE per threshold
    // crossing (issue #2596). Requiring tasksCompletedCount !== lastResetAtCount
    // stops the #2203 readiness guard from re-triggering the restart when it
    // re-enters tmuxSendKeys() at the same, unchanged count — the re-entry that
    // otherwise loops forever and starves the next task.
    const needsCliRestart = BACKEND !== 'claude' && tasksCompletedCount > 0 &&
      tasksCompletedCount % RESET_EVERY_N === 0 && tasksCompletedCount !== lastResetAtCount;
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
      // Record that we serviced this count BEFORE relaunching, so when the
      // readiness callback flushes the queued prompt back through here the
      // predicate is already false and we fall through to deliver the next task
      // instead of restarting again (issue #2596).
      lastResetAtCount = tasksCompletedCount;
      console.log(`Restarting ${BACKEND} CLI for memory cleanup (task ${tasksCompletedCount})`);
      quitLiveCLI();
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
    // Snapshot the verdict line already on the pane BEFORE this prompt is
    // typed, so progressTick() can refuse to read the PREVIOUS task's
    // HIVE_VERDICT line as this task's completion (#5650). Captured here rather
    // than at assignment because this is the moment the transcript stops being
    // "whatever was there" and starts being this task's own.
    const priorVerdict = detectCompletionVerdict(captureTmuxLines(TMUX_TAIL_LINES));
    deliveredVerdictBaseline = priorVerdict ? priorVerdict.line : null;
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
        taskPromptDelivered = true;
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

// Keep category names in exact parity with src/pkg/logscrub/handler.go. The Go
// test reads this declaration and fails if one implementation gains or loses a
// category without the other (hivecommons/hive#5478).
const RELAY_SECRET_PATTERNS = [
  { category: 'hive-canary', pattern: /HIVE-CANARY-[A-Fa-f0-9]{48}/g },
  // The open-ended body is deliberate: an exact upper bound would redact only
  // a prefix of a longer future token and leak its tail (#4267). The 10-char
  // floor and underscore support match pkg/logscrub.
  { category: 'github-token', pattern: /(ghs_|ghp_|gho_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]{10,}/g },
  { category: 'jwt', pattern: /eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}/g },
  { category: 'aws-access-key', pattern: /\b(AKIA|ASIA)[0-9A-Z]{16}\b/g },
  { category: 'bearer-token', pattern: /\bBearer\s+[A-Za-z0-9._~+/=-]{16,}\b/gi },
  { category: 'private-key', pattern: /-----BEGIN\s+(?:(?:RSA|EC|OPENSSH|DSA)\s+)?PRIVATE\s+KEY-----.*?-----END\s+(?:(?:RSA|EC|OPENSSH|DSA)\s+)?PRIVATE\s+KEY-----/gs },
  { category: 'encrypted-private-key', pattern: /-----BEGIN\s+ENCRYPTED\s+PRIVATE\s+KEY-----.*?-----END\s+ENCRYPTED\s+PRIVATE\s+KEY-----/gs },
  { category: 'pgp-private-key', pattern: /-----BEGIN\s+PGP\s+PRIVATE\s+KEY\s+BLOCK-----.*?-----END\s+PGP\s+PRIVATE\s+KEY\s+BLOCK-----/gs },
];

function redactTokens(text) {
  let output = text;
  for (const { pattern } of RELAY_SECRET_PATTERNS) {
    output = output.replace(pattern, '[REDACTED]');
  }
  return BACKEND === 'pi' ? redactPiCredentials(output, PI_SELECTION, PI_ENV) : output;
}

function captureTmuxLines(n) {
  try {
    const output = execSync(
      `tmux capture-pane -t ${TMUX_SESSION} -p -S -${n} 2>/dev/null`,
      { encoding: 'utf8', timeout: 15000 }
    );
    // Scrub the pane as one string before splitting it into protocol lines.
    // Private-key patterns span several terminal lines and cannot match if each
    // line is redacted independently.
    return redactTokens(output).trim().split('\n').slice(-n);
  } catch (_) {
    return [];
  }
}

// Best-effort scan of the agent's recent output for a GitHub pull-request URL
// it opened for this task. Reported on task_complete as pr_url so the hub can
// tell "work shipped" from "agent merely went idle" and pick the right issue
// cooldown (hivecommons/hive#2393 item 7). This is intentionally best-effort:
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

// ── The HIVE_VERDICT: sentinel family (hivecommons/hive#3987, #5376) ─────────
//
// The hub's task prompt asks the agent to end a task by printing ONE line of
// the exact form
//
//   HIVE_VERDICT: <verdict> — <short reason>
//
// Two verdicts are defined:
//
//   no_work_needed  (#3987) — the agent affirmatively determined there is
//     NOTHING shippable (the remainder is gated on an unanswered maintainer
//     decision, or merged PRs already cover it). Reported on task_complete as
//     verdict/verdict_reason so the hub parks the issue for the long
//     offer-suppression window instead of re-offering it every short-cooldown
//     period forever (the #2547 shape that escalation only bounded).
//
//   complete        (#5376) — the agent is DONE with the task, whatever it
//     shipped. This is the completion signal the interactive relay lacked:
//     before it, "is this task done" was inferred from the vendor's terminal
//     rendering (see classifyTmuxPane), which produced thirteen separate
//     issues (#1566, #4026, #4064, #4067, #4078, #4080, #4128, #4182, #4265,
//     #5094, #5121, #5156, #5162) as one CLI after another restyled its
//     chrome. Chrome is a vendor's cosmetic output; this line is the agent's
//     own statement. Only the second is a contract.
//
// Both are parsed by ONE anchored, echo-guarded scanner below, deliberately:
// the anti-false-positive handling is the hard-won part and there must not be
// a second copy of it to drift.
//
// The marker spelling must stay in sync with buildTaskPrompt in
// src/pkg/dashboard/contribute_ws.go.
const HIVE_VERDICT_NO_WORK = 'no_work_needed';
const HIVE_VERDICT_COMPLETE = 'complete';

// detectHiveVerdict scans `lines` newest-first for any of `wanted` (an array of
// verdict tokens) and returns { verdict, reason } for the first — i.e. the
// LAST-printed — match, or null.
//
// Returns null rather than throwing on junk input: every caller is on a
// best-effort path reading a terminal capture that may be empty.
function detectHiveVerdict(lines, wanted) {
  if (!Array.isArray(lines) || lines.length === 0) return null;
  if (!Array.isArray(wanted) || wanted.length === 0) return null;
  // Anchored at line start: the task PROMPT quotes the marker mid-sentence
  // ("...the exact form 'HIVE_VERDICT: ...'"), and an anchored match keeps
  // that instruction echo from reading as the agent's own verdict. Codex
  // renders its completed assistant messages with a leading bullet (•,
  // U+2022) and Claude Code with a filled circle (●, U+25CF) — presentation
  // chrome rather than part of the verdict. The claude glyph was missing
  // until bin/test_backend_smoke.sh drove a REAL claude pane through the
  // relay: the agent printed the sentinel, this regex missed it, and every
  // interactive claude completion silently degraded to the chrome_idle
  // fallback the sentinel exists to replace.
  //
  // The verdict token is an alternation of exactly the wanted tokens with a \b
  // after it, so "no_work_neededX" and "completely rewrote the parser" are both
  // non-matches — a prose line that merely STARTS with a verdict word must not
  // become a verdict.
  const alt = wanted.map(w => w.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|');
  const VERDICT_RE = new RegExp(`^\\s*(?:[•●]\\s*)?HIVE_VERDICT:\\s*(${alt})\\b[\\s:—–-]*(.*)$`, 'i');
  // Scan newest-first so the agent's final conclusion wins over anything it
  // merely quoted or considered earlier in the transcript.
  for (let i = lines.length - 1; i >= 0; i--) {
    const m = VERDICT_RE.exec(lines[i]);
    if (!m) continue;
    const reason = (m[2] || '').trim();
    // tmux may wrap the prompt's instruction so its quoted marker lands at a
    // visual line start; its giveaway is the literal "<short reason>"
    // placeholder. Never treat that echo as a real verdict.
    if (reason.startsWith('<')) continue;
    // `line` is the RAW pane line this verdict was read from. progressTick()
    // compares it against the line that was already on the pane when the task's
    // prompt was delivered, which is how a verdict gets attributed to a task at
    // all (#5650).
    return { verdict: m[1].toLowerCase(), reason, line: lines[i] };
  }
  return null;
}

// Best-effort scan for the no_work_needed sentinel. Unchanged in behaviour
// from #3987/#4265; it now shares the scanner above. Returns null when no
// marker is found — the hub then treats the completion exactly as an idle one.
function detectNoWorkVerdict(lines) {
  return detectHiveVerdict(lines, [HIVE_VERDICT_NO_WORK]);
}

// detectCompletionVerdict reports whether the agent SAID it finished (#5376).
//
// Either verdict counts as "the agent declared this task over": no_work_needed
// is a completion too — it is the agent concluding the task with nothing to
// ship — and requiring a second `complete` line after it would make a
// compliant agent look non-compliant.
function detectCompletionVerdict(lines) {
  return detectHiveVerdict(lines, [HIVE_VERDICT_COMPLETE, HIVE_VERDICT_NO_WORK]);
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

function recentPaneLines(text, limit = 12) {
  return text
    .split('\n')
    .map(line => line.trim())
    .filter(Boolean)
    .slice(-limit);
}

// Why a blocked pane is blocked (hivecommons/hive#5281). BLOCKED_ON_HUMAN
// conflates two populations, and only one of them can be helped without a
// person:
//
//   question       — a plain "?", a y/N, an elicitation form. The agent forgot
//                    its standing instruction to decide for itself, and a
//                    one-line reminder is usually all it takes.
//   menu           — a numbered menu. Deliberately NOT nudge-eligible: a menu
//                    TUI may read typed text as a selection filter rather than
//                    as chat input, so covering it properly needs Escape
//                    handling this does not attempt.
//   human-required — login, credential entry, trust/consent, permission. Only
//                    a person can answer these, and typing at them is actively
//                    harmful.
const BLOCKED_REASON_QUESTION = 'question';
const BLOCKED_REASON_MENU = 'menu';
const BLOCKED_REASON_HUMAN_REQUIRED = 'human-required';

// The confirmation half of the old blockingPatterns list: prompts an agent
// working autonomously is entitled to answer for itself.
const QUESTION_BLOCKING_PATTERNS = [
  /\[[Yy]\/[Nn]\]|\([Yy]\/[Nn]\)|\b[Yy]es\/[Nn]o\b/,
  /\b(?:continue|proceed|confirm|approve|allow|deny|accept|reject|choose|select)\b.*\?/i,
  /\bPress Enter to continue\b/i,
  /\bEnter to confirm\b/i,
];

// The other half: prompts where a person is the only possible answer. Kept as
// its own list because it is a veto, not a detector — see
// classifyBlockedOnHumanReason.
const HUMAN_REQUIRED_BLOCKING_PATTERNS = [
  /\b(?:approval|consent|trust this folder|Do you trust|Confirm folder trust)\b/i,
  /\bpermission\b.*\b(?:allow|approve|confirm|continue|proceed)\b/i,
  /\b(?:allow|approve|confirm|continue|proceed)\b.*\bpermission\b/i,
  /\b(?:Allow|Approve|Run|Execute)\b.*\b(?:command|tool|edit|file|operation)\b/i,
  /\b(?:Paste|Enter).*(?:API key|token|code|password)\b/i,
];

// classifyBlockedOnHumanReason returns one of the BLOCKED_REASON_* constants,
// or null when the pane is not blocked at all.
function classifyBlockedOnHumanReason(text) {
  const lines = recentPaneLines(text);
  if (lines.length === 0) return null;
  const recent = lines.join('\n');
  const last = lines[lines.length - 1];
  const beforePrompt = [...lines].reverse().find(line =>
    !/^([>$❯]|goose>|G\s*>|> Enter to send|\/ commands.*help)$/i.test(line)
  ) || last;
  const currentMenuLine = /^(?:[❯>]\s*)?(?:\d+[\).]|[A-Za-z][\).])\s+\S+/.test(beforePrompt);

  const hasQuestion = /\?\s*$/.test(beforePrompt);
  const hasNumberedMenu =
    /\b(?:choose|select|option|pick|which|how (?:should|to) proceed|what (?:would|should).+like)\b/i.test(recent) &&
    currentMenuLine &&
    (recent.match(/(?:^|\n)\s*(?:[❯>]\s*)?\d+[\).]\s+\S+/g) || []).length >= 2;

  // Elicitation / fill-in-a-form prompt (hivecommons/hive#2844). Goose (and any
  // backend that raises an MCP elicitation) can pause mid-turn and render a form
  // for the operator to fill in. Such a pane usually ends in a bare "> " or
  // still shows goose's "> Enter to send" hint, so the per-backend classifier
  // sees hasIdlePrompt && hasCompletionMarker and calls the turn DONE — the exact
  // false "complete" this function exists to prevent. A form does NOT necessarily
  // carry a trailing "?", a y/N, a numbered menu, or a permission keyword, so the
  // checks above miss it. Detect it POSITIVELY and CONTEXTUALLY: require an
  // explicit request-for-input lead-in AND a form/field structure (or one of
  // goose's own elicitation-timeout markers). Requiring both — the lead-in is the
  // load-bearing half — keeps ordinary finished output that merely contains a
  // "label: value" line (e.g. "opened a PR: https://…") from matching, the same
  // bare-substring lesson as the /login false-positive fix.
  const hasInputRequestLeadIn =
    /\b(?:needs?|need)\s+(?:some\s+|more\s+|the\s+following\s+)?(?:information|input|details|details? to proceed)\b/i.test(recent) ||
    /\b(?:please\s+)?(?:fill\s+in|provide|enter|supply|complete|specify)\b.*\b(?:the\s+following|form|field|details|information|value|below)\b/i.test(recent) ||
    /\b(?:the\s+following|these)\s+(?:information|details|fields|values)\b.*\b(?:required|needed|to proceed)\b/i.test(recent) ||
    /\bwaiting\s+for\s+(?:your\s+|user\s+)?(?:input|response|answer)\b/i.test(recent);
  const hasFormStructure =
    /\[\s*[^\]\n]*\s*\]/.test(recent) ||             // a bracketed input/field or [ Submit ]/[ Cancel ] button
    /^\s*\S.*:\s*(?:_+|\[.*\]|)\s*$/m.test(recent);  // "Label:" field rows (optionally blank/underscore/bracket)
  // Goose bounds elicitation with its own timeout; these strings are an
  // unambiguous "was blocked on a human" signal all on their own.
  const hasElicitationMarker =
    /\bElicitation request timed out\b/i.test(recent) ||
    /\bTimeout waiting for user response\b/i.test(recent);
  const hasElicitationForm = (hasInputRequestLeadIn && hasFormStructure) || hasElicitationMarker;
  const blockingPatterns = [...QUESTION_BLOCKING_PATTERNS, ...HUMAN_REQUIRED_BLOCKING_PATTERNS];

  const blocked = hasQuestion || hasNumberedMenu || hasElicitationForm ||
    blockingPatterns.some(re => re.test(beforePrompt));
  if (!blocked) return null;

  // Human-required WINS over every other signal, and is asked of the whole
  // recent window rather than just the line above the prompt (#5281). A trust
  // dialog or a credential request often renders its heading a few lines up
  // while the cursor line is a bare "Do you want to proceed?" — classifying
  // that as an ordinary question is exactly the mistake that would type an
  // autonomy reminder into a /login flow or submit it as a password.
  //
  // Widening the window can only move a pane from question to human-required,
  // never make an unblocked pane blocked: `blocked` above is computed exactly
  // as it always was. When in doubt, human-required — waiting costs 30 minutes,
  // a wrong nudge costs a credential prompt answered with prose.
  if (HUMAN_REQUIRED_BLOCKING_PATTERNS.some(re => re.test(recent))) {
    return BLOCKED_REASON_HUMAN_REQUIRED;
  }
  if (hasNumberedMenu) return BLOCKED_REASON_MENU;
  return BLOCKED_REASON_QUESTION;
}

// paneLooksBlockedOnHuman is the original boolean, now derived from the
// classifier so there is exactly one definition of "blocked". Its answer is
// unchanged: classifyBlockedOnHumanReason returns non-null for precisely the
// panes this used to return true for.
function paneLooksBlockedOnHuman(text) {
  return classifyBlockedOnHumanReason(text) !== null;
}

// paneTail returns the last n lines of a pane capture. Pure, so the detectors
// below are table-testable without tmux.
function paneTail(text, n) {
  return String(text || '').split('\n').slice(-n).join('\n');
}

// paneShowsTransientAPIError reports whether the visible tail carries a
// retryable API failure. Every candidate line must carry the "API Error:"
// chrome AND a known-retryable pattern, so prose that merely mentions a dropped
// connection ("the user reported connection lost mid-response earlier") does
// not trip it.
function paneShowsTransientAPIError(text) {
  const lines = paneTail(text, TRANSIENT_API_ERROR_TAIL_LINES).split('\n');
  return lines.some((line) => {
    const lower = line.toLowerCase();
    if (!lower.includes('api error:')) return false;
    if (TRANSIENT_API_ERROR_PATTERNS.some((pat) => lower.includes(pat))) return true;
    return TRANSIENT_API_ERROR_STATUS_RE.test(line);
  });
}

// paneShowsUnretryableAPIError detects failures a repeat cannot clear — an
// authorization refusal or an exhausted quota. LINE-WISE and gated on the same
// "API Error:" chrome as the transient detector, and the gate matters MORE
// here: this verdict actively fails the task, so a false positive fails work
// that genuinely completed. An agent working on hive's own quota-handling code
// can legitimately print "budget_exceeded" in its final summary (the repo's
// test files contain these strings verbatim); without the chrome gate that
// completed turn would be booked as an environment failure. Claude renders
// every real quota/authorization error under the chrome on the same line
// ("API Error: 429 {\"type\":\"budget_exceeded\"...}"), so the gate costs
// nothing for the errors this exists to catch. A chrome-less quota banner
// (copilot/bob render some) falls through to the pre-#5094 behavior and is
// part of the documented #5121 residual.
function paneShowsUnretryableAPIError(text) {
  const lines = paneTail(text, TRANSIENT_API_ERROR_TAIL_LINES).split('\n');
  return lines.some((line) => {
    const lower = line.toLowerCase();
    if (!lower.includes('api error:')) return false;
    if (UNRETRYABLE_API_ERROR_PATTERNS.some((pat) => lower.includes(pat))) return true;
    return UNRETRYABLE_API_ERROR_STATUS_RE.test(line);
  });
}

// paneShowsLoginRequiredError detects an AUTHENTICATION failure — the CLI's
// credential expired mid-session and it is asking for /login. Neither of the
// other two buckets fits: a retry cannot clear it (typing "try again" at an
// expired credential is a wall), and failing it releases a task a human can
// rescue in thirty seconds by logging in. The honest state is BLOCKED_ON_HUMAN
// — a person genuinely is the only thing that can move it — which the hub
// already renders with an attention flag.
//
// 401 is authentication, NOT the 403 the fatal bucket catches: the hub's #4400
// rule is that /login fixes a 401 and fixes nothing about a 403. Ordering in
// classifyTmuxPane preserves that: the fatal check runs first, so a line
// carrying both a login hint and a 403/authorization refusal stays fatal.
//
// Without this, a mid-session credential expiry — the exact scenario #5088
// reported — rendered "● Please run /login · API Error: 401 …" above the idle
// prompt and was booked as a COMPLETED task.
function paneShowsLoginRequiredError(text) {
  const lines = paneTail(text, TRANSIENT_API_ERROR_TAIL_LINES).split('\n');
  return lines.some((line) => {
    const lower = line.toLowerCase();
    if (lower.includes('please run /login')) return true;
    return lower.includes('api error:') && /\b401\b/.test(line);
  });
}

// How long an attached tmux client must have been silent before the relay
// stops treating it as a person who owns the pane (hivecommons/hive#5277).
//
// The guard this feeds exists so a watchdog never types over someone
// mid-keystroke, and that is worth keeping. But "a client is connected" is not
// "a human is here": a dashboard terminal tab left open an hour ago
// (bin/ttyd-tmux.sh attaches one, and the dashboard's browser terminal proxies
// to it) was indistinguishable from someone actively typing, and it disabled
// API-error auto-retry for the whole 30-minute task ceiling.
//
// Five minutes, and the two bounds are asymmetric. Below ~2 minutes the
// threshold is not observable at all: the only caller runs on the
// PROGRESS_REPORT_INTERVAL_MS tick, 120s apart. Above it, every extra minute is
// a minute of a stranded task, and the cost of being wrong in that direction is
// mild — "try again" typed at a prompt nobody is typing at is visible and
// harmless, while the cost of being wrong in the other direction is the bug
// this fixes. Long enough to cover reading a diff; far short of the 30-minute
// strand it replaces.
const HUMAN_PRESENCE_IDLE_MS = Number(process.env.HIVE_HUMAN_PRESENCE_IDLE_MS) || 5 * 60 * 1000;

// tmuxSessionHumanPresence reports whether a human is at the agent's tmux
// session, and how confident that answer is.
//
//   attached — some client is connected at all.
//   active   — some client has typed within HUMAN_PRESENCE_IDLE_MS. This, not
//               `attached`, is the question a watchdog must ask before typing.
//   idleMs   — how long the most recently active client has been quiet, or
//               null when tmux did not say.
//
// `client_activity` is tmux's per-client timestamp of last input, in epoch
// seconds — the signal that distinguishes an abandoned tab from a person.
//
// EVERY uncertain answer resolves to active:true, because the failure this
// guard prevents (typing over someone mid-keystroke) is worse than the failure
// it causes (a retry deferred one tick). tmux erroring, tmux returning
// unparseable activity values, and a clock skewed into the future all take that
// branch. Only a client that positively reports itself quiet for long enough
// releases the pane.
function tmuxSessionHumanPresence() {
  try {
    const out = execSync(
      `tmux list-clients -t ${TMUX_SESSION} -F '#{client_activity}' 2>/dev/null || true`,
      { encoding: 'utf8', timeout: 15000 });
    const text = String(out).trim();
    if (!text) return { attached: false, active: false, idleMs: null };

    let newestSec = null;
    for (const line of text.split('\n')) {
      const seconds = Number(String(line).trim());
      if (!Number.isFinite(seconds) || seconds <= 0) continue;
      if (newestSec === null || seconds > newestSec) newestSec = seconds;
    }
    if (newestSec === null) {
      // Attached, but tmux told us nothing usable about when — an old tmux
      // whose client_activity is not an epoch integer, say. Presence unknown,
      // so presence assumed.
      return { attached: true, active: true, idleMs: null };
    }

    // A negative age means the client's clock is ahead of ours; clamping to
    // zero makes that read as "just now", which is the cautious direction.
    const idleMs = Math.max(0, Date.now() - newestSec * 1000);
    return { attached: true, active: idleMs < HUMAN_PRESENCE_IDLE_MS, idleMs };
  } catch (_) {
    return { attached: true, active: true, idleMs: null };
  }
}

// tmuxSessionHasAttachedClient reports only whether a client is CONNECTED. It
// deliberately says nothing about whether a person is there — see
// tmuxSessionHumanPresence for the question callers actually want. Kept because
// "is anything attached at all" is still a real question, and because failing
// closed on a tmux error is the same rule at both layers.
function tmuxSessionHasAttachedClient() {
  return tmuxSessionHumanPresence().attached;
}

// tmuxSendNudge types a short literal message and submits it.
//
// Deliberately NOT tmuxSendKeys(): that function is the TASK-PROMPT path and
// carries machinery a nudge must not trigger — a /clear once the context
// crosses CLEAR_CONTEXT_THRESHOLD_PCT, the periodic every-N-tasks CLI restart,
// and the /tmp sweep. A nudge exists precisely to preserve the session context
// that makes recovery cheap; clearing or restarting would throw away the very
// thing being rescued.
function tmuxSendNudge(message) {
  execSync(`tmux send-keys -t ${TMUX_SESSION} -l '${message}'`, { timeout: 15000 });
  sleepMs(ENTER_DELAY_MS);
  tmuxSendEnters();
}

// paneUnknownAPIErrorLine returns the first line of the visible tail that
// carries Claude Code's own error rendering — a line-leading "● API Error:" —
// or null. Reached only after the three curated detectors above have NOT
// matched (classifyTmuxPane's ordering), so a hit here is an API failure the
// tables cannot name (hivecommons/hive#5121): a 400, a 404, a 429 phrased in a
// way nobody anticipated, a brand-new gateway message.
//
// The anchor is deliberately STRICTER than the curated detectors' anywhere-in-
// the-line match. They pair the chrome with a known pattern, which is already
// two independent signals; this one has no pattern to pair with, so the chrome
// must be the CLI's own rendering — the ● bullet at line start is how Claude
// Code prints its errors — or an agent whose completed-turn prose merely
// mentions "API Error: 418" would be held and retried instead of credited.
// The residual is an agent whose rendered message BEGINS with the literal
// string "API Error:", which is as narrow as this can get from pane text.
//
// Returning the line (not a boolean) is the instrumentation half of #5121:
// every hit is logged verbatim at the call site, so the curated lists can be
// grown from what actually occurs in the wild instead of from guesses.
function paneUnknownAPIErrorLine(text) {
  const lines = paneTail(text, TRANSIENT_API_ERROR_TAIL_LINES).split('\n');
  for (const line of lines) {
    if (/^\s*●\s*API Error:/i.test(line)) return line.trim();
  }
  return null;
}

function classifyTmuxPane(text) {
  let hasIdlePrompt, hasCompletionMarker, isWorking;

  if (BACKEND === 'claude') {
    const claudeTail = text.split('\n').slice(-15).join('\n');
    // Claude's optional footer hints change when a background shell is still
    // running. Its own state markers do not: an in-flight turn renders
    // "esc to interrupt", while an idle turn retains the ⏵⏵ / agents chrome.
    // Prefer those markers over transcript verbs, which may describe finished
    // work. Keep the verb heuristic only for an unrecognised footer so an
    // unknown Claude UI still errs toward busy.
    hasIdlePrompt = /⏵⏵|← for agents|bypass permissions|shift\+tab to cycle/.test(claudeTail);
    hasCompletionMarker = /[✻✶✽] \S+ed for \d+[ms]|Honking|tokens\)/.test(text);
    // #5654: Claude Code retries a dropped API connection SILENTLY — no
    // "● API Error:" chrome, just a spinner-glyph countdown line:
    //
    //   ✻ Waiting for API response · will retry in 1m 57s · check your network
    //
    // That pane is mid-turn, but every gate below said otherwise: no busy
    // marker, no recognised error line, and the persistent ⏵⏵ footer plus a
    // PREVIOUS turn's "✻ Worked for …" summary satisfied the completion test —
    // the ✻ glyph is Claude's spinner, not a completion signal — so a stalled
    // agent could be booked IDLE_COMPLETE mid-turn. A retry countdown is the
    // CLI saying it is still working, so it counts as a BUSY marker: the task
    // stays WORKING, nothing is typed into the pane (interrupting a self-
    // recovering retry would cause the stall it prevents — see the ordering
    // note above paneShowsUnretryableAPIError below), and a retry loop that
    // never resolves is bounded by the existing stall backstop and
    // MAX_TASK_DURATION rather than mis-booked here.
    //
    // The two halves of the line are matched independently because a narrow
    // pane wraps it; the digit anchor on "will retry in" keeps completed-turn
    // prose ("the job will retry indefinitely") from pinning an idle pane, and
    // the tail scope — same window as every other marker in this branch —
    // keeps a scrolled-past mention from doing so either.
    const claudeRetryMarker = /Waiting for API response|will retry in \d/i.test(claudeTail);
    const claudeBusyMarker = /esc to interrupt/i.test(claudeTail) || claudeRetryMarker;
    isWorking = claudeBusyMarker ||
      (!hasIdlePrompt && (/─.*Bash\(|Reading|Editing|Writing|Searching/.test(claudeTail) || /ing…/.test(claudeTail)));
  } else if (BACKEND === 'copilot') {
    hasIdlePrompt = /\/ commands.*help/.test(text);
    hasCompletionMarker = true;
    isWorking = /esc cancel/.test(text);
  } else if (BACKEND === 'gemini') {
    hasIdlePrompt = />\s*$|❯\s*$/.test(text);
    hasCompletionMarker = /completed|Done|finished/i.test(text);
    isWorking = /Thinking|Running|Searching/i.test(text);
  } else if (BACKEND === 'goose') {
    hasIdlePrompt = /goose is ready|> Enter to send|>\s*$|goose>|G\s*>/.test(text);
    hasCompletionMarker = true;
    isWorking = /working|running|executing|calling/i.test(text);
  } else if (BACKEND === 'bob') {
    const BOB_IDLE_CHROME = /Enter your prompt, \/ for commands|Auto-approve:|Tokens left:/;
    const BOB_SPINNER = /\(esc to cancel/;
    const bobRunning = bobIsRunning();
    hasIdlePrompt = BOB_IDLE_CHROME.test(text) || !bobRunning;
    hasCompletionMarker = true;
    isWorking = bobRunning && BOB_SPINNER.test(text);
  } else if (BACKEND === 'codex') {
    // Codex retains prior tool rows in its long-lived pane.  Scope transient
    // activity words to the tail so an old "Running" row cannot pin a
    // completed turn in WORKING forever.
    const codexTail = text.split('\n').slice(-15).join('\n');
    // Same marker mismatch as getCLIState(): '›' (U+203A), not '>'.
    hasIdlePrompt = /codex>|›|>\s*$/.test(text);
    // Not a prose match. codex writes its own completion summary in whatever
    // words the work calls for, and requiring "completed|done|finished" makes
    // finishing a task depend on which English word it happened to reach for.
    //
    // Observed live: a task that ran to completion and opened
    // hivecommons/hive#4259 ready for review summarised itself as "Opened
    // ready-for-review PR #4259 … Conclusion: direct .kube reuse is not viable
    // … Branch is pushed and clean … Worked for 6m 22s". None of the three
    // words appear, and there is no no_work_needed verdict either (it shipped a
    // PR), so hasCompletionMarker was false, the IDLE_COMPLETE arm could not be
    // reached, and the pane fell through to PANE_STATE_WORKING with the agent
    // sitting idle at its prompt.
    //
    // The same reliance on prose in the other direction produced #4182 for agy.
    //
    // codex's real state signal is its status row, which isWorking below reads:
    // an in-flight turn renders "esc to interrupt", and an idle one does not.
    // hasIdlePrompt cannot carry that distinction here — codex draws its "›"
    // input line while it is working too — so gating completion on a completion
    // WORD added nothing except a way to miss finished work. copilot, goose,
    // agy and bob all set this true for the same reason.
    hasCompletionMarker = true;
    // Prefer codex's own status row over guessing from prose, exactly as the
    // agy branch below does after #4182.
    //
    // The bare verbs are matched case-insensitively against the tail, and codex
    // narrates in plain English — including in the summary it prints when a turn
    // FINISHES. A summary that happens to say "I'm running the tests" or
    // "executing the plan" pins a finished pane to WORKING, the relay keeps
    // renewing the lease, and the task dies at the stall backstop or
    // MAX_TASK_DURATION with its PR already open. That is #4182, which was the
    // same latent shape on agy until a summary tripped it.
    //
    // Captured from a live pane, codex's markers are:
    //
    //   working -> "• Working (46s • esc to interrupt)"  AND  "› Ask Codex to…"
    //   idle    ->                                             "› Ask Codex to…"
    //
    // so "esc to interrupt" is the ONLY discriminator; the "›" input line is
    // drawn in both states, which is why hasIdlePrompt cannot carry this and
    // why the verb list was doing the work.
    //
    // The second alternative keeps the protection the bare verbs were really
    // providing, without the prose exposure. codex marks an in-flight tool call
    // with its OWN bullet chrome — "• Running <cmd>", against "• Ran <cmd>" once
    // finished — so anchoring to the bullet distinguishes codex saying it is
    // running something from the model narrating that it ran something:
    //
    //   "• Running gh issue view 4066"        -> chrome, in flight   -> WORKING
    //   "- While running the tests I ..."     -> prose, in a summary -> not
    //
    // That matters beyond this bug: it is what stops a stale
    // "HIVE_VERDICT: no_work_needed" higher in the scrollback from being
    // reported as the completion of a turn that has since started new work.
    const codexBusyMarker = /esc to interrupt/i.test(codexTail) ||
      /(?:^|\n)\s*[•·▸]\s*(?:Running|Executing|Thinking)\b/i.test(codexTail);
    isWorking = codexBusyMarker;
  } else if (BACKEND === 'pi') {
    hasIdlePrompt = /pi v\d|0\.0%|auto\)|\d+\.\d+%/.test(text);
    hasCompletionMarker = /completed|done|finished|tokens\)|\d+\.\d+%/i.test(text);
    isWorking = /Reading|Writing|Bash|Editing|thinking|running/i.test(text);
  } else if (BACKEND === 'agy') {
    // Scope the activity check to the TAIL, exactly as the claude branch above
    // does. agy narrates in plain English inside the transcript ("I am running
    // the pkg/agent tests…", "Analyzing…"), and those lines stay on screen after
    // the turn ends. A whole-pane, case-insensitive scan for bare verbs
    // therefore reads a FINISHED turn as still working — forever, since the
    // stale line never scrolls off on its own. Observed live: a pane with
    // hasIdlePrompt=true was pinned to WORKING by a single narration line left
    // over from the PREVIOUS task, so the relay never reported completion and
    // kept renewing the hub's task lease; the contributor had to Ctrl-C.
    //
    // The marker SET is deliberately unchanged — only the window it looks at.
    // Narrowing which verbs count would need a live agy turn to verify against,
    // and getting that wrong would be the opposite (and worse) bug: reporting a
    // busy agent as idle. The stall backstop in progressTick() covers whatever
    // this still misses.
    const agyTail = text.split('\n').slice(-15).join('\n');
    // agy formerly ended idle turns with "? for shortcuts". Current Gemini
    // builds render a bare input line followed by the model footer instead.
    // Keep the bare ">" constrained to that footer so a Markdown quote in
    // an in-flight response cannot be mistaken for an idle prompt.
    //
    // The input box is CLOSED by a second box-drawing rule between the ">" and
    // the footer, so the gap is not pure whitespace and "\s*" cannot cross it.
    // Observed live: a turn that finished and opened hivecommons/hive#4127 sat
    // at this exact idle chrome, classified WORKING, and was killed 20 minutes
    // later by the progressTick() stall backstop and reported as an
    // `environment` FAILURE — for a task that had shipped a real PR. Allow the
    // rule character (U+2500) in the gap so the footer is reachable.
    //
    // Safety direction is preserved by the footer itself: while a turn is in
    // flight agy renders "esc to cancel" on that same line, which is neither
    // whitespace nor a rule, so a busy pane still cannot match here.
    hasIdlePrompt = /\? for shortcuts/.test(text) ||
      /(?:^|\n)>\s*\n[\s─]*\n?\s*Gemini\b[^\n]*\s*$/m.test(agyTail);
    hasCompletionMarker = true;
    // Prefer agy's OWN state markers over guessing from prose.
    //
    // The bare verb scan below is a case-insensitive word match, and agy
    // narrates in plain English — including in the summary it prints when a
    // turn FINISHES. Observed live: a completed task that opened
    // hivecommons/hive#4181 ended with "Replaced inline token export
    // instructions with writing HIVE_GITHUB_TOKEN to a local .env file". That
    // "writing" is in the last 15 lines by construction (it is the summary),
    // so isWorking stayed true, and because isWorking short-circuits before
    // hasIdlePrompt is consulted the finished pane classified WORKING and the
    // stall backstop failed a task whose PR was already open.
    //
    // Narrowing the verb list is the wrong lever: any word list will collide
    // with prose eventually, and getting it wrong the other way (a busy agent
    // read as idle) is the worse bug. Use the status bar instead, which agy
    // renders itself and which says exactly one thing at a time:
    //   in flight -> "esc to cancel"
    //   at rest   -> "? for shortcuts", or the bare model footer
    //
    // Order matters. An explicit busy marker wins. Failing that, an explicit
    // idle prompt means not working, whatever the transcript above it says.
    // Only when neither marker is present — an agy build whose chrome we do
    // not recognise — fall back to the verb heuristic, so an unknown UI still
    // errs toward "busy" rather than reporting a working agent complete.
    const agyBusyMarker = /esc to cancel/.test(agyTail);
    isWorking = agyBusyMarker ||
      (!hasIdlePrompt && /Running|Searching|Reading|Writing|Editing/i.test(agyTail));
  } else {
    hasIdlePrompt = />\s*$|\$\s*$/.test(text);
    hasCompletionMarker = /completed|done|finished/i.test(text);
    isWorking = false;
  }

  if (paneLooksBlockedOnHuman(text)) return PANE_STATE_BLOCKED_ON_HUMAN;
  if (isWorking) return PANE_STATE_WORKING;
  // A turn that ended in a RETRYABLE API failure is not a completed turn
  // (hivecommons/hive#5094). This must sit above the completion test: the
  // completion markers below are "the turn stopped" signals — claude's
  // "✻ …ed for 9m 24s" duration summary is printed for an errored turn exactly
  // as for a successful one — so without this check an API error reads as
  // success and the task is reported complete having shipped nothing.
  //
  // Below isWorking, though: a CLI that is streaming or mid-retry (Claude Code
  // retries some failures itself, rendering a countdown) is left alone, because
  // interrupting that would CAUSE the stall this is meant to prevent.
  // Unretryable FIRST, so a pane carrying both signals fails rather than retries
  // — the veto has to win, or a 403 rendered under the same "API Error:" chrome
  // as a dropped connection would be nudged forever.
  //
  // Both branches exist for one reason: a turn that ended in an API error did not
  // complete. Closing only the retryable half (the original #5094 fix) left a 403
  // or an exhausted quota falling straight through to the completion test and
  // being booked as a finished task — the same defect, one branch over.
  if (paneShowsUnretryableAPIError(text)) {
    return PANE_STATE_FATAL_API_ERROR;
  }
  // Authentication (401 / "Please run /login") AFTER the fatal check — a line
  // carrying both a login hint and an authorization refusal must stay fatal,
  // because /login fixes a 401 and fixes nothing about a 403 (#4400). A human
  // logging in is the only recovery, so this is blocked-on-human, not an error
  // to retry or fail.
  if (paneShowsLoginRequiredError(text)) {
    return PANE_STATE_BLOCKED_ON_HUMAN;
  }
  if (paneShowsTransientAPIError(text)) {
    return PANE_STATE_TRANSIENT_API_ERROR;
  }
  // LAST of the error checks, FIRST before completion: an anchored API error
  // the curated lists cannot name (#5121). Order matters twice over — the
  // curated buckets get first claim on their lines, and a turn that ended in
  // ANY API error must not fall through to the completion test below, which is
  // exactly how #5094's false completions happened.
  if (paneUnknownAPIErrorLine(text) !== null) {
    return PANE_STATE_UNKNOWN_API_ERROR;
  }
  if (hasIdlePrompt && hasCompletionMarker) return PANE_STATE_IDLE_COMPLETE;
  return PANE_STATE_WORKING;
}

function checkTmuxPaneState() {
  try {
    const output = execSync(
      `tmux capture-pane -t ${TMUX_SESSION} -p 2>/dev/null`,
      { encoding: 'utf8', timeout: 15000 }
    );
    const text = output.toString();
    const hasNetworkError = BACKEND === 'goose' && /Network error:|Please resend your message|Could not connect/i.test(text);
    if (hasNetworkError && /> Enter to send/.test(text)) {
      console.log('Goose network error detected — pressing Enter to retry');
      try {
        execSync(`tmux send-keys -t ${TMUX_SESSION} Enter`, { timeout: 15000 });
      } catch (_) {}
      return PANE_STATE_WORKING;
    }
    return classifyTmuxPane(text);
  } catch (_) {
    return PANE_STATE_WORKING;
  }
}

// Relaunch the backend CLI in the tmux session using the flags from
// backends.conf, the same way contributor-agent.sh first launched it.
// launchCommandWithCwd prefixes the launch with a cd into the relay's own
// working directory (the repo root, where `just contribute-hive` starts node).
//
// A relaunch lands in whatever directory the pane's shell is sitting in, and a
// long-lived tmux server can hand out a cwd that no longer exists — every pane
// it forks inherits the dead directory, the shell prints "shell-init: error
// retrieving current directory", and a backend that needs a resolvable cwd dies
// shortly after its first task (agy exits 2; claude/codex/goose tolerate it).
// The Justfile pins the cwd for the FIRST launch; without this, the first
// relaunch would silently undo that.
//
// Prefer HIVE_AGENT_CWD, which both entrypoints export for exactly this: it is
// the neutral directory they launch the CLI from ($HOME). process.cwd() is the
// RELAY's directory, which in local mode is the hive checkout `just
// contribute-hive` was run from — also a clone of the repo the agent is
// assigned to work on. Relaunching there puts the agent back in the tree it
// must not treat as its checkout, silently undoing the launch-side fix on the
// first stall recovery. Fall back to process.cwd() so an older entrypoint that
// does not export the variable keeps its previous behaviour.
function launchCommandWithCwd(launchCmd) {
  const cwd = AGENT_CWD || process.cwd();
  if (!cwd) return launchCmd;
  return `cd ${shellQuote(cwd)} && ${launchCmd}`;
}

function relaunchCLI() {
  const launchCmd = buildLaunchCommand();
  // The pane may be wedged in bash PS2 continuation; clear it or the relaunch
  // command is swallowed as more continuation text and never runs.
  recoverWedgedShell();
  execSync(`tmux send-keys -t ${TMUX_SESSION} ${shellQuote(launchCommandWithCwd(launchCmd))} Enter`, { timeout: 15000 });
  // The CLI is NOT up yet. cliReady must stay false until the readiness
  // classifier positively confirms it, or a task prompt sent in the meantime
  // is typed as literal keystrokes into a bare shell (issue #2203, bug 2).
  cliReady = false;
  // Same recovery contract as the startup path: a relaunch that never reaches
  // a prompt must hand its task back rather than sit on it silently.
  armCLIReadyWait();
  return launchCmd;
}

// dropTaskCredential removes the repo-scoped GitHub token this relay was given
// for the task that is ending.
//
// The token lives in exactly one place — the 0600 GH_TOKEN_CACHE written by
// injectGhToken — and it stays valid for the remainder of wsTokenTTL (~55min)
// no matter what the relay reports. Leaving it on disk after the hub has
// released the work means a turn that is still running can keep pushing and
// opening PRs against an issue the hub has already offered to someone else.
//
// Kept separate from the stop so the ordering in stopAgentForTaskExit() is
// visible at its single call site rather than buried in a compound helper.
function dropTaskCredential() {
  try { fs.unlinkSync(GH_TOKEN_CACHE); } catch (_) {}
  tokenExpiresAt = null;
  // The credential this failure was ABOUT is gone, so the condition dies with
  // it — otherwise a stale "refresh failed" would colour the next task's
  // warnings (#5447).
  tokenRefreshFailedAt = null;
  lastTokenExpiryWarnAt = 0;
}

// stopAgentForTaskExit ends the AGENT, not just the bookkeeping, when a task
// stops being ours (hivecommons/hive#5353 cause B).
//
// Reporting task_complete or task_failed tells the hub to revoke the lease,
// book a cooldown and offer the issue to someone else. Before this existed,
// only five of the relay's task-exit paths touched the pane, so the other
// paths left the original agent running in the same pane, on the same context,
// holding a live scoped token — and it would eventually open a PR against an
// issue the hub had already reassigned. That is the duplicate-PR shape #2356
// exists to prevent, produced from inside the contributor rather than outside
// it, which is why the hub's cooldown accounting cannot see it.
//
// The sequence is the one the task_revoke handler already got right, and the
// ORDER is load-bearing:
//
//  1. Unlink the credential FIRST, so a turn that survives the interrupt (or
//     races it) cannot keep using it. Interrupting first leaves a window in
//     which the agent is being killed but is still authorized.
//  2. Two Ctrl-Cs via quitLiveCLI() — one only cancels a claude/codex/agy
//     turn and leaves the CLI running, so the relaunch command that follows
//     would be typed into the CLI as a chat message (#2203).
//  3. Relaunch, which sets cliReady=false and re-arms armCLIReadyWait(), so
//     the next task's prompt is queued until a clean prompt is confirmed.
//
// Re-entrancy: callers that have ALREADY stopped or relaunched the pane pass
// { skipCLI: true } and get only step 1 — nesting a second quit/relaunch into
// a relaunch already in flight is how double-launches happen. Headless mode
// has no pane at all; there the in-flight one-shot child is killed instead,
// matching what the revoke handler does.
//
// opts.reason names the exit in the relaunch log line, and opts.onRelaunchFailed
// lets a caller with its own post-relaunch latch (the revoke handler's
// readyAfterInteractiveRevoke) unwind it — the latch is only meaningful if a
// relaunch actually happened.
//
// opts.noRelaunch runs steps 1 and 2 but not step 3 — for the signal-shutdown
// path (hivecommons/hive#5655), where the PROCESS is exiting: relaunching
// would type a fresh CLI launch into a pane that may outlive the relay (a
// detached or container-owned tmux session), leaving an orphaned agent nobody
// drives, and would re-arm armCLIReadyWait() timers that can never fire.
//
// Best-effort by design, like quitLiveCLI(): every caller is already on an
// exit path, and a relaunch that lands badly is recovered by the
// armCLIReadyWait() contract.
function stopAgentForTaskExit(opts) {
  const skipCLI = !!(opts && opts.skipCLI);
  const noRelaunch = !!(opts && opts.noRelaunch);
  const reason = (opts && opts.reason) || 'a task exit';
  // Step 1, always — even when the pane is deliberately left alone. A task
  // that is no longer ours must not keep its credential under any branch.
  dropTaskCredential();
  if (skipCLI) return;
  if (CONTRIBUTOR_MODE === MODE_HEADLESS) {
    if (headlessChild) {
      try { headlessChild.kill('SIGKILL'); } catch (_) {}
      headlessChild = null;
      writeHeadlessStatus(HEADLESS_STATE_WAITING);
    }
    return;
  }
  cliReady = false;
  quitLiveCLI();
  if (noRelaunch) return;
  try {
    console.log(`Relaunching ${BACKEND} after ${reason}: ${relaunchCLI()}`);
  } catch (e) {
    cliReadyFailed = true;
    if (opts && opts.onRelaunchFailed) opts.onRelaunchFailed();
    console.error(`Failed to stop and relaunch ${BACKEND} after ${reason}: ${e.message}`);
  }
}

// --- Pane stall backstop ------------------------------------------------
//
// A relay that BELIEVES it is working renews the hub's task lease on every
// progress report, so the hub's wedged-worker reclaim (wsTaskTimeout +
// cleanupLoop in src/pkg/dashboard/contribute_ws.go) can never fire against it.
// That guard only catches a relay that goes SILENT — a crash or a hang. A relay
// stuck in a false "working" belief keeps the lease alive forever, the task
// stays in-progress, no further work is offered, and the only way out is a
// human pressing Ctrl-C. That is not a state a contributor should have to
// notice, let alone fix by hand.
//
// So: if the pane content has not CHANGED at all for this long while we are
// reporting "working", stop asserting progress we cannot substantiate and hand
// the task back as an `environment` failure — the honest verdict, since a
// frozen pane tells us nothing about whether the work itself was done. The hub
// then requeues it through its normal release path.
//
// Deliberately generous: a real agent can sit on one silent command (a long
// test suite, a slow clone) for many minutes without drawing anything new.
const PANE_STALL_TIMEOUT_MS = Number(process.env.HIVE_PANE_STALL_TIMEOUT_MS) || 20 * 60 * 1000;

// Observed live (hivecommons/hive): a task crossed PANE_STALL_TIMEOUT_MS while
// agy sat blocked on a slow `gh pr create` network round trip. The relay
// declared it a failure and moved on to the next task, and the pane then, only
// seconds to minutes later, printed the CLI's real completion summary — with a
// genuine PR link. The pane fingerprint at the instant of the stall check
// cannot contain output that has not streamed in yet, so checking it harder at
// that single instant cannot fix this; giving the CLI a FEW more ticks to
// reach a real PANE_STATE_IDLE_COMPLETE (which already runs full PR/no-work
// detection, see detectPRURL/detectNoWorkVerdict below) can. So the stall
// verdict must be CONFIRMED on this many consecutive ticks — each
// PROGRESS_REPORT_INTERVAL_MS apart, and each one re-running
// checkTmuxPaneState() first — before the relay gives up. A tick where the
// pane has since gone idle-complete, or produced any new output, exits this
// path before the confirm count is ever consulted.
const PANE_STALL_CONFIRM_TICKS = Math.max(1, Number(process.env.HIVE_PANE_STALL_CONFIRM_TICKS) || 2);

// ── Chrome-idle grace before an unverdicted completion (#5376) ───────────────
//
// THE DEMOTION. classifyTmuxPane() used to be the whole completion contract:
// PANE_STATE_IDLE_COMPLETE meant "task done", full stop. It is no longer
// allowed to say that on its own. It says "this pane looks idle" — a liveness
// judgement its per-backend chrome CAN support — and the agent's own
// HIVE_VERDICT: line says whether the task is done.
//
// THE FALLBACK, and why this shape. Not every backend will emit the sentinel
// reliably; some builds ignore instructions in a long prompt, and the marker
// can scroll out of the fifteen-line tail on a chatty summary. Two honest
// options were on the table:
//
//   (a) idle-without-verdict is "still running" until the progress lease
//       expires. Rejected. A non-compliant agent that genuinely finished draws
//       nothing more, so paneChangedSince() stops re-arming the lease and the
//       task dies at PANE_STALL_TIMEOUT_MS as an `environment` FAILURE — with
//       its PR already open. That converts every success by a non-compliant
//       backend into a false failure and a wasted re-offer. It is the #4182 /
//       #4127 shape (a finished task killed by the stall backstop) reintroduced
//       deliberately, and it is worse than the bug this issue exists to end.
//
//   (b) a BOUNDED grace period after idle, then complete anyway. Chosen.
//
// What (b) buys, precisely: the sentinel becomes the fast path — an agent that
// says it is done is believed on the spot, verdict recorded — while chrome
// alone must hold idle for CHROME_IDLE_GRACE_TICKS consecutive ticks before it
// is allowed to conclude anything. That directly targets the failure mode the
// thirteen issues share: every one of them was a MOMENTARY misread — a
// duration summary printed mid-turn, a status row between tool calls, an
// errored turn parked at the prompt. A pane that has rendered idle chrome and
// nothing else across several minutes is a far weaker claim than a single
// frame, and any new output at all resets the count (see recordChromeIdleTick).
//
// What (b) does NOT buy: it is still chrome, so it is still fallible, just
// slower and much harder to trip. The verdict path is the one that is
// trustworthy. The grace exists so that adopting it costs nothing when an
// agent does not comply, which is what makes the demotion shippable at all.
//
// The completion is marked `chrome_idle` when it comes from this path, so the
// hub and the operator can see which signal ended a task and per-backend
// non-compliance is measurable rather than guessed at.
const CHROME_IDLE_GRACE_TICKS = Math.max(1, Number(process.env.HIVE_CHROME_IDLE_GRACE_TICKS) || 3);

// How many CONSECUTIVE ticks the pane has classified IDLE_COMPLETE with no
// completion verdict in sight. Reset on task start and on any tick that does
// not see an unverdicted idle pane.
let chromeIdleTicks = 0;

// recordChromeIdleTick advances (or resets) the grace counter and reports
// whether chrome alone has now earned the right to end the task.
//
// PURE with respect to the pane fingerprint: it takes the already-captured
// lines and never reads the pane itself. paneStalled() is destructive — the
// first call seeing new output consumes it (#5333) — so nothing on the tick
// path may take a second reading.
function recordChromeIdleTick(idleWithoutVerdict) {
  if (!idleWithoutVerdict) {
    chromeIdleTicks = 0;
    return false;
  }
  chromeIdleTicks++;
  return chromeIdleTicks >= CHROME_IDLE_GRACE_TICKS;
}

function resetChromeIdleGrace() {
  chromeIdleTicks = 0;
}

let lastPaneFingerprint = null;
let lastPaneChangeAt = 0;
// How many CONSECUTIVE ticks paneStalled() has now returned true. Distinct
// from the fingerprint clock above: that clock says "how long has it been
// unchanged", this says "how many chances has the CLI had to prove otherwise
// since we first noticed". Reset by resetPaneStallClock() and by any tick
// where paneStalled() is false (new output resets the whole stall story).
let stallConfirmCount = 0;

// Transient-API-error nudge state (hivecommons/hive#5094), scoped to the
// CURRENT task: how many retries we have typed and when the last one went out.
// Both are reset at task start — a previous task's exhausted budget must not
// deny this one its retries.
let transientNudgeCount = 0;
let lastTransientNudgeAt = 0;

function resetTransientNudgeState() {
  transientNudgeCount = 0;
  lastTransientNudgeAt = 0;
}

// Autonomy-nudge state (hivecommons/hive#5281), scoped to the CURRENT task.
// Budget of exactly one: a question the agent re-asks AFTER being told to
// proceed autonomously is a question it genuinely cannot answer itself, and
// re-nudging it would loop until the max-duration ceiling. Once spent, the pane
// reports blocked_on_human exactly as it does today.
let autonomyNudgeSent = false;

function resetAutonomyNudgeState() {
  autonomyNudgeSent = false;
}

function resetPaneStallClock() {
  lastPaneFingerprint = null;
  lastPaneChangeAt = Date.now();
  stallConfirmCount = 0;
  // A new task also starts with a clean CLI-liveness count: shell readings from
  // the previous task say nothing about this one.
  consecutiveShellReadings = 0;
  // Likewise the chrome-idle grace (#5376): idle ticks accumulated while the
  // PREVIOUS task wound down must never count toward ending this one.
  resetChromeIdleGrace();
}

// paneStalled records the current pane content and reports whether it has been
// byte-for-byte identical for longer than PANE_STALL_TIMEOUT_MS.
function paneStalled(tmuxLines) {
  const fingerprint = Array.isArray(tmuxLines) ? tmuxLines.join('\n') : String(tmuxLines || '');
  const now = Date.now();
  if (fingerprint !== lastPaneFingerprint) {
    lastPaneFingerprint = fingerprint;
    lastPaneChangeAt = now;
    return false;
  }
  // An empty capture means tmux told us nothing (session gone, capture failed).
  // That is not evidence of a stalled AGENT, and other paths already handle a
  // missing pane, so never let it trip this backstop.
  if (!fingerprint) return false;
  if (!lastPaneChangeAt) { lastPaneChangeAt = now; return false; }
  return now - lastPaneChangeAt >= PANE_STALL_TIMEOUT_MS;
}

// paneChangedSince reports whether the pane differs from the last fingerprint
// paneStalled() recorded — i.e. whether the agent produced output since the
// previous tick (hivecommons/hive#5321).
//
// PURE BY CONSTRUCTION: it must not update lastPaneFingerprint or
// lastPaneChangeAt. paneStalled() is destructive — the first call that sees new
// output records it and returns false, so a second call in the same tick sees
// no change. progressTick() calls this one FIRST and paneStalled() (via
// paneStallConfirmed) later in the same tick; if this function recorded, the
// stall detector would see an already-consumed change every time and could
// never accumulate a stall. Read only.
//
// A null fingerprint means no tick has recorded one yet (fresh task): that is
// not evidence of progress, and treating it as such would hand a task that has
// never drawn anything a free lease renewal.
function paneChangedSince(tmuxLines) {
  if (lastPaneFingerprint === null) return false;
  const fingerprint = Array.isArray(tmuxLines) ? tmuxLines.join('\n') : String(tmuxLines || '');
  // An empty capture means tmux told us nothing (session gone, capture failed).
  // paneStalled() refuses to read that as a stall; symmetrically it must not be
  // read as progress either.
  if (!fingerprint) return false;
  return fingerprint !== lastPaneFingerprint;
}

// paneStallConfirmed wraps paneStalled() with the multi-tick confirmation
// described above it. Any tick where paneStalled() is false (new output
// appeared) resets the count — the CLI gets full credit for proving it is not
// stuck, not just a one-shot escape. Kept separate from paneStalled() itself
// so tests of the underlying timeout signal are unaffected by the confirm
// gate, and vice versa.
function paneStallConfirmed(tmuxLines) {
  if (!paneStalled(tmuxLines)) {
    stallConfirmCount = 0;
    return false;
  }
  stallConfirmCount++;
  return stallConfirmCount >= PANE_STALL_CONFIRM_TICKS;
}

function flushPendingTask() {
  if (!pendingTask) return;
  const t = pendingTask;
  pendingTask = null;
  tmuxSendKeys(t);
}

function checkTmuxIdle() {
  return checkTmuxPaneState() === PANE_STATE_IDLE_COMPLETE;
}

const TASK_GRACE_PERIOD_MS = 180000;
let taskAssignedAt = 0;
let tasksCompletedCount = 0;
// The completed-task count at which the periodic memory-cleanup restart last
// fired (issue #2596). The restart predicate below is re-entered by the #2203
// readiness/pending-task guard: the restart queues the prompt, clears cliReady,
// relaunches, and the readiness callback calls flushPendingTask() ->
// tmuxSendKeys() again. tasksCompletedCount only changes on an actual
// completion, so without latching, "count % RESET_EVERY_N === 0" stays true and
// the CLI restarts forever, never delivering the next task. Latching the count
// makes the reset one-shot per threshold crossing; a value that no real count
// reaches keeps the first crossing (count 0 is excluded anyway) from being
// treated as already-serviced.
let lastResetAtCount = -1;
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

// failCurrentTask reports the active task as failed.
//
// opts.kind (hivecommons/hive#2547) optionally states WHY: 'environment' when
// this client's own runtime could not run the work (the CLI never started, it
// crashed, the backend has no headless mode) versus 'task' when the work was
// attempted and failed on its merits. Omit it when the cause is genuinely
// ambiguous — the hub normalizes absent to 'unspecified', and guessing would be
// worse than saying nothing, since an operator reads this to attribute failures.
//
// It is advisory: the hub records and displays it and does not route, gate, or
// change the work item's failure cooldown on it. Older hubs ignore the field.
//
// opts.skipCLI (hivecommons/hive#5353) says the CALLER has already dealt with
// the pane — it quit and relaunched the CLI itself, or the CLI is already gone.
// The credential is still dropped; only the quit/relaunch is skipped, so a
// relaunch already in flight is not nested inside another one.
function failCurrentTask(reason, opts) {
  if (!currentTask) return;
  const permanent = !!(opts && opts.permanent);
  const kind = (opts && opts.kind) || undefined;
  const taskId = currentTask.task_id;
  const taskGen = currentTask.task_gen;
  // Captured BEFORE the agent is stopped: the pane text is the evidence the
  // hub and the operator read to understand the failure, and quitLiveCLI()
  // followed by a relaunch overwrites it with launch chrome.
  const tmuxLines = captureTmuxLines(TMUX_TAIL_LINES);
  // Cause B (#5353): the hub is about to release this issue and offer it to
  // someone else. Stop the agent and drop its token FIRST, so the report and
  // the reality agree at the instant the hub acts on it.
  stopAgentForTaskExit({ skipCLI: !!(opts && opts.skipCLI) });
  console.error(`Task ${taskId} failed${permanent ? ' permanently' : ''}${kind ? ` [${kind}]` : ''}: ${reason}`);
  send({
    type: 'task_failed',
    seq: nextSeq(),
    task_id: taskId,
    task_gen: taskGen,
    result: 'failed',
    reason,
    permanent,
    failure_kind: kind,
    tmux_output: tmuxLines,
    ...effectiveSelectionFields(),
  });
  currentTask = null;
  taskAssignedAt = 0;
  if (progressInterval) { clearInterval(progressInterval); progressInterval = null; }
  if (taskTimeoutHandle) { clearTimeout(taskTimeoutHandle); taskTimeoutHandle = null; }
  // skipReady: the caller knows this contributor cannot run anything right now
  // (the CLI never reached its prompt), so it hands the task back WITHOUT
  // claiming to be free. Advertising 'ready' here would just pull in another
  // task the CLI still cannot run. The caller re-advertises on recovery.
  if (!(opts && opts.skipReady)) {
    send({ type: 'ready', seq: nextSeq() });
  }
}

function startProgressReporting() {
  if (progressInterval) clearInterval(progressInterval);
  if (taskTimeoutHandle) clearTimeout(taskTimeoutHandle);
  if (!taskAssignedAt) taskAssignedAt = Date.now();
  lastProgressTick = Date.now();
  // Every task starts with a clean stall clock — the previous task's pane
  // fingerprint says nothing about this one.
  resetPaneStallClock();
  // Likewise the retry budget: a previous task that exhausted its API-error
  // retries must not deny this one its own (#5094).
  resetTransientNudgeState();
  // And the one-shot autonomy reminder (#5281), for the same reason.
  resetAutonomyNudgeState();

  armTaskProgressLease();

  progressInterval = setInterval(progressTick, PROGRESS_REPORT_INTERVAL_MS);
}

// armTaskProgressLease (re)starts the max-duration timer from NOW.
//
// Called once at task start and again from every tick that observes forward
// progress, which is what turns MAX_TASK_DURATION_MS from a wall-clock budget
// into a lease (hivecommons/hive#5321). An agent producing output keeps its
// lease; a silent one lets it run down.
//
// Deliberately mirrors the sibling per-task clocks armed alongside it —
// resetPaneStallClock(), resetTransientNudgeState(), resetAutonomyNudgeState()
// — all of which were already progress-aware. This one was the odd clock out.
//
// Takes no locks and touches no shared connection state: it clears and re-sets
// a timer handle owned by this module, so it is safe to call from inside
// progressTick without regard to what the caller already holds.
function armTaskProgressLease() {
  if (taskTimeoutHandle) clearTimeout(taskTimeoutHandle);
  // Deliberately NOT unref'd: no other timer in this relay is, and the handle
  // is cleared on every task exit (completion, failure, revoke), so it never
  // outlives the task it bounds. Changing process-exit semantics is not part of
  // this fix.
  taskTimeoutHandle = setTimeout(onTaskProgressLeaseExpired, MAX_TASK_DURATION_MS);
}

// onTaskProgressLeaseExpired runs when MAX_TASK_DURATION_MS elapsed with no
// observed progress.
//
// It re-checks the progress signal rather than trusting the timer alone: the
// tick loop re-arms on output, but a tick that lands microseconds after the
// timer fired would otherwise lose the race and kill a live agent for it. If
// the pane HAS changed within the lease window, the lease is simply renewed.
//
// Reaching the kill means the relay saw no progress for the lease window AND
// (normally) the stall detector already had its say — so this is a runtime
// verdict, not a judgement of the work: kind 'environment' (#5321). Previously
// this path passed no opts at all, so an infrastructure ceiling was recorded as
// a plain task failure.
function onTaskProgressLeaseExpired() {
  if (!currentTask) return;
  const now = Date.now();
  const elapsed = taskAssignedAt ? now - taskAssignedAt : 0;

  // Absolute backstop first: past this, no amount of output buys more time.
  if (elapsed >= ABSOLUTE_TASK_DEADLINE_MS) {
    failCurrentTask(
      `task exceeded the absolute deadline (${Math.round(ABSOLUTE_TASK_DEADLINE_MS / 60000)}min) without completing`,
      { kind: 'environment' }
    );
    return;
  }

  // Forward progress since the lease was armed? Renew it and say nothing.
  // lastPaneChangeAt is maintained by paneStalled() on every tick, so it is the
  // same signal the stall detector uses — one definition of "progress", not two.
  if (lastPaneChangeAt && now - lastPaneChangeAt < MAX_TASK_DURATION_MS) {
    armTaskProgressLease();
    return;
  }

  failCurrentTask(
    `no observed progress for ${MAX_TASK_DURATION_MS / 60000}min — the agent CLI is not visibly working`,
    { kind: 'environment' }
  );
}

// One iteration of the progress/completion/crash-detection loop. Extracted from
// the setInterval body so it can be driven deterministically from tests.
// handleTransientAPIError recovers a task whose turn ended in a retryable API
// failure (hivecommons/hive#5094).
//
// Before this existed the pane classified as IDLE_COMPLETE and the relay
// reported the task COMPLETED — the hub booked a completion that shipped
// nothing, reassigned the contributor, and the half-finished work was orphaned.
// Every branch here is a way of NOT doing that: retry it, hand it to the human
// already watching, or fail it honestly. None of them claims success.
//
// The goose backend has had this shape since long before #5094 — see
// checkTmuxPaneState, which presses Enter on a goose network error and returns
// WORKING. This generalises that precedent rather than inventing one.
function handleTransientAPIError(tmuxLines) {
  if (!currentTask) return;
  const now = Date.now();
  const progressBase = {
    type: 'task_progress',
    seq: nextSeq(),
    task_id: currentTask.task_id,
    task_gen: currentTask.task_gen,
    tmux_output: tmuxLines,
  };

  // A human AT the pane owns it, and a watchdog must never type over someone
  // mid-keystroke. But presence is a recency question, not a connection one
  // (#5277): a dashboard terminal tab left open is a connected client and not a
  // person, and treating the two alike disabled recovery entirely for as long
  // as the tab lived. An attached-but-quiet client falls through to the retry
  // below; only a recently active one still takes this branch.
  const presence = tmuxSessionHumanPresence();
  if (presence.active) {
    const since = presence.idleMs === null
      ? 'activity unknown'
      : `last input ${Math.round(presence.idleMs / 1000)}s ago`;
    console.warn(`Task ${currentTask.task_id} stopped on a retryable API error; ` +
      `someone is active on ${TMUX_SESSION} (${since}), so not typing a retry`);
    send({
      ...progressBase,
      status: 'blocked_on_human',
      attention: true,
      summary: 'Agent stopped on a retryable API error; a human is active in the pane',
      ...progressModelFields(),
    });
    return;
  }
  if (presence.attached) {
    console.warn(`Task ${currentTask.task_id} stopped on a retryable API error; ` +
      `a client is attached to ${TMUX_SESSION} but has been idle ` +
      `${Math.round(presence.idleMs / 1000)}s, so proceeding with the retry`);
  }

  // Bounded: a persistent upstream failure ends as an honest environment
  // failure, which the hub records and can re-offer, rather than an infinite
  // typing loop or a fabricated completion.
  if (transientNudgeCount >= TRANSIENT_API_ERROR_MAX_NUDGES) {
    failCurrentTask(
      `agent stopped on a retryable API error and did not recover after ` +
      `${TRANSIENT_API_ERROR_MAX_NUDGES} retries`,
      { kind: 'environment' }
    );
    return;
  }

  // Give the previous retry time to land before typing another.
  if (lastTransientNudgeAt && now - lastTransientNudgeAt < TRANSIENT_API_ERROR_NUDGE_COOLDOWN_MS) {
    send({ ...progressBase, status: 'working', ...progressModelFields() });
    return;
  }

  transientNudgeCount++;
  lastTransientNudgeAt = now;
  console.warn(`Transient API error on ${currentTask.task_id} — sending retry ` +
    `${transientNudgeCount}/${TRANSIENT_API_ERROR_MAX_NUDGES}`);
  try {
    tmuxSendNudge(TRANSIENT_API_ERROR_NUDGE_MESSAGE);
  } catch (e) {
    console.error('Failed to send the retry nudge:', e.message);
  }
  send({
    ...progressBase,
    status: 'working',
    summary: `Retrying after a transient API error ` +
      `(${transientNudgeCount}/${TRANSIENT_API_ERROR_MAX_NUDGES})`,
    ...progressModelFields(),
  });
}

// paneHasPresentHuman is the one place this file asks "is a person there?".
//
// It exists as a named seam because #5281 and #5094 must answer it the SAME
// way: a guard that diverges between two nudges is how you get a pane that is
// safe from one watchdog and not the other.
//
// Today it is the bare attached check — a client is connected. #5277 is
// replacing that with a recency test on tmux's `client_activity`, because a
// dashboard terminal tab left open is a connected client and not a person.
// When that lands this body becomes `return tmuxSessionHumanPresence().active;`
// and both callers inherit it; that one line is the whole follow-up.
function paneHasPresentHuman() {
  return tmuxSessionHasAttachedClient();
}

// maybeSendAutonomyNudge types a one-shot reminder at an unattended pane that
// stopped to ask a question it was already instructed to answer for itself
// (hivecommons/hive#5281), and reports whether it did.
//
// Detection without recovery is what this fixes. The relay already SEES the
// question and raises `attention`, but an attention flag only helps someone who
// is watching something, and a contributor run by a user who never attaches to
// tmux is a supported way to run one. For that user every question the agent
// asks costs 20-30 minutes and a failed task.
//
// Four things must all hold, and each one is a separate way to get this wrong:
//
//  1. The pane is blocked on a QUESTION, not on something only a person can
//     answer. See classifyBlockedOnHumanReason.
//  2. It is not a login/401 pane. Belt to the classifier's braces: a /login
//     flow is reached by a different route through checkTmuxPaneState (#4400),
//     so excluding it here makes "never nudge a login" true by construction
//     rather than true by coincidence.
//  3. Nobody is at the pane.
//  4. The one-shot budget is unspent.
function maybeSendAutonomyNudge(tmuxLines) {
  if (!currentTask) return false;
  if (autonomyNudgeSent) return false;

  const pane = tmuxLines.join('\n');
  if (paneShowsLoginRequiredError(pane)) return false;
  if (classifyBlockedOnHumanReason(pane) !== BLOCKED_REASON_QUESTION) return false;
  if (paneHasPresentHuman()) return false;

  // Spend the budget BEFORE typing. A send that throws has still disturbed the
  // pane, and retrying it on the next tick is the loop this budget exists to
  // prevent.
  autonomyNudgeSent = true;
  console.warn(`Task ${currentTask.task_id} is blocked on a question with nobody attached to ` +
    `${TMUX_SESSION} — reminding it to proceed autonomously (once per task)`);
  try {
    tmuxSendNudge(AUTONOMY_NUDGE_MESSAGE);
  } catch (e) {
    console.error('Failed to send the autonomy reminder:', e.message);
    return false;
  }
  send({
    type: 'task_progress',
    seq: nextSeq(),
    task_id: currentTask.task_id,
    task_gen: currentTask.task_gen,
    status: 'working',
    summary: 'Agent asked a question with no human attached; reminded it to proceed autonomously',
    tmux_output: tmuxLines,
    ...progressModelFields(),
  });
  return true;
}

function progressTick() {
  lastProgressTick = Date.now();
  if (!currentTask) return;

  // Surface the credential's remaining lifetime BEFORE the grace-period return
  // and before any of the pane judging below, so a token that is about to lapse
  // is reported on its own schedule rather than only on ticks that happen to get
  // as far as a progress report (#5447). Warn-only — see warnOnTokenExpiry.
  warnOnTokenExpiry();

  if (Date.now() - taskAssignedAt < TASK_GRACE_PERIOD_MS) return;

  // #4117: re-detect the running model each tick so a mid-session model switch
  // (claude `/model`) reaches the hub within one progress interval, piggybacked
  // on the task_progress reports below.
  refreshDetectedModel();

  try {
    // See probeCLIPresence(): this asks the PANE what it is running, rather than
    // grepping the whole process table for the backend's name — a scan the
    // relay's own launcher and tmux session always satisfied.
    const presence = probeCLIPresence();
    const cliAlive = !presence.gone;
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
        // skipCLI: this branch's premise is that the CLI process is ALREADY
        // gone (probeCLIPresence confirmed it), and the relaunch that follows
        // is this path's own. There is no live turn to interrupt, so quitting
        // here would only send Ctrl-Cs at a bare shell and then race the
        // relaunch below. The token is dropped regardless (#5353).
        failCurrentTask(
          `CLI process exited ${MAX_TASK_CLI_RESTARTS} times for ${key} — giving up on this task (relay still accepting other work)`,
          { permanent: true, skipCLI: true }
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
      // environment: the agent CLI process died; nothing was judged about the work.
      // skipCLI for the same reason as the give-up branch above — the process
      // is gone and the relaunch just above is this path's own (#5353).
      failCurrentTask('CLI process exited — restarted', { kind: 'environment', skipCLI: true });
      return;
    }
    // A pane sitting at a shell is never evidence that the AGENT finished: the
    // CLI is simply not there. Without this, the first (still unconfirmed)
    // shell reading falls through to the completion check below, where the
    // dead CLI's LAST FRAME — ready chrome and all, still on screen — reads as
    // "agent idle" and reports a task nobody did as completed. Hold here and
    // let the next tick either confirm the death or clear it.
    //
    // bob is exempt: it exits at the end of every turn, so for bob a shell pane
    // IS the completion signal (see cliExitIsNormal above).
    if (presence.isShell && !cliExitIsNormal) {
      console.warn(`Pane is at a shell prompt, not ${BACKEND} — awaiting confirmation before judging the task`);
      send({ type: 'task_progress', seq: nextSeq(), task_id: currentTask.task_id, task_gen: currentTask.task_gen, status: 'working', tmux_output: captureTmuxLines(TMUX_TAIL_LINES), ...progressModelFields() });
      return;
    }
  } catch (_) {}

  // Never judge a task the agent has not been given (hivecommons/hive#5650).
  //
  // tmuxSendKeys() QUEUES the prompt instead of typing it whenever the CLI is
  // not confirmed ready (or the pane has fallen back to a shell), and
  // flushPendingTask() delivers it later. Until that happens the pane holds
  // only the PREVIOUS task's transcript, so everything read below — the
  // completion verdict, the idle chrome, the stall fingerprint — is evidence
  // about work this task never touched. That is how a task whose prompt was
  // still queued got booked `completed` with no PR: the pane still showed the
  // prior task's HIVE_VERDICT line and satisfied the completion check on the
  // first tick past the grace period.
  //
  // Reporting `working` and returning is deliberate rather than failing here:
  // armCLIReadyWait() already owns this case and hands the task back with a
  // real reason at CLI_READY_TIMEOUT_MS. The max-duration lease is the second
  // backstop, and it is deliberately NOT renewed by this path — a pane the
  // agent was never prompted with is not forward progress.
  if (CONTRIBUTOR_MODE !== MODE_HEADLESS && !taskPromptDelivered) {
    console.warn(`Task ${currentTask.task_id} has not been typed into the ${BACKEND} pane yet — reporting progress without judging the pane`);
    send({ type: 'task_progress', seq: nextSeq(), task_id: currentTask.task_id, task_gen: currentTask.task_gen, status: 'working', tmux_output: captureTmuxLines(TMUX_TAIL_LINES), ...progressModelFields() });
    return;
  }

  const paneState = checkTmuxPaneState();
  const tmuxLines = captureTmuxLines(TMUX_TAIL_LINES);

  // #5321: forward progress renews the max-duration lease. Recorded here,
  // before any branch below can return, so EVERY pane state gets the credit —
  // an agent stepping through blocked_on_human or a retried API error is still
  // visibly alive, and none of those states should burn down a deadline whose
  // question is "is this thing moving at all". paneChangedSince() is a pure
  // read of the fingerprint clock paneStalled() maintains; the stall detector
  // below still does its own recording, unaffected.
  if (paneChangedSince(tmuxLines)) armTaskProgressLease();

  // #5376: the agent's own completion sentinel, read BEFORE the pane state is
  // consulted, because it — not the chrome — is what now decides the task is
  // done. Both HIVE_VERDICT: complete and HIVE_VERDICT: no_work_needed count.
  //
  // Read from the already-captured tmuxLines: no second pane read, so the
  // destructive paneStalled() fingerprint (#5333) is untouched.
  const paneVerdict = detectCompletionVerdict(tmuxLines);

  // #5650: a verdict has to belong to THIS task. The relay drives one
  // long-lived CLI, so a task begins against a pane still showing the previous
  // task's finished transcript — HIVE_VERDICT line included — and reading that
  // line back is not a completion, it is the last task's statement being
  // re-read. deliveredVerdictBaseline is exactly the line that was on the pane
  // when this task's prompt was typed, so an identical line cannot be about
  // this task.
  //
  // Suppressing it does not strand the task: with no verdict the chrome-idle
  // grace below becomes the signal, precisely as it is for an agent that never
  // prints the sentinel at all.
  const staleVerdict = !!paneVerdict && paneVerdict.line === deliveredVerdictBaseline;
  if (staleVerdict) {
    console.warn(`Ignoring the HIVE_VERDICT line already on the pane when ${currentTask.task_id} was dispatched — it is the previous task's verdict, not this one's`);
  }
  const completionVerdict = staleVerdict ? null : paneVerdict;

  // Chrome-idle grace (#5376). classifyTmuxPane() saying IDLE_COMPLETE is now
  // only a hint; it must repeat across CHROME_IDLE_GRACE_TICKS ticks before it
  // may end a task on its own. A verdict short-circuits the wait entirely.
  const idleWithoutVerdict = paneState === PANE_STATE_IDLE_COMPLETE && !completionVerdict;
  const chromeIdleGraceElapsed = recordChromeIdleTick(idleWithoutVerdict);

  // A verdict ends the task from ANY pane state. This is the point of the
  // change: an agent that says it is finished is finished, whatever its CLI
  // chose to render around the statement. It is precisely the case the
  // thirteen chrome issues kept getting wrong from the other side — a real
  // completion the classifier read as WORKING (#4127, #4181, #4259) and the
  // stall backstop then failed with the PR already open.
  //
  // The two error states are excluded, and deliberately: a pane showing an
  // authorization refusal or a truncated retryable response has NOT completed,
  // and a stale verdict line still on screen from earlier in the transcript
  // must not launder that into a success. Those branches below own those panes.
  const apiErrorState = paneState === PANE_STATE_TRANSIENT_API_ERROR ||
    paneState === PANE_STATE_UNKNOWN_API_ERROR ||
    paneState === PANE_STATE_FATAL_API_ERROR;
  const verdictCompletes = !!completionVerdict && !apiErrorState;

  if (verdictCompletes || (paneState === PANE_STATE_IDLE_COMPLETE && chromeIdleGraceElapsed)) {
    // How this task ended, recorded so the hub and the operator can tell the
    // trustworthy signal from the fallback — and so per-backend sentinel
    // non-compliance is measurable rather than guessed at.
    const completionSignal = verdictCompletes ? 'verdict' : 'chrome_idle';
    console.log(`Task ${currentTask.task_id} completed — signal=${completionSignal}` +
      (verdictCompletes ? ` (HIVE_VERDICT: ${completionVerdict.verdict})` : ` (pane idle for ${chromeIdleTicks} consecutive checks, no verdict emitted)`));
    resetChromeIdleGrace();
    // Successful completion clears this work item's crash-retry budget.
    cliRestartCounts.delete(taskKey(currentTask));
    // Best-effort: report the PR the agent opened, if one is visible in its
    // recent output, so the hub can distinguish "shipped a PR" from "just went
    // idle" and pick the right issue cooldown (hivecommons/hive#2393 item 7).
    // Empty when no PR link is found — the hub then applies the short cooldown.
    const prURL = detectPRURL(tmuxLines, currentTask.repo);
    if (prURL) console.log(`Detected PR for ${currentTask.task_id}: ${prURL}`);
    // #3987: only report a no_work_needed verdict when no PR was shipped — a
    // visible PR contradicts "nothing shippable" (the hub would override the
    // claim with "shipped" anyway).
    const noWork = prURL || !completionVerdict || completionVerdict.verdict !== HIVE_VERDICT_NO_WORK
      ? null
      : completionVerdict;
    if (noWork) console.log(`Detected no_work_needed verdict for ${currentTask.task_id}: ${noWork.reason || '(no reason)'}`);
    // Cause B (#5353). "Idle" here is a verdict read off the pane's rendering
    // chrome, and it is wrong often enough to have produced thirteen separate
    // issues. When it is wrong, the agent is still mid-turn — and reporting
    // task_complete makes the hub revoke the lease, book the cooldown, and
    // offer the issue to somebody else while that turn keeps running in this
    // pane on this token. Stopping the CLI and dropping the credential here
    // makes the misread cost a retry instead of a duplicate PR.
    //
    // Note the ordering against `send` below: the agent is stopped BEFORE the
    // hub is told, so at the instant the hub acts on the completion the claim
    // is already true. tmuxLines was captured above, so the evidence the hub
    // receives is still the agent's own output and not launch chrome.
    //
    // bob is exempt from the quit half: it is not a persistent REPL and has
    // already exited at the end of its turn, so the pane is a bare shell and
    // there is nothing to interrupt — sending Ctrl-C at that shell and then
    // racing the bob-specific relaunch below is how a pane ends up with two
    // launches in flight. Its credential is still dropped.
    const bobAlreadyExited = BACKEND === 'bob' && !bobIsRunning();
    stopAgentForTaskExit({ skipCLI: bobAlreadyExited });
    const completionSummary = noWork
      ? 'Agent returned to idle (reported no_work_needed)'
      : (verdictCompletes
        ? 'Agent reported the task complete (HIVE_VERDICT)'
        : `Agent returned to idle (no verdict emitted; pane idle for ${CHROME_IDLE_GRACE_TICKS} consecutive checks)`);
    send({ type: 'task_complete', seq: nextSeq(), task_id: currentTask.task_id, task_gen: currentTask.task_gen, result: 'completed', summary: completionSummary, tmux_output: tmuxLines, pr_url: prURL, completion_signal: completionSignal, verdict: noWork ? noWork.verdict : undefined, verdict_reason: noWork ? noWork.reason : undefined });
    // bob exits after each turn, so the pane is now a bare shell. Bring it
    // back up before the next task, or the prompt would be typed into bash
    // ("-bash: <prompt>: command not found") and silently lost.
    if (bobAlreadyExited) {
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
  } else if (paneState === PANE_STATE_IDLE_COMPLETE) {
    // Idle chrome, no verdict, grace not yet elapsed (#5376). Report progress
    // and wait — this is the tick or two in which a momentary misread (a
    // duration summary printed mid-turn, a status row between tool calls)
    // resolves itself by the pane simply carrying on.
    //
    // This branch MUST exist ahead of the stall backstop below rather than
    // falling into it. An idle pane is byte-for-byte identical frame to frame,
    // so the stall detector would accumulate against it and eventually hand the
    // task back as an `environment` failure — a finished task reported as a
    // failure, which is exactly the #4127/#4182 shape and strictly worse than
    // the false completion this change is removing. The grace counter above is
    // the bound here; the stall clock is not.
    console.log(`Task ${currentTask.task_id}: pane looks idle but no HIVE_VERDICT yet — ${chromeIdleTicks}/${CHROME_IDLE_GRACE_TICKS} checks before completing on chrome alone`);
    send({ type: 'task_progress', seq: nextSeq(), task_id: currentTask.task_id, task_gen: currentTask.task_gen, status: 'working', tmux_output: tmuxLines, ...progressModelFields() });
  } else if (paneState === PANE_STATE_BLOCKED_ON_HUMAN) {
    // #5281: before reporting a blocked pane to a human who may not be there,
    // see whether this is a question the agent was already told to answer
    // itself. At most once per task; everything below is unchanged and is what
    // runs on every later tick.
    if (maybeSendAutonomyNudge(tmuxLines)) return;
    console.warn(`Task ${currentTask.task_id} is blocked waiting for human input`);
    send({
      type: 'task_progress',
      seq: nextSeq(),
      task_id: currentTask.task_id,
      task_gen: currentTask.task_gen,
      status: 'blocked_on_human',
      attention: true,
      summary: 'Agent is waiting for human input in the tmux pane',
      tmux_output: tmuxLines,
      ...progressModelFields(),
    });
  } else if (paneState === PANE_STATE_TRANSIENT_API_ERROR) {
    handleTransientAPIError(tmuxLines);
  } else if (paneState === PANE_STATE_UNKNOWN_API_ERROR) {
    // Instrumentation first (#5121): log the exact line the curated lists
    // could not name, so the lists can be grown from real occurrences. Then
    // the bounded transient path — retry up to the budget, honest environment
    // failure after it, blocked_on_human if someone is attached. Shared budget
    // and cooldown with the transient state: it is the same task either way.
    console.warn(`Unrecognised API error (hivecommons/hive#5121) — treating as transient: ` +
      `${paneUnknownAPIErrorLine(tmuxLines.join('\n')) || '(line scrolled away)'}`);
    handleTransientAPIError(tmuxLines);
  } else if (paneState === PANE_STATE_FATAL_API_ERROR) {
    // No retry: an authorization refusal or an exhausted quota cannot be cleared
    // by repeating the request (#4400, #4583). Hand the task back honestly so the
    // hub records it and can re-offer it once an operator fixes the cause —
    // rather than claiming a completion that shipped nothing.
    failCurrentTask(
      'agent stopped on an API failure a retry cannot clear (authorization or quota)',
      { kind: 'environment' }
    );
  } else {
    // Stall backstop: a pane frozen this long is not evidence of work, and
    // continuing to report "working" would renew the hub's lease forever.
    // Confirmed over PANE_STALL_CONFIRM_TICKS ticks rather than acted on
    // immediately — see the comment above PANE_STALL_CONFIRM_TICKS for why a
    // single instant cannot distinguish "stuck" from "about to finish".
    if (paneStallConfirmed(tmuxLines)) {
      // The CLI may still be mid-turn on the task we are about to give up on
      // (observed live: a slow `gh pr create` returned, with a real PR link,
      // seconds after the stall verdict). Relaunch it so the NEXT task starts
      // on a demonstrably fresh CLI, rather than risking its prompt landing on
      // top of whatever the abandoned turn still produces.
      //
      // quitLiveCLI() FIRST, and that ordering is load-bearing. Reaching this
      // line proves the CLI is alive: the `presence.isShell` guard earlier in
      // this function returns before the completion check whenever the pane has
      // fallen back to a shell, so a confirmed stall is by construction a pane
      // whose foreground program is still the agent CLI. relaunchCLI() on its
      // own only clears a wedged SHELL (recoverWedgedShell's single C-c); against
      // a live CLI that cancels the turn without exiting, and the launch command
      // is then typed into the CLI as a chat prompt — #2203 again, and worse here
      // because the "prompt" is a shell command an agent may simply run.
      //
      // Now done by failCurrentTask via stopAgentForTaskExit (#5353), which
      // adds the credential unlink ahead of the interrupt and captures the
      // stalled pane as evidence BEFORE the relaunch overwrites it — this path
      // previously reported the launch chrome as the failure's tmux_output.
      failCurrentTask(
        `no pane activity for ${Math.round(PANE_STALL_TIMEOUT_MS / 60000)}+ minutes, confirmed over ${PANE_STALL_CONFIRM_TICKS} checks — the agent CLI is not visibly working`,
        { kind: 'environment' }
      );
      return;
    }
    if (stallConfirmCount > 0) {
      console.warn(`Pane unchanged for ${Math.round(PANE_STALL_TIMEOUT_MS / 60000)}+ minutes — confirming before giving up on ${currentTask.task_id} (${stallConfirmCount}/${PANE_STALL_CONFIRM_TICKS})`);
    }
    send({ type: 'task_progress', seq: nextSeq(), task_id: currentTask.task_id, task_gen: currentTask.task_gen, status: 'working', tmux_output: tmuxLines, ...progressModelFields() });
  }
}

function handleMessage(data, hub) {
  // hub defaults to hubs[0] so existing single-hub callers (and the test
  // harness, which calls handleMessage(json) directly with no hub arg) keep
  // working unchanged — there is always at least one entry in hubs[].
  hub = hub || hubs[0];
  let msg;
  try { msg = JSON.parse(data); } catch (_) { return; }

  switch (msg.type) {
    case 'auth_challenge':
      // Always replies on the SAME hub that challenged us, regardless of
      // currentTask/activeHubIndex — send() would route elsewhere.
      sendTo(hub, {
        type: 'auth_response',
        seq: nextSeq(),
        registration_token: hub.regToken,
        cli_backend: BACKEND,
        // Pi derives this evidence from the canonical provider/model input. It
        // remains advisory and is never used by the hub to route work.
        provider: effectiveProvider() || undefined,
        // #4117: AGENT_MODEL if set, else the model detected from the CLI's
        // own session transcript, else '' (today's degrade for backends with
        // no known transcript format).
        model: refreshDetectedModel(),
        reasoning_effort: effectiveReasoningEffort() || undefined,
        role: AGENT_ROLE,
        // Multi-session-per-account: additive, optional. An older hub ignores
        // this unknown field and treats the relay as a single session.
        session: AGENT_SESSION || undefined,
        // #2547 declare half + #2567: additive, optional self-report of runtime
        // posture and protocol version. An older hub ignores these unknown fields.
        protocol_version: RELAY_PROTOCOL_VERSION,
        capabilities: detectCapabilities(),
      });
      break;

    case 'auth_ok':
      console.log(`Authenticated with ${hub.url} as ${msg.contributor_id} (tier: ${msg.trust_tier})`);
      // #2567: the hub advertises its protocol version + capability set here. We
      // log them (forward-compatible: unknown/absent fields are simply skipped)
      // so a newer relay can adapt to what the deployed server supports instead
      // of probing. No behaviour is gated on them today.
      if (msg.protocol_version || (msg.server_capabilities && msg.server_capabilities.length)) {
        console.log(`Hub protocol ${msg.protocol_version || 'unversioned'}; capabilities: ${(msg.server_capabilities || []).join(', ') || 'none'}`);
      }
      // #2547 (peer-compatibility): both sides have STATED a version since #2567,
      // but neither COMPARED them, so "an old relay against a new hub" was still
      // only detectable by watching it misbehave. Say it once, plainly, on the
      // contributor's own terminal — this is the half of the detection the hub
      // log cannot deliver, because the person running the relay is usually not
      // the person reading the hub.
      //
      // Advisory in BOTH directions: we never refuse to connect, never stop
      // asking for work, and never change what we send. A relay that downgraded
      // itself on a version mismatch would strand its own contributor for a
      // difference that is, by the additive-versioning rule, usually harmless.
      warnOnProtocolDrift(hub, msg.protocol_version);
      hub.authenticated = true;
      hub.authFailed = false;
      hub.reconnectDelay = BASE_RECONNECT_DELAY_MS;
      if (currentTask && hub === currentTaskHub()) {
        console.log(`Reconnected while working on ${currentTask.repo}#${currentTask.number} — resuming`);
        sendTo(hub, { type: 'task_accepted', seq: nextSeq(), task_id: currentTask.task_id });
        sendTo(hub, { type: 'task_progress', seq: nextSeq(), task_id: currentTask.task_id, task_gen: currentTask.task_gen, kind: currentTask.kind, repo: currentTask.repo, number: currentTask.number, title: currentTask.title, status: 'working' });
        startProgressReporting();
      } else if (!currentTask && hub === hubs[activeHubIndex]) {
        // Only the hub currently in the poll rotation asks for work. A hub
        // that authenticates while it's not its turn just sits connected
        // (heartbeating) until task_unavailable rotates the active slot to it.
        if (!cliReadyFailed) {
          sendTo(hub, { type: 'ready', seq: nextSeq() });
        } else {
          console.log('Authenticated, but CLI readiness previously failed — withholding ready until the CLI recovers');
        }
      }
      break;

    case 'auth_failed':
      console.error(`Authentication with ${hub.url} failed: ${msg.reason}`);
      if (msg.accepted_models && msg.accepted_models.length > 0) {
        console.error('\nThis hive accepts the following models:');
        msg.accepted_models.forEach(m => console.error('  - ' + m));
        console.error('\nSet your model: export AGENT_MODEL=<model>');
      }
      // A bad token for ONE hub must not take down a working connection to
      // another — only abort the whole process when every configured hub has
      // failed auth (or there is only one, matching the prior behaviour).
      hub.authFailed = true;
      if (hubs.every(h => h.authFailed)) {
        process.exit(1);
      } else {
        console.error(`Continuing with the remaining ${hubs.filter(h => !h.authFailed).length} hub(s).`);
        if (!currentTask && hub === hubs[activeHubIndex]) {
          const next = advanceActiveHub(hub);
          if (next && next.authenticated) {
            sendTo(next, { type: 'ready', seq: nextSeq() });
          }
        }
      }
      break;

    case 'task_assign':
      if (!currentTask && hub !== hubs[activeHubIndex]) {
        console.log(`Rejecting task ${msg.repo}#${msg.number} from ${hub.url} — hub is not the active polling slot`);
        sendTo(hub, { type: 'task_failed', seq: nextSeq(), task_id: msg.task_id, reason: 'Hub is not the active polling slot' });
        break;
      }
      if (currentTask) {
        console.log(`Rejecting task ${msg.repo}#${msg.number} from ${hub.url} — already working on ${currentTask.repo}#${currentTask.number}`);
        sendTo(hub, { type: 'task_failed', seq: nextSeq(), task_id: msg.task_id, reason: 'Already has active task' });
        break;
      }
      // A task we already gave up on permanently must not restart the loop if
      // the hub reassigns it anyway (issue #2203, bug 3). Reject it up front
      // and stay available for other work.
      if (isGivenUp(taskKey(msg))) {
        console.log(`Rejecting ${taskKey(msg)} — previously given up on after ${MAX_TASK_CLI_RESTARTS} CLI crashes`);
        sendTo(hub, { type: 'task_failed', seq: nextSeq(), task_id: msg.task_id, reason: `previously given up on after ${MAX_TASK_CLI_RESTARTS} CLI crashes`, permanent: true });
        sendTo(hub, { type: 'ready', seq: nextSeq() });
        break;
      }
      currentTask = msg;
      // Non-enumerable: currentTask IS msg, and msg gets JSON.stringify'd
      // wholesale to TASK_FILE a few lines down. hub carries live
      // setInterval/setTimeout handles (heartbeatInterval, reconnectTimer),
      // which are circular — a plain assignment here crashed every task
      // (TypeError: Converting circular structure to JSON), which crashed the
      // process, on the very first task after adding multi-hub support.
      Object.defineProperty(currentTask, '_hub', { value: hub, enumerable: false, writable: true, configurable: true });
      activeHubIndex = hubs.indexOf(hub);
      console.log(`Task assigned: ${msg.kind} ${msg.repo}#${msg.number} — ${msg.title} (from ${hub.url})`);
      if (msg.github_token) {
        injectGhToken(msg.github_token);
        tokenExpiresAt = msg.token_expires_at ? new Date(msg.token_expires_at).getTime() : null;
        // Fresh task, fresh credential: no inherited refresh failure (#5447).
        tokenRefreshFailedAt = null;
        lastTokenExpiryWarnAt = 0;
      }
      // TASK_FILE is observability/debug state with no reader that needs the
      // credential; the live token's one legitimate on-disk home is the 0600
      // GH_TOKEN_CACHE written by injectGhToken above. Strip it and keep the
      // file owner-only (chmod covers overwriting a pre-existing 0644 file)
      // so a task-scoped GitHub token never sits world-readable under /tmp
      // (hivecommons/hive#5065).
      const { github_token: _omittedToken, ...taskFileRecord } = msg;
      fs.writeFileSync(TASK_FILE, JSON.stringify(taskFileRecord, null, 2), { mode: 0o600 });
      try { fs.chmodSync(TASK_FILE, 0o600); } catch (_) { /* content is already token-free */ }
      send({ type: 'task_accepted', seq: nextSeq(), task_id: msg.task_id, task_gen: msg.task_gen });
      if (CONTRIBUTOR_MODE === MODE_HEADLESS) {
        // Non-interactive path (hivecommons/hive#2538): drive a one-shot CLI
        // invocation and report completion/failure from its exit status — no
        // tmux, no pane scraping, no watchdog waiting on an invisible prompt.
        runHeadlessTask(msg);
      } else {
        const taskPrompt = msg.prompt || `Work on ${msg.kind} ${msg.repo}#${msg.number}: ${msg.title}`;
        // tmuxSendKeys() itself queues when the CLI is not confirmed ready, so
        // there is a single gate rather than two that can disagree.
        tmuxSendKeys(taskPrompt);
        startProgressReporting();
      }
      break;

    case 'token_refresh':
      if (!currentTask || currentTaskHub() !== hub) {
        console.log(`Ignoring token_refresh from ${hub.url} — it does not own the active task`);
        break;
      }
      if (msg.github_token) {
        injectGhToken(msg.github_token);
        tokenExpiresAt = msg.token_expires_at ? new Date(msg.token_expires_at).getTime() : null;
        // A delivered credential resolves any earlier renewal failure, and
        // re-arms the expiry warning for the new token's own window (#5447).
        tokenRefreshFailedAt = null;
        lastTokenExpiryWarnAt = 0;
        console.log('GitHub token refreshed');
      }
      break;

    // token_refresh_failed (hivecommons/hive#5447): the hub could not re-mint
    // this task's credential. The token we hold is still the OLD one and stays
    // installed — the hub retries on its next heartbeat — so there is nothing to
    // drop and nothing to fail here. Recording it is the entire point: without
    // it, the first evidence of a stale credential is a push failing about an
    // hour into a long task, surfaced to the agent as a generic auth error
    // (#5343's misleading-symptom class).
    case 'token_refresh_failed': {
      if (!currentTask || currentTaskHub() !== hub) {
        console.log(`Ignoring token_refresh_failed from ${hub.url} — it does not own the active task`);
        break;
      }
      tokenRefreshFailedAt = Date.now();
      const status = tokenLifetimeStatus();
      const remaining = status.known
        ? (status.expired
          ? `the current token expired ${formatDuration(status.remainingMs)} ago`
          : `the current token expires in ${formatDuration(status.remainingMs)}`)
        : 'the current token has no known expiry';
      console.error(`GitHub token refresh FAILED for ${taskKey(currentTask)}: ${msg.reason || 'no reason given'} — ${remaining}. Pushes may fail with a generic auth error until the hub renews it.`);
      break;
    }

    case 'task_revoke':
      if (!currentTask) {
        console.log(`Ignoring task_revoked from ${hub.url} for ${msg.task_id} — no active task`);
        break;
      }
      if (currentTaskHub() !== hub || currentTask.task_id !== msg.task_id) {
        console.log(`Ignoring task_revoked from ${hub.url} for ${msg.task_id} — active task belongs to another hub`);
        break;
      }
      console.log(`Task revoked: ${msg.task_id} — ${msg.reason}`);
      currentTask = null;
      taskAssignedAt = 0;
      if (progressInterval) { clearInterval(progressInterval); progressInterval = null; }
      // The max-duration lease dies with the task it bounds. Previously leaked
      // here — harmless only because the callback guards on currentTask, so a
      // revoke followed by a NEW task within the window would have had the old
      // timer fire against the new task's assignment. startProgressReporting()
      // re-arms it, which masked this; clearing it makes the lifecycle explicit
      // and matches every other task-exit path (#5321).
      if (taskTimeoutHandle) { clearTimeout(taskTimeoutHandle); taskTimeoutHandle = null; }
      // Stop the agent and drop its credential. This is the sequence
      // stopAgentForTaskExit() was factored out of (#5353): the token is
      // unlinked BEFORE the interrupt so a surviving turn cannot keep using
      // it; two Ctrl-C events are required because one cancels a Claude/Codex/
      // Pi turn but leaves the CLI alive; relaunchCLI gates ready on a clean
      // prompt; and in headless mode the in-flight one-shot child is killed
      // instead, so the revoked task's process does not keep running.
      if (CONTRIBUTOR_MODE !== MODE_HEADLESS) {
        // Set before the stop: the relaunch's readiness callback consumes this
        // latch to re-advertise availability, and it is only meaningful if a
        // relaunch actually happened — hence the unwind on failure.
        readyAfterInteractiveRevoke = true;
      }
      stopAgentForTaskExit({
        reason: 'task revoke',
        onRelaunchFailed: () => { readyAfterInteractiveRevoke = false; },
      });
      // Stay with the hub that just revoked — it's clearly alive and reachable.
      activeHubIndex = hubs.indexOf(hub);
      if (CONTRIBUTOR_MODE === MODE_HEADLESS) sendTo(hub, { type: 'ready', seq: nextSeq() });
      break;

    case 'task_unavailable':
      if (hub !== hubs[activeHubIndex]) {
        console.log(`Ignoring task_unavailable from inactive hub ${hub.url}`);
        break;
      }
      // #2436 finding 1/2/3 (and #2546): the hub explicitly declined to assign
      // work and told us why (reason: no_work / token_mint_failed /
      // tier_disabled / concurrency_limit) — this ack is NEVER silent. Surface
      // the reason instead of hanging, then re-ask after a delay so a
      // transient condition (a freed slot, a fixed installation permission)
      // recovers on its own.
      //
      // Multi-hub round-robin: this is the ONLY place the active poll slot
      // rotates. task_unavailable is a reliable negative-ack (unlike silence,
      // which could just mean network lag), so rotating on it — rather than
      // on a guessed timeout — means we never sit idle on a hub with no work
      // while a different configured hub has some.
      console.log(`No task assigned on ${hub.url} — reason: ${msg.reason || 'unspecified'}; retrying in ${TASK_UNAVAILABLE_RETRY_MS / 1000}s`);
      setTimeout(() => {
        if (currentTask) return; // picked up work elsewhere in the meantime
        if (hubs.length > 1 && hub === hubs[activeHubIndex]) {
          advanceActiveHub(hub);
        }
        const next = hubs[activeHubIndex];
        // If `next` isn't connected/authenticated yet, its own auth_ok
        // handler sends 'ready' once it comes up and finds itself the active
        // hub (see the auth_ok case above) — self-healing, no extra state.
        sendTo(next, { type: 'ready', seq: nextSeq() });
      }, TASK_UNAVAILABLE_RETRY_MS);
      break;

    case 'notice':
      console.log(msg.message || msg.reason || 'Notice from hub');
      break;

    case 'ping':
      sendTo(hub, { type: 'pong', seq: msg.seq });
      break;

    case 'pong':
      hub.lastPong = Date.now();
      break;

    default:
      console.log('Unknown message type:', msg.type);
  }
}

// WebSocket close codes the relay can meaningfully name. Anything else is
// reported by number rather than guessed at.
const WS_CLOSE_CODE_NAMES = {
  1000: 'normal closure',
  1001: 'going away',
  1002: 'protocol error',
  1003: 'unsupported data',
  1005: 'no status received',
  1006: 'abnormal closure',
  1008: 'policy violation',
  1009: 'message too big',
  1011: 'internal server error',
  1012: 'service restart',
  1013: 'try again later',
  1015: 'TLS handshake failure',
};

// describeWsClose renders the close code and reason for the log line.
//
// THE GAP THIS FILLS (hivecommons/hive#5090): this handler used to ignore both
// arguments and log only "closed. Reconnecting in 1000ms...", so a contributor
// whose socket flapped every 30-90 seconds had no way to tell a deliberate
// server hangup from a network drop — the two produce identical output, and the
// backoff never grows past 1s because each reconnect succeeds, so even the delay
// carries no signal.
//
// 1006 is called out explicitly because it is the one code that is never sent
// on the wire: the `ws` library synthesises it when the connection died WITHOUT
// a close frame. Seeing it means the socket was cut — by the network, a proxy,
// or a peer calling close() without the courtesy frame — rather than closed
// with a stated reason. That distinction is the whole diagnostic.
function describeWsClose(code, reason) {
  const text = reason === undefined || reason === null ? '' : String(reason).trim();
  const name = WS_CLOSE_CODE_NAMES[code];
  const label = name ? `code=${code} ${name}` : `code=${code}`;
  if (code === 1006) {
    return `${label} — no close frame; the socket was cut (network, proxy, or an abrupt peer close)`;
  }
  return text ? `${label}: ${text}` : label;
}

function connectHub(hub) {
  if (hub.reconnectTimer) { clearTimeout(hub.reconnectTimer); hub.reconnectTimer = null; }
  if (hub.heartbeatInterval) { clearInterval(hub.heartbeatInterval); hub.heartbeatInterval = null; }
  if (hub.ws) { try { hub.ws.removeAllListeners(); hub.ws.terminate(); } catch (_) {} }
  const gen = ++hub.connectGeneration;
  console.log(`Connecting to ${hub.url}...`);
  hub.ws = new WebSocket(hub.url);

  hub.ws.on('open', () => {
    if (gen !== hub.connectGeneration) return;
    console.log(`Connected to ${hub.url}`);
    hub.reconnectDelay = BASE_RECONNECT_DELAY_MS;
    hub.lastPong = Date.now();

    hub.heartbeatInterval = setInterval(() => {
      if (gen !== hub.connectGeneration) { clearInterval(hub.heartbeatInterval); return; }
      if (Date.now() - hub.lastPong > HEARTBEAT_TIMEOUT_MS) {
        console.error(`Heartbeat timeout on ${hub.url} — reconnecting`);
        hub.ws.terminate();
        return;
      }
      sendTo(hub, { type: 'ping', seq: nextSeq() });
      // Also emit a PROTOCOL-level Ping control frame (hivecommons/hive#5090).
      // The JSON ping above is an ordinary text frame; an L7 proxy that scores
      // tunnel idleness on control-frame traffic does not count it, so a
      // connection heartbeating every 30s was still reaped as idle — the
      // frameless-1006 flap this issue measured. `ws` answers an inbound Ping
      // with a Pong automatically, so the hub needs nothing extra to see this.
      // Wrapped because ping() throws if the socket left OPEN between the
      // readyState check and the call; the heartbeat-timeout check above stays
      // the authority on when to give up.
      try { hub.ws.ping(); } catch { /* socket already closing; close handler reconnects */ }
    }, HEARTBEAT_INTERVAL_MS);
  });

  hub.ws.on('message', (data) => {
    if (gen !== hub.connectGeneration) return;
    handleMessage(data.toString(), hub);
  });

  // A PROTOCOL-level Pong counts as liveness exactly as the JSON 'pong' does
  // (hivecommons/hive#5090), so a hub answering only control frames cannot trip
  // this relay's HEARTBEAT_TIMEOUT_MS sweep. An inbound Ping is likewise
  // evidence the hub is alive; `ws` auto-replies with a Pong for us.
  hub.ws.on('pong', () => {
    if (gen !== hub.connectGeneration) return;
    hub.lastPong = Date.now();
  });
  hub.ws.on('ping', () => {
    if (gen !== hub.connectGeneration) return;
    hub.lastPong = Date.now();
  });

  hub.ws.on('close', (code, reason) => {
    if (gen !== hub.connectGeneration) return;
    console.log(`Connection to ${hub.url} closed (${describeWsClose(code, reason)}). ` +
      `Reconnecting in ${hub.reconnectDelay}ms...`);
    if (hub.heartbeatInterval) { clearInterval(hub.heartbeatInterval); hub.heartbeatInterval = null; }
    hub.reconnectTimer = setTimeout(() => connectHub(hub), hub.reconnectDelay);
    hub.reconnectDelay = Math.min(hub.reconnectDelay * 2, MAX_RECONNECT_DELAY_MS);
  });

  hub.ws.on('error', (err) => {
    if (gen !== hub.connectGeneration) return;
    console.error(`WebSocket error on ${hub.url}:`, err.message);
  });
}

function connect() {
  // Kept as the entry point (bottom of file, SIGTERM/SIGINT) so those call
  // sites don't need to know about hubs[] — connects every configured hub.
  hubs.forEach(connectHub);
}

function cleanup() {
  hubs.forEach(hub => {
    if (hub.heartbeatInterval) { clearInterval(hub.heartbeatInterval); hub.heartbeatInterval = null; }
  });
  if (progressInterval) { clearInterval(progressInterval); progressInterval = null; }
  // A shutdown with a task in flight must run the same task-exit contract as
  // every other way a task stops being ours (hivecommons/hive#5655, #5353).
  // Ctrl-C is the NORMAL way a contributor stops a relay, and this path used
  // to clear timers only: the per-task scoped token stayed on disk at
  // GH_TOKEN_CACHE, valid for the rest of its ~55-minute lifetime, after the
  // hub had already released the issue and could offer it to someone else —
  // the #2356 shape, reached from the shutdown direction.
  //
  // stopAgentForTaskExit() unlinks the credential FIRST (its step 1, always),
  // then interrupts the live agent — which matters when the tmux session is
  // detached or container-owned and does not die with the relay. noRelaunch:
  // this process is exiting, so starting a fresh CLI would only orphan one.
  // The hub is deliberately NOT messaged here: the socket drop already books
  // the release through the disconnect handler's cooldown path (#5097).
  if (currentTask) {
    stopAgentForTaskExit({ reason: 'relay shutdown', noRelaunch: true });
    currentTask = null;
  }
}

process.on('SIGTERM', () => { cleanup(); process.exit(0); });
process.on('SIGINT', () => { cleanup(); process.exit(0); });

// Last-resort backstop (hivecommons/hive#5655): the scoped token must never
// outlive the process, however it exits. 'exit' fires on a normal return, on
// the process.exit(0) in the signal handlers above, and on the default
// crash path of an uncaught exception — everything short of SIGKILL. Exit
// handlers must be synchronous; a bare unlink is, and it is a no-op when
// cleanup() already dropped the credential (or none was ever written).
process.on('exit', () => {
  try { fs.unlinkSync(GH_TOKEN_CACHE); } catch (_) {}
});

// Test hook: when HIVE_RELAY_TEST_MODE=1 the relay exposes its internals and
// does NOT open a hub connection, so contributor-relay.test.js can drive the
// restart/queueing/give-up logic directly. Production runs never set this.
if (process.env.HIVE_RELAY_TEST_MODE === '1') {
  module.exports = {
    buildLaunchCommand,
    detectCapabilities,
    detectAgentCLIVersion,
    sanitizeDeclaredValue,
    handleMessage,
    injectGhToken,
    GH_TOKEN_CACHE,
    tokenLifetimeStatus,
    warnOnTokenExpiry,
    TOKEN_EXPIRY_WARN_MS,
    tmuxSendKeys,
    flushPendingTask,
    relaunchCLI,
    failCurrentTask,
    startProgressReporting,
    progressTick,
    classifyTmuxPane,
    paneLooksBlockedOnHuman,
    blockingPromptKey,
    PANE_STATE_WORKING,
    PANE_STATE_BLOCKED_ON_HUMAN,
    PANE_STATE_IDLE_COMPLETE,
    PANE_STATE_TRANSIENT_API_ERROR,
    PANE_STATE_FATAL_API_ERROR,
    PANE_STATE_UNKNOWN_API_ERROR,
    paneUnknownAPIErrorLine,
    paneShowsTransientAPIError,
    paneShowsUnretryableAPIError,
    paneShowsLoginRequiredError,
    handleTransientAPIError,
    resetTransientNudgeState,
    classifyBlockedOnHumanReason,
    BLOCKED_REASON_QUESTION,
    BLOCKED_REASON_MENU,
    BLOCKED_REASON_HUMAN_REQUIRED,
    maybeSendAutonomyNudge,
    resetAutonomyNudgeState,
    AUTONOMY_NUDGE_MESSAGE,
    tmuxSessionHasAttachedClient,
    tmuxSessionHumanPresence,
    HUMAN_PRESENCE_IDLE_MS,
    TRANSIENT_API_ERROR_MAX_NUDGES,
    TRANSIENT_API_ERROR_NUDGE_MESSAGE,
    getTransientNudgeCount: () => transientNudgeCount,
    __clearTransientNudgeCooldown: () => { lastTransientNudgeAt = 0; },
    // Run one progress tick with the grace period already elapsed.
    __crashTick: () => { taskAssignedAt = Date.now() - TASK_GRACE_PERIOD_MS - 1; progressTick(); },
    paneStalled,
    paneStallConfirmed,
    paneChangedSince,
    resetPaneStallClock,
    PANE_STALL_CONFIRM_TICKS,
    // Completion-signal surface (hivecommons/hive#5376).
    CHROME_IDLE_GRACE_TICKS,
    HIVE_VERDICT_COMPLETE,
    HIVE_VERDICT_NO_WORK,
    detectHiveVerdict,
    detectCompletionVerdict,
    recordChromeIdleTick,
    resetChromeIdleGrace,
    getChromeIdleTicks: () => chromeIdleTicks,
    // Max-duration lease surface (hivecommons/hive#5321).
    MAX_TASK_DURATION_MS,
    ABSOLUTE_TASK_DEADLINE_MS,
    HEADLESS_TASK_TIMEOUT_MS,
    armTaskProgressLease,
    onTaskProgressLeaseExpired,
    getTaskTimeoutHandle: () => taskTimeoutHandle,
    // Backdate the task-assignment clock so the absolute backstop can be
    // crossed without waiting hours.
    __ageTaskAssignedAt: (ms) => { if (taskAssignedAt) taskAssignedAt -= ms; },
    setTaskAssignedAt: (v) => { taskAssignedAt = v; },
    getTaskAssignedAt: () => taskAssignedAt,
    getStallConfirmCount: () => stallConfirmCount,
    launchCommandWithCwd,
    cliProcessLooksGone,
    paneForegroundCommand,
    quitLiveCLI,
    stopAgentForTaskExit,
    dropTaskCredential,
    CLI_GONE_CONFIRMATIONS,
    PANE_STALL_TIMEOUT_MS,
    // Backdate the stall clock so a test can cross the timeout without
    // sleeping — the two ticks it needs otherwise land in the same millisecond.
    __agePaneStallClock: (ms) => { lastPaneChangeAt -= ms; },
    // Run a progress tick past the startup grace period, as __crashTick does,
    // so the stall backstop can be exercised without waiting it out.
    __stallTick: () => { taskAssignedAt = Date.now() - TASK_GRACE_PERIOD_MS - 1; progressTick(); },
    cleanup,
    restartBackoffMs,
    NO_MODEL_FLAG_BACKENDS,
    effectiveReasoningEffort,
    // Model auto-detection from the CLI session transcript (#4117).
    detectRunningModel,
    refreshDetectedModel,
    effectiveModel,
    progressModelFields,
    effectiveProvider,
    effectiveSelectionFields,
    PI_SELECTION,
    __setDetectedModel: (v) => { detectedModel = v; },
    MAX_TASK_CLI_RESTARTS,
    setCliReady: (v) => { cliReady = v; },
    getCliReady: () => cliReady,
    setCliReadyFailed: (v) => { cliReadyFailed = v; },
    getCliReadyFailed: () => cliReadyFailed,
    getPendingTask: () => pendingTask,
    setPendingTask: (v) => { pendingTask = v; },
    // Per-task prompt-delivery surface (hivecommons/hive#5650).
    getTaskPromptDelivered: () => taskPromptDelivered,
    setTaskPromptDelivered: (v) => { taskPromptDelivered = v; },
    getDeliveredVerdictBaseline: () => deliveredVerdictBaseline,
    setDeliveredVerdictBaseline: (v) => { deliveredVerdictBaseline = v; },
    setTasksCompletedCount: (v) => { tasksCompletedCount = v; },
    getTasksCompletedCount: () => tasksCompletedCount,
    setLastResetAtCount: (v) => { lastResetAtCount = v; },
    getLastResetAtCount: () => lastResetAtCount,
    getCurrentTask: () => currentTask,
    setCurrentTask: (v) => { currentTask = v; },
    blockingPromptKey,
    getCLIState,
    setWs: (w) => { hubs[0].ws = w; },
    getHubs: () => hubs,
    // Peer-protocol compatibility (hivecommons/hive#2547). Exported so the
    // relay-side half of "both sides can detect an incompatible peer" is tested
    // behaviourally here, not just asserted to exist from the Go side.
    RELAY_PROTOCOL_VERSION,
    parseProtocolVersion,
    classifyPeerProtocol,
    warnOnProtocolDrift,
    describeWsClose,
    // Headless (non-interactive) mode surface (hivecommons/hive#2538).
    CONTRIBUTOR_MODE,
    MODE_INTERACTIVE,
    MODE_HEADLESS,
    HEADLESS_BACKENDS,
    HEADLESS_STATE_WAITING,
    HEADLESS_STATE_WORKING,
    HEADLESS_STATE_DONE,
    HEADLESS_STATE_FAILED,
    headlessSupportsBackend,
    buildHeadlessArgv,
    runHeadlessTask,
    getHeadlessChild: () => headlessChild,
    // Attach-hint surface (hivecommons/hive#5145): the exact command the
    // needs-authentication banner tells a human to paste.
    ATTACH_COMMAND,
    CONTAINER_NAME,
    CONTAINER_RUNTIME,
    // Coverage for previously untested pure/isolated functions (#4267).
    redactTokens,
    captureTmuxLines,
    detectNoWorkVerdict,
    detectPRURL,
    resolveBackend,
    shellQuote,
    looksLikeModelName,
    taskKey,
    tailLinesReversed,
    readFileTail,
    newestByMtime,
    nextSeq,
    modelFlagFor,
    sleepMs,
    isGivenUp,
    recentPaneLines,
    sendTo,
    tmuxSendEnters,
    GIVE_UP_MEMORY_MS,
    // Test hook: mark a task key given-up at a chosen timestamp so isGivenUp's
    // expiry pruning can be exercised without waiting an hour.
    __setGivenUp: (key, at) => { givenUpTasks.set(key, at); },
  };
} else {
  if (BACKEND === 'pi' && !PI_SELECTION.valid) {
    console.error(`FATAL: ${PI_SELECTION.error}`);
    if (CONTRIBUTOR_MODE === MODE_HEADLESS) {
      writeHeadlessStatus(HEADLESS_STATE_FAILED, { result: 'failed', reason: PI_SELECTION.error });
    }
    process.exit(1);
  }
  // Warm the capability cache BEFORE the first hub connection. detectCapabilities()
  // is called from the auth_challenge handler, and the hub bounds a handshake at
  // 30s (wsAuthTimeout); doing the probes here keeps every one of them — backend
  // resolution and the `--version` call added for the CLI version — off the auth
  // path entirely, where a slow host could otherwise eat that budget. Failures are
  // already absorbed field-by-field, so this cannot stop the relay starting.
  detectCapabilities();
  connect();
}
