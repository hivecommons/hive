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

const rawHub = process.env.HIVE_HUB || 'wss://hive.kubestellar.io:3001/contribute';
// Multi-hub (kubestellar/hive#multi-hive): HIVE_HUB and HIVE_REGISTRATION_TOKEN
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
const MODEL = process.env.AGENT_MODEL || process.env.GOOSE_MODEL || '';
const REASONING_EFFORT = process.env.AGENT_REASONING_EFFORT || '';
const AGENT_ROLE = (process.env.HIVE_AGENT_ROLE || '').trim();
// Neutral directory both entrypoints launch the CLI from ($HOME). Used to pin
// the cwd on relaunch; see launchCommandWithCwd for why the relay's own cwd is
// the wrong answer in local mode.
const AGENT_CWD = (process.env.HIVE_AGENT_CWD || '').trim();
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
// before. See kubestellar/hive#1861 / #3842 (audit N14).
const GH_TOKEN_CACHE = process.env.HIVE_GH_TOKEN_CACHE || (fs.existsSync('/var/run/hive-metrics')
  ? '/var/run/hive-metrics/contributor-gh-token.cache'
  : '/tmp/hive-gh-token.cache');
const TASK_FILE = process.env.HIVE_TASK_FILE || '/tmp/contributor-task.json';

// --- Delivery mode (kubestellar/hive#2538) -------------------------------
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
const MAX_TASK_DURATION_MS = 1800000;
// Hard ceiling on a single headless one-shot invocation (kubestellar/hive#2538).
// The interactive path bounds a task with MAX_TASK_DURATION_MS via a
// tmux-scraping watchdog; the headless child gets the SAME bound enforced
// directly on the process, so a wedged CLI is killed and reported failed rather
// than hanging the pod forever.
const HEADLESS_TASK_TIMEOUT_MS = MAX_TASK_DURATION_MS;
const NETWORK_ERROR_RETRY_DELAY_MS = 5000;
// After the hub sends an explicit task_unavailable negative-ack (no admissible
// work, a disabled tier, a concurrency limit, or a token-mint failure — see
// kubestellar/hive#2436), wait before re-asking so we neither hang forever
// (the old silent-nil behaviour) nor busy-loop the hub.
const TASK_UNAVAILABLE_RETRY_MS = 30000;

// RELAY_PROTOCOL_VERSION is the contributor-protocol version this relay speaks
// (kubestellar/hive#2567). It is DECLARED to the hub in auth_response (additive,
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
// task_unavailable is the only signal (kubestellar/hive#2436/#2546 — the hub
// always sends it, never stays silent) that a hub genuinely has no work.
let activeHubIndex = 0;

let seq = 0;
let currentTask = null;
let progressInterval = null;
let tokenExpiresAt = null;

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
  // Agent CLI version: the hub schema, the operator docs and the Operations row
  // ("cli 1.2.3") have carried this field since the declare half shipped, but the
  // relay never populated it — so the one axis #2547's own evidence names first
  // ("an agent CLI old enough to lack a flag the prompt assumes") was the one an
  // operator could not see. Best-effort: omitted entirely when the probe fails.
  const cliVersion = detectAgentCLIVersion();
  if (cliVersion) caps.agent_cli_version = cliVersion;
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
// different contributor-protocol version than this relay (kubestellar/hive#2547).
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
// contributor-agent.sh builds "$CMD $PERM_FLAG $MODEL_FLAG" for the FIRST
// launch; every restart path in this file previously rebuilt only "$CMD $PERM"
// inline, silently dropping the resolved model for the rest of the container's
// life. Build it once here and reuse it everywhere so the paths cannot drift.
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
  return MODEL && !NO_MODEL_FLAG_BACKENDS.includes(BACKEND) ? `--model ${MODEL}` : '';
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

// --- Headless (non-interactive) one-shot dispatch (kubestellar/hive#2538) ---
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
// pi drive an interactive TUI), so headless mode refuses them LOUDLY at
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
  codex: { flag: 'exec' },
  // goose run --no-session -t "<prompt>" — goose's one-shot sub-command. The
  // bare `goose` binary drives the interactive TUI, but `goose run` is a
  // documented non-interactive entry point (#2828): `-t` takes the prompt as
  // its VALUE (not a trailing positional), and --no-session skips creating or
  // resuming a session file, which one-shot dispatch never needs. Verified
  // against goose 1.37.0 — the version src/Dockerfile pins via GOOSE_VERSION —
  // that `run`, `-t` and `--no-session` all exist and that a failed run exits
  // non-zero, which is the exit-code contract runHeadlessTask() relies on.
  goose: { flag: ['run', '--no-session', '-t'] },
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
  // (kubestellar/hive#4970). Unlike agy, opencode is the ONLY launch mode
  // this backend gets: there is no interactive-tmux wiring for it (see the
  // getCLIState()/classifyTmuxPane() backend lists below, which opencode
  // deliberately does not join), so it is only ever reached through
  // CONTRIBUTOR_MODE=headless. `opencode run` exits with a real status code
  // on completion, the exit-code contract runHeadlessTask() relies on.
  opencode: { flag: 'run' },
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

  const { bin, args } = buildHeadlessArgv(prompt);
  console.log(`Headless: running ${bin} (one-shot) for ${task.repo}#${task.number}`);
  writeHeadlessStatus(HEADLESS_STATE_WORKING, { task_id: task.task_id, repo: task.repo, number: task.number });
  send({ type: 'task_progress', seq: nextSeq(), task_id: task.task_id, task_gen: task.task_gen, kind: task.kind, repo: task.repo, number: task.number, title: task.title, status: 'working' });

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
        console.error(`Headless task ${task.task_id} failed: ${reason}`);
        writeHeadlessStatus(HEADLESS_STATE_FAILED, { task_id: task.task_id, reason });
        failCurrentTask(reason, { permanent: false });
      });
      return;
    }
    finish(() => {
      console.log(`Headless task ${task.task_id} completed (exit 0)`);
      const prURL = detectPRURL(outTail, task.repo);
      if (prURL) console.log(`Detected PR for ${task.task_id}: ${prURL}`);
      // #3987: only report a no_work_needed verdict when no PR was shipped —
      // a visible PR contradicts "nothing shippable" (the hub would override
      // the claim with "shipped" anyway).
      const noWork = prURL ? null : detectNoWorkVerdict(outTail);
      if (noWork) console.log(`Detected no_work_needed verdict for ${task.task_id}: ${noWork.reason || '(no reason)'}`);
      writeHeadlessStatus(HEADLESS_STATE_DONE, { task_id: task.task_id, pr_url: prURL });
      send({ type: 'task_complete', seq: nextSeq(), task_id: task.task_id, task_gen: task.task_gen, result: 'completed', summary: 'Headless one-shot invocation exited 0', tmux_output: outTail, pr_url: prURL, verdict: noWork ? noWork.verdict : undefined, verdict_reason: noWork ? noWork.reason : undefined });
      currentTask = null;
      taskAssignedAt = 0;
      tasksCompletedCount++;
      writeHeadlessStatus(HEADLESS_STATE_WAITING);
      send({ type: 'ready', seq: nextSeq() });
    });
  });
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
  return null;
}

function getCLIState() {
  try {
    const text = capturePaneText();
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
      // agy shows "? for shortcuts" at the bottom when its interactive prompt
      // is ready. The generic />\s*$/ fires too early (during the splash).
      if (/\? for shortcuts/.test(text)) return 'ready';
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
// True once a CLI-readiness wait has timed out and we handed its task back.
// Used so the eventual recovery re-advertises availability to the hub, which
// we deliberately withheld at failure time (see armCLIReadyWait).
let cliReadyFailed = false;

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
  waitForCLI().then(() => {
    cliReady = true;
    cliReadyFailed = false;
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
      failCurrentTask(`CLI never became ready: ${e.message}`, { skipReady: true, kind: 'environment' });
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
  // {36,} not {36}: GitHub documents that token length may grow, and an exact
  // bound would redact only the first 36 characters of a longer token, leaking
  // its tail into the hub log line (kubestellar/hive#4267).
  return text.replace(/gho_[A-Za-z0-9]{36,}/g, 'gho_***REDACTED***')
    .replace(/ghp_[A-Za-z0-9]{36,}/g, 'ghp_***REDACTED***')
    .replace(/ghs_[A-Za-z0-9]{36,}/g, 'ghs_***REDACTED***')
    .replace(/ghu_[A-Za-z0-9]{36,}/g, 'ghu_***REDACTED***')
    .replace(/ghr_[A-Za-z0-9]{36,}/g, 'ghr_***REDACTED***');
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

// Best-effort scan of the agent's recent output for the no_work_needed
// sentinel (kubestellar/hive#3987). The hub's task prompt instructs the agent:
// when it affirmatively determines there is NOTHING shippable (the remainder
// is gated on an unanswered maintainer decision, or merged PRs already cover
// it), it prints a line of the exact form
//   HIVE_VERDICT: no_work_needed — <short reason>
// instead of opening a PR. Reported on task_complete as verdict/verdict_reason
// so the hub can park the issue for the long offer-suppression window instead
// of re-offering it every short-cooldown period forever (the #2547 shape that
// escalation only bounded). Returns null when no marker is found — the hub
// then treats the completion exactly as an idle one (today's semantics). The
// marker spelling must stay in sync with buildTaskPrompt in
// src/pkg/dashboard/contribute_ws.go.
function detectNoWorkVerdict(lines) {
  if (!Array.isArray(lines) || lines.length === 0) return null;
  // Anchored at line start: the task PROMPT quotes the marker mid-sentence
  // ("...the exact form 'HIVE_VERDICT: ...'"), and an anchored match keeps
  // that instruction echo from reading as the agent's own verdict. Codex
  // renders its completed assistant messages with a leading bullet, which is
  // presentation chrome rather than part of the verdict.
  const VERDICT_RE = /^\s*(?:•\s*)?HIVE_VERDICT:\s*no_work_needed\b[\s:—–-]*(.*)$/i;
  // Scan newest-first so the agent's final conclusion wins over anything it
  // merely quoted or considered earlier in the transcript.
  for (let i = lines.length - 1; i >= 0; i--) {
    const m = VERDICT_RE.exec(lines[i]);
    if (!m) continue;
    const reason = (m[1] || '').trim();
    // tmux may wrap the prompt's instruction so its quoted marker lands at a
    // visual line start; its giveaway is the literal "<short reason>"
    // placeholder. Never treat that echo as a real verdict.
    if (reason.startsWith('<')) continue;
    return { verdict: 'no_work_needed', reason };
  }
  return null;
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

function paneLooksBlockedOnHuman(text) {
  const lines = recentPaneLines(text);
  if (lines.length === 0) return false;
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

  // Elicitation / fill-in-a-form prompt (kubestellar/hive#2844). Goose (and any
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
  const blockingPatterns = [
    // Confirmation prompts and TUI continuation screens.
    /\[[Yy]\/[Nn]\]|\([Yy]\/[Nn]\)|\b[Yy]es\/[Nn]o\b/,
    /\b(?:continue|proceed|confirm|approve|allow|deny|accept|reject|choose|select)\b.*\?/i,
    /\bPress Enter to continue\b/i,
    /\bEnter to confirm\b/i,
    // Permission/auth/onboarding prompts seen from Claude/Copilot/Goose/Bob.
    /\b(?:approval|consent|trust this folder|Do you trust|Confirm folder trust)\b/i,
    /\bpermission\b.*\b(?:allow|approve|confirm|continue|proceed)\b/i,
    /\b(?:allow|approve|confirm|continue|proceed)\b.*\bpermission\b/i,
    /\b(?:Allow|Approve|Run|Execute)\b.*\b(?:command|tool|edit|file|operation)\b/i,
    /\b(?:Paste|Enter).*(?:API key|token|code|password)\b/i,
  ];

  return hasQuestion || hasNumberedMenu || hasElicitationForm || blockingPatterns.some(re => re.test(beforePrompt));
}

function classifyTmuxPane(text) {
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
    // kubestellar/hive#4259 ready for review summarised itself as "Opened
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
    // Observed live: a turn that finished and opened kubestellar/hive#4127 sat
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
    // kubestellar/hive#4181 ended with "Replaced inline token export
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

// Observed live (kubestellar/hive): a task crossed PANE_STALL_TIMEOUT_MS while
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

let lastPaneFingerprint = null;
let lastPaneChangeAt = 0;
// How many CONSECUTIVE ticks paneStalled() has now returned true. Distinct
// from the fingerprint clock above: that clock says "how long has it been
// unchanged", this says "how many chances has the CLI had to prove otherwise
// since we first noticed". Reset by resetPaneStallClock() and by any tick
// where paneStalled() is false (new output resets the whole stall story).
let stallConfirmCount = 0;

function resetPaneStallClock() {
  lastPaneFingerprint = null;
  lastPaneChangeAt = Date.now();
  stallConfirmCount = 0;
  // A new task also starts with a clean CLI-liveness count: shell readings from
  // the previous task say nothing about this one.
  consecutiveShellReadings = 0;
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
// opts.kind (kubestellar/hive#2547) optionally states WHY: 'environment' when
// this client's own runtime could not run the work (the CLI never started, it
// crashed, the backend has no headless mode) versus 'task' when the work was
// attempted and failed on its merits. Omit it when the cause is genuinely
// ambiguous — the hub normalizes absent to 'unspecified', and guessing would be
// worse than saying nothing, since an operator reads this to attribute failures.
//
// It is advisory: the hub records and displays it and does not route, gate, or
// change the work item's failure cooldown on it. Older hubs ignore the field.
function failCurrentTask(reason, opts) {
  if (!currentTask) return;
  const permanent = !!(opts && opts.permanent);
  const kind = (opts && opts.kind) || undefined;
  const taskId = currentTask.task_id;
  const taskGen = currentTask.task_gen;
  const tmuxLines = captureTmuxLines(TMUX_TAIL_LINES);
  console.error(`Task ${taskId} failed${permanent ? ' permanently' : ''}${kind ? ` [${kind}]` : ''}: ${reason}`);
  send({ type: 'task_failed', seq: nextSeq(), task_id: taskId, task_gen: taskGen, reason, permanent, failure_kind: kind, tmux_output: tmuxLines });
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
      // environment: the agent CLI process died; nothing was judged about the work.
      failCurrentTask('CLI process exited — restarted', { kind: 'environment' });
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

  const paneState = checkTmuxPaneState();
  const tmuxLines = captureTmuxLines(TMUX_TAIL_LINES);
  if (paneState === PANE_STATE_IDLE_COMPLETE) {
    console.log(`Task ${currentTask.task_id} completed — agent idle`);
    // Successful completion clears this work item's crash-retry budget.
    cliRestartCounts.delete(taskKey(currentTask));
    // Best-effort: report the PR the agent opened, if one is visible in its
    // recent output, so the hub can distinguish "shipped a PR" from "just went
    // idle" and pick the right issue cooldown (kubestellar/hive#2393 item 7).
    // Empty when no PR link is found — the hub then applies the short cooldown.
    const prURL = detectPRURL(tmuxLines, currentTask.repo);
    if (prURL) console.log(`Detected PR for ${currentTask.task_id}: ${prURL}`);
    // #3987: only report a no_work_needed verdict when no PR was shipped — a
    // visible PR contradicts "nothing shippable" (the hub would override the
    // claim with "shipped" anyway).
    const noWork = prURL ? null : detectNoWorkVerdict(tmuxLines);
    if (noWork) console.log(`Detected no_work_needed verdict for ${currentTask.task_id}: ${noWork.reason || '(no reason)'}`);
    send({ type: 'task_complete', seq: nextSeq(), task_id: currentTask.task_id, task_gen: currentTask.task_gen, result: 'completed', summary: noWork ? 'Agent returned to idle (reported no_work_needed)' : 'Agent returned to idle', tmux_output: tmuxLines, pr_url: prURL, verdict: noWork ? noWork.verdict : undefined, verdict_reason: noWork ? noWork.reason : undefined });
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
  } else if (paneState === PANE_STATE_BLOCKED_ON_HUMAN) {
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
      quitLiveCLI();
      try {
        console.log(`Relaunching ${BACKEND} after a confirmed pane stall: ${relaunchCLI()}`);
      } catch (e) {
        console.error('Failed to relaunch after a confirmed pane stall:', e.message);
      }
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
        // #4117: AGENT_MODEL if set, else the model detected from the CLI's
        // own session transcript, else '' (today's degrade for backends with
        // no known transcript format).
        model: refreshDetectedModel(),
        reasoning_effort: effectiveReasoningEffort() || undefined,
        role: AGENT_ROLE,
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
      }
      fs.writeFileSync(TASK_FILE, JSON.stringify(msg, null, 2));
      send({ type: 'task_accepted', seq: nextSeq(), task_id: msg.task_id, task_gen: msg.task_gen });
      if (CONTRIBUTOR_MODE === MODE_HEADLESS) {
        // Non-interactive path (kubestellar/hive#2538): drive a one-shot CLI
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
        console.log('GitHub token refreshed');
      }
      break;

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
      // Headless mode: kill the in-flight one-shot child so the revoked task's
      // process does not keep running (and holding the credential) after the
      // hub took the work back.
      if (CONTRIBUTOR_MODE === MODE_HEADLESS && headlessChild) {
        try { headlessChild.kill('SIGKILL'); } catch (_) {}
        headlessChild = null;
        writeHeadlessStatus(HEADLESS_STATE_WAITING);
      }
      // Stay with the hub that just revoked — it's clearly alive and reachable.
      activeHubIndex = hubs.indexOf(hub);
      sendTo(hub, { type: 'ready', seq: nextSeq() });
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
    }, HEARTBEAT_INTERVAL_MS);
  });

  hub.ws.on('message', (data) => {
    if (gen !== hub.connectGeneration) return;
    handleMessage(data.toString(), hub);
  });

  hub.ws.on('close', () => {
    if (gen !== hub.connectGeneration) return;
    console.log(`Connection to ${hub.url} closed. Reconnecting in ${hub.reconnectDelay}ms...`);
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
}

process.on('SIGTERM', () => { cleanup(); process.exit(0); });
process.on('SIGINT', () => { cleanup(); process.exit(0); });

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
    // Run one progress tick with the grace period already elapsed.
    __crashTick: () => { taskAssignedAt = Date.now() - TASK_GRACE_PERIOD_MS - 1; progressTick(); },
    paneStalled,
    paneStallConfirmed,
    resetPaneStallClock,
    PANE_STALL_CONFIRM_TICKS,
    getStallConfirmCount: () => stallConfirmCount,
    launchCommandWithCwd,
    cliProcessLooksGone,
    paneForegroundCommand,
    quitLiveCLI,
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
    __setDetectedModel: (v) => { detectedModel = v; },
    MAX_TASK_CLI_RESTARTS,
    setCliReady: (v) => { cliReady = v; },
    getCliReady: () => cliReady,
    setCliReadyFailed: (v) => { cliReadyFailed = v; },
    getCliReadyFailed: () => cliReadyFailed,
    getPendingTask: () => pendingTask,
    setPendingTask: (v) => { pendingTask = v; },
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
    // Peer-protocol compatibility (kubestellar/hive#2547). Exported so the
    // relay-side half of "both sides can detect an incompatible peer" is tested
    // behaviourally here, not just asserted to exist from the Go side.
    RELAY_PROTOCOL_VERSION,
    parseProtocolVersion,
    classifyPeerProtocol,
    warnOnProtocolDrift,
    // Headless (non-interactive) mode surface (kubestellar/hive#2538).
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
    // Coverage for previously untested pure/isolated functions (#4267).
    redactTokens,
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
  // Warm the capability cache BEFORE the first hub connection. detectCapabilities()
  // is called from the auth_challenge handler, and the hub bounds a handshake at
  // 30s (wsAuthTimeout); doing the probes here keeps every one of them — backend
  // resolution and the `--version` call added for the CLI version — off the auth
  // path entirely, where a slow host could otherwise eat that budget. Failures are
  // already absorbed field-by-field, so this cannot stop the relay starting.
  detectCapabilities();
  connect();
}
