#!/usr/bin/env node
// contributor-relay.js — ClankeR, the contributor relay: the WebSocket client
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
//                           codex (-c model_reasoning_effort), by agy
//                           (--effort low|medium|high), by muse
//                           (--reasoning-effort) and by claude
//                           (--effort low|medium|high|xhigh|max, #8377);
//                           ignored elsewhere.
//   HIVE_AGENT_ROLE        — optional spoke agent role to claim (scanner,
//                           quality, outreach, etc.; hub-enforced)
//   HIVE_AGENT_SESSION     — tmux session name for the agent (default: contributor)

'use strict';

const WebSocket = require('ws');
const { execSync, execFileSync, spawn } = require('child_process');
const fs = require('fs');
const path = require('path');
const DEFAULT_NEEDS_DECISION_LABEL = 'needs-decision';
const {
  parsePiModelSelection,
  redactPiCredentials,
  piReadiness,
} = require('./pi-backend.js');
// Pure pane-classification (kubestellar/hive#6429): readiness/login/onboarding
// tables, the busy/idle/blocked/error state machine, the blocked-on-human
// reason breakdown, the modal-dismiss keystroke table, and the pane-tail/
// API-error detectors they share. See bin/lib/pane-classifier.js for the
// module boundary — this relay only captures pane text (tmux) and passes it
// in; it does no classification of its own.
const {
  PANE_STATE_WORKING,
  PANE_STATE_BLOCKED_ON_HUMAN,
  PANE_STATE_IDLE_COMPLETE,
  PANE_STATE_TRANSIENT_API_ERROR,
  PANE_STATE_FATAL_API_ERROR,
  PANE_STATE_UNKNOWN_API_ERROR,
  BLOCKED_REASON_QUESTION,
  BLOCKED_REASON_MENU,
  BLOCKED_REASON_HUMAN_REQUIRED,
  blockingPromptKey: classifyBlockingPromptKey,
  classifyReadiness,
  recentPaneLines,
  classifyBlockedOnHumanReason,
  paneLooksBlockedOnHuman,
  paneTail,
  paneHoldsUnsubmittedPrompt,
  paneShowsTransientAPIError,
  paneShowsUnretryableAPIError,
  paneQuotaExhaustion,
  paneShowsLoginRequiredError,
  paneUnknownAPIErrorLine,
  classifyPane,
} = require('./lib/pane-classifier.js');

// Cross-process quota-pool store + scoped override channel (hivecommons/hive#6953).
const quotaPoolStore = require('./lib/quota-pool-store.js');

const rawHub = process.env.HIVE_HUB || 'wss://hive.hivecommons.dev/contribute';
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
// The GitHub login this contributor pushes and opens PRs as — exported by
// contributor-agent.sh, which defaults it to the literal string "unknown" when
// registration did not supply one. Used to decide whether a PR seen in the pane
// is OURS (kubestellar/hive#6662); prAttributionEvidence() treats "unknown" as
// "identity not available" rather than as a login, so the authorship check is
// simply skipped instead of refusing every PR on a relay without one.
const CONTRIBUTOR_LOGIN = (process.env.HIVE_CONTRIBUTOR_USERNAME || '').trim();
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
const CONTRIBUTOR_CONFIG_DIR = process.env.HIVE_CONTRIBUTOR_CONFIG_DIR || path.join(process.env.HOME || process.cwd(), '.config', 'hive');
const CONTRIBUTOR_ENV_FILE = process.env.HIVE_CONTRIBUTOR_ENV || path.join(CONTRIBUTOR_CONFIG_DIR, 'contributor.env');
const RELAY_PID_FILE = process.env.HIVE_RELAY_PID_FILE || path.join(CONTRIBUTOR_CONFIG_DIR, 'contributor-relay.pid');
const HUBS_SEEN_FILE = process.env.HIVE_HUBS_SEEN_FILE || path.join(CONTRIBUTOR_CONFIG_DIR, 'hubs-seen.json');
const HUBS_SEEN_MIN_WRITE_MS = 60000;
let hubsSeenWriteTimer = null;
let hubsSeenDirty = false;
let lastHubsSeenWrite = 0;

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
// The same variable WITHOUT the cwd fallback, for the one consumer that must
// never guess: the task-exit checkout sweep (taskCheckoutDir, #7790). In local
// mode the relay's cwd is the operator's own hive checkout, and a sweep that
// fell back to it would stash the operator's work.
const TASK_CHECKOUT_ROOT = (process.env.HIVE_WORKSPACE_DIR || '').trim();

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


// What we type. Short and free of shell metacharacters by construction — it is
// interpolated into a tmux send-keys command line below.
const TRANSIENT_API_ERROR_NUDGE_MESSAGE = 'try again';
// Bounded so a persistent upstream failure ends as an honest task failure
// rather than an infinite typing loop. Mirrors the hub's cap and cooldown.
const TRANSIENT_API_ERROR_MAX_NUDGES = 3;
const TRANSIENT_API_ERROR_NUDGE_COOLDOWN_MS = 90000;

// What the relay types at an unattended pane that stopped to ask a question
// (kubestellar/hive#5281). The task prompts already tell agents to decide for
// themselves — an agent that stops to ask is one that forgot, and a human
// watching would type exactly this line. When nobody is watching, nobody does.
//
// Letters, spaces and one comma, by construction. tmuxSendNudge passes this as
// a literal argv element, so punctuation is safe; the test remains as a cheap
// pin that this unattended prompt stays plain.
const AUTONOMY_NUDGE_MESSAGE =
  'no human is available to answer, so proceed autonomously with your best judgment';

// What the relay types when a backend's own reviewer posts notes UNDER the
// agent's HIVE_VERDICT line (hivecommons/hive#7759). omp's `--advisor` runtime
// reviews every turn passively and injects its notes after the turn ends, so
// its review of the agent's FINAL turn lands on the pane after the sentinel —
// and the sentinel being final (#5376, #7662, #7733) means the relay used to
// kill the CLI with those notes unread. Two live tasks each ended under a stack
// of ⟦concern⟧ notes; one was a real, cheap fix the agent would have made.
//
// The agent is asked ONCE per task to address them and re-print the verdict;
// the second verdict is final whatever appears under it. See
// maybeRequestPostVerdictReview for the bound and POST_VERDICT_REVIEW_MARKERS
// for which backends and which notes qualify.
//
// The opening phrase doubles as the anchor postVerdictReviewAnswered() looks
// for in the CLI's echo of this message, so that a verdict is only read as the
// SECOND one when it sits below that echo. Keep it at the very start, short
// enough to survive tmux wrapping at any sane pane width, and keep
// POST_VERDICT_REVIEW_ANCHOR a verbatim prefix of it.
//
// What actually gets typed is buildPostVerdictReviewMessage() below, which
// quotes the notes in question (#7935); this constant is the wording it falls
// back to when there is nothing quotable.
const POST_VERDICT_REVIEW_MESSAGE =
  'Advisor notes were posted after your verdict. Address the concerns that apply to your change, skip nits and anything already handled, then print the HIVE_VERDICT line again on its own line.';
const POST_VERDICT_REVIEW_ANCHOR = 'Advisor notes were posted after your verdict';
// The instruction half of the message, reused verbatim by the quoting form
// below so the two spellings cannot drift.
const POST_VERDICT_REVIEW_INSTRUCTION =
  'Address the ones that apply to your change, skip nits and anything already handled, then print the HIVE_VERDICT line again on its own line.';

// #7935: the message above names the EVENT ("notes were posted") but not the
// NOTES. That is unambiguous only when the pane holds exactly the notes the
// relay means. It usually does not: an advisor that reviews every turn has
// already posted 1–3 mid-turn notes the agent read and acted on, so "advisor
// notes were posted after your verdict" reads perfectly well as "the ones you
// already handled". Observed on projectbluefin/utah#24: the agent matched the
// nudge to two mid-turn ⟦blocker⟧s it had resolved, searched the hub and the
// PR for anything newer, found nothing, and re-printed the verdict — and the
// wrong-file citation the advisor had actually flagged shipped in utah#225.
// The relay has the notes in hand when it types the nudge (it already logs
// them), so it quotes them.
//
// Quoted as ONE line, joined with ` | `: the nudge path types a literal
// keystroke burst (tmuxSendNudge) with no bracketed-paste settle behind it, so
// an embedded newline risks submitting the first line on its own and typing
// the rest into a working agent. A single line has no such failure mode.
const POST_VERDICT_NOTE_JOINER = ' | ';
// Per-note and per-message bounds. A note block is the advisor's own prose and
// can run long; the point of quoting is to identify WHICH note, and the full
// text is on the pane right above the nudge either way.
const POST_VERDICT_NOTE_MAX_CHARS = 400;
const POST_VERDICT_NOTES_MAX_QUOTED = 4;

// sanitizePostVerdictNote makes one advisor note safe to type back into the
// pane: one line, no control characters, and — the load-bearing part — no
// live `HIVE_VERDICT:` sentinel.
//
// The CLI echoes what it is typed, and a long echo WRAPS, so any fragment of
// the nudge can land at the start of a pane row. hiveVerdictLineRe() anchors
// at line start, so an advisor note quoting the agent's own
// `HIVE_VERDICT: complete` line would be read back by detectHiveVerdict() as
// the SECOND verdict the follow-up is waiting for and finalize the task on the
// spot — the follow-up answering itself. Dropping the colon defuses it (the
// regex requires `HIVE_VERDICT:`) and still reads as prose. The existing
// "the echoed request must not itself read as a verdict" pin on the static
// message is the same rule; this is it applied to text the relay did not write.
function sanitizePostVerdictNote(text) {
  return String(text == null ? '' : text)
    // eslint-disable-next-line no-control-regex
    .replace(/[\u0000-\u001f\u007f]/g, ' ')
    .replace(/HIVE_VERDICT\s*:/gi, 'HIVE_VERDICT')
    .replace(/\s+/g, ' ')
    .trim();
}

function truncatePostVerdictNote(text) {
  return text.length > POST_VERDICT_NOTE_MAX_CHARS
    ? `${text.slice(0, POST_VERDICT_NOTE_MAX_CHARS - 1).trimEnd()}…`
    : text;
}

// buildPostVerdictReviewMessage renders the follow-up with the notes that
// earned it quoted inline. Falls back to the bare POST_VERDICT_REVIEW_MESSAGE
// when there is nothing quotable, so a pane shape the block parser cannot read
// degrades to exactly the pre-#7935 behaviour rather than to a truncated
// sentence. POST_VERDICT_REVIEW_ANCHOR stays a verbatim prefix either way,
// which is what keeps postVerdictReviewAnswered() and
// paneHoldsUnsubmittedPrompt() unchanged.
function buildPostVerdictReviewMessage(notes) {
  const quotable = (Array.isArray(notes) ? notes : [])
    .map(n => truncatePostVerdictNote(sanitizePostVerdictNote(n)))
    .filter(Boolean);
  if (quotable.length === 0) return POST_VERDICT_REVIEW_MESSAGE;
  const shown = quotable.slice(0, POST_VERDICT_NOTES_MAX_QUOTED);
  const omitted = quotable.length - shown.length;
  const quoted = shown.map((n, i) => `(${i + 1}) "${n}"`).join(POST_VERDICT_NOTE_JOINER);
  const more = omitted > 0
    ? ` (and ${omitted} more note${omitted === 1 ? '' : 's'} below your verdict on the pane)`
    : '';
  return `${POST_VERDICT_REVIEW_ANCHOR} — these ones, not any note you already handled earlier in this turn: ` +
    `${quoted}${more}. ${POST_VERDICT_REVIEW_INSTRUCTION}`;
}
// #7879: hard cap on review follow-ups per task. The first is earned by any
// new ⟦blocker⟧/⟦concern⟧ under the verdict; the second ONLY by a ⟦blocker⟧
// that was not on the pane at the previous verdict. Never a third.
const POST_VERDICT_REVIEW_MAX_FOLLOWUPS = 2;
// Heading under which notes that were still unaddressed when the task
// finalized are recorded in the task_complete summary and the PR comment.
const UNADDRESSED_ADVISOR_NOTES_HEADING = 'Advisor notes not addressed before completion';

// #7862: a `HIVE_VERDICT: complete` with no PR behind it gets one follow-up
// before it is finalized. Same once-per-task bound and same pending/answered
// mechanics as the #7759 review follow-up above.
const PR_CLAIM_FOLLOWUP_MESSAGE =
  'Your verdict says complete, but no PR for this task exists. Open it now — branch, commit, push, gh pr create — and then print the HIVE_VERDICT line again on its own line; or, if there is nothing to ship, print HIVE_VERDICT: no_work_needed — <reason> instead.';
const PR_CLAIM_FOLLOWUP_ANCHOR = 'Your verdict says complete, but no PR for this task exists';
// The verdict's reason text claims a PR. Only such a claim triggers the
// follow-up: a bare `complete` with no PR is booked evidence-less by the hub
// (same issue), but it is not a contradiction the relay can put to the agent.
const PR_CLAIM_PATTERN = /\bPRs?\b|pull[ -]request|\bopened\b/i;

const BYTES_PER_MIB = 1024 * 1024;
const DEFAULT_HEADLESS_MAX_OUTPUT_MIB = 16;
const DEFAULT_HEADLESS_MAX_OUTPUT_BYTES = DEFAULT_HEADLESS_MAX_OUTPUT_MIB * BYTES_PER_MIB;
const HEADLESS_MAX_OUTPUT_ENV = 'HIVE_RELAY_MAX_OUTPUT_BYTES';

function parsePositiveIntegerEnv(name, fallback) {
  const raw = process.env[name];
  if (!raw) return fallback;
  const parsed = Number.parseInt(raw, 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

// Cap on captured child output kept in memory / sent to the hub, so a chatty
// CLI cannot grow the buffer without bound. The tail is what matters for an
// audit trail, mirroring TMUX_TAIL_LINES on the interactive path.
const HEADLESS_MAX_OUTPUT_BYTES = parsePositiveIntegerEnv(HEADLESS_MAX_OUTPUT_ENV, DEFAULT_HEADLESS_MAX_OUTPUT_BYTES);

// #7932: the hub reads a contributor frame under a hard cap — wsMaxMessageSize
// in src/pkg/dashboard/contribute_ws.go, 64 KiB — installed with
// conn.SetReadLimit. gorilla/websocket does not truncate an oversized message:
// it closes the connection with 1009 "message too big" and the frame is LOST.
// For a task_complete that is a reconnect loop rather than a dropped log line —
// the completion never lands, the hub's lease outlives the close, the same task
// is handed back, and the agent redoes work it already shipped (four times over
// against projectbluefin/utah#24 on the live Bluefin hub, one of the runs having
// opened a real PR).
//
// It is a headless-mode failure in practice. The interactive path's tmux_output
// is fifteen terminal ROWS, which cannot be large. A JSON-streaming backend such
// as pi (`--mode json`) emits one whole tool_execution_end event — embedded diff
// and all — per LINE, so the same fifteen lines is routinely hundreds of KiB.
// The bound therefore belongs on BYTES, at the point every frame passes through
// (sendTo), not on a line count at each call site that happens to build a tail.
const DEFAULT_HUB_MAX_FRAME_BYTES = 64 * 1024;
// Headroom kept under the hub's ceiling. The read limit measures the decoded
// payload, which is exactly the JSON we serialize, so the arithmetic is not in
// question — but a hub deployed with a slightly different bound, or a proxy that
// counts a frame's overhead against it, must not put us back on the wrong side
// of a hard close. What the slack costs is audit tail; what it buys is that a
// completion always lands.
const WS_FRAME_HEADROOM_BYTES = 4 * 1024;
// Floor for a hub-advertised limit, so a hub advertising something smaller than
// the headroom cannot leave the relay with a zero or negative budget.
const MIN_WS_FRAME_BYTES = 4 * 1024;
// Budget for a hub that does not advertise its limit (every hub released before
// #7932). 64 KiB has been the hub's value for the life of the protocol.
const WS_FRAME_BYTES = DEFAULT_HUB_MAX_FRAME_BYTES - WS_FRAME_HEADROOM_BYTES;
// How much captured output any single frame carries. Far below the frame budget
// on purpose: tmux_output is an audit TAIL — the last thing the agent printed,
// read by a human on the ops surface — not a transcript. 8 KiB is several
// screens of ordinary CLI output and still leaves the frame budget almost
// entirely to the fields that carry meaning (pr_url, verdict, summary).
const OUTPUT_TAIL_MAX_BYTES = 8 * 1024;
const OUTPUT_TAIL_TRUNCATED_MARKER = '[relay: output tail truncated to fit the hub frame limit]';
const TEXT_TRUNCATED_SUFFIX = ' […truncated]';
// Frame fields that are pure payload: truncating one changes how much of the
// story the frame tells, never what the frame MEANS. An allowlist, not a
// denylist — type, task_id, task_gen, result, verdict, pr_url and every other
// protocol field must survive a clamp intact, and a field nobody has thought
// about yet is protocol until someone says otherwise. Ordered most-expendable
// first: the tail before the human-readable summary, the summary before the
// reason a task failed.
const FRAME_TRUNCATABLE_FIELDS = ['tmux_output', 'prompt', 'summary', 'title', 'reason', 'verdict_reason'];

const TMUX_TAIL_LINES = 15;
const NEEDS_LOGIN_CONFIRM_TICKS = 3;
// #6667: the window detectPRURL scans is NOT the 15-line protocol payload.
// TMUX_TAIL_LINES is sized for the audit trail the hub stores, and of those 15
// terminal rows roughly ten are TUI chrome — the input box, the status bar, the
// hint line — so only a handful of real output rows survive. A PR URL the agent
// genuinely printed scrolls out of that window within seconds of being printed,
// and the relay then reports no PR for a task that shipped one. Scan deep
// scrollback for the URL while still sending only the tail upstream.
const PR_SCAN_LINES = 400;
const HEARTBEAT_INTERVAL_MS = 30000;
const HEARTBEAT_TIMEOUT_MS = 90000;
const RELAY_TEST_TIMING = process.env.HIVE_RELAY_TEST_TIMING === '1';
const PROGRESS_REPORT_INTERVAL_MS = RELAY_TEST_TIMING ? 100 : 120000;
// #7841: how often the relay glances at the pane for a HIVE_VERDICT line
// between progress ticks. The tick loop is what credits a verdict, but it runs
// every PROGRESS_REPORT_INTERVAL_MS, so a verdict printed just after a tick sat
// unacted-on for up to two minutes — and whatever re-woke the agent in that
// window (a background shell exiting, omp's todo reminder) ran on after the
// task had, in every sense that matters, finished. The watch is a pure pane
// read; when it sees a fresh verdict it runs the SAME progressTick early rather
// than duplicating any of its judgement.
const VERDICT_WATCH_INTERVAL_MS = RELAY_TEST_TIMING ? 30 : 5000;
// #7907: how long a HIVE_VERDICT line must have been on the pane before the
// tick that finalizes on it. omp queues the advisor notes generated mid-turn
// and appends them at the END of the agent's turn — the same instant as the
// final message — and renders them under the verdict about a second later.
// The #7841 fast path saw the verdict while that message was still streaming
// and judged a pane the notes had not reached yet, so whether #7879 recorded
// them depended on the phase of a 5 s timer (utah#218 lost four). A verdict
// first seen on this capture is re-captured on a later tick, at least this
// long after; one extra interval at most, no new turns. The test harness sets
// it to 0 via __setVerdictSettleMs and opts in per case.
let VERDICT_SETTLE_MS = 2000;
const MAX_RECONNECT_DELAY_MS = 60000;
const BASE_RECONNECT_DELAY_MS = 1000;
const TOKEN_REFRESH_MARGIN_MS = 300000;
const TMUX_COMMAND_TIMEOUT_MS = Number(process.env.HIVE_TMUX_COMMAND_TIMEOUT_MS) || 15000;
const LIVE_CLI_FIRST_INTERRUPT_DELAY_MS = Number(process.env.HIVE_LIVE_CLI_FIRST_INTERRUPT_DELAY_MS) || 1000;
const LIVE_CLI_SECOND_INTERRUPT_DELAY_MS = Number(process.env.HIVE_LIVE_CLI_SECOND_INTERRUPT_DELAY_MS) || 2000;
const LIVE_CLI_SHELL_WAIT_TIMEOUT_MS = Number(process.env.HIVE_LIVE_CLI_SHELL_WAIT_TIMEOUT_MS) || 5000;
const LIVE_CLI_SHELL_WAIT_POLL_MS = Number(process.env.HIVE_LIVE_CLI_SHELL_WAIT_POLL_MS) || 250;
const LIVE_CLI_RESPAWN_SETTLE_MS = Number(process.env.HIVE_LIVE_CLI_RESPAWN_SETTLE_MS) || 500;
// MAX_TASK_DURATION_MS is a PROGRESS lease, not a wall-clock budget
// (kubestellar/hive#5321). It bounds how long a task may go without the relay
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

// Hard ceiling on a single headless one-shot invocation (kubestellar/hive#2538).
// The interactive path has no pane to scrape for progress on the headless path,
// so a headless child cannot use the progress lease above: there is no
// equivalent signal. It gets the ABSOLUTE bound enforced directly on the
// process instead, so a wedged CLI is killed and reported failed rather than
// hanging the pod forever — and, per #5321, a long-but-live headless run is no
// longer killed at 30 minutes either.
//
// This bound only holds if the HUB agrees (hivecommons/hive#7778). The hub's
// own lease on the task is a progress lease: it is renewed by every
// task_progress frame and reclaimed — the task revoked, the issue put in
// failure cooldown — after 30 minutes without one. The interactive path feeds
// it from progressTick(); the headless path used to send a single task_progress
// when the child started and nothing more, so the hub took every headless task
// back at 30 minutes regardless of this ceiling, and the revoke killed a live
// child mid-run. runHeadlessTask() now reports progress on the same cadence as
// the interactive path for as long as its child is alive (headlessProgressTick).
const HEADLESS_TASK_TIMEOUT_MS = Number(process.env.HIVE_HEADLESS_TASK_TIMEOUT_MS) || ABSOLUTE_TASK_DEADLINE_MS;
const NETWORK_ERROR_RETRY_DELAY_MS = 5000;
// After the hub sends an explicit task_unavailable negative-ack (no admissible
// work, a disabled tier, a concurrency limit, or a token-mint failure — see
// kubestellar/hive#2436), wait before re-asking so we neither hang forever
// (the old silent-nil behaviour) nor busy-loop the hub.
const TASK_UNAVAILABLE_RETRY_MS = 30000;

// ── Contributor quota guard (hivecommons/hive#6833) ────────────────────────
//
// This is the local, pre-acceptance safety guard for subscription headroom. It
// intentionally evaluates normalized limit windows (`kind`, `pct_remaining`,
// `resets_at`, optional `scope`) matching RFC #5698's provider-headroom model
// instead of baking provider-specific quota facts into the relay admission path.
const QUOTA_GUARD_MODE = (process.env.HIVE_CONTRIBUTOR_QUOTA_GUARD || 'ask').trim().toLowerCase();
const QUOTA_GUARD_DEFAULT_RESERVE = parseQuotaPctEnv('HIVE_CONTRIBUTOR_QUOTA_MIN_REMAINING_PCT', 20);
const QUOTA_GUARD_SHORT_RESERVE = parseOptionalQuotaPctEnv('HIVE_CONTRIBUTOR_QUOTA_SHORT_MIN_REMAINING_PCT');
const QUOTA_GUARD_WEEKLY_RESERVE = parseOptionalQuotaPctEnv('HIVE_CONTRIBUTOR_QUOTA_WEEKLY_MIN_REMAINING_PCT');
const QUOTA_GUARD_TIER_RESERVES = {
  simple: parseOptionalQuotaPctEnv('HIVE_CONTRIBUTOR_QUOTA_SIMPLE_MIN_REMAINING_PCT'),
  medium: parseOptionalQuotaPctEnv('HIVE_CONTRIBUTOR_QUOTA_MEDIUM_MIN_REMAINING_PCT'),
  complex: parseOptionalQuotaPctEnv('HIVE_CONTRIBUTOR_QUOTA_COMPLEX_MIN_REMAINING_PCT'),
  unknown: parseOptionalQuotaPctEnv('HIVE_CONTRIBUTOR_QUOTA_UNKNOWN_MIN_REMAINING_PCT'),
};
if (!['ask', 'pause', 'off'].includes(QUOTA_GUARD_MODE)) {
  console.error(`FATAL: HIVE_CONTRIBUTOR_QUOTA_GUARD must be ask, pause, or off (got ${JSON.stringify(QUOTA_GUARD_MODE)})`);
  process.exit(1);
}
const QUOTA_READING_FILE = (process.env.HIVE_CONTRIBUTOR_QUOTA_READING_FILE || '').trim();
const QUOTA_READING_JSON = (process.env.HIVE_CONTRIBUTOR_QUOTA_READING_JSON || '').trim();
// The Go rotation publisher probes every 5 minutes (rotation.pollInterval). A
// reading older than three missed publish cycles is treated as stale: generous
// enough for transient probe/write delays and short enough to avoid admitting
// indefinitely against frozen quota after the publisher dies.
const QUOTA_READING_PUBLISH_INTERVAL_MS = 5 * 60 * 1000;
const QUOTA_READING_STALE_AFTER_MS = (() => {
  const raw = (process.env.HIVE_CONTRIBUTOR_QUOTA_READING_TTL_MS || '').trim();
  if (raw === '') return 3 * QUOTA_READING_PUBLISH_INTERVAL_MS;
  if (!/^\d+$/.test(raw) || Number(raw) <= 0) {
    console.error(`FATAL: HIVE_CONTRIBUTOR_QUOTA_READING_TTL_MS must be a positive integer of milliseconds (got ${JSON.stringify(raw)})`);
    process.exit(1);
  }
  return Number(raw);
})();
// How often a guarded relay re-reads the quota and re-advertises if it clears
// (kubestellar/hive#6951). Default 60s: the readings this guard consumes move
// on the order of minutes, and a held relay is doing nothing else.
const QUOTA_GUARD_RETRY_MS = (() => {
  const raw = (process.env.HIVE_CONTRIBUTOR_QUOTA_RETRY_MS || '').trim();
  if (raw === '') return 60000;
  if (!/^\d+$/.test(raw) || Number(raw) <= 0) {
    console.error(`FATAL: HIVE_CONTRIBUTOR_QUOTA_RETRY_MS must be a positive integer of milliseconds (got ${JSON.stringify(raw)})`);
    process.exit(1);
  }
  return Number(raw);
})();
let contributorQuotaPaused = false;
let contributorQuotaStayPaused = false;

// ── Cross-process quota-pool store + override channel (hivecommons/hive#6953) ─
//
// #6833 wants guard state keyed by the local quota POOL, not by relay process,
// so two relays on one provider account share one reserve, and it wants scoped
// overrides that reach a DETACHED relay through a reliable command path. Both
// live in one on-disk pool directory (see lib/quota-pool-store.js for why they
// are one mechanism).
//
// SAFE-SUBSET / DEVIATION (documented on #6953): the pool store is OPT-IN,
// active only when HIVE_CONTRIBUTOR_QUOTA_POOL_DIR is set. Nothing derives the
// provider account identity yet (that prober is descoped to the adapter
// sibling), so keying every default install by a guessed pool identity — or
// serializing two relays that may not even share an account — would change
// default behaviour with no reading to justify it, the same reason #6951's
// "unknown ⇒ hold" was scoped to a distinct `unprovisioned` admit. When the
// dir is unset the guard behaves exactly as before: purely in-process.
const QUOTA_POOL_DIR = (process.env.HIVE_CONTRIBUTOR_QUOTA_POOL_DIR || '').trim();
// The account component of the pool identity. It is HASHED into an opaque key
// and never logged raw (#6833 privacy). Unset keys the pool off the backend
// alone — same-backend relays on one host share, which can only UNDER-, never
// over-subscribe the true account pool.
const QUOTA_POOL_ACCOUNT = (process.env.HIVE_CONTRIBUTOR_QUOTA_POOL_ACCOUNT || '').trim();
// A stable id for THIS relay session, so a disable-session override targets one
// process and a session opt-out expires when this process exits. Falls back to
// the HIVE_SESSION label, then the pid — never an account identifier.
const QUOTA_SESSION_ID = (process.env.HIVE_CONTRIBUTOR_QUOTA_SESSION_ID || '').trim()
  || (process.env.HIVE_SESSION || '').trim()
  || `pid-${process.pid}`;
// Per-process reservation identity. A reservation is this relay's claim on the
// shared pool while it runs a task; peers must account for it before admitting.
const QUOTA_RELAY_ID = `${QUOTA_SESSION_ID}-${process.pid}-${Math.random().toString(36).slice(2, 8)}`;
// A reservation is a lease, not a lock: if a relay crashes mid-task its file is
// swept once this TTL lapses so a dead peer cannot wedge the pool forever. It
// is refreshed on every progress tick, so a long, healthy task keeps its claim.
const QUOTA_RESERVATION_TTL_MS = RELAY_TEST_TIMING ? 200 : 15 * 60 * 1000;

const quotaPool = QUOTA_POOL_DIR
  ? { dir: QUOTA_POOL_DIR, poolKey: quotaPoolStore.derivePoolKey({ backend: BACKEND, account: QUOTA_POOL_ACCOUNT }) }
  : null;

function quotaPoolActive() { return quotaPool !== null; }

// The subscription backends the guard has an adapter for (kubestellar/hive#6967
// / #6833). On these, the Go rotation probers publish a normalized reading into
// the pool directory, so "no reading yet" is a transient startup/torn state to
// HOLD on, not the permanent `unprovisioned` admit an unsupported backend gets.
// `pi` fronts anthropic and `gemini` fronts google in the default rotation set.
const QUOTA_GUARD_SUPPORTED_BACKENDS = new Set(['claude', 'pi', 'codex', 'agy', 'gemini']);
const quotaBackendSupported = QUOTA_GUARD_SUPPORTED_BACKENDS.has(BACKEND);

// The pool-keyed reading file the Go publisher writes and this relay reads when
// no reading env var is set. Same directory and key as the #6953 store, its own
// `.reading.json` suffix. Matches rotation.ContributorReadingPath in Go.
function quotaPublishedReadingFile() {
  return quotaPool ? path.join(quotaPool.dir, `${quotaPool.poolKey}.reading.json`) : '';
}

// defaultContributorPoolDir derives the per-install pool directory used when the
// operator has NOT set HIVE_CONTRIBUTOR_QUOTA_POOL_DIR (kubestellar/hive#6987).
// It makes publishing default-on for supported backends (#6967 criterion 1): a
// default install reads the reading the Go publisher writes here with no hand-set
// env var. The path MUST equal rotation.DefaultContributorPoolDir() in Go, or
// the relay reads where the publisher never wrote — the same parity #6983 pinned
// for the pool key. Both honour XDG_CONFIG_HOME first (Go's os.UserConfigDir
// ignores it on darwin), then the platform user config dir, matching the
// hivectl session-cache precedent in src/pkg/hivectl/session.go. An
// unresolvable base returns '' so the relay stays on its unprovisioned/admit
// default rather than holding with no route.
function defaultContributorPoolDir() {
  let base = (process.env.XDG_CONFIG_HOME || '').trim();
  if (!base) {
    const home = (process.env.HOME || require('os').homedir() || '').trim();
    if (process.platform === 'win32') {
      base = (process.env.AppData || '').trim();
    } else if (process.platform === 'darwin') {
      base = home ? path.join(home, 'Library', 'Application Support') : '';
    } else {
      base = home ? path.join(home, '.config') : '';
    }
  }
  if (!base) return '';
  return path.join(base, 'hive', 'contributor-quota');
}

// The default-derived pool directory, captured once at load so it is stable for
// the life of the relay (process.env is read at module load, mirroring
// QUOTA_POOL_DIR). Empty when no base directory can be resolved.
const QUOTA_DEFAULT_POOL_DIR = defaultContributorPoolDir();

// The published reading file inside the DEFAULT (non-env) pool directory, for a
// supported backend when no explicit HIVE_CONTRIBUTOR_QUOTA_POOL_DIR is set.
// Same pool key as the explicit store, so the publisher and relay agree on the
// path.
function quotaDefaultPublishedReadingFile() {
  return QUOTA_DEFAULT_POOL_DIR
    ? path.join(QUOTA_DEFAULT_POOL_DIR, `${quotaPoolStore.derivePoolKey({ backend: BACKEND, account: QUOTA_POOL_ACCOUNT })}.reading.json`)
    : '';
}

// The publisher PRESENCE marker inside the DEFAULT pool directory
// (kubestellar/hive#6987, condition (b) in src/docs/contributor-relay.md). The
// Go publisher writes `<poolKey>.publisher.json` when its probe loop starts and
// refreshes it with every reading it publishes, so a FRESH marker is a positive
// declaration that a publisher on this host feeds this pool. That is the
// distinguisher the flip was gated on: with it, "reading missing but publisher
// expected" can HOLD while "nothing will ever write here" (relay-only host,
// dead publisher, uninstalled CLI — the publisher removes the marker on
// not_installed) keeps the non-stranding unprovisioned admit. Matches
// rotation.ContributorPublisherMarkerPath in Go.
function quotaDefaultPublisherMarkerFile() {
  return QUOTA_DEFAULT_POOL_DIR
    ? path.join(QUOTA_DEFAULT_POOL_DIR, `${quotaPoolStore.derivePoolKey({ backend: BACKEND, account: QUOTA_POOL_ACCOUNT })}.publisher.json`)
    : '';
}

// Freshness is judged on the marker file's MTIME, never its contents: the
// publisher rewrites it atomically each publish cycle, and an unparsable or
// torn marker therefore cannot strand a host — a file nothing refreshes simply
// goes stale and the guard falls back to the unprovisioned admit. Reuses the
// reading TTL: both are "how long without a publisher write before we stop
// believing one is alive".
function quotaPublisherMarkerFresh(markerFile, nowMs = Date.now()) {
  if (!markerFile) return false;
  try {
    const st = fs.statSync(markerFile);
    return nowMs - st.mtimeMs <= QUOTA_READING_STALE_AFTER_MS;
  } catch (e) {
    return false;
  }
}


// The reset epoch of a normalized window, however the adapter spells it. This
// is what makes an until-reset override expire MECHANICALLY: the override pins
// the epoch it was created against, and the moment the window reports a
// different one the override no longer matches and is inert.
function quotaWindowResetEpoch(window) {
  const raw = window && (window.reset_epoch ?? window.resets_at ?? window.reset_at);
  const n = Number(raw);
  return Number.isFinite(n) ? n : null;
}

// A continue-* override is consent to spend BELOW the configured reserve. It is
// deliberately NOT honoured for unknown/stale/unprovisioned readings: those are
// "we cannot see the quota", and consenting to fly blind is the fail-open this
// guard exists to prevent. So overrides only ever convert a `guarded` (reserve)
// hold into an admit, and only when the override genuinely still applies.
function quotaOverrideApplies(override, guardWindow, reading) {
  if (!override) return false;
  switch (override.directive) {
    case 'disable_session':
      // Session opt-out: expires when this process exits (a new process gets a
      // new QUOTA_SESSION_ID, so a stale file never authorizes a later run).
      return override.session_id === QUOTA_SESSION_ID;
    case 'continue_once':
      // Expires after one admitted assignment; consumption happens at the
      // task-admission site, not here (evaluate also runs for `ready`).
      return true;
    case 'continue_until_reset': {
      // Bound to the EXACT guarded window and the reset epoch it was created
      // against. A different window, or the same window after its reset epoch
      // rolls, no longer matches.
      if (!guardWindow) return false;
      const id = guardWindow.id || guardWindow.kind;
      if (override.window_id !== id) return false;
      const epoch = quotaWindowResetEpoch(guardWindow);
      return epoch !== null && override.reset_epoch === epoch;
    }
    default:
      return false;
  }
}

// A pause-until-reset override forces a hold even when a reading has cleared,
// until the window it names resets — an explicit pause outranking an automatic
// resume (#6833). Absent that window from the reading (or a rolled epoch) it has
// expired and no longer holds.
function quotaPauseOverrideActive(override, reading) {
  if (!override || override.directive !== 'pause_until_reset') return false;
  const windows = (reading && reading.limits) || [];
  for (const w of windows) {
    const id = w.id || w.kind;
    if (override.window_id !== id) continue;
    const epoch = quotaWindowResetEpoch(w);
    if (epoch !== null && override.reset_epoch === epoch) return true;
  }
  return false;
}

function readQuotaOverride() {
  return quotaPoolActive() ? quotaPoolStore.readOverride(quotaPool) : null;
}

// consumeQuotaOverrideOnce clears a continue-once override the instant the one
// assignment it authorized is admitted, so it can never silently become a
// standing authorization to keep spending.
function consumeQuotaOverrideOnce() {
  if (!quotaPoolActive()) return;
  const ov = quotaPoolStore.readOverride(quotaPool);
  if (ov && ov.directive === 'continue_once') quotaPoolStore.clearOverride(quotaPool);
}

// Claim this relay's share of the shared pool while a task is in flight.
function reserveQuotaPool() {
  if (!quotaPoolActive()) return;
  try { quotaPoolStore.writeReservation(quotaPool, QUOTA_RELAY_ID, Date.now() + QUOTA_RESERVATION_TTL_MS); } catch (_) {}
}

function refreshQuotaPoolReservation() {
  if (!quotaPoolActive() || !currentTask) return;
  reserveQuotaPool();
}

function releaseQuotaPoolReservation() {
  if (!quotaPoolActive()) return;
  try { quotaPoolStore.releaseReservation(quotaPool, QUOTA_RELAY_ID); } catch (_) {}
}

function quotaPoolHasPeerReservation() {
  if (!quotaPoolActive()) return false;
  return quotaPoolStore.activePeerReservations(quotaPool, QUOTA_RELAY_ID).length > 0;
}


function parseQuotaPctEnv(name, fallback) {
  const raw = process.env[name];
  if (raw === undefined || String(raw).trim() === '') return fallback;
  if (!/^\d+$/.test(String(raw).trim())) {
    console.error(`FATAL: ${name} must be a percentage from 0 to 100 (got ${JSON.stringify(raw)})`);
    process.exit(1);
  }
  const n = Number(String(raw).trim());
  if (n < 0 || n > 100) {
    console.error(`FATAL: ${name} must be a percentage from 0 to 100 (got ${JSON.stringify(raw)})`);
    process.exit(1);
  }
  return n;
}

function parseOptionalQuotaPctEnv(name) {
  return process.env[name] === undefined || String(process.env[name]).trim() === ''
    ? null
    : parseQuotaPctEnv(name, 0);
}

function normalizeTaskComplexity(task) {
  const raw = ((task && (task.complexity || task.task_complexity || task.complexity_tier)) || '').toString().trim().toLowerCase();
  if (['simple', 'medium', 'complex'].includes(raw)) return raw;
  const labels = Array.isArray(task && task.labels) ? task.labels.map(l => String(l).toLowerCase()) : [];
  for (const l of labels) {
    if (l === 'complexity/simple' || l === 'simple') return 'simple';
    if (l === 'complexity/medium' || l === 'medium') return 'medium';
    if (l === 'complexity/complex' || l === 'complex') return 'complex';
  }
  return 'unknown';
}

// The window kinds this build has been taught. This list decides which reserve
// override applies and what the hold is CALLED — never whether a window counts.
// Gating on it is exactly the fail-open #6951 removed, so it must stay out of
// the admit/refuse decision (kubestellar/hive#6951).
const QUOTA_SHORT_WINDOW_KINDS = ['session', 'short', 'five_hour'];
const QUOTA_WEEKLY_WINDOW_KINDS = ['weekly', 'weekly_scoped'];

function quotaWindowKindIsKnown(kind) {
  return QUOTA_SHORT_WINDOW_KINDS.includes(kind) || QUOTA_WEEKLY_WINDOW_KINDS.includes(kind);
}

function quotaWindowReserve(kind) {
  if (QUOTA_SHORT_WINDOW_KINDS.includes(kind) && QUOTA_GUARD_SHORT_RESERVE !== null) return QUOTA_GUARD_SHORT_RESERVE;
  if (QUOTA_WEEKLY_WINDOW_KINDS.includes(kind) && QUOTA_GUARD_WEEKLY_RESERVE !== null) return QUOTA_GUARD_WEEKLY_RESERVE;
  return QUOTA_GUARD_DEFAULT_RESERVE;
}

function quotaRequiredReserve(window, complexity) {
  const windowReserve = quotaWindowReserve((window && window.kind) || '');
  const tierReserve = QUOTA_GUARD_TIER_RESERVES[complexity] === null
    ? QUOTA_GUARD_DEFAULT_RESERVE
    : QUOTA_GUARD_TIER_RESERVES[complexity];
  return Math.max(windowReserve, tierReserve);
}


function contributorReadingCapturedAtMs(reading) {
  if (!reading || !Object.prototype.hasOwnProperty.call(reading, 'captured_at')) return null;
  const raw = reading.captured_at;
  if (typeof raw !== 'string' || raw.trim() === '') return null;
  const ms = Date.parse(raw);
  return Number.isFinite(ms) ? ms : null;
}

function contributorQuotaReadingWithFreshness(reading, nowMs = Date.now()) {
  const capturedAtMs = contributorReadingCapturedAtMs(reading);
  if (capturedAtMs === null) return reading;
  if (nowMs - capturedAtMs > QUOTA_READING_STALE_AFTER_MS) {
    return { ...reading, state: 'stale' };
  }
  return reading;
}

// readContributorQuotaReading never throws (kubestellar/hive#6951).
//
// This is called from inside the `task_assign` handler and from sendTo(), both
// of which run under hub.ws.on('message'), which has no try/catch — and the
// relay installs no `uncaughtException` handler. A half-written reading file
// therefore used to raise SyntaxError straight out of the message handler and
// terminate the relay. #6833 requires readings be stored atomically, but the
// writer is a separate concern on a separate issue, and a reader that assumes
// the writer got it right is one partial flush away from killing the process.
//
// An unreadable reading is `unknown`, which the guard holds on: refusing to
// start work because we cannot see the quota is the safe direction.
function readContributorQuotaReading() {
  if (QUOTA_GUARD_MODE === 'off') return { state: 'available', limits: [] };
  const parse = (label, fn) => {
    try {
      const reading = fn();
      if (!reading || typeof reading !== 'object' || Array.isArray(reading)) {
        throw new Error('reading is not a JSON object');
      }
      return contributorQuotaReadingWithFreshness(reading);
    } catch (e) {
      console.warn(`Contributor quota reading (${label}) could not be read: ${e.message}. Treating quota as unknown.`);
      return { state: 'unknown', limits: [] };
    }
  };
  if (QUOTA_READING_JSON) {
    return parse('HIVE_CONTRIBUTOR_QUOTA_READING_JSON', () => JSON.parse(QUOTA_READING_JSON));
  }
  if (QUOTA_READING_FILE) {
    return parse('HIVE_CONTRIBUTOR_QUOTA_READING_FILE', () => JSON.parse(fs.readFileSync(QUOTA_READING_FILE, 'utf8')));
  }
  // With no reading env var set, a supported subscription backend reads the
  // reading the Go rotation publisher writes (kubestellar/hive#6967). The pool
  // directory is EXPLICIT when HIVE_CONTRIBUTOR_QUOTA_POOL_DIR is set, otherwise
  // per-install DERIVED so publishing is default-on (kubestellar/hive#6987).
  //
  // The two dirs differ in one deliberate way. An EXPLICIT pool dir is the
  // operator declaring "a publisher feeds this pool", so "no reading yet" (or a
  // torn file) is a transient `unknown` that HOLDS while the publisher catches
  // up — the opt-in flip #6967 shipped. The DERIVED default dir gets the same
  // hold only on POSITIVE evidence of a publisher: a fresh `.publisher.json`
  // presence marker (condition (b), kubestellar/hive#6987). A missing reading
  // with NO fresh marker means "no reading route on this host" — a relay-only
  // host, a dead publisher, or an uninstalled CLI — and must fall through to
  // the `unprovisioned` admit, NOT a hold. This is exactly the fleet-wide stop
  // #6951's ruling guards against: a default install with no route must never
  // hold forever. A default file that DOES exist is read like any other: a torn
  // or state:"unknown" reading still HOLDS (fail-closed), so the publisher
  // writing a failed-probe `unknown` still guards, and only a genuinely absent
  // route admits.
  if (QUOTA_POOL_DIR) {
    const publishedFile = quotaPublishedReadingFile();
    if (publishedFile && quotaBackendSupported) {
      return parse('published quota reading', () => JSON.parse(fs.readFileSync(publishedFile, 'utf8')));
    }
  } else if (quotaBackendSupported) {
    const defaultFile = quotaDefaultPublishedReadingFile();
    if (defaultFile && fs.existsSync(defaultFile)) {
      return parse('published quota reading', () => JSON.parse(fs.readFileSync(defaultFile, 'utf8')));
    }
    // Condition (b) distinguisher (kubestellar/hive#6987): a FRESH publisher
    // marker is a live publisher's positive declaration that it feeds this
    // pool, so a missing reading here is "expected but not written yet" — a
    // transient to HOLD on, exactly like the explicit-dir case above. Without
    // a fresh marker (relay-only host, dead publisher, CLI not installed —
    // the publisher removes the marker on not_installed) nothing will ever
    // write here, and the non-stranding `unprovisioned` admit below stands.
    if (defaultFile && quotaPublisherMarkerFresh(quotaDefaultPublisherMarkerFile())) {
      warnQuotaGuardAwaitingPublisherOnce();
      return { state: 'unknown', limits: [] };
    }
  }
  // "No reading source configured" is NOT the same as "a configured source we
  // cannot read", and #6951 turns on the difference. A configured-but-broken
  // source is `unknown` and holds. An unconfigured one means the guard was
  // never provisioned on this host: nothing populates a reading here (no pool
  // directory, or a backend with no adapter), so holding would stop the relay
  // from ever asking for work again. It stays inert and says so, once.
  return { state: 'unprovisioned', limits: [] };
}

let warnedQuotaGuardUnprovisioned = false;

function warnQuotaGuardUnprovisionedOnce() {
  if (warnedQuotaGuardUnprovisioned) return;
  warnedQuotaGuardUnprovisioned = true;
  console.warn(`Contributor quota guard is ${QUOTA_GUARD_MODE} but no reading source is configured ` +
    '(HIVE_CONTRIBUTOR_QUOTA_READING_FILE / HIVE_CONTRIBUTOR_QUOTA_READING_JSON are unset), ' +
    'so it cannot see quota and is not guarding anything this session.');
}

let warnedQuotaGuardAwaitingPublisher = false;

function warnQuotaGuardAwaitingPublisherOnce() {
  if (warnedQuotaGuardAwaitingPublisher) return;
  warnedQuotaGuardAwaitingPublisher = true;
  console.warn('Contributor quota guard: a publisher presence marker is fresh but no reading has been ' +
    'published for this pool yet — holding until the reading lands or the marker goes stale (kubestellar/hive#6987).');
}

// opts.baseReserveOnly evaluates against the window reserve alone, ignoring the
// per-complexity tier reserves. #6833 specifies the base window reserve for the
// `ready` hold; the tier reserves exist to judge a SPECIFIC task, and `ready`
// is not a task (kubestellar/hive#6951).
function evaluateContributorQuota(task, reading = readContributorQuotaReading(), opts = {}) {
  if (QUOTA_GUARD_MODE === 'off') return { admit: true };
  if (contributorQuotaStayPaused) return { admit: false, wait: true, reason: 'explicit_pause' };
  // Overrides and pool state come from the shared on-disk store when one is
  // configured (kubestellar/hive#6953). An explicit pause-until-reset override
  // outranks a fresh reading, exactly like the in-process stay-paused above:
  // someone told this pool to stand down and a recovered reading is not consent
  // to resume.
  const override = readQuotaOverride();
  if (quotaPauseOverrideActive(override, reading)) {
    return { admit: false, wait: true, reason: 'override_pause' };
  }
  const state = (reading && reading.state) || 'unknown';
  if (state === 'unprovisioned') {
    warnQuotaGuardUnprovisionedOnce();
    return { admit: true, reason: 'unprovisioned' };
  }
  if (state === 'unknown' || state === 'stale') return { admit: false, wait: true, reason: state };
  const complexity = normalizeTaskComplexity(task);
  for (const window of (reading.limits || [])) {
    const kind = (window.kind || '').toString();
    // Every window is evaluated, including kinds this build has never heard of
    // (kubestellar/hive#6951). Skipping unrecognized kinds meant an EXHAUSTED
    // window admitted work purely because the provider had renamed it or added
    // a new one — `weekly_scoped` was itself a late addition to Claude's
    // reporting, so new kinds demonstrably do arrive. An unknown kind falls
    // back to the default reserve, so a HEALTHY unknown window still admits and
    // this cannot cause spurious pauses.
    const remaining = Number(window.pct_remaining ?? window.remaining_pct);
    if (!Number.isFinite(remaining)) return { admit: false, wait: true, reason: 'unknown' };
    const required = opts.baseReserveOnly ? quotaWindowReserve(kind) : quotaRequiredReserve(window, complexity);
    if (remaining <= required) {
      // A scoped continue-* override is a human's explicit consent to spend
      // below the reserve on THIS window. It admits, but records the scope it
      // relied on so the caller can expire a continue-once after exactly one
      // assignment -- it must never silently become a standing spend authority.
      if (quotaOverrideApplies(override, window, reading)) {
        contributorQuotaPaused = false;
        return { admit: true, reason: `override_${override.directive}`, override: override.directive, window_id: window.id || kind, remaining_pct: remaining, required_reserve_pct: required, complexity };
      }
      // Same refusal either way -- only the label differs, so an operator can
      // read "your quota is low" apart from "the provider showed us a window
      // kind this build has never seen" (kubestellar/hive#6951). The second
      // is the case worth noticing: it means the reserve overrides for that
      // window fell back to the base percentage, and it is how a kind like
      // `weekly_scoped` announces itself the first time.
      const reason = quotaWindowKindIsKnown(kind) ? 'guarded' : 'guarded_unknown_window';
      return { admit: false, wait: true, reason, window_id: window.id || kind, window_kind: kind, remaining_pct: remaining, required_reserve_pct: required, complexity, reset_epoch: quotaWindowResetEpoch(window) };
    }
  }
  // Reading is healthy, but a peer relay on the SAME pool may already hold a
  // reservation against it (kubestellar/hive#6953). Admitting anyway is the
  // per-process oversubscription #6833 forbids: two relays each keeping their
  // own reserve against one account. Hold until the peer releases; the retry
  // timer re-checks. A continue-* override does NOT bypass this — it is consent
  // to spend the contributor's OWN reserve, not to double-book a peer's.
  if (!opts.ignorePoolReservation && quotaPoolHasPeerReservation()) {
    return { admit: false, wait: true, reason: 'pool_reserved' };
  }
  contributorQuotaPaused = false;
  return { admit: true };
}

// ── Guard resume (kubestellar/hive#6951) ─────────────────────────────────────
//
// Every `ready` send in this relay is event-driven — a task completing, the CLI
// recovering. None of those fire while the guard is holding, because the hold
// is precisely the state of having no task and asking for none. #6541's
// provider hold arms a timer to release itself for exactly this reason; the
// #6833 guard armed nothing, so once it suppressed a `ready` between tasks the
// relay never asked for work again for the life of the process.
let contributorQuotaRetryTimer = null;

function armContributorQuotaRetry() {
  if (contributorQuotaRetryTimer) return;
  contributorQuotaRetryTimer = setTimeout(() => {
    contributorQuotaRetryTimer = null;
    retryContributorQuota();
  }, QUOTA_GUARD_RETRY_MS);
  // Must not hold the process open on its own, same as the #6541 hold timer.
  if (typeof contributorQuotaRetryTimer.unref === 'function') contributorQuotaRetryTimer.unref();
}

function retryContributorQuota() {
  if (!contributorQuotaPaused) return;
  // An explicit contributor pause outranks a fresh reading: someone asked for
  // this host to stand down, and a quota recovery is not consent to resume.
  if (contributorQuotaStayPaused) { armContributorQuotaRetry(); return; }
  const decision = evaluateContributorQuota(null, readContributorQuotaReading(), { baseReserveOnly: true });
  if (!decision.admit) { armContributorQuotaRetry(); return; }
  contributorQuotaPaused = false;
  console.log('Contributor quota guard released — a fresh reading clears every effective reserve. Asking for work again.');
  if (!currentTask && !cliReadyFailed && !quotaHoldActive()) {
    sendTo(hubs[activeHubIndex], { type: 'ready', seq: nextSeq() });
  }
}

function logContributorQuotaDecision(task, decision) {
  contributorQuotaPaused = true;
  // Nothing else will restart the loop once `ready` is being suppressed
  // (kubestellar/hive#6951).
  armContributorQuotaRetry();
  // Publish what's holding to the shared pool so a detached control command
  // (`just contribute-quota …`) can pin an until-reset override to the EXACT
  // window/epoch, and target this session for a disable-session opt-out
  // (kubestellar/hive#6953). Carries no account id, credential, or billing
  // figure — only window id, reset epoch, and the two percentages.
  if (quotaPoolActive()) {
    try {
      quotaPoolStore.writeStatus(quotaPool, {
        backend: BACKEND,
        session_id: QUOTA_SESSION_ID,
        reason: decision.reason,
        window_id: decision.window_id || null,
        reset_epoch: decision.reset_epoch ?? null,
        remaining_pct: decision.remaining_pct ?? null,
        required_reserve_pct: decision.required_reserve_pct ?? null,
      });
    } catch (_) {}
  }
  console.warn('');
  console.warn('┌─ CONTRIBUTOR QUOTA GUARD ─────────────────────────────────');
  console.warn(`│ ${BACKEND} is not accepting new work: ${decision.reason}`);
  if (decision.window_id) console.warn(`│ Window ${decision.window_id}: ${decision.remaining_pct}% remaining; reserve is ${decision.required_reserve_pct}%.`);
  if (decision.reason === 'guarded_unknown_window') {
    console.warn(`│ Kind ${JSON.stringify(decision.window_kind)} is not one this build recognizes, so the base`);
    console.warn('│ reserve applied and any short/weekly override did not. It is still a real limit.');
  }
  // Reset time is shown when a normalized window supplied one. #6833 wants it in
  // the banner; the adapter that populates it is a sibling issue, so this is
  // conditional rather than a hard-coded placeholder that would lie.
  if (Number.isFinite(Number(decision.reset_epoch))) {
    console.warn(`│ Window resets at ${new Date(Number(decision.reset_epoch)).toISOString()}.`);
  }
  if (task) console.warn(`│ Pending task: ${task.title || task.task_id || 'unknown'} (${normalizeTaskComplexity(task)}).`);
  console.warn('│ No new work will start while paused. Continuing may consume paid credits if your provider has them enabled.');
  // `ask` advertises the four scoped controls #6833 asks for and the reliable
  // command path that reaches this relay even detached. `pause` is the same
  // safe hold WITHOUT offering an override, which is what makes the two modes
  // distinguishable at last (kubestellar/hive#6953). No TTY and no response is
  // already the safe default: the relay simply stays held.
  if (QUOTA_GUARD_MODE === 'ask') {
    console.warn('│ Scoped controls (safe default is to stay paused until this window resets):');
    console.warn('│   just contribute-quota continue-once          — admit one task, then re-check');
    console.warn('│   just contribute-quota continue-until-reset   — admit until this window resets');
    console.warn('│   just contribute-quota disable-session        — turn the guard off for this session');
    console.warn('│   just contribute-quota pause-until-reset      — stay paused even if quota recovers');
    console.warn('│   just contribute-quota resume                 — clear an override');
    if (!quotaPoolActive()) {
      console.warn('│   (set HIVE_CONTRIBUTOR_QUOTA_POOL_DIR to enable the command path for a detached relay)');
    }
  }
  console.warn('│ Set HIVE_CONTRIBUTOR_QUOTA_GUARD=off at launch to opt out for this session.');
  console.warn('└────────────────────────────────────────────────────────────');
  console.warn('');
}

// ── Provider quota hold (kubestellar/hive#6541) ──────────────────────────────
//
// When the provider refuses on quota, the relay used to fail the task and go
// straight back to `ready`. The next assignment hit the same wall seconds
// later, and the cycle repeated for the whole reset window — the reporter
// measured two provider rejections 45 seconds apart, with distinct provider
// error IDs, so each one genuinely cost a round-trip, a hub assignment slot,
// and a hive issue marked failed. Over the 4h42m window agy stated, that is a
// steady drip of failures for a condition nothing on this host caused and
// nothing on this host could fix.
//
// Quota is a property of the provider ACCOUNT, not of the task, and it EXPIRES.
// So: stop asking for work, and come back when the provider says to.
//
// QUOTA_HOLD_FALLBACK_MS is used when the banner states no expiry (most
// backends do not print one). Short enough that a wrongly-held relay costs
// minutes rather than an evening, and the hold re-arms on the next refusal if
// the quota is genuinely still out.
const QUOTA_HOLD_FALLBACK_MS = RELAY_TEST_TIMING ? 50 : 15 * 60 * 1000;
// Hard ceiling on a PARSED window. The duration comes off a provider banner —
// text this relay does not control and cannot validate — so a malformed or
// absurd "Resets in 999h" must not wedge a contributor out of the fleet.
// Whatever the banner claims, the relay re-probes by this point at the latest;
// if the quota really is still out, the next refusal re-arms the hold.
const QUOTA_HOLD_MAX_MS = RELAY_TEST_TIMING ? 200 : 6 * 60 * 60 * 1000;
// Providers round their own countdown down, and the relay's clock is not
// theirs. Asking one second after the stated reset invites an immediate second
// refusal and another full hold; a small grace makes the first re-ask count.
const QUOTA_HOLD_GRACE_MS = RELAY_TEST_TIMING ? 10 : 30 * 1000;

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
const RELAY_PROTOCOL_VERSION = '1.4';

// RELAY_CAPABILITIES is this relay's OUTBOUND capability set — the mirror of the
// hub's server_capabilities (kubestellar/hive#6954). It is DECLARED to the hub in
// auth_response so the hub gates on what the relay ADVERTISES, not on a
// protocol-version proxy. quota_preflight_v1 is listed because this relay
// genuinely implements the preflight: the task_assign handler runs
// evaluateContributorQuota and answers an offered task with a
// local_capacity_guard task_declined BEFORE the scoped credential is delivered
// (#6833). The list is additive and optional — an older hub ignores the unknown
// relay_capabilities field and treats this relay exactly as one that declared
// nothing, and a relay that dropped a token would take the hub's legacy
// immediate-delivery path rather than break. Adding a NEW optional field is not
// a wire-contract change under the additive-versioning rule (see
// contributorProtocolVersion), so RELAY_PROTOCOL_VERSION is deliberately NOT
// bumped here and stays in step with the hub, keeping
// TestRelayProtocolVersionMatchesHub honest.
const RELAY_CAPABILITIES = ['quota_preflight_v1', 'run-stage'];

const KNOWLEDGE_AGENT_MD = process.env.HIVE_AGENT_MD || path.join(process.env.HOME || require('os').homedir() || process.cwd(), 'agent.md');
const KNOWLEDGE_STATE_POLL_MS = Number(process.env.HIVE_KNOWLEDGE_STATE_POLL_MS || (RELAY_TEST_TIMING ? 200 : 30000));
const KNOWLEDGE_ERROR_MAX = 500;

function boundKnowledgeError(reason) {
  reason = String(reason || '').replace(/\s+/g, ' ').trim();
  return reason.length > KNOWLEDGE_ERROR_MAX ? `${reason.slice(0, KNOWLEDGE_ERROR_MAX)}…` : reason;
}

function knowledgeExportLooksValid(file) {
  let text;
  try {
    const st = fs.statSync(file);
    if (!st.isFile() || st.size <= 0) return { ok: false, reason: `${file} is absent or empty` };
    text = fs.readFileSync(file, 'utf8');
  } catch (e) {
    return { ok: false, reason: `${file} is not readable: ${e.message}` };
  }
  const lines = text.split(/\r?\n/);
  if (lines[0] !== '# Agent Knowledge') return { ok: false, reason: `${file} is not a knowledge export (expected first line "# Agent Knowledge")` };
  if (!lines.includes('This file is auto-generated from the hive knowledge base.')) {
    return { ok: false, reason: `${file} is not a knowledge export (missing generated marker)` };
  }
  return { ok: true, reason: '' };
}

function currentKnowledgeState() {
  const valid = knowledgeExportLooksValid(KNOWLEDGE_AGENT_MD);
  return {
    knowledge_loaded: !!valid.ok,
    knowledge_error: valid.ok ? '' : boundKnowledgeError(valid.reason),
  };
}

function knowledgeStateChanged(a, b) {
  return !a || !b || a.knowledge_loaded !== b.knowledge_loaded || a.knowledge_error !== b.knowledge_error;
}

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
function hubWsURL(url) {
  return String(url || '').trim().replace(/\/contribute\/?$/, '/api/contribute/ws');
}

function hubPublicURL(url) {
  return String(url || '').trim().replace(/\/api\/contribute\/ws\/?$/, '/contribute');
}

function makeHub(url, token) {
  return {
    url: hubWsURL(url),
    sourceURL: hubPublicURL(url),
    regToken: token,
    ws: null,
    reconnectDelay: BASE_RECONNECT_DELAY_MS,
    heartbeatInterval: null,
    lastPong: Date.now(),
    lastPingSentAt: 0,
    connectionId: '',
    connectGeneration: 0,
    reconnectTimer: null,
    authenticated: false,
    authFailed: false,
    // #2547: set once we have reported a contributor-protocol difference with
    // this hub, so a reconnect loop does not repeat the same advisory line.
    protocolDriftReported: false,
    serverCapabilities: [],
    contributeNeedsDecisionLabel: DEFAULT_NEEDS_DECISION_LABEL,
    announcementSeen: new Set(),
    // #7732: true from the moment a `ready` is actually transmitted to this hub
    // until the hub answers it (task_assign or task_unavailable), or the
    // conversation it belonged to ends (socket close, re-auth). While it is
    // set, this relay has ALREADY asked for work and must not ask again: the
    // hub reads a second `ready` as "give back whatever you were just assigned"
    // (#2545). Maintained in sendTo() and the answer handlers, never at a
    // `ready` call site — see armCLIReadyWait for the one reader.
    readyOutstanding: false,
    // #7932: the largest frame this hub will read, less headroom. Replaced on
    // auth_ok by whatever the hub advertises; the default is the 64 KiB every
    // hub released before that advertisement enforces silently.
    maxFrameBytes: WS_FRAME_BYTES,
  };
}

const hubs = rawHubList.map((url, i) => makeHub(url, rawTokenList[i] || rawTokenList[0]));
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
let verdictWatchInterval = null;
let verdictFastPathLine = null;
let tokenExpiresAt = null;
// tokenRefreshFailedAt records when the hub last told us a mid-task re-mint
// FAILED (a token_refresh_failed, kubestellar/hive#5447). Null means "no known
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
// past its advertised expiry (kubestellar/hive#5447).
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

// ── Quota hold state (kubestellar/hive#6541) ─────────────────────────────────
// quotaHoldUntil is the epoch ms at which this relay may ask for work again; 0
// means not held. quotaHoldReason keeps the provider's own banner line so the
// release log can say what it was waiting on.
let quotaHoldUntil = 0;
let quotaHoldReason = '';
let quotaHoldTimer = null;

function quotaHoldActive() {
  return quotaHoldUntil > Date.now();
}

function formatQuotaHoldRemaining(ms) {
  const total = Math.max(0, Math.round(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  return h > 0 ? `${h}h${m}m${s}s` : (m > 0 ? `${m}m${s}s` : `${s}s`);
}

// enterQuotaHold parks the relay until the provider's stated reset.
//
// hint is paneQuotaExhaustion()'s { line, resetMs }. A banner with no parseable
// expiry still gets a hold — the fallback window — because "we know the account
// is out and we know nothing about when" is still a reason not to ask for the
// next task immediately.
//
// SAID ONCE, AND SAID LOUDLY. Before this, the banner lived only inside the agy
// pane while the relay log said `[environment]`; an operator reading the log had
// no way to learn their provider quota was gone for the next four hours, or that
// switching model/backend was the remedy. This is the only place that
// information surfaces outside the pane.
function enterQuotaHold(hint) {
  const stated = hint && Number.isFinite(hint.resetMs) && hint.resetMs > 0 ? hint.resetMs : null;
  const holdMs = Math.min(
    stated !== null ? stated + QUOTA_HOLD_GRACE_MS : QUOTA_HOLD_FALLBACK_MS,
    QUOTA_HOLD_MAX_MS
  );
  const until = Date.now() + holdMs;
  // Never SHORTEN a live hold: a second banner arriving mid-hold (a raced tick,
  // or a task that slipped through) restates the same exhaustion, and taking
  // the smaller window would walk the release time backwards on every repeat.
  if (until <= quotaHoldUntil) return;

  quotaHoldUntil = until;
  quotaHoldReason = (hint && hint.line) || 'provider quota exhausted';
  if (quotaHoldTimer) clearTimeout(quotaHoldTimer);

  console.warn('');
  console.warn('┌─ PROVIDER QUOTA EXHAUSTED ─────────────────────────────────');
  console.warn(`│ ${quotaHoldReason}`);
  console.warn(`│ This is the provider refusing ${BACKEND}, not a fault of this machine.`);
  console.warn(`│ Not asking the hub for work for ${formatQuotaHoldRemaining(holdMs)}` +
    `${stated === null ? ' (the banner stated no reset time — will re-probe)' : ' (the reset the provider stated)'}.`);
  console.warn('│ To work sooner: switch AGENT_MODEL/AGENT_BACKEND, or upgrade the plan.');
  console.warn('└────────────────────────────────────────────────────────────');
  console.warn('');

  quotaHoldTimer = setTimeout(() => {
    quotaHoldTimer = null;
    releaseQuotaHold('the provider reset window has passed');
  }, holdMs);
  // A hold outliving the work it bounds must not keep the process alive on its
  // own — every other timer here is cleared on a task exit, and this one has no
  // task to hang off.
  if (typeof quotaHoldTimer.unref === 'function') quotaHoldTimer.unref();
}

// releaseQuotaHold clears the hold and re-advertises. The explicit `ready` is
// the whole point: `ready` is suppressed while held (see sendTo), so nothing
// else would restart the loop — the hub has heard nothing from this contributor
// since the hold began and is not going to offer work unprompted.
function releaseQuotaHold(why) {
  if (!quotaHoldUntil) return;
  const was = quotaHoldReason;
  quotaHoldUntil = 0;
  quotaHoldReason = '';
  if (quotaHoldTimer) { clearTimeout(quotaHoldTimer); quotaHoldTimer = null; }
  console.log(`Provider quota hold released — ${why}. Asking for work again (was: ${was})`);
  if (!currentTask && !cliReadyFailed) {
    sendTo(hubs[activeHubIndex], { type: 'ready', seq: nextSeq() });
  }
}

// ── Login hold state (hivecommons/hive#7996) ─────────────────────────────────
// A contributor whose Claude login had expired ran all night: the CLI answered
// every task prompt with "Login expired · Please run /login" and returned to
// its prompt, the relay read the prompt as ready, and each of nine assignments
// died on the 30-minute watchdog as "no observed progress". The pane state
// machine already classifies that pane BLOCKED_ON_HUMAN (#5088) so a present
// human can rescue the task by logging in — but with nobody attached, blocked
// is a 30-minute wait for the watchdog, and the relaunch after the failure
// draws a fresh splash that re-latches ready for the next one.
//
// The hold is the quota hold's shape (#6541): once a login wall has sat
// unattended for LOGIN_WALL_GRACE_TICKS progress ticks, the relay hands the
// task back at once as an `environment` failure naming the login, stops
// advertising `ready` at the send choke point, declines pushed assignments,
// and does NOT relaunch the CLI (the pane holding "Please run /login" is the
// evidence a human who attaches needs). It probes the pane on a timer and
// releases the hold when the wall is gone and the CLI shows a signed-in
// state — the CLI's own login-success line, or a ready pane with a human at
// it — then re-advertises.
const LOGIN_WALL_GRACE_TICKS = Math.max(1, Number(process.env.HIVE_LOGIN_WALL_GRACE_TICKS) || 3);
const LOGIN_HOLD_PROBE_MS = RELAY_TEST_TIMING ? 100 : 10000;
// What the CLIs print once a sign-in completes. Claude: "Login successful" /
// "Logged in as <account>"; copilot: "Logged in as"; gemini: "Authenticated".
const LOGIN_SUCCESS_PATTERN = /Login successful|Logged in as|Successfully logged in|Successfully authenticated|You are now logged in/i;
let loginHoldReason = '';
let loginHoldSince = 0;
let loginHoldTimer = null;
let loginWallTicks = 0;

function loginHoldActive() {
  return loginHoldReason !== '';
}

// loginWallLine returns the line of the pane tail that carries the login
// wall, for the hold reason and the failure report.
function loginWallLine(text) {
  const lines = String(text || '').split('\n');
  for (let i = lines.length - 1; i >= 0; i--) {
    const lower = lines[i].toLowerCase();
    if (lower.includes('please run /login') || lower.includes('use /login') || (lower.includes('api error:') && /\b401\b/.test(lower))) {
      return lines[i].trim();
    }
  }
  return 'the CLI is asking for /login';
}

function enterLoginHold(line) {
  if (loginHoldActive()) return;
  loginHoldReason = line || 'the CLI is asking for /login';
  loginHoldSince = Date.now();
  loginWallTicks = 0;

  console.warn('');
  console.warn('┌─ CLI LOGIN EXPIRED ────────────────────────────────────────');
  console.warn(`│ ${loginHoldReason}`);
  console.warn(`│ ${BACKEND} is up but refuses every prompt until someone signs in again.`);
  console.warn('│ Not asking the hub for work until the pane shows a signed-in CLI.');
  for (const l of loginBannerLines(BACKEND, ATTACH_COMMAND)) console.warn(`│ ${l}`);
  console.warn('└────────────────────────────────────────────────────────────');
  console.warn('');

  if (loginHoldTimer) clearInterval(loginHoldTimer);
  loginHoldTimer = setInterval(probeLoginHold, LOGIN_HOLD_PROBE_MS);
  if (typeof loginHoldTimer.unref === 'function') loginHoldTimer.unref();
}

// probeLoginHold reads the pane and releases the hold once the wall is gone
// AND the CLI shows a signed-in state. Both halves are required: the wall
// scrolling off, or a relaunch drawing a fresh splash, must not count as a
// login — a fresh claude splash is exactly what fooled the readiness latch
// in the incident.
function probeLoginHold() {
  if (!loginHoldActive()) return false;
  let text = '';
  try { text = capturePaneText(); } catch (_) { return false; }
  if (paneShowsLoginRequiredError(text)) return false;
  const signedIn = LOGIN_SUCCESS_PATTERN.test(text) ||
    (paneHasPresentHuman() && classifyReadiness(text, BACKEND) === 'ready');
  if (!signedIn) return false;
  releaseLoginHold('the pane shows a signed-in CLI');
  return true;
}

// releaseLoginHold clears the hold and re-advertises. As with the quota hold,
// the explicit `ready` is the point: it was suppressed for the whole hold and
// the hub is not going to offer work unprompted.
function releaseLoginHold(why) {
  if (!loginHoldActive()) return;
  const was = loginHoldReason;
  loginHoldReason = '';
  loginHoldSince = 0;
  loginWallTicks = 0;
  if (loginHoldTimer) { clearInterval(loginHoldTimer); loginHoldTimer = null; }
  console.log(`CLI login hold released — ${why}. Asking for work again (was: ${was})`);
  cliReady = true;
  cliReadyFailed = false;
  if (!currentTask && !quotaHoldActive()) {
    sendTo(hubs[activeHubIndex], { type: 'ready', seq: nextSeq() });
  }
}

// ---------------------------------------------------------------------------
// #7932 — frame-size clamping. Everything below is pure: it takes a frame and
// a byte budget and returns a frame that fits, leaving the frame untouched
// (same object identity) when it already did.
// ---------------------------------------------------------------------------

function utf8Bytes(text) {
  return Buffer.byteLength(String(text), 'utf8');
}

function frameByteLength(msg) {
  return utf8Bytes(JSON.stringify(msg));
}

// hubFrameBytes is the budget for one frame to THIS hub: what it advertised on
// auth_ok, less the headroom, or the long-standing 64 KiB default for a hub
// that advertises nothing.
function hubFrameBytes(hub) {
  return (hub && hub.maxFrameBytes) || WS_FRAME_BYTES;
}

// truncateTextTail keeps the LAST maxBytes of a string. A split multi-byte
// character decodes to U+FFFD, exactly as it does in createBoundedOutputCapture
// — the tail is for a human to read, not to parse.
function truncateTextTail(text, maxBytes) {
  const buf = Buffer.from(String(text), 'utf8');
  if (buf.length <= maxBytes) return buf.toString('utf8');
  if (maxBytes <= 0) return '';
  return buf.subarray(buf.length - maxBytes).toString('utf8');
}

// truncateTextHead keeps the FIRST maxBytes of a string, marking the cut. Used
// for summaries and failure reasons, where the opening words are the ones that
// say what happened; the tail-keeping form above is for captured output, where
// the last thing printed is the interesting one.
function truncateTextHead(text, maxBytes) {
  const buf = Buffer.from(String(text), 'utf8');
  if (buf.length <= maxBytes) return String(text);
  const room = maxBytes - utf8Bytes(TEXT_TRUNCATED_SUFFIX);
  if (room <= 0) return '';
  return buf.subarray(0, room).toString('utf8') + TEXT_TRUNCATED_SUFFIX;
}

function tailArrayBytes(lines) {
  // +1 per line for the newline a reader will put back between them.
  return lines.reduce((total, line) => total + utf8Bytes(line) + 1, 0);
}

// truncateTailLines bounds an output-tail array to maxBytes, keeping the LAST
// lines and marking the cut. Returns the input array itself when it already
// fits, so a frame that never needed clamping is byte-identical to the one the
// relay sent before this existed.
function truncateTailLines(lines, maxBytes) {
  if (!Array.isArray(lines)) return lines;
  if (tailArrayBytes(lines) <= maxBytes) return lines;
  const budget = maxBytes - utf8Bytes(OUTPUT_TAIL_TRUNCATED_MARKER) - 1;
  if (budget <= 0) return [];
  const kept = [];
  let used = 0;
  for (let i = lines.length - 1; i >= 0; i--) {
    const line = String(lines[i]);
    const cost = utf8Bytes(line) + 1;
    if (used + cost > budget) {
      // A SINGLE line can be larger than the whole budget — that is the pi
      // case this fix exists for, one tool_execution_end event with a diff
      // inside it. Keep that line's tail rather than reporting nothing at all.
      if (!kept.length) {
        const room = budget - used;
        if (room > 0) kept.unshift(truncateTextTail(line, room));
      }
      break;
    }
    kept.unshift(line);
    used += cost;
  }
  return [OUTPUT_TAIL_TRUNCATED_MARKER].concat(kept);
}

// frameFieldBytes is the serialized size of one field's value.
function frameFieldBytes(msg, field) {
  return Buffer.byteLength(JSON.stringify(msg[field]), 'utf8');
}

function truncateFrameField(value, maxBytes) {
  return Array.isArray(value)
    ? truncateTailLines(value, Math.max(maxBytes, 0))
    : truncateTextHead(value, Math.max(maxBytes, 0));
}

// clampFrame returns a frame that fits maxFrameBytes, shrinking only the
// payload fields in FRAME_TRUNCATABLE_FIELDS. Two separate bounds, because they
// answer different questions: tmux_output is held to OUTPUT_TAIL_MAX_BYTES on
// EVERY frame (how much audit tail is worth sending), and the whole frame is
// held to maxFrameBytes (what the hub will accept at all).
function clampFrame(msg, maxFrameBytes) {
  if (!msg || typeof msg !== 'object') return msg;
  let clamped = msg;
  if (Array.isArray(msg.tmux_output)) {
    const bounded = truncateTailLines(msg.tmux_output, OUTPUT_TAIL_MAX_BYTES);
    if (bounded !== msg.tmux_output) clamped = { ...clamped, tmux_output: bounded };
  }
  if (frameByteLength(clamped) <= maxFrameBytes) return clamped;
  // Largest field first. In allowlist order a small tail and a normal summary
  // were emptied to make room for a huge later field (`room` went negative
  // with that field still inside `rest`), and the frame STILL did not fit
  // until the loop reached the culprit. Shrinking the biggest field first
  // fits the frame in one step and leaves the fields that already fit alone.
  const bySize = FRAME_TRUNCATABLE_FIELDS
    .filter((field) => clamped[field] !== undefined && clamped[field] !== null)
    .sort((a, b) => frameFieldBytes(clamped, b) - frameFieldBytes(clamped, a));
  for (const field of bySize) {
    const value = clamped[field];
    const rest = { ...clamped };
    delete rest[field];
    // What is left once the rest of the frame and this field's own JSON wrapper
    // (`,"<field>":""`) are paid for. Measured against the SERIALIZED frame
    // afterwards rather than trusted: JSON escaping expands a byte of raw
    // output into as many as six, so the first estimate can still be over.
    let room = maxFrameBytes - frameByteLength(rest) - field.length - 8;
    for (let attempt = 0; attempt < 8; attempt++) {
      clamped = { ...clamped, [field]: truncateFrameField(value, room) };
      if (frameByteLength(clamped) <= maxFrameBytes) break;
      if (room <= 0) break;
      room = Math.floor(room / 2);
    }
    if (frameByteLength(clamped) <= maxFrameBytes) return clamped;
  }
  return clamped;
}

function sendTo(hub, msg) {
  // #5715: a local-only task (the synthetic pr-review cycle) has no
  // server-issued lease, so an ownership frame naming it can only ever be
  // answered with a revoke — which the relay used to treat as terminal,
  // aborting the review. Withhold those frames here, at the single point every
  // one of them passes through, rather than at each call site: the bug was
  // introduced by a call site that did not know the distinction existed, and a
  // new one would reintroduce it. See LOCAL_TASK_ID_PREFIX for the full
  // rationale. Everything else about the task is unchanged — it still runs,
  // still ticks locally, and still reports task_complete/ready when it ends.
  if (msg && HUB_OWNERSHIP_FRAMES.has(msg.type) && isLocalOnlyTaskId(msg.task_id)) return;
  // #6541: while the provider has refused this account on quota, do not ask for
  // work. Enforced HERE, at the one point every frame passes through, rather
  // than at each of the eight `ready` call sites — a guard per call site is the
  // shape that lets the next call site reintroduce the bug, and the whole
  // failure being fixed is a `ready` that should not have been sent. Only
  // `ready` is withheld: progress, completion and failure frames for work
  // already in flight must still reach the hub.
  if (msg && msg.type === 'ready' && quotaHoldActive()) return;
  // #7996: same choke point for an expired CLI login — see enterLoginHold.
  if (msg && msg.type === 'ready' && loginHoldActive()) return;
  if (msg && msg.type === 'ready') {
    // Base window reserve, not the simple tier (kubestellar/hive#6951): #6833
    // specifies the base reserve for the `ready` hold, and `ready` is not a
    // task to size a tier reserve against.
    const quotaDecision = evaluateContributorQuota(null, readContributorQuotaReading(), { baseReserveOnly: true });
    if (!quotaDecision.admit) {
      logContributorQuotaDecision(null, quotaDecision);
      return;
    }
  }
  if (hub && hub.ws && hub.ws.readyState === WebSocket.OPEN) {
    // #7932: a frame the hub cannot read is a frame that never arrives — it is
    // answered with a 1009 close, not an error the relay can see. Bound it HERE,
    // at the one point every frame passes through, rather than at each call site
    // that builds a tail: the oversized frame was built by a call site that had
    // no idea a limit existed, and a guard per call site is the shape that lets
    // the next one reintroduce it.
    const budget = hubFrameBytes(hub);
    const framed = clampFrame(msg, budget);
    const payload = JSON.stringify(framed);
    if (framed !== msg) {
      console.warn(`Trimmed the ${msg.type} frame for ${hub.url || 'the hub'} to ${utf8Bytes(payload)} bytes (limit ${budget}) — the captured output it carries is shortened, the task result is not`);
    }
    if (utf8Bytes(payload) > budget) {
      // Unreachable while the protocol fields themselves are small, which is
      // every frame this relay builds. Say so loudly rather than let the hub
      // answer it with a close nobody can attribute.
      console.error(`Frame ${msg.type} is still ${utf8Bytes(payload)} bytes after trimming (limit ${budget}) — the hub may close the connection with code 1009`);
    }
    hub.ws.send(payload);
    // #7732: recorded only for a frame that actually left, so a `ready`
    // dropped on a closed socket does not look like an open question.
    if (msg && msg.type === 'ready') hub.readyOutstanding = true;
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

let lastKnowledgeState = null;
function knowledgeStateFrame(state) {
  return {
    type: 'knowledge_state',
    seq: nextSeq(),
    knowledge_loaded: state.knowledge_loaded,
    knowledge_error: state.knowledge_error || undefined,
  };
}

function broadcastKnowledgeStateIfChanged() {
  const state = currentKnowledgeState();
  if (!knowledgeStateChanged(lastKnowledgeState, state)) return;
  lastKnowledgeState = state;
  for (const hub of hubs) {
    if (hub && hub.authenticated) sendTo(hub, knowledgeStateFrame(state));
  }
}

let knowledgeStateTimer = setInterval(broadcastKnowledgeStateIfChanged, KNOWLEDGE_STATE_POLL_MS);
if (typeof knowledgeStateTimer.unref === 'function') knowledgeStateTimer.unref();

function currentTaskHub() {
  return (currentTask && currentTask._hub) || hubs[activeHubIndex];
}

function hubSupportsQuotaPreflight(hub) {
  return !!(hub && Array.isArray(hub.serverCapabilities) && hub.serverCapabilities.includes('quota_preflight_v1'));
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

function parseContributorEnvFile(file) {
  const out = {};
  const text = fs.readFileSync(file, 'utf8');
  for (const rawLine of text.split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line || line.startsWith('#')) continue;
    const idx = line.indexOf('=');
    if (idx <= 0) continue;
    out[line.slice(0, idx)] = line.slice(idx + 1);
  }
  return out;
}

function hubListFromEnv(env) {
  const hubList = String(env.HIVE_HUB || '').split(',').map(s => s.trim()).filter(Boolean);
  const tokenList = String(env.HIVE_REGISTRATION_TOKEN || '').split(',').map(s => s.trim()).filter(Boolean);
  if (hubList.length === 0) throw new Error(`HIVE_HUB is empty in ${CONTRIBUTOR_ENV_FILE}`);
  if (tokenList.length === 0) throw new Error(`HIVE_REGISTRATION_TOKEN is empty in ${CONTRIBUTOR_ENV_FILE}`);
  if (hubList.length > 1 && tokenList.length !== hubList.length) {
    throw new Error(`HIVE_HUB lists ${hubList.length} hub(s) but HIVE_REGISTRATION_TOKEN lists ${tokenList.length} token(s)`);
  }
  return hubList.map((url, i) => ({ url, token: tokenList[i] || tokenList[0] }));
}

function stopHub(hub, reason) {
  if (!hub) return;
  console.log(`Disconnecting from ${hub.url}${reason ? ` (${reason})` : ''}`);
  if (hub.reconnectTimer) { clearTimeout(hub.reconnectTimer); hub.reconnectTimer = null; }
  if (hub.heartbeatInterval) { clearInterval(hub.heartbeatInterval); hub.heartbeatInterval = null; }
  if (hub.ws) {
    try { hub.ws.removeAllListeners(); hub.ws.close(1000, 'profile switch'); } catch (_) {
      try { hub.ws.terminate(); } catch (_) {}
    }
    hub.ws = null;
  }
  hub.authenticated = false;
  hub.readyOutstanding = false;
  hub.connectGeneration++;
}

function maybeAskActiveHubForWork() {
  const hub = hubs[activeHubIndex];
  if (!hub || currentTask || !hub.authenticated || hub.readyOutstanding) return;
  if (cliReadyFailed) {
    console.log('Active hive switched, but CLI readiness previously failed — withholding ready until the CLI recovers');
    return;
  }
  if (CONTRIBUTOR_MODE === MODE_HEADLESS || cliReady) {
    sendTo(hub, { type: 'ready', seq: nextSeq() });
  } else {
    console.log('Active hive switched, but CLI is not ready yet — withholding ready until the CLI reaches its prompt');
  }
}

function reloadHubsFromProjection() {
  const entries = hubListFromEnv(parseContributorEnvFile(CONTRIBUTOR_ENV_FILE));
  const oldByURL = new Map(hubs.map(h => [h.url, h]));
  const next = entries.map(entry => {
    const url = hubWsURL(entry.url);
    const existing = oldByURL.get(url);
    if (existing) {
      existing.sourceURL = hubPublicURL(entry.url);
      if (existing.regToken !== entry.token) {
        existing.regToken = entry.token;
        stopHub(existing, 'registration token changed');
        connectHub(existing);
      }
      oldByURL.delete(url);
      return existing;
    }
    const hub = makeHub(entry.url, entry.token);
    connectHub(hub);
    return hub;
  });
  for (const removed of oldByURL.values()) stopHub(removed, 'removed from contributor.env');
  hubs.splice(0, hubs.length, ...next);
  activeHubIndex = 0;
  console.log(`Reloaded ${hubs.length} hive profile(s) from ${CONTRIBUTOR_ENV_FILE}; active hive is ${hubs[0] ? hubs[0].sourceURL : '(none)'}`);
  maybeAskActiveHubForWork();
  return hubs.length;
}

function handleProfileSwitchSignal() {
  try {
    reloadHubsFromProjection();
  } catch (e) {
    console.error(`Failed to reload hive profiles from ${CONTRIBUTOR_ENV_FILE}: ${e.message}`);
  }
}

function writeRelayPidFile() {
  try {
    fs.mkdirSync(path.dirname(RELAY_PID_FILE), { recursive: true, mode: 0o700 });
    const record = {
      pid: process.pid,
      started_at: new Date().toISOString(),
      env_file: CONTRIBUTOR_ENV_FILE,
      container_runtime: (process.env.HIVE_CONTAINER_RUNTIME || '').trim() || undefined,
      container_name: (process.env.HIVE_CONTAINER_NAME || '').trim() || undefined,
    };
    fs.writeFileSync(RELAY_PID_FILE, JSON.stringify(record, null, 2) + '\n', { mode: 0o600 });
  } catch (e) {
    console.error(`WARNING: could not write relay pid file ${RELAY_PID_FILE}: ${e.message}`);
  }
}

function removeRelayPidFile() {
  try { fs.unlinkSync(RELAY_PID_FILE); } catch (_) {}
}

function writeHubsSeenNow() {
  hubsSeenWriteTimer = null;
  if (!hubsSeenDirty) return;
  const seen = {};
  for (const hub of hubs) {
    if (hub.lastSeenAt) seen[hub.sourceURL || hubPublicURL(hub.url)] = hub.lastSeenAt;
  }
  try {
    fs.mkdirSync(path.dirname(HUBS_SEEN_FILE), { recursive: true, mode: 0o700 });
    fs.writeFileSync(HUBS_SEEN_FILE, JSON.stringify(seen, null, 2) + '\n', { mode: 0o600 });
    hubsSeenDirty = false;
    lastHubsSeenWrite = Date.now();
  } catch (e) {
    console.error(`WARNING: could not write hub last-seen file ${HUBS_SEEN_FILE}: ${e.message}`);
  }
}

function recordHubSeen(hub, now = Date.now()) {
  if (!hub) return;
  hub.lastSeenAt = new Date(now).toISOString();
  hubsSeenDirty = true;
  const wait = Math.max(0, HUBS_SEEN_MIN_WRITE_MS - (now - lastHubsSeenWrite));
  if (wait === 0) {
    writeHubsSeenNow();
  } else if (!hubsSeenWriteTimer) {
    hubsSeenWriteTimer = setTimeout(writeHubsSeenNow, wait);
    if (typeof hubsSeenWriteTimer.unref === 'function') hubsSeenWriteTimer.unref();
  }
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
// worse than no command at all (kubestellar/hive#5145).
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
    // Outbound negotiated capability set: names the features this relay
    // implements so the hub gates on the advertised token, not a version proxy
    // (kubestellar/hive#6954). Copied so a caller cannot mutate the constant.
    relay_capabilities: RELAY_CAPABILITIES.slice(),
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


// sanitizeHubText neutralizes hub-supplied text before it reaches this
// terminal. Frames arrive from every configured hub — including third-party
// hives (multi-hub, #2846) — so their free-text fields are untrusted input:
// raw ANSI/OSC escape sequences in a title, reason, or announcement could
// rewrite the screen, retitle the terminal, or write the clipboard (OSC 52).
// Strips C0 controls, DEL, and C1 controls (U+0080–U+009F: a bare 0x9b is a
// one-byte CSI), collapses runs of whitespace, and bounds the length by code
// point. Line-oriented cousin of sanitizeDeclaredValue, which guards the
// other direction (local CLI output leaving for the hub).
const HUB_TEXT_MAX_LEN = 2000;
function sanitizeHubText(raw) {
  if (typeof raw !== 'string') return '';
  const clean = raw
    .replace(/[\x00-\x1f\x7f\u0080-\u009f]/g, ' ')
    .replace(/\s+/g, ' ')
    .trim();
  const points = Array.from(clean);
  return points.length > HUB_TEXT_MAX_LEN
    ? points.slice(0, HUB_TEXT_MAX_LEN).join('').trim()
    : clean;
}

function formatHubAnnouncementLine(hub, ann) {
  const text = ann && typeof ann.text === 'string' ? sanitizeHubText(ann.text) : '';
  if (!ann || !ann.id || !text) return '';
  const label = hub && (hub.sourceURL || hub.url) ? hubPublicURL(hub.sourceURL || hub.url) : 'hub';
  return `${label}: ${text}`;
}

function printHubAnnouncementOnce(hub, ann) {
  if (!hub || !ann || !ann.id) return false;
  if (!hub.announcementSeen) hub.announcementSeen = new Set();
  if (hub.announcementSeen.has(ann.id)) return false;
  const line = formatHubAnnouncementLine(hub, ann);
  if (!line) return false;
  hub.announcementSeen.add(ann.id);
  const decorated = process.stdout && process.stdout.isTTY && !process.env.NO_COLOR
    ? `[7m${line}[0m`
    : line;
  console.log(decorated);
  return true;
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
// The entrypoint may export AGENT_LAUNCH_CMD with the exact command used for
// the FIRST launch. Prefer it so local mode keeps its sandbox/allowlist posture
// across restarts instead of rebuilding the more-permissive container default
// (#5652). Older entrypoints fall back to resolving backend flags here.
let cachedLaunchCommand = null;
let cachedBackendResolution = null;
let cachedShellBackendResolution = null;

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

// resolveBackendShell returns the same backend binary plus permission flags
// escaped for a shell command line. The interactive tmux launcher types the
// resulting text into a shell, unlike headless execFile which needs raw argv.
function resolveBackendShell() {
  if (cachedShellBackendResolution) return cachedShellBackendResolution;
  const raw = resolveBackend();
  const confPaths = ['/usr/local/etc/hive/backends.conf', path.join(process.cwd(), 'config/backends.conf')];
  const confPath = confPaths.find(p => fs.existsSync(p)) || confPaths[0];
  let perm = raw.perm;
  try {
    perm = execSync(`bash -c 'source ${confPath} 2>/dev/null; backend_perm_flag_shell ${BACKEND}'`, { encoding: 'utf8', timeout: 15000 }).trim();
  } catch (e) {
    console.error(`Could not resolve shell backend flags from ${confPath}: ${e.message}`);
  }
  cachedShellBackendResolution = { cmd: raw.cmd, perm };
  return cachedShellBackendResolution;
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
// Muse Code's own accepted effort values (`--reasoning-effort
// none|minimal|low|medium|high|xhigh|max|ultra`, default high). muse exits 2
// on anything else, so an unrecognised contributor value is dropped rather
// than turned into a launch that cannot start.
const MUSE_EFFORTS = ['none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra'];
// Claude Code's `--effort` levels (claude --help, v2.1.273+), mirroring
// config.ReasoningEffortsByBackend["claude"] hub-side (hivecommons/hive#8377).
// Like muse, a value outside the set is dropped rather than passed — the CLI
// would refuse the flag and the task would die at argv parsing — and unset
// leaves Claude Code at its own default. Only the plain `claude` backend gets
// the flag; inference routes (litellm) that drive the same binary do not.
const CLAUDE_EFFORTS = ['low', 'medium', 'high', 'xhigh', 'max'];

function effectiveReasoningEffort() {
  // agy is the only backend whose effort is conditional on a model being passed.
  if (BACKEND === 'agy') return modelFlagFor() ? agyEffort : '';
  // muse applies effort with or without a model, but only for values it takes.
  if (BACKEND === 'muse') return MUSE_EFFORTS.includes(REASONING_EFFORT) ? REASONING_EFFORT : '';
  // claude likewise: --effort with or without --model, only for its own levels.
  if (BACKEND === 'claude') return CLAUDE_EFFORTS.includes(REASONING_EFFORT) ? REASONING_EFFORT : '';
  // omp takes its effort from its own config (the `:level` suffix on the model
  // selection), read by detectOmpSelection; the env var still wins when set,
  // the same precedence effectiveModel() applies (#7760).
  return REASONING_EFFORT || detectedEffort || '';
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

// --- omp: primary model + effort, and the advisor's (hivecommons/hive#7760) ---
//
// omp chooses its models from its own config rather than from a flag, so an
// omp contributor almost never exports AGENT_MODEL — the env var would be a
// second, drift-prone copy — and showed up everywhere in hive as `omp` with
// `model: null`. With `--advisor` two models did the work and hive named
// neither. omp records everything needed locally and machine-readably:
//
//   ~/.omp/agent/config.yml        modelRoles: { default: <sel>, advisor: <sel> }
//                                  advisor: { enabled: true }
//   ~/.omp/agent/sessions/<slug>/<ts>_<id>.jsonl
//                                  first record {"type":"model_change","model":
//                                  "openai-codex/gpt-5.6-terra", ...} — the
//                                  primary, in the shape the claude/copilot
//                                  detectors read
//   ~/.omp/agent/sessions/<slug>/<ts>_<id>/__advisor.jsonl
//                                  the advisor's sidecar session; its assistant
//                                  records carry {"provider": ..., "model": ...}
//
// A selection is spelled `provider/model[:effort]`; the suffix is omp's
// thinking level. The primary comes from the newest session's model_change
// (the model ACTUALLY running, so a mid-task /model switch is reflected) with
// config.yml's modelRoles.default as the fallback; its effort comes from the
// config spelling, since the session record carries none. The advisor comes
// from config.yml's modelRoles.advisor when the advisor is enabled, or from a
// __advisor.jsonl sidecar next to the newest session when one exists. Both are
// reported as `provider/model` — provider travels inside the model, as for pi —
// with the effort split off into its own field. Anything not found degrades
// to '' and is omitted from the wire, so a bare omp with no advisor renders
// exactly as a single-model backend does.
const OMP_AGENT_DIR = process.env.HIVE_OMP_AGENT_DIR || process.env.PI_CODING_AGENT_DIR || path.join(MODEL_DETECT_HOME, '.omp', 'agent');
// The thinking levels omp accepts as a `:suffix`; pi's plus the wider codex
// and muse vocabularies. Only one of these is split off as the effort, so an
// Ollama-style tag (`llama3:8b`) or a revision stays part of the model name.
const OMP_EFFORT_LEVELS = ['off', 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra'];

// splitOmpSelection turns `provider/model[:effort]` into { model, effort },
// keeping the provider inside model. Returns empty fields for junk.
function splitOmpSelection(raw) {
  if (typeof raw !== 'string') return { model: '', effort: '' };
  let value = raw.trim().replace(/^["']|["']$/g, '');
  if (!value || /\s/.test(value)) return { model: '', effort: '' };
  let effort = '';
  const colon = value.lastIndexOf(':');
  if (colon > 0) {
    const suffix = value.slice(colon + 1).toLowerCase();
    if (OMP_EFFORT_LEVELS.includes(suffix)) {
      effort = suffix;
      value = value.slice(0, colon);
    }
  }
  return { model: looksLikeModelName(value) ? value : '', effort };
}

// parseOmpConfig reads the two things this relay needs from omp's config.yml
// with the same deliberately small YAML subset omp-backend.js uses: the scalar
// values under `modelRoles:` and the `enabled:` flag under `advisor:`.
// Anything else in the file is ignored; a missing file yields empty fields.
function parseOmpConfig(configFile) {
  const out = { defaultSelection: '', advisorSelection: '', advisorEnabled: null };
  let text;
  try { text = fs.readFileSync(configFile, 'utf8'); } catch (_) { return out; }
  let section = '';
  for (const rawLine of text.split('\n')) {
    const line = rawLine.replace(/\s+#.*$/, '').replace(/\r$/, '');
    if (line.trim() === '') continue;
    if (!/^\s/.test(line)) {
      section = /^modelRoles:\s*$/.test(line) ? 'modelRoles' : (/^advisor:\s*$/.test(line) ? 'advisor' : '');
      continue;
    }
    const m = /^\s+([A-Za-z0-9_-]+):\s*(.+?)\s*$/.exec(line);
    if (!m) continue;
    const value = m[2].replace(/^["']|["']$/g, '');
    if (section === 'modelRoles' && m[1] === 'default') out.defaultSelection = value;
    else if (section === 'modelRoles' && m[1] === 'advisor') out.advisorSelection = value;
    else if (section === 'advisor' && m[1] === 'enabled') out.advisorEnabled = /^(true|yes|on)$/i.test(value);
  }
  return out;
}

// ompSessionFiles lists the transcript files under ~/.omp/agent/sessions/*/,
// newest-first candidates for newestByMtime. The advisor sidecar lives in a
// directory named after its session (`<ts>_<id>/__advisor.jsonl`) and is
// deliberately not a candidate here: it would otherwise win the mtime race and
// report the advisor as the primary.
function ompSessionFiles() {
  const sessionsDir = path.join(OMP_AGENT_DIR, 'sessions');
  const files = [];
  for (const d of fs.readdirSync(sessionsDir, { withFileTypes: true })) {
    if (!d.isDirectory()) continue;
    const dir = path.join(sessionsDir, d.name);
    for (const f of fs.readdirSync(dir)) {
      if (f.endsWith('.jsonl') && !f.startsWith('__')) files.push(path.join(dir, f));
    }
  }
  return files;
}

// ompProviderCredentialState reads the credential store omp runs against —
// in container mode the staged copy (#7678), locally the host's own — and
// says whether `provider` has a credential omp will use (hivecommons/hive
// #7922). omp reads only rows whose disabled_cause IS NULL: a row it disabled
// after a failed refresh ("oauth refresh failed: … invalid_grant …", the
// mark a revoked single-use refresh token leaves) is invisible to it, and
// omp then quietly resolves some other provider's model — the host's ollama
// entries, in the incident — while the config still names the original.
//   usable    at least one row for the provider that omp will read
//   disabled  the provider has rows and omp has disabled every one of them
//   absent    no row at all (an API key in the environment may still serve)
//   unknown   no node:sqlite, no store, an old schema, or a read error
// Only `disabled` is definitive evidence that the configured model cannot
// run, and it is the only state anything acts on.
function ompProviderCredentialState(provider) {
  const unknown = { state: 'unknown', cause: '' };
  let sqlite;
  try { sqlite = require('node:sqlite'); } catch (_) { return unknown; }
  const dbFile = path.join(OMP_AGENT_DIR, 'agent.db');
  if (!fs.existsSync(dbFile)) return unknown;
  let rows;
  try {
    const db = new sqlite.DatabaseSync(dbFile, { readOnly: true });
    try {
      const columns = db.prepare('PRAGMA table_info(auth_credentials)').all().map((c) => String(c.name));
      if (!columns.includes('disabled_cause')) return unknown;
      rows = db.prepare('SELECT disabled_cause FROM auth_credentials WHERE lower(provider) = ?').all(provider.toLowerCase());
    } finally { db.close(); }
  } catch (_) { return unknown; }
  if (rows.length === 0) return { state: 'absent', cause: '' };
  if (rows.some((r) => r.disabled_cause === null || r.disabled_cause === undefined)) return { state: 'usable', cause: '' };
  // The cause is printed to the relay log, which the host tails: in container
  // mode the store is the container's writable copy, so strip control
  // characters (terminal escapes included) and cap it, as for any declared
  // value — just with room for the provider's whole error message.
  // eslint-disable-next-line no-control-regex
  const cause = Array.from(String(rows[0].disabled_cause).replace(/[\x00-\x1f\x7f-\x9f]/g, ' ').replace(/\s+/g, ' ').trim());
  return { state: 'disabled', cause: cause.length > 400 ? `${cause.slice(0, 400).join('')}…` : cause.join('') };
}

// ompConfiguredProviderBlocked returns { provider, model, cause } when the
// model omp is configured to run — `selection` if given, else AGENT_MODEL
// when it names a provider, else config.yml's modelRoles.default — belongs
// to a provider whose stored credential omp has disabled (#7922), and null
// otherwise. This is the check that keeps the relay from advertising `ready`
// (and the configured model) for an omp that will actually answer with
// whatever it fell back to.
function ompConfiguredProviderBlocked(selection) {
  if (selection === undefined) {
    const fromEnv = splitOmpSelection(MODEL).model;
    selection = fromEnv.includes('/') ? fromEnv : splitOmpSelection(parseOmpConfig(path.join(OMP_AGENT_DIR, 'config.yml')).defaultSelection).model;
  }
  const slash = typeof selection === 'string' ? selection.indexOf('/') : -1;
  if (slash <= 0) return null;
  const provider = selection.slice(0, slash);
  const cred = ompProviderCredentialState(provider);
  if (cred.state !== 'disabled') return null;
  return { provider, model: selection, cause: cred.cause };
}

// detectOmpSelection returns { model, effort, advisorModel, advisorEffort,
// source }, each '' when not found. `source` says where the primary came
// from: 'transcript' (a session's model_change record — what omp actually
// resolved), 'config' (config.yml's default, which is only what omp WILL
// resolve if that provider's credential works), or ''. Never throws: every
// read is best-effort, and a missing sessions directory simply means "not
// running yet", which the config.yml fallback still answers.
//
// A configured model whose provider credential omp has disabled (#7922) is
// NOT reported: omp writes no session until its first turn, so before then
// the config was the only source, and the hub was told `claude-sonnet-5`
// for a container whose omp had silently fallen back to a local 30B model.
// Better no model than a confidently wrong one, as for looksLikeModelName.
function detectOmpSelection() {
  const config = parseOmpConfig(path.join(OMP_AGENT_DIR, 'config.yml'));
  const configured = splitOmpSelection(config.defaultSelection);
  const out = { model: configured.model, effort: configured.effort, advisorModel: '', advisorEffort: '', source: configured.model ? 'config' : '' };

  let newest = null;
  try { newest = newestByMtime(ompSessionFiles()); } catch (_) {}
  if (newest) {
    // The model_change record is the session's FIRST line, so the newest
    // record wins on a tail read only when the session switched models
    // late; read the whole tail newest-first exactly like the other detectors.
    try {
      for (const obj of tailLinesReversed(newest)) {
        if (obj && obj.type === 'model_change' && looksLikeModelName(obj.model)) {
          const running = splitOmpSelection(obj.model);
          if (running.model) {
            out.model = running.model;
            out.source = 'transcript';
            // A session record carries the model but not the level; keep the
            // configured effort only when it was configured for this model.
            out.effort = running.effort || (configured.model === running.model ? configured.effort : '');
          }
          break;
        }
      }
    } catch (_) {}
  }
  if (out.source === 'config' && ompConfiguredProviderBlocked(out.model)) {
    out.model = '';
    out.effort = '';
    out.source = '';
  }

  const advisorFromConfig = splitOmpSelection(config.advisorSelection);
  let sidecar = null;
  if (newest) {
    const candidate = path.join(newest.slice(0, -'.jsonl'.length), '__advisor.jsonl');
    try { if (fs.statSync(candidate).isFile()) sidecar = candidate; } catch (_) {}
  }
  if (config.advisorEnabled !== false && (config.advisorEnabled === true || sidecar)) {
    out.advisorModel = advisorFromConfig.model;
    out.advisorEffort = advisorFromConfig.effort;
    if (sidecar) {
      try {
        for (const obj of tailLinesReversed(sidecar)) {
          if (obj && looksLikeModelName(obj.model)) {
            const provider = typeof obj.provider === 'string' && obj.provider && !obj.model.includes('/') ? `${obj.provider}/` : '';
            out.advisorModel = `${provider}${obj.model}`;
            if (advisorFromConfig.model !== out.advisorModel) out.advisorEffort = '';
            break;
          }
        }
      } catch (_) {}
    }
  }
  return out;
}

const MODEL_DETECTORS = { claude: detectClaudeModel, copilot: detectCopilotModel, bob: detectBobModel, omp: () => detectOmpSelection().model };

// Backends whose transcript yields more than a model name: the effort in
// effect and a second, reviewing model (#7760). Read on the same schedule as
// MODEL_DETECTORS — at auth and on every progress tick — and reported through
// effectiveReasoningEffort() / advisorFields() below.
const SELECTION_DETECTORS = { omp: detectOmpSelection };

// The last model detected from the transcript. Refreshed at auth and on every
// progress tick, so a mid-session `/model` switch is reflected within one
// PROGRESS_REPORT_INTERVAL_MS.
let detectedModel = '';
// The rest of a SELECTION_DETECTORS reading (#7760): the effort in effect when
// the backend carries it in its own config rather than a flag, and the second
// model that reviewed the work. All '' for backends without a selection
// detector, which leaves every wire field exactly as before.
let detectedEffort = '';
let detectedAdvisorModel = '';
let detectedAdvisorEffort = '';

// detectRunningModel reads the transcript once and returns the model, or ''.
// Never throws; never runs at all when AGENT_MODEL is set (explicit intent
// wins, so there is nothing to detect) or the backend has no known transcript.
function detectRunningModel() {
  if (MODEL) return '';
  const detector = MODEL_DETECTORS[BACKEND];
  if (!detector) return '';
  try { return sanitizeDeclaredValue(detector() || ''); } catch (_) { return ''; }
}

// detectRunningSelection is detectRunningModel's richer sibling for the
// backends in SELECTION_DETECTORS: one read of the transcript and config
// yields the model, its effort and the advisor pair, each sanitized the same
// way a detected model is. Null for every other backend, and on any error.
function detectRunningSelection() {
  const detector = SELECTION_DETECTORS[BACKEND];
  if (!detector) return null;
  try {
    const sel = detector() || {};
    return {
      model: sanitizeDeclaredValue(sel.model || ''),
      effort: sanitizeDeclaredValue(sel.effort || ''),
      advisorModel: sanitizeDeclaredValue(sel.advisorModel || ''),
      advisorEffort: sanitizeDeclaredValue(sel.advisorEffort || ''),
      source: sel.source === 'transcript' || sel.source === 'config' ? sel.source : '',
    };
  } catch (_) { return null; }
}

// refreshDetectedModel re-detects and returns the model currently in effect
// under the fixed precedence (AGENT_MODEL → detected → ''). For a backend with
// a selection detector the same read also refreshes the detected effort and
// the advisor pair (#7760), so a mid-task change to any of them reaches the
// hub on the next progress tick along with the model.
function refreshDetectedModel() {
  const sel = detectRunningSelection();
  // AGENT_MODEL wins over detection for the primary exactly as before; the
  // advisor has no env var, so it is always what the CLI reports.
  const m = sel ? (MODEL ? '' : sel.model) : detectRunningModel();
  if (m && m !== detectedModel) {
    detectedModel = m;
    // Say where the value came from (#7922): a config.yml default is what omp
    // is SET to run, not evidence of what it resolved — the transcript is.
    const from = sel && sel.source === 'config'
      ? `${BACKEND} config.yml (no session transcript yet)`
      : `${BACKEND} session transcript`;
    console.log(`Detected running model from ${from}: ${m}`);
  }
  if (sel) {
    detectedEffort = sel.effort;
    if (sel.advisorModel !== detectedAdvisorModel || sel.advisorEffort !== detectedAdvisorEffort) {
      detectedAdvisorModel = sel.advisorModel;
      detectedAdvisorEffort = sel.advisorEffort;
      if (detectedAdvisorModel) {
        console.log(`Detected ${BACKEND} advisor model: ${detectedAdvisorModel}${detectedAdvisorEffort ? ` (${detectedAdvisorEffort})` : ''}`);
      }
    }
  }
  return effectiveModel();
}

// advisorFields returns the optional advisor pair (#7760) for the auth frame
// and for progress reports: the second model that reviewed this work and the
// effort it ran at. Omitted entirely when there is none — an older hub, or a
// backend with no advisor, sees no new field.
function advisorFields() {
  const out = {};
  if (detectedAdvisorModel) out.advisor_model = detectedAdvisorModel;
  if (detectedAdvisorEffort) out.advisor_reasoning_effort = detectedAdvisorEffort;
  return out;
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
  Object.assign(out, advisorFields());
  return out;
}

function buildLaunchCommand() {
  if (cachedLaunchCommand) return cachedLaunchCommand;
  if (ENTRYPOINT_LAUNCH_CMD) {
    cachedLaunchCommand = ENTRYPOINT_LAUNCH_CMD;
    return cachedLaunchCommand;
  }
  const { cmd, perm } = resolveBackendShell();
  const modelFlag = modelFlagFor();
  const reasoningFlag = BACKEND === 'codex' && REASONING_EFFORT
    ? `-c 'model_reasoning_effort="${REASONING_EFFORT}"'`
    : '';
  // Paired with modelFlag, never on its own: agy without --model needs no
  // --effort, and passing one alone would be a flag agy has no model to apply.
  const agyEffortFlag = BACKEND === 'agy' && modelFlag ? `--effort ${effectiveReasoningEffort()}` : '';
  // muse takes effort on its own, independent of --model (unlike agy), and
  // validates the value itself. Only values muse actually accepts are passed,
  // so effectiveReasoningEffort() never advertises an effort muse rejected.
  const museEffortFlag = BACKEND === 'muse' && effectiveReasoningEffort()
    ? `--reasoning-effort ${effectiveReasoningEffort()}`
    : '';
  // claude: `--effort <v>` only when a claude-valid effort is set (#8377).
  const claudeEffortFlag = BACKEND === 'claude' && effectiveReasoningEffort()
    ? `--effort ${effectiveReasoningEffort()}`
    : '';
  cachedLaunchCommand = [cmd, perm, modelFlag, reasoningFlag, agyEffortFlag, museEffortFlag, claudeEffortFlag].filter(Boolean).join(' ');
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
  // (kubestellar/hive#4970). Unlike agy, opencode is the ONLY launch mode
  // this backend gets: there is no interactive-tmux wiring for it (see the
  // getCLIState()/classifyTmuxPane() backend lists below, which opencode
  // deliberately does not join), so it is only ever reached through
  // CONTRIBUTOR_MODE=headless. `opencode run` exits with a real status code
  // on completion, the exit-code contract runHeadlessTask() relies on.
  opencode: { flag: 'run' },
  // Kilo is OpenCode-derived but uses distinct credentials and config.
  kilo: { flag: 'run' },
  // muse exec "<prompt>" — Muse Code's documented non-interactive
  // sub-command ("Run one prompt non-interactively (headless)"). Verified
  // against Muse Code 1.0.3 (1.0.3-R2198.1): a completed run exits 0, a run
  // with no usable credential exits 1 ("missing meta credentials: run `muse
  // login` or set META_API_KEY..."), and a bad flag/value exits 2 — the
  // exit-code contract runHeadlessTask() relies on. muse DOES have an
  // interactive TUI, but hive has no interactive-tmux wiring for it (it
  // deliberately does not join the getCLIState()/classifyTmuxPane() backend
  // lists below), so headless is the only launch mode hive gives it.
  //
  // flagsAfterCommand: muse is a SUB-COMMAND-FIRST CLI. Its options are parsed
  // by `muse exec` itself, not by the `muse` root, so the usual
  // "<perm flags> <one-shot token> <prompt>" order silently breaks: `muse
  // --approval-mode never exec "<prompt>"` prints the root help, exits 0, and
  // never runs the task — a no-op that would look like a passing run. Verified
  // against 1.0.3: flags must follow `exec`.
  muse: { flag: 'exec', flagsAfterCommand: true },
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
  const museEffortArgs = BACKEND === 'muse' && effectiveReasoningEffort()
    ? ['--reasoning-effort', effectiveReasoningEffort()]
    : [];
  // Same claude-valid-only rule as the interactive launch (#8377).
  const claudeEffortArgs = BACKEND === 'claude' && effectiveReasoningEffort()
    ? ['--effort', effectiveReasoningEffort()]
    : [];
  const flagArgs = [...permArgs, ...modelArgs, ...reasoningArgs, ...agyEffortArgs, ...museEffortArgs, ...claudeEffortArgs];
  // Sub-command-first CLIs parse their options on the sub-command, not the
  // root binary, so the one-shot token has to lead (see flagsAfterCommand).
  const args = spec.flagsAfterCommand
    ? [...oneShotArgs, ...flagArgs, prompt]
    : [...flagArgs, ...oneShotArgs, prompt];
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
function createBoundedOutputCapture(maxBytes) {
  let buffer = Buffer.alloc(0);
  let truncated = false;
  return {
    append(chunk) {
      if (!chunk || maxBytes <= 0) return;
      const next = Buffer.concat([buffer, Buffer.isBuffer(chunk) ? chunk : Buffer.from(String(chunk))]);
      if (next.length > maxBytes) {
        truncated = true;
        buffer = next.subarray(next.length - maxBytes);
      } else {
        buffer = next;
      }
    },
    text() { return buffer.toString('utf8'); },
    truncated() { return truncated; },
  };
}

// headlessProgressFrame is the task_progress a headless run sends — once when
// the child starts, and then on every progress tick while it is alive (#7778).
// Nothing is scraped: a live child IS the progress signal in this mode, exactly
// as the hub's lease model needs ("still reporting" means "still alive").
function headlessProgressFrame(task) {
  return { type: 'task_progress', seq: nextSeq(), task_id: task.task_id, task_gen: task.task_gen, kind: task.kind, repo: task.repo, number: task.number, title: task.title, status: 'working', ...effectiveSelectionFields() };
}

// headlessProgressTick is the headless analogue of progressTick(), armed on the
// same progressInterval handle and on the same PROGRESS_REPORT_INTERVAL_MS
// cadence so the hub's 30-minute progress lease is renewed for a headless task
// the way it is for an interactive one (hivecommons/hive#7778). Every task-exit
// path already clears progressInterval, so a tick can only run while the relay
// believes the task is live; the guards below make it a no-op if the child has
// gone or the assignment has changed hands, so a stale timer can never renew a
// lease for work that is not happening.
function headlessProgressTick(task) {
  if (!currentTask || currentTask.task_id !== task.task_id || currentTask.task_gen !== task.task_gen) return;
  if (!headlessChild || headlessChild.killed) return;
  send(headlessProgressFrame(task));
}

// resolveTaskPrompt is the one place a task's prompt text is produced for the
// agent, on both dispatch paths (hivecommons/hive#7908).
//
// The hub names the checkout as `$HIVE_WORKSPACE_DIR/<owner>/<repo>` because
// it cannot know the path — it differs per contributor (contributor-agent.sh
// defaults it to ~/workspace; local mode differs again). Inside the shell
// commands the prompt quotes that is fine. But the agent reads it as THE PATH
// of the repo and hands the same string to its CLI's non-shell file tools,
// which do not expand shell variables: omp began 3 of 5 sessions in one night
// with `Path not found: $HIVE_WORKSPACE_DIR/...`, and the advisor spent a
// concern explaining it. Every task is a fresh CLI session, so the agent
// cannot learn it once. The relay is the process that types the prompt and
// the one that knows the answer (TASK_WORKSPACE_DIR), so it substitutes the
// literal path before the agent ever sees the variable. The quoted shell
// commands work identically with a literal path.
const WORKSPACE_DIR_VARIABLE = /\$\{?HIVE_WORKSPACE_DIR\}?/g;
function resolveTaskPrompt(task) {
  const prompt = task.prompt || `Work on ${task.kind} ${task.repo}#${task.number}: ${task.title}`;
  return prompt.replace(WORKSPACE_DIR_VARIABLE, TASK_WORKSPACE_DIR);
}

// installRepoToolchain runs bin/repo-toolchain.sh against the task's checkout
// — when one already exists under $HIVE_WORKSPACE_DIR — and calls `then` once
// it has finished, so a repository's declared `.hive/tools` pip requirements
// are in the container BEFORE the prompt is typed (hivecommons/hive#7925).
// The first task on a repo has no checkout yet (the agent clones it), so that
// task runs without the extras and every later one gets them; the script
// itself never fails a task, and neither does anything here: a missing
// script, a spawn error or the time box all fall through to `then`.
const REPO_TOOLCHAIN_SCRIPT = path.join(__dirname, 'repo-toolchain.sh');
const REPO_TOOLCHAIN_MANIFEST = path.join('.hive', 'tools');
const REPO_TOOLCHAIN_TIMEOUT_MS = Number(process.env.HIVE_REPO_TOOLCHAIN_TIMEOUT_MS || 180000);

function installRepoToolchain(task, then) {
  const dir = taskCheckoutDir(task && task.repo);
  if (!dir || !fs.existsSync(path.join(dir, REPO_TOOLCHAIN_MANIFEST))) { then(); return; }
  let done = false;
  const finish = () => { if (done) return; done = true; then(); };
  console.log(`Installing ${task.repo}'s declared toolchain (${REPO_TOOLCHAIN_MANIFEST}) into the container before the task prompt (#7925)`);
  let child;
  try {
    child = spawn('bash', [REPO_TOOLCHAIN_SCRIPT, dir], { stdio: ['ignore', 'pipe', 'pipe'] });
  } catch (e) {
    console.error(`repo-toolchain could not start: ${e.message} — continuing without the declared tools`);
    finish();
    return;
  }
  let output = '';
  child.stdout.on('data', (d) => { output += d.toString(); });
  child.stderr.on('data', (d) => { output += d.toString(); });
  const timer = setTimeout(() => {
    console.error(`repo-toolchain did not finish within ${REPO_TOOLCHAIN_TIMEOUT_MS} ms — continuing without the declared tools`);
    try { child.kill('SIGKILL'); } catch (_) { /* already gone */ }
    finish();
  }, REPO_TOOLCHAIN_TIMEOUT_MS);
  child.on('error', (e) => {
    clearTimeout(timer);
    console.error(`repo-toolchain failed to run: ${e.message} — continuing without the declared tools`);
    finish();
  });
  child.on('close', (code) => {
    clearTimeout(timer);
    for (const line of output.split('\n')) if (line.trim()) console.log(`  ${line.trimEnd()}`);
    if (code !== 0) console.error(`repo-toolchain exited ${code} — continuing without the declared tools`);
    finish();
  });
}

function runHeadlessTask(task) {
  const prompt = resolveTaskPrompt(task);
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
  send(headlessProgressFrame(task));

  let settled = false;
  const finish = (fn) => { if (settled) return; settled = true; fn(); };

  const output = createBoundedOutputCapture(HEADLESS_MAX_OUTPUT_BYTES);
  let timedOut = false;
  let spawnError = null;
  const timeout = setTimeout(() => {
    timedOut = true;
    if (headlessChild && !headlessChild.killed) headlessChild.kill('SIGKILL');
  }, HEADLESS_TASK_TIMEOUT_MS);
  if (timeout.unref) timeout.unref();

  headlessChild = spawn(bin, args, {
    stdio: ['pipe', 'pipe', 'pipe'],
    cwd: TASK_WORKSPACE_DIR,
  });
  if (headlessChild.stdout) headlessChild.stdout.on('data', chunk => output.append(chunk));
  if (headlessChild.stderr) headlessChild.stderr.on('data', chunk => output.append(chunk));
  headlessChild.on('error', err => { spawnError = err; });
  // codex exec prints "Reading additional input from stdin..." and then blocks
  // on stdin-EOF even with the prompt already passed as an argv element; with
  // execFile's default piped stdio nothing ever closes that pipe, so a
  // headless codex task produced zero output and hung until the timeout
  // killed it (found live by bin/test_backend_smoke.sh). Close stdin for
  // every backend — a one-shot child has no interactive input coming.
  if (headlessChild.stdin) headlessChild.stdin.end();
  // #7778: keep the hub's progress lease alive for as long as the child is.
  // Without this the hub heard exactly one task_progress per headless task and
  // reclaimed it at wsTaskTimeout (30 min), two hours or more before the
  // ceiling above — killing a live run and cooling down its issue. Reuses the
  // interactive path's handle so every task-exit path (completion, failure,
  // revoke, shutdown) already stops it.
  if (progressInterval) clearInterval(progressInterval);
  progressInterval = setInterval(() => headlessProgressTick(task), PROGRESS_REPORT_INTERVAL_MS);
  headlessChild.on('close', (code, signal) => {
    clearTimeout(timeout);
    // The child is gone: stop renewing the hub's lease for it (#7778). Cleared
    // here rather than only in the exit paths below because the revoked-task
    // return just under this must not leave a timer running either.
    if (progressInterval) { clearInterval(progressInterval); progressInterval = null; }
    headlessChild = null;
    // Tokens can appear in agent output; redact before the tail leaves the host.
    const outText = output.truncated()
      ? `[relay: captured output truncated to the last ${HEADLESS_MAX_OUTPUT_BYTES} bytes]\n${output.text()}`
      : output.text();
    const outLines = redactTokens(outText).split('\n');
    const outTail = outLines.slice(-TMUX_TAIL_LINES);
    // #6667: headless has no TUI chrome, but a build log easily pushes a PR URL
    // past fifteen lines, so scan the same deep window the interactive path does.
    const outScan = outLines.slice(-PR_SCAN_LINES);
    // A revoke clears currentTask before killing the child. Ignore any callback
    // that arrives afterwards — including a raced exit 0 — so stale work cannot
    // emit completion after its assignment generation was fenced out.
    if (!currentTask || currentTask.task_id !== task.task_id || currentTask.task_gen !== task.task_gen) {
      writeHeadlessStatus(HEADLESS_STATE_WAITING, { revoked_task_id: task.task_id });
      return;
    }
    const wasTimedOut = timedOut || signal === 'SIGKILL';
    const failed = spawnError || wasTimedOut || code !== 0 || signal;
    if (failed) {
      // A non-zero exit, a spawn failure (ENOENT), or the timeout kill all land
      // here. err.killed && err.signal signals the timeout; report a real
      // failure either way so the hub can reassign — never a silent hang.
      // Preserve one bounded, token-redacted diagnostic line. In particular,
      // Codex automatic-review denial/timeout is an expected terminal outcome
      // for an unattended run and must reach Hive as an actionable failure,
      // rather than being flattened to an opaque exit code.
      const diagnostic = outTail.map(line => line.trim()).filter(Boolean).slice(-1)[0];
      const diagnosticSuffix = diagnostic ? `: ${diagnostic.slice(0, 500)}` : '';
      const reason = wasTimedOut
        ? `headless task exceeded ${HEADLESS_TASK_TIMEOUT_MS / 60000}min and was killed`
        : `headless CLI exited with error: ${code !== null && code !== undefined ? `code ${code}` : (spawnError ? spawnError.message : `signal ${signal}`)}${diagnosticSuffix}`;
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
      const prFinding = resolveTaskPR(outScan, {
        repo: task.repo,
        taskId: task.task_id,
        taskStartedAt: taskAssignedAt,
        contributorLogin: CONTRIBUTOR_LOGIN,
      });
      const prURL = prFinding.url;
      // #3987: only report a no_work_needed verdict when no PR was shipped —
      // a visible PR contradicts "nothing shippable" (the hub would override
      // the claim with "shipped" anyway). #6662: only a PR THIS TASK OPENED
      // does; see resolveTaskPR.
      const noWork = prFinding.suppressesVerdict ? null : detectNoWorkVerdict(outTail);
      if (noWork) console.log(`Detected ${noWork.verdict} verdict for ${task.task_id}: ${noWork.reason || '(no reason)'}`);
      writeHeadlessStatus(HEADLESS_STATE_DONE, { task_id: task.task_id, task_gen: task.task_gen, result: 'completed', pr_url: prURL });
      // #7924: the label goes on with the task credential, so it must be
      // applied BEFORE stopAgentForTaskExit drops it below.
      if (noWork && noWork.verdict === HIVE_VERDICT_BLOCKED) markIssueBlocked(task, noWork.reason);
      if (noWork && noWork.needsDecision) markIssueNeedsDecision(task, noWork.reason);
      if (isAlreadyDoneNoWork(noWork)) markIssueAlreadyDone(task, noWork.reason);
      // #5353: the one-shot child has already exited (this callback is its
      // exit), so there is no process to stop — but the task-scoped token it
      // was given stays valid for the rest of wsTokenTTL. Drop it with the
      // task, so a credential never outlives the assignment it belongs to.
      stopAgentForTaskExit();
      send({ type: 'task_complete', seq: nextSeq(), task_id: task.task_id, task_gen: task.task_gen, result: 'completed', summary: 'Headless one-shot invocation exited 0', tmux_output: outTail, pr_url: prURL, ...verdictWireFields(noWork), ...effectiveSelectionFields() });
      currentTask = null;
      releaseQuotaPoolReservation();
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
// #6776/#7733: pi and omp do NOT reliably terminate on C-c. After this
// sequence their process can still be the pane's foreground program, and
// `relaunchCLI()` would type its launch command into the live TUI as a chat
// prompt. Verify the pane has fallen back to a shell; if not, force the
// foreground process down with `tmux respawn-pane -k` before relaunching.
// Backends that exit cleanly still avoid respawn-pane because the shell check
// succeeds first.
//
// Best-effort by design: if tmux is unreachable the caller is already on a
// failure path, and a relaunch that lands badly is recovered by the
// armCLIReadyWait() contract rather than by anything here.
function quitLiveCLI() {
  try {
    execSync(`tmux send-keys -t ${TMUX_SESSION} C-c`, { timeout: TMUX_COMMAND_TIMEOUT_MS });
    sleepMs(LIVE_CLI_FIRST_INTERRUPT_DELAY_MS);
    execSync(`tmux send-keys -t ${TMUX_SESSION} C-c`, { timeout: TMUX_COMMAND_TIMEOUT_MS });
    sleepMs(LIVE_CLI_SECOND_INTERRUPT_DELAY_MS);

    // A backend that absorbed both interrupts is still the pane foreground
    // program. Relaunching now would type the shell launch line into the live
    // TUI as a prompt (#7733). Wait for the pane to become a shell, then
    // escalate to tmux respawn-pane -k if it does not.
    if (!waitForPaneShell(LIVE_CLI_SHELL_WAIT_TIMEOUT_MS)) {
      execSync(`tmux respawn-pane -k -t ${TMUX_SESSION}`, { timeout: TMUX_COMMAND_TIMEOUT_MS });
      sleepMs(LIVE_CLI_RESPAWN_SETTLE_MS);
      waitForPaneShell(LIVE_CLI_SHELL_WAIT_TIMEOUT_MS);
    }
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
      { encoding: 'utf8', timeout: TMUX_COMMAND_TIMEOUT_MS }
    ).toString().trim();
  } catch (_) {
    return '';
  }
}

function paneCommandIsShell(command) {
  return !!command && PANE_SHELL_COMMANDS.has(command);
}

function waitForPaneShell(timeoutMs) {
  const attempts = Math.max(1, Math.ceil(timeoutMs / LIVE_CLI_SHELL_WAIT_POLL_MS));
  for (let i = 0; i < attempts; i++) {
    if (paneCommandIsShell(paneForegroundCommand())) return true;
    if (i < attempts - 1) sleepMs(LIVE_CLI_SHELL_WAIT_POLL_MS);
  }
  return false;
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
  const isShell = paneCommandIsShell(fg);
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
  return paneCommandIsShell(fg);
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
// is on screen, or null meaning "a bare Enter is the right answer". Thin
// wrapper over the pure classifier (bin/lib/pane-classifier.js), bound to this
// relay's BACKEND.
function blockingPromptKey(text) {
  return classifyBlockingPromptKey(text, BACKEND);
}

// getCLIState captures the pane and hands it to the pure readiness classifier
// (classifyReadiness in bin/lib/pane-classifier.js). Kept as the one place
// that couples the CAPTURE (tmux) to the CLASSIFICATION (pure).
// The last disabled-credential cause the omp gate below logged, so a poll
// that finds the same one every CLI_READY_POLL_MS says it once.
let ompBlockedCauseLogged = '';

function getCLIState() {
  try {
    const text = capturePaneText();
    const state = classifyReadiness(text, BACKEND);
    // #7922: an omp whose configured provider credential is DISABLED draws
    // exactly the chrome a ready one does — it has already, silently, picked
    // some other provider's model (the incident: `qwen3-coder:30b` in the
    // footer, `claude-sonnet-5` reported to the hub). The pane cannot tell
    // the two apart; the credential store can. Withhold `ready`, as for a
    // login prompt: the remedy is the same sign-in, on the host in
    // container mode, and the login banner names it.
    if (state === 'ready' && BACKEND === 'omp') {
      const blocked = ompConfiguredProviderBlocked();
      if (blocked) {
        if (ompBlockedCauseLogged !== blocked.cause) {
          ompBlockedCauseLogged = blocked.cause;
          console.log(`omp is configured for ${blocked.model}, but the stored ${blocked.provider} credential is disabled: ${blocked.cause}`);
          console.log(`omp would run some other provider's model in its place without saying so, so this relay is not advertising ready. Sign in to ${blocked.provider} again where omp's credential store lives (on the host, for container mode: run omp, then /login) and restart.`);
        }
        return 'needs-login';
      }
    }
    return state;
  } catch (_) {
    return 'starting';
  }
}

// BACKEND_LOGIN_HELP says what a blocked pane actually needs, per backend.
//
// getCLIState() can return 'needs-login' for five backends -- claude, copilot,
// gemini, bob and agy -- and the banner below used to be one hardcoded block
// announcing "Claude Code needs authentication" and "Then type: /login" for all
// of them (#6437). Four fifths of the time that named the wrong product and
// gave an instruction that cannot work.
//
// bob is the clearest case and the reason this is a table rather than a
// substituted product name: its needs-login patterns are the Bob-Shell API-key
// prompt, and the fix is an environment variable the Justfile already fails
// fast on -- attaching to the pane and typing anything cannot resolve it. The
// remedy is per-backend, not just the name.
const BACKEND_LOGIN_HELP = {
  claude: {
    product: 'Claude Code',
    steps: (attach) => [
      'In another terminal, run:',
      `  ${attach}`,
      'Then type: /login',
      'Complete the login, then press Ctrl-B D to detach.',
    ],
  },
  copilot: {
    product: 'GitHub Copilot CLI',
    steps: (attach) => [
      'In another terminal, run:',
      `  ${attach}`,
      'Then run: copilot login   (or: gh auth login)',
      'Complete the login, then press Ctrl-B D to detach.',
    ],
  },
  gemini: {
    product: 'Gemini CLI',
    steps: (attach) => [
      'In another terminal, run:',
      `  ${attach}`,
      'Then complete the sign-in the CLI prompts for.',
      'When it is done, press Ctrl-B D to detach.',
    ],
  },
  agy: {
    product: 'Antigravity (agy)',
    steps: (attach) => [
      'In another terminal, run:',
      `  ${attach}`,
      'Then complete the Antigravity sign-in the CLI prompts for.',
      'When it is done, press Ctrl-B D to detach.',
    ],
  },
  omp: {
    product: 'Oh My Pi',
    // omp's setup wizard offers a paste flow ("Paste the authorization code
    // (or full redirect URL)"), so a browser on the HOST is enough to finish
    // a sign-in inside a container. What that sign-in cannot do is persist:
    // container mode writes to a throwaway copy of ~/.omp (#7678), so the
    // lasting fix is to sign in on the host once, where contribute-hive
    // stages it from.
    steps: (attach) => [
      'In another terminal, run:',
      `  ${attach}`,
      'Complete the provider sign-in OMP prompts for (open its login URL in a browser',
      'here and paste the code back into the pane), then press Ctrl-B D to detach.',
      'In container mode that sign-in lasts only for this run; to keep it, sign in',
      'on the host once (run: omp) and restart with just contribute-hive omp.',
    ],
  },
  bob: {
    product: 'bob (Bob-Shell)',
    // No attach step: bob takes an API key from the environment, so there is
    // nothing a human can type into the pane that fixes this.
    steps: () => [
      'bob authenticates with an API key, not an interactive login.',
      'Stop the relay, set the key, and start it again:',
      '  export BOBSHELL_API_KEY=<your-bob-api-key>',
      '  just contribute-hive bob',
    ],
    // The generic closing line would be a lie here: no login is coming, and the
    // readiness wait will simply expire. Say what will actually happen.
    waiting: 'Until then this relay will wait, and time out.',
  },
};

// loginBannerLines returns the banner's content lines for a backend. Pure, so
// the per-backend text is table-testable without driving waitForCLI().
function loginBannerLines(backend, attach) {
  const help = BACKEND_LOGIN_HELP[backend];
  if (!help) {
    // An unknown backend still gets an honest banner rather than another
    // backend's instructions.
    return [
      `The ${backend} CLI needs authentication.`,
      'In another terminal, run:',
      `  ${attach}`,
      'Complete the sign-in it prompts for, then press Ctrl-B D to detach.',
      'Waiting for login to complete...',
    ];
  }
  return [
    `${help.product} needs authentication.`,
    ...help.steps(attach),
    help.waiting || 'Waiting for login to complete...',
  ];
}

// renderBoxedBanner draws lines in a box sized to its content.
//
// The old block used a fixed-width box with hand-padded borders, and printed
// ATTACH_COMMAND on a line with no closing bar -- so any attach command longer
// than the box (every container-mode one, which carries a runtime, a container
// name and a session name) broke the border (#6437).
function renderBoxedBanner(lines) {
  const width = Math.max(...lines.map((l) => l.length));
  const rule = '\u2550'.repeat(width + 2);
  return [`\u2554${rule}\u2557`]
    .concat(lines.map((l) => `\u2551 ${l.padEnd(width)} \u2551`))
    .concat([`\u255a${rule}\u255d`]);
}

// ── The tmux session itself can disappear (hivecommons/hive#7863) ────────────
//
// capturePaneText() returns '' when `tmux capture-pane` fails for ANY reason,
// and the readiness classifier reads '' as `starting`. So when the whole tmux
// server was gone — an operator's attached client ended the pane's shell in
// the ~1 s window between the CLI exiting and the relaunch being typed — the
// relay saw a CLI that was forever "starting": it accepted a task, renewed its
// lease every tick for the full CLI_READY_TIMEOUT_MS (10 min), then failed it
// as `environment` and sat idle until a human restarted it. Nothing in the
// relay asked whether the session existed, and nothing could recreate it.
//
// tmuxSessionMissing() asks. recreateTmuxSession() rebuilds the session the
// entrypoint would have — same name, same geometry (bin/contributor-agent.sh),
// same cwd the launch command cds into — so the normal launch/readiness path
// can resume. Both are best-effort probes on the readiness poll, so a failure
// here is reported and retried on the next poll rather than thrown.
const TMUX_SESSION_GEOMETRY = '-x 200 -y 50';

function tmuxSessionMissing() {
  try {
    execSync(`tmux has-session -t ${TMUX_SESSION} 2>/dev/null`, { timeout: TMUX_COMMAND_TIMEOUT_MS });
    return false;
  } catch (_) {
    return true;
  }
}

function recreateTmuxSession() {
  const cwd = AGENT_CWD || process.cwd();
  try {
    execSync(`tmux new-session -d -s ${TMUX_SESSION} ${TMUX_SESSION_GEOMETRY}${cwd ? ` -c ${shellQuote(cwd)}` : ''}`,
      { timeout: TMUX_COMMAND_TIMEOUT_MS });
    return true;
  } catch (e) {
    console.error(`Could not recreate tmux session '${TMUX_SESSION}': ${e && e.message ? e.message : e}`);
    return false;
  }
}

// typeLaunchCommand types the CLI launch into the pane. Shared by relaunchCLI()
// and the #7863 session-recreate path, which must NOT go through relaunchCLI():
// that re-arms armCLIReadyWait(), and the recreate runs from inside the wait
// that is already armed.
function typeLaunchCommand() {
  const launchCmd = buildLaunchCommand();
  execSync(`tmux send-keys -t ${TMUX_SESSION} ${shellQuote(launchCommandWithCwd(launchCmd))} Enter`, { timeout: 15000 });
  return launchCmd;
}

function waitForCLI() {
  let loginMessageShown = false;
  let needsLoginTicks = 0;
  return new Promise((resolve, reject) => {
    const start = Date.now();
    const check = () => {
      // #7863: a missing SESSION is a hard condition, not `starting`. Handle it
      // before classifying the (necessarily empty) capture.
      if (tmuxSessionMissing()) {
        console.error(`tmux session '${TMUX_SESSION}' no longer exists — the pane's shell was ended (an attached client closing it, or the server dying). The relay owns the session now: recreating it (#7863).`);
        if (recreateTmuxSession()) {
          try {
            typeLaunchCommand();
            console.error(`Recreated tmux session '${TMUX_SESSION}' and relaunched ${BACKEND}; waiting for it to become ready.`);
          } catch (e) {
            console.error(`Recreated tmux session '${TMUX_SESSION}' but could not type the ${BACKEND} launch: ${e && e.message ? e.message : e}`);
          }
        } else if (currentTask) {
          // The task cannot be worked on this host right now; hand it back at
          // once instead of holding its lease for CLI_READY_TIMEOUT_MS. The
          // poll keeps going so a session an operator recreates by hand — or
          // that the next poll manages to create — is picked up.
          discardPendingTask('the tmux session is gone and could not be recreated');
          failCurrentTask(`tmux session '${TMUX_SESSION}' is gone and could not be recreated`,
            { skipReady: true, skipCLI: true, kind: 'environment' });
        }
        setTimeout(check, CLI_READY_POLL_MS);
        return;
      }
      const state = getCLIState();
      if (state === 'ready') {
        if (loginMessageShown) {
          console.log('CLI authentication prompt cleared; continuing.');
        }
        console.log('CLI ready — accepting tasks');
        resolve();
      } else if (state === 'onboarding') {
        needsLoginTicks = 0;
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
      } else if (state === 'needs-login') {
        needsLoginTicks++;
        if (!loginMessageShown && needsLoginTicks >= NEEDS_LOGIN_CONFIRM_TICKS) {
          loginMessageShown = true;
          console.log('');
          for (const line of renderBoxedBanner(loginBannerLines(BACKEND, ATTACH_COMMAND))) {
            console.log(line);
          }
          console.log('');
        }
        setTimeout(check, CLI_READY_POLL_MS);
      } else if (Date.now() - start > CLI_READY_TIMEOUT_MS) {
        reject(new Error('CLI did not become ready within timeout'));
      } else {
        needsLoginTicks = 0;
        setTimeout(check, CLI_READY_POLL_MS);
      }
    };
    check();
  });
}

let cliReady = false;
let pendingTask = null;
// The task_id the queued prompt belongs to (hivecommons/hive#7779), stamped
// from currentTask when the prompt is queued. A queued prompt is only ever a
// task's prompt, and it must die with that task: a revoke or failure that
// lands while the CLI is still coming up used to leave the prompt in the queue,
// and the readiness callback then typed it into the fresh CLI — the agent
// started working an issue the hub had already taken back and possibly handed
// to someone else, while currentTask said the relay held nothing. Every
// task-exit path now discards the queue, and flushPendingTask() refuses to type
// a prompt whose owner is not the task the relay currently holds.
let pendingTaskId = null;
// True once a CLI-readiness wait has timed out and we handed its task back.
// Used so the eventual recovery re-advertises availability to the hub, which
// we deliberately withheld at failure time (see armCLIReadyWait).
let cliReadyFailed = false;

// False until the CURRENT task's prompt actually reached the pane
// (kubestellar/hive#5650). tmuxSendKeys() queues rather than types whenever the
// CLI is not confirmed ready or the pane has fallen back to a shell, and a task
// whose prompt is still queued has told the agent nothing — so nothing on the
// pane is evidence about it. progressTick() consults this before judging.
let taskPromptDelivered = false;

// The HIVE_VERDICT: line already on the pane when the current task's prompt was
// typed into it, or null when the pane held none (kubestellar/hive#5650).
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
//
// On readiness the relay advertises AT MOST ONCE, and only when three things
// hold: it is idle, the hub it would ask has authenticated it, and no earlier
// `ready` to that hub is still awaiting an answer (hub.readyOutstanding,
// #7732). The third guard is what makes this callback safe to arm from every
// task-exit path. finishCurrentTask()/failCurrentTask() already send `ready`
// themselves and THEN relaunch the CLI, which arms this; with a backend whose
// pane reads ready on the very first waitForCLI() poll (omp sits at its prompt
// after the two Ctrl-Cs), this callback ran before the hub could possibly have
// answered and sent a second `ready` in the same event-loop turn. The hub
// answered the first with an assignment and read the second as the relay
// giving that assignment back (#2545): an abandoned_handback row, a cooldown
// on the fresh task, and a third assignment the relay rejected because it was
// already running the second. The interactive-revoke path had the same shape
// from two sends inside this one callback.
//
// The cases this single condition replaces were all instances of it: the
// startup path (#6655) where auth_ok withheld `ready` because the CLI was
// still coming up; recovery after a readiness failure, where the task was
// handed back with skipReady and nothing has asked since; and the revoke
// path, whose task is gone and whose `ready` was deliberately deferred until
// a fresh CLI was confirmed (#5042). In every one of them a `ready` is owed
// exactly when no other path has sent one — and a `ready` while currentTask
// is set (a hub that pushed work during the relaunch) would hand that work
// back, so idleness is checked here rather than assumed from the path.
// queuePendingTask parks a task prompt for flushPendingTask() to type once the
// CLI is confirmed ready, remembering which task it belongs to (#7779).
function queuePendingTask(text) {
  pendingTask = text;
  pendingTaskId = currentTask ? currentTask.task_id : null;
}

// discardPendingTask drops a queued prompt that must never be typed: the task
// it was for has ended (revoked, failed, completed) or the CLI it was waiting
// on never came up (#7779). Called from every task-exit path, and by
// flushPendingTask() itself when the owner no longer matches.
function discardPendingTask(why) {
  if (pendingTask === null) return;
  console.log(`Dropping the queued prompt for ${pendingTaskId || 'no task'} — ${why}`);
  pendingTask = null;
  pendingTaskId = null;
}

function armCLIReadyWait() {
  waitForCLI().then(() => {
    cliReady = true;
    cliReadyFailed = false;
    const hub = currentTaskHub();
    if (!currentTask && hub.authenticated && !hub.readyOutstanding) {
      send({ type: 'ready', seq: nextSeq() });
    }
    flushPendingTask();
  }).catch(e => {
    cliReadyFailed = true;
    console.error(e.message);
    // Drop the queued prompt first: if the CLI later recovers, flushing a
    // prompt for a task the hub has already reassigned would have this
    // contributor silently working on someone else's issue.
    discardPendingTask('the CLI never became ready');
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

// ── Confirming the prompt was SUBMITTED, not just typed (#6717) ─────────────
//
// tmuxSendEnters() has always been fire-and-forget: the send loop below retries
// when tmux itself errors, but nothing ever checked whether the keystrokes
// achieved anything. A TUI that collapses the burst into a paste placeholder
// swallows those Enters as newlines inside the pasted text, so the prompt sits
// in the input widget and the agent never runs — while the relay logs
// "Task prompt sent to CLI" and moves on (see paneHoldsUnsubmittedPrompt).
//
// ENTER_COUNT is not the lever. The problem is not a dropped keystroke but a
// widget consuming newlines as content; three of them are consumed exactly as
// one is. What does help is giving the widget time to finish processing the
// burst before the submit arrives, and then LOOKING at the pane and trying
// again if the prompt is still sitting there.
const PROMPT_PASTE_SETTLE_MS = 1200;
const PROMPT_SUBMIT_RETRIES = 3;
const PROMPT_SUBMIT_RETRY_DELAY_MS = 1500;

// confirmPromptSubmitted re-sends Enter while the pane still shows the prompt
// collapsed in its input widget, and reports whether it ended up submitted.
//
// Returns true both when submission is confirmed and when this backend's
// widget rendering is unknown to paneHoldsUnsubmittedPrompt() — "no evidence of
// a stuck prompt" is the only honest answer there, and it is also the
// pre-#6717 behaviour, so no backend regresses into extra keystrokes it never
// needed. The chrome-idle veto in progressTick() is the backstop for whatever
// this cannot see.
//
// A bare Enter is the only key sent, and only while the placeholder is still
// there: on a pane that did submit, the widget is empty (or holding its
// "Ask Codex to…" placeholder) and an Enter is a no-op.
function confirmPromptSubmitted() {
  if (!paneHoldsUnsubmittedPrompt(capturePaneText(), BACKEND)) return true;
  for (let attempt = 1; attempt <= PROMPT_SUBMIT_RETRIES; attempt++) {
    console.warn(`Task prompt is still sitting unsubmitted in the ${BACKEND} input widget (collapsed paste) — re-sending Enter, attempt ${attempt}/${PROMPT_SUBMIT_RETRIES}`);
    try {
      execSync(`tmux send-keys -t ${TMUX_SESSION} Enter`, { timeout: 15000 });
    } catch (e) {
      console.error(`Re-sending Enter failed: ${e.message}`);
    }
    sleepMs(PROMPT_SUBMIT_RETRY_DELAY_MS);
    if (!paneHoldsUnsubmittedPrompt(capturePaneText(), BACKEND)) {
      console.log(`Task prompt submitted after ${attempt} extra Enter(s)`);
      return true;
    }
  }
  // Deliberately NOT a silent give-up, and deliberately not left for the
  // 30-minute lease to notice either. The prompt is still in the widget, so the
  // agent has been told nothing — say so at the moment it is known, and let the
  // chrome-idle veto turn the resulting empty pane into a FAILURE the hub
  // re-offers rather than the false completion #6717 reports.
  console.error(`Task prompt could NOT be submitted to ${BACKEND} after ${PROMPT_SUBMIT_RETRIES} extra Enter(s) — the agent has not been given this task`);
  return false;
}

// ── "Has this agent done anything at all since it was prompted?" (#6717) ────
//
// The pane as it stood the moment the prompt was delivered. Anything the agent
// subsequently draws — a spinner, a tool row, prose, a summary, its own echo of
// the submitted prompt — changes this. A pane still byte-identical to it has
// produced nothing since being prompted, which is what a never-submitted prompt
// looks like and what a working (or worked) agent cannot look like.
//
// Null when no prompt has been delivered for the current task, in which case
// nothing is claimed: #5650's taskPromptDelivered guard owns that case and
// returns before any of this is consulted.
let promptDeliveryFingerprint = null;

// False only once confirmPromptSubmitted() has SEEN the prompt stuck in the
// input widget and failed to clear it. Default true so that every backend
// whose widget rendering is unknown, and every path that never reaches the
// send loop, behaves exactly as it did before #6717.
let promptSubmissionConfirmed = true;

function paneFingerprint(tmuxLines) {
  return Array.isArray(tmuxLines) ? tmuxLines.join('\n') : String(tmuxLines || '');
}

// paneChangedSinceDelivery reports whether the pane differs from the delivery
// snapshot — i.e. whether ANY output has appeared since this task's prompt was
// typed in.
//
// PURE, like paneChangedSince() next to it and for the same reason: it reads
// the already-captured lines and never touches the destructive paneStalled()
// fingerprint (#5333), and it never updates the delivery snapshot either — the
// snapshot is a fixed point in this task's history, not a rolling one.
//
// An unset snapshot or an empty capture means "no evidence", and both answer
// TRUE (changed). Direction matters: this function's only caller uses a FALSE
// to veto a completion, so an absent reading must never be read as grounds to
// fail a task.
function paneChangedSinceDelivery(tmuxLines) {
  if (promptDeliveryFingerprint === null) return true;
  const fingerprint = paneFingerprint(tmuxLines);
  if (!fingerprint) return true;
  return fingerprint !== promptDeliveryFingerprint;
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
  // must not judge a task in that state (kubestellar/hive#5650).
  taskPromptDelivered = false;
  // Same up-front clear, same reason (#6717): a queued or abandoned send has
  // submitted nothing and left no delivery snapshot to compare a pane against.
  promptSubmissionConfirmed = true;
  promptDeliveryFingerprint = null;
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
    queuePendingTask(text);
    return;
  }
  if (paneIsRunningShell()) {
    console.log(`Pane is at a shell prompt, not ${BACKEND} — queuing task prompt instead of typing it into the shell`);
    queuePendingTask(text);
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
      queuePendingTask(text);
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
    //
    // #7662: the baseline is read from the SAME deep window progressTick()
    // scans for the verdict (PR_SCAN_LINES), not the 15-row display tail.
    // The two must move together: a previous task's verdict that sits 20
    // rows up is inside the tick's scan window, so it has to be inside the
    // baseline too, or the next task would be completed off it on its first
    // tick.
    const deliveryBaselineLines = captureTmuxLines(PR_SCAN_LINES);
    resetTaskAgentActivity(deliveryBaselineLines);
    // The baseline is the NEWEST sentinel, whatever kind: #7861's preference
    // must not apply here, or a previous task's trailing "complete" would sit
    // above the baseline and complete the next task on its first tick.
    const priorVerdict = detectHiveVerdict(deliveryBaselineLines, HIVE_VERDICT_TOKENS);
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
        // #6717: settle before submitting. A task prompt is ~2 KB and arrives
        // as one burst; a TUI with bracketed-paste handling is still ingesting
        // it 300ms later, and an Enter that lands while the widget is in that
        // state is taken as a newline INSIDE the pasted text instead of as
        // submit. Waiting for the widget to finish is what makes the Enter a
        // keypress. sleepMs() is a no-op under HIVE_RELAY_TEST_MODE, so this
        // costs the test suite nothing.
        sleepMs(PROMPT_PASTE_SETTLE_MS);
        tmuxSendEnters();
        console.log('Task prompt sent to CLI');
        // #6717: "typed" is not "submitted". Check the pane and re-send Enter
        // if the prompt is still collapsed in the input widget.
        //
        // taskPromptDelivered is set TRUE either way, on purpose. It answers
        // #5650's question — "did these keystrokes reach the pane" — and they
        // did; a false here would park the task on progressTick()'s
        // no-judgement branch until the max-duration lease expired, silently,
        // half an hour later. The unsubmitted case is instead reported as a
        // FAILURE by the chrome-idle veto, which has the evidence to say so.
        promptSubmissionConfirmed = confirmPromptSubmitted();
        // The delivery snapshot, taken AFTER the submit attempts: everything
        // the agent draws from here on changes it, and a pane still identical
        // to it when the chrome-idle grace elapses has produced nothing at all.
        promptDeliveryFingerprint = paneFingerprint(captureTmuxLines(TMUX_TAIL_LINES));
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
// category without the other (kubestellar/hive#5478).
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
// cooldown (kubestellar/hive#2393 item 7). This is intentionally best-effort:
// when no PR link is visible we return '' and the hub applies its short no-PR
// cooldown.
//
// A CANDIDATE, NOT A CONCLUSION (kubestellar/hive#6662). A regex over pane text
// cannot tell a PR the agent OPENED from one it merely READ ABOUT, and an agent
// researching prior art prints plenty of the latter — `gh pr list` and
// `gh issue view --comments` both render full URLs. Everything this returns is
// run past prAttributionEvidence() below before it is reported as this task's
// work or allowed to outrank the agent's own verdict.
//
// THE CROSS-REPO FALLBACK IS GONE. This used to return the first PR URL in ANY
// repo when nothing matched the task's repo, on the reasoning that an
// approximate audit trail beats none. It does not: pr_url is a value the hub
// books cooldowns and credits work on, so an approximate one is a wrong one. A
// PR in a different repository cannot be the PR for this task's issue.
//
// ALL CANDIDATES, IN THE ORDER WORTH VERIFYING THEM (hivecommons/hive#7789).
// This used to return the FIRST matching URL top-down and resolveTaskPR()
// verified only that one — so when the agent had read an older PR before
// opening its own (the task prompt tells it to check for prior PRs first), the
// researched PR sat higher on the pane, was the one examined, was refuted, and
// the relay stopped there: the PR the agent actually shipped, further down,
// was never looked at and the task was booked with no PR at all. Observed
// live on utah#131, which shipped utah#205 and was credited `verdict=idle`.
// #7759 makes this shape routine: a no_work_needed verdict cites prior PRs by
// construction, and the advisor can now turn it into a shipped PR in the same
// pane.
//
// So this returns every distinct matching URL, ordered by how likely each is
// to be the agent's OWN:
//
//   1. URLs on a HIVE_VERDICT: line, newest verdict first. The sentinel is
//      the agent's deliberate statement of what it did, and a `complete`
//      verdict that names a PR is naming the one it opened.
//   2. Everything else, newest-printed first. The agent researches before it
//      ships, so its own PR is printed after the ones it read about.
//
// resolveTaskPR() verifies them in this order and stops at the first that is
// not refuted, so the order only decides which candidate wins when several
// survive (gh offline: every one is UNKNOWN) and how many gh lookups a pane
// full of researched PRs costs before the real one is reached.
function detectPRURLs(lines, repo) {
  if (!Array.isArray(lines) || lines.length === 0) return [];
  // Matches https://github.com/<owner>/<repo>/pull/<number>, capturing owner/repo.
  const PR_URL_RE = /https:\/\/github\.com\/([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)\/pull\/\d+/g;
  // #7862: agents cite their own PR by number on the verdict line ("complete
  // — PR #198 delivers …") at least as often as by URL. With the task repo
  // known, that IS a candidate — synthesized as the repo's URL and put
  // through the same verification as a pasted one, so a `#N` that turns out
  // to be someone else's PR is refuted like any other researched reference.
  // Verdict lines only: a bare "#42" elsewhere in a transcript is usually an
  // issue.
  const PR_REF_RE = /\b(?:PR|pull[ -]request)\s*#(\d+)/gi;
  const onVerdictLine = [];
  const elsewhere = [];
  for (let i = lines.length - 1; i >= 0; i--) {
    const line = lines[i];
    if (typeof line !== 'string') continue;
    const verdictLine = isHiveVerdictLine(line);
    const bucket = verdictLine ? onVerdictLine : elsewhere;
    let m;
    PR_URL_RE.lastIndex = 0;
    while ((m = PR_URL_RE.exec(line)) !== null) {
      // With no task repo to compare against there is nothing to attribute the
      // URL to either way; every URL is a candidate, as the old code allowed.
      if (repo && m[1] !== repo) continue;
      bucket.push(m[0]);
    }
    if (verdictLine && repo) {
      PR_REF_RE.lastIndex = 0;
      while ((m = PR_REF_RE.exec(line)) !== null) {
        bucket.push(`https://github.com/${repo}/pull/${m[1]}`);
      }
    }
  }
  const seen = new Set();
  const ordered = [];
  for (const url of onVerdictLine.concat(elsewhere)) {
    if (seen.has(url)) continue;
    seen.add(url);
    ordered.push(url);
  }
  return ordered;
}

// detectPRURL is the single best candidate — the head of detectPRURLs() — kept
// for callers and tests that want one URL rather than the ranked list.
function detectPRURL(lines, repo) {
  const urls = detectPRURLs(lines, repo);
  return urls.length > 0 ? urls[0] : '';
}

// ── Did THIS task open that PR? (kubestellar/hive#6662) ──────────────────────
//
// The precedence at the completion site reads "a visible PR contradicts
// 'nothing shippable'". That is true of a PR this task opened. It is exactly
// inverted for a PR a maintainer merged a month ago: there, the PR is the
// evidence that makes no_work_needed CORRECT.
//
// And that is not a corner case — it is the #3987 population by construction.
// #3987's own step 1 is "an issue's shippable parts land across several PRs
// referencing it; those PRs merge". For that shape a merged reference PR is
// always present, and the agent must cite it to justify the verdict at all. So
// the very evidence that makes the verdict right was what discarded it, and the
// issue was booked as shipped — re-entering the offer pool when no merge ever
// materialised, which is the #2547 loop #3987 exists to close.
//
// Measured over one 45-minute container session: 3 of 10 completions attributed
// a third party's already-merged PR to this contributor and lost a correct
// verdict. Whether a task landed in that 3 came down to whether the agent
// happened to print a bare `#1103` (which PR_URL_RE does not match, verdict
// survives) or a full URL (verdict discarded) — a clean split with nothing else
// distinguishing the groups.
//
// PR_ATTRIBUTION_CLOCK_SKEW_MS guards the timestamp comparisons. taskAssignedAt
// is this host's clock and createdAt/mergedAt are GitHub's; a couple of minutes
// of drift between them is ordinary. The misattributions this exists to catch
// are off by weeks, so a few minutes of slack costs nothing and stops a genuine
// PR being refused over a clock difference.
const PR_ATTRIBUTION_CLOCK_SKEW_MS = 5 * 60 * 1000;

const PR_ATTRIBUTION_CONFIRMED = 'confirmed';
const PR_ATTRIBUTION_REFUTED = 'refuted';
const PR_ATTRIBUTION_UNKNOWN = 'unknown';

// prAttributionEvidence decides whether PR metadata is consistent with "this
// task opened it". PURE — it takes the already-fetched metadata, so the rules
// are testable without a network or a gh binary.
//
// meta is gh's JSON shape: { url, author: {login}, createdAt, mergedAt, state }.
// Returns { status, reason }.
//
// Refutation is on FACTS THAT CANNOT BE TRUE OF OUR OWN PR, in increasing order
// of how much they depend on knowing who we are:
//
//   1. Merged before the task started. The cheapest and strongest check, and on
//      its own it catches all three observed misattributions. A PR that merged
//      before this task began cannot be the PR this task opened.
//   2. Created before the task started. Same argument one step earlier; catches
//      an open third-party PR the agent read about.
//   3. Authored by somebody else. Only usable when the contributor identity is
//      known — contributor-agent.sh defaults HIVE_CONTRIBUTOR_USERNAME to the
//      literal "unknown", which is not a login and must not be compared as one.
//
// Anything else is `confirmed`. UNKNOWN is reserved for "we could not look",
// and the caller treats it as weaker than the agent's explicit sentinel rather
// than as a refutation — a lookup failure must not start discarding real PRs.
function prAttributionEvidence(meta, opts) {
  const o = opts || {};
  if (!meta || typeof meta !== 'object') {
    return { status: PR_ATTRIBUTION_UNKNOWN, reason: 'no PR metadata available' };
  }
  const startedAt = Number(o.taskStartedAt) || 0;
  const login = typeof o.contributorLogin === 'string' ? o.contributorLogin.trim() : '';
  // "unknown" is contributor-agent.sh's placeholder, not a GitHub login.
  const knownLogin = login && login.toLowerCase() !== 'unknown' ? login : '';

  const parseTs = (v) => {
    const t = Date.parse(v || '');
    return Number.isFinite(t) ? t : null;
  };

  if (startedAt > 0) {
    const mergedAt = parseTs(meta.mergedAt);
    if (mergedAt !== null && mergedAt < startedAt - PR_ATTRIBUTION_CLOCK_SKEW_MS) {
      return {
        status: PR_ATTRIBUTION_REFUTED,
        reason: `merged ${meta.mergedAt} — before this task started`,
      };
    }
    const createdAt = parseTs(meta.createdAt);
    if (createdAt !== null && createdAt < startedAt - PR_ATTRIBUTION_CLOCK_SKEW_MS) {
      return {
        status: PR_ATTRIBUTION_REFUTED,
        reason: `created ${meta.createdAt} — before this task started`,
      };
    }
  }

  const author = meta.author && typeof meta.author === 'object' ? (meta.author.login || '') : '';
  if (knownLogin && author && author.toLowerCase() !== knownLogin.toLowerCase()) {
    return {
      status: PR_ATTRIBUTION_REFUTED,
      reason: `authored by ${author}, not ${knownLogin}`,
    };
  }

  return { status: PR_ATTRIBUTION_CONFIRMED, reason: 'authored by this contributor during this task' };
}

// verifyTaskPR is the effectful half: ask gh about the candidate, then apply the
// pure rules above. Bounded and non-fatal — a gh that is missing, rate-limited
// or offline yields UNKNOWN, never a refutation.
function verifyTaskPR(url, opts) {
  if (!url) return { status: PR_ATTRIBUTION_UNKNOWN, reason: 'no candidate URL' };
  let meta = null;
  try {
    const raw = execSync(
      `gh pr view ${shellQuote(url)} --json url,author,createdAt,mergedAt,state 2>/dev/null`,
      { encoding: 'utf8', timeout: 20000 }
    );
    meta = JSON.parse(raw);
  } catch (e) {
    return {
      status: PR_ATTRIBUTION_UNKNOWN,
      reason: `could not query GitHub for ${url}: ${(e && e.message) || 'unknown error'}`,
    };
  }
  return prAttributionEvidence(meta, opts);
}

// resolveTaskPR turns a pane scrape into the two decisions the completion site
// needs: what to REPORT as this task's PR, and whether that PR is strong enough
// to outrank an explicit HIVE_VERDICT.
//
// The split is the point (#6662 suggestion 3). A regex hit on scrollback prose
// is much weaker evidence than a sentinel the agent deliberately printed, so
// only a CONFIRMED PR suppresses the verdict. An UNKNOWN one is still reported
// as a best-effort audit trail — dropping it on a transient gh failure would
// start losing real PRs — but it no longer gets to silently overrule the agent.
//
// A refutation is a verdict on ONE candidate, not on the pane (#7789). The
// agent is told to look for prior PRs before it works, so a researched PR
// above its own is the normal shape of a pane that shipped something — and
// stopping at the first refutation credited exactly those tasks with nothing.
// Candidates come from detectPRURLs() already ranked (the verdict line's URL
// first, then newest-printed first), each refuted one is logged as before,
// and the walk continues until a candidate survives.
//
// PR_ATTRIBUTION_MAX_LOOKUPS bounds the gh calls. Each is a blocking, up to
// 20-second execSync in the tick loop, and a pane that rendered `gh pr view`
// for a dozen prior PRs must not spend minutes refuting them one by one. The
// ranking puts the agent's own PR at the front, so the cap is a backstop, not
// something a normal pane reaches.
const PR_ATTRIBUTION_MAX_LOOKUPS = 8;

function resolveTaskPR(lines, opts) {
  const o = opts || {};
  const candidates = detectPRURLs(lines, o.repo);
  if (candidates.length === 0) return { url: '', evidence: null, suppressesVerdict: false };
  let evidence = null;
  const budget = Math.min(candidates.length, PR_ATTRIBUTION_MAX_LOOKUPS);
  for (let i = 0; i < budget; i++) {
    const candidate = candidates[i];
    evidence = verifyTaskPR(candidate, o);
    if (evidence.status === PR_ATTRIBUTION_REFUTED) {
      console.log(`Ignoring PR ${candidate} for ${o.taskId || 'task'} — not this task's work (${evidence.reason}); ` +
        `it was visible in the pane because the agent researched it (kubestellar/hive#6662)`);
      continue;
    }
    if (evidence.status === PR_ATTRIBUTION_UNKNOWN) {
      console.log(`Detected PR for ${o.taskId || 'task'}: ${candidate} (UNVERIFIED — ${evidence.reason}; ` +
        `reporting it, but not letting it override the agent's verdict)`);
      return { url: candidate, evidence, suppressesVerdict: false };
    }
    console.log(`Detected PR for ${o.taskId || 'task'}: ${candidate}`);
    return { url: candidate, evidence, suppressesVerdict: true };
  }
  if (candidates.length > budget) {
    console.warn(`Stopped verifying PR candidates for ${o.taskId || 'task'} after ${budget} refutations; ` +
      `${candidates.length - budget} more PR URL(s) on the pane were not checked (#7789)`);
  }
  // Every candidate examined was refuted: the last refutation stands in for the
  // pane, exactly as the single refutation did before.
  return { url: '', evidence, suppressesVerdict: false };
}

// ── The HIVE_VERDICT: sentinel family (kubestellar/hive#3987, #5376) ─────────
//
// The hub's task prompt asks the agent to end a task by printing ONE line of
// the exact form
//
//   HIVE_VERDICT: <verdict> — <short reason>
//
// Three verdicts are defined:
//
//   no_work_needed  (#3987) — the agent affirmatively determined there is
//     NOTHING shippable (the remainder is gated on an unanswered maintainer
//     decision, or merged PRs already cover it). Reported on task_complete as
//     verdict/verdict_reason so the hub parks the issue for the long
//     offer-suppression window instead of re-offering it every short-cooldown
//     period forever (the #2547 shape that escalation only bounded).
//
//   blocked         (hivecommons/hive#7924) — no_work_needed's sibling for
//     the case where nothing in THIS repo can change until something outside
//     it lands: another repo's release or build, a dependency that has not
//     published, an external service. utah#100 reached exactly that verdict
//     (the packages had recipes but no factory image carried them yet) and,
//     booked as a plain no_work_needed, the hub re-ran the same ten minutes
//     of research on the 4h backoff and threw the finding away. Reported as
//     verdict 'blocked'; the hub holds the issue for the full with-PR
//     cooldown, and when the task credential can write issues the relay
//     applies the repo's `blocked` label (markIssueBlocked below) so the
//     hub's existing admission gate withholds it until a human clears the
//     label. `no_work_needed — blocked: <reason>` is accepted as the same
//     verdict, for an agent that reaches for the older sentinel first.
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
// All three are parsed by ONE anchored, echo-guarded scanner below,
// deliberately: the anti-false-positive handling is the hard-won part and
// there must not be a second copy of it to drift.
//
// The marker spelling must stay in sync with buildTaskPrompt in
// src/pkg/dashboard/contribute_task_prompt.go.
const HIVE_VERDICT_NO_WORK = 'no_work_needed';
const HIVE_VERDICT_COMPLETE = 'complete';
const HIVE_VERDICT_BLOCKED = 'blocked';
// Every sentinel the scanner recognises, for the callers that want "any
// verdict at all" (completion, the #5650 delivery baseline, PR-URL ranking).
const HIVE_VERDICT_TOKENS = [HIVE_VERDICT_COMPLETE, HIVE_VERDICT_NO_WORK, HIVE_VERDICT_BLOCKED];
// The `no_work_needed — blocked: <reason>` spelling (#7924): the older
// sentinel with the blocked marker as the reason's first word. Read as the
// blocked verdict, with the marker stripped from the reason.
const NO_WORK_BLOCKED_REASON_RE = /^blocked\s*(?:(?:[:—–-])|(?:(?:on|by)\b))\s*/i;
const NO_WORK_NEEDS_DECISION_REASON_RE = /^(?:decision|needs[_ -]?decision|maintainer[_ -]?decision)\s*[:—–-]\s*/i;
const NATURAL_NEEDS_DECISION_REASON_RE = /(?:maintainer|design|policy|approval|\/approve).{0,80}(?:decision|approval|\/approve)|(?:decision|approval).{0,80}maintainer/i;
const ALREADY_DONE_REASON_KIND = 'already_done';
const ALREADY_DONE_MERGED_PR_RE = /\bmerged\s+(?:pull\s+request|PR)\s*#(\d+)\b/i;
const ALREADY_DONE_TEXT_RE = /\b(?:already\s+(?:resolved|fixed|implemented|merged|done)|no[-\s]?op)\b/i;
const COMMIT_SHA_RE = /\b[0-9a-f]{7,40}\b/i;
const ALREADY_DONE_WORKFLOW_LABEL = process.env.HIVE_ALREADY_DONE_LABEL || 'hive/already-done';

// isNoWorkVerdict says whether a verdict object is one of the "nothing to
// ship" family — no_work_needed or blocked — which the hub books the same way
// (task_complete verdict/verdict_reason) and a confirmed PR overrides the same
// way (resolveTaskPR's suppressesVerdict).
function isNoWorkVerdict(v) {
  return !!v && (v.verdict === HIVE_VERDICT_NO_WORK || v.verdict === HIVE_VERDICT_BLOCKED);
}

// verdictWireFields renders a "nothing to ship" verdict (or null) as the
// task_complete fields the hub reads. A blocked verdict goes on the wire as
// `verdict: no_work_needed` plus `verdict_blocked: true` — the marker, not a
// new token — so a hub that predates #7924 sees exactly the no_work_needed it
// already books (long offer-suppression), instead of an unknown verdict it
// would normalize to a bare idle and re-offer on the short cooldown. A hub
// that knows the marker books it as blocked (normalizeCompletionVerdict in
// src/pkg/dashboard/contribute_ledgers.go).
function verdictWireFields(noWork) {
  if (!isNoWorkVerdict(noWork)) return {};
  const alreadyDone = alreadyDoneVerdictFields(noWork.reason);
  return {
    verdict: HIVE_VERDICT_NO_WORK,
    verdict_reason: noWork.reason,
    ...alreadyDone,
    verdict_blocked: noWork.verdict === HIVE_VERDICT_BLOCKED ? true : undefined,
    verdict_needs_decision: noWork.needsDecision ? true : undefined,
  };
}

function alreadyDoneVerdictFields(reason) {
  reason = String(reason || '');
  if (!ALREADY_DONE_MERGED_PR_RE.test(reason) && !ALREADY_DONE_TEXT_RE.test(reason)) return {};
  const evidence = {};
  const pr = ALREADY_DONE_MERGED_PR_RE.exec(reason);
  if (pr) evidence.pr = Number(pr[1]);
  const sha = COMMIT_SHA_RE.exec(reason);
  if (sha && /[a-f]/i.test(sha[0])) evidence.commit = sha[0];
  return {
    verdict_reason_kind: ALREADY_DONE_REASON_KIND,
    evidence: Object.keys(evidence).length ? evidence : undefined,
  };
}

function isAlreadyDoneNoWork(noWork) {
  if (!isNoWorkVerdict(noWork)) return false;
  return alreadyDoneVerdictFields(noWork.reason).verdict_reason_kind === ALREADY_DONE_REASON_KIND;
}

// hiveVerdictLineRe builds the one regex that recognises a sentinel line, for
// any subset of the verdict tokens. Groups: 1 = optional Markdown emphasis
// opener, 2 = the verdict token, 3 = the rest of the line (the reason).
//
// Anchored at line start: the task PROMPT quotes the marker mid-sentence
// ("...the exact form 'HIVE_VERDICT: ...'"), and an anchored match keeps
// that instruction echo from reading as the agent's own verdict. Codex
// renders its completed assistant messages with a leading bullet (•,
// U+2022) and Claude Code with a filled circle (●, U+25CF) — presentation
// chrome rather than part of the verdict. Some backends also wrap the whole
// line in Markdown emphasis (for example **HIVE_VERDICT: complete — done**),
// which is likewise presentation rather than sentinel content. The claude
// glyph was missing
// until bin/test_backend_smoke.sh drove a REAL claude pane through the
// relay: the agent printed the sentinel, this regex missed it, and every
// interactive claude completion silently degraded to the chrome_idle
// fallback the sentinel exists to replace.
//
// The verdict token is an alternation of exactly the wanted tokens with a \b
// after it, so "no_work_neededX" and "completely rewrote the parser" are both
// non-matches — a prose line that merely STARTS with a verdict word must not
// become a verdict.
function hiveVerdictLineRe(wanted) {
  const alt = wanted.map(w => w.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|');
  return new RegExp(`^\\s*(?:[•●]\\s*)?([*_]{1,3})?\\s*HIVE_VERDICT:\\s*(${alt})\\b[\\s:—–-]*(.*)$`, 'i');
}

// isHiveVerdictLine says whether one pane line is a sentinel the agent
// printed (any verdict), with the same anchoring and the same echo
// exclusion detectHiveVerdict() applies. detectPRURLs() uses it to rank a PR
// URL the agent named IN its verdict above one it merely printed (#7789).
function isHiveVerdictLine(line) {
  if (typeof line !== 'string') return false;
  const m = hiveVerdictLineRe(HIVE_VERDICT_TOKENS).exec(line);
  if (!m) return false;
  // The prompt's own "<short reason>" placeholder, wrapped to a line start.
  return !(m[3] || '').trim().startsWith('<');
}

// detectHiveVerdict scans `lines` newest-first for any of `wanted` (an array of
// verdict tokens) and returns { verdict, reason } for the first — i.e. the
// LAST-printed — match, or null.
//
// Returns null rather than throwing on junk input: every caller is on a
// best-effort path reading a terminal capture that may be empty.
function detectHiveVerdict(lines, wanted) {
  const all = detectHiveVerdicts(lines, wanted);
  return all.length > 0 ? withoutPaneIndex(all[0]) : null;
}

// withoutPaneIndex drops the pane index detectHiveVerdicts() carries; the
// single-verdict shape callers compare and log is { verdict, reason, line }.
function withoutPaneIndex(v) {
  const out = { verdict: v.verdict, reason: v.reason, line: v.line };
  if (v.needsDecision) out.needsDecision = true;
  return out;
}

// detectHiveVerdicts is detectHiveVerdict for EVERY sentinel on the pane,
// newest-first, each carrying its pane index — so a caller can reason about
// the pair an agent prints when it narrates a closing "complete" after its
// real verdict (#7861).
function detectHiveVerdicts(lines, wanted) {
  if (!Array.isArray(lines) || lines.length === 0) return [];
  if (!Array.isArray(wanted) || wanted.length === 0) return [];
  const found = [];
  // The anchoring, chrome tolerance and token boundary are all in
  // hiveVerdictLineRe() above, shared with isHiveVerdictLine().
  const VERDICT_RE = hiveVerdictLineRe(wanted);
  // Scan newest-first so the agent's final conclusion wins over anything it
  // merely quoted or considered earlier in the transcript.
  for (let i = lines.length - 1; i >= 0; i--) {
    const m = VERDICT_RE.exec(lines[i]);
    if (!m) continue;
    const emphasis = m[1] || '';
    let reason = (m[3] || '').trim();
    // A matching Markdown delimiter at the far end closes the optional
    // leading emphasis; it is not part of the human-readable reason. Only
    // remove it when an opener was present, so an ordinary reason ending in
    // `*` or `_` is preserved verbatim.
    if (emphasis && reason.endsWith(emphasis)) {
      reason = reason.slice(0, -emphasis.length).trimEnd();
    }
    // tmux may wrap the prompt's instruction so its quoted marker lands at a
    // visual line start; its giveaway is the literal "<short reason>"
    // placeholder. Never treat that echo as a real verdict.
    if (reason.startsWith('<')) continue;
    let verdict = m[2].toLowerCase();
    // #7924: `no_work_needed — blocked: <reason>` is the blocked verdict in
    // the older sentinel's clothing. Promote it whenever the caller would
    // have accepted a no_work_needed at all: the two are booked as one
    // family (isNoWorkVerdict), so no caller that wants one rejects the other.
    let needsDecision = false;
    if (verdict === HIVE_VERDICT_NO_WORK && (NO_WORK_NEEDS_DECISION_REASON_RE.test(reason) || NATURAL_NEEDS_DECISION_REASON_RE.test(reason))) {
      needsDecision = true;
      reason = reason.replace(NO_WORK_NEEDS_DECISION_REASON_RE, '').trim();
    } else if (verdict === HIVE_VERDICT_NO_WORK && NO_WORK_BLOCKED_REASON_RE.test(reason)) {
      verdict = HIVE_VERDICT_BLOCKED;
      reason = reason.replace(NO_WORK_BLOCKED_REASON_RE, '').trim();
    }
    // `line` is the RAW pane line this verdict was read from. progressTick()
    // compares it against the line that was already on the pane when the task's
    // prompt was delivered, which is how a verdict gets attributed to a task at
    // all (#5650).
    found.push({ verdict, reason, line: lines[i], index: i, needsDecision });
  }
  return found;
}

// Best-effort scan for the "nothing to ship" sentinels — no_work_needed, and
// since #7924 its blocked sibling. Unchanged in behaviour from #3987/#4265
// for the first; it shares the scanner above. Returns null when no marker is
// found — the hub then treats the completion exactly as an idle one.
function detectNoWorkVerdict(lines) {
  return detectHiveVerdict(lines, [HIVE_VERDICT_NO_WORK, HIVE_VERDICT_BLOCKED]);
}

// detectCompletionVerdict reports whether the agent SAID it finished (#5376).
//
// Either verdict counts as "the agent declared this task over": no_work_needed
// is a completion too — it is the agent concluding the task with nothing to
// ship — and requiring a second `complete` line after it would make a
// compliant agent look non-compliant.
//
// #7861: when the agent prints BOTH sentinels for the same task — a
// no_work_needed with its real reason, then a narrated "complete" (the prompt
// forbids it; Flash does it anyway) — the last-printed rule reported `complete`,
// which with no PR behind it degrades to a bare `idle` in the hub's ledger,
// while the informative verdict sat one line above. Prefer the no_work_needed
// in that case: the prompt already defines it as the completion when nothing
// shipped, and a PR-less `complete` carries strictly less. A `complete` WITH a
// PR still wins — not here, but where the verdicts are acted on: a confirmed
// PR suppresses no_work_needed (resolveTaskPR's suppressesVerdict), so this
// preference can never demote a real shipment.
//
// `baselineLine` is the sentinel that was already on the pane when this task's
// prompt was delivered (#5650). Only a no_work_needed printed AFTER it belongs
// to this task; one at or above it is a previous task's and must not be
// preferred — and when the newest line IS the baseline the caller discards it,
// so the answer must stay the newest line, exactly as before.
function detectCompletionVerdict(lines, baselineLine = deliveredVerdictBaseline) {
  const all = detectHiveVerdicts(lines, HIVE_VERDICT_TOKENS);
  if (all.length === 0) return null;
  const newest = withoutPaneIndex(all[0]);
  if (newest.verdict !== HIVE_VERDICT_COMPLETE || newest.line === baselineLine) return newest;
  const baselineAt = typeof baselineLine === 'string' ? lines.lastIndexOf(baselineLine) : -1;
  for (let i = 1; i < all.length; i++) {
    if (all[i].index <= baselineAt) break;
    if (isNoWorkVerdict(all[i])) {
      console.log(`Both HIVE_VERDICT lines are on the pane for this task — keeping ${all[i].verdict} over the complete printed after it; a PR this task opened still overrides it (#7861)`);
      return withoutPaneIndex(all[i]);
    }
  }
  return newest;
}

// ── Review output that lands after the verdict (hivecommons/hive#7759) ──────
//
// Backends whose CLI posts REVIEW output after the agent's final line, keyed by
// backend name. Only omp today: its `--advisor` runtime reviews each turn and
// injects "Advisor N note" blocks under it, each note tagged ⟦blocker⟧,
// ⟦concern⟧ or ⟦nit⟧. The shape — a reviewer feature that writes below the agent's
// statement — is likely to recur with other CLIs, so the hook is per-backend
// data rather than an omp special case in the tick loop.
//
//   note     — the header line of one review block. Live captures render the
//              leading glyph differently ("ⓘ Advisor 1 note" in #7759,
//              "@ Advisor 1 note" in #7662), so only the words are matched.
//   concern  — the marker that earns the agent one more turn. ⟦blocker⟧ is
//              included because it is strictly stronger than a concern. ⟦nit⟧ is
//              deliberately NOT included: the advisor emits nits freely and
//              they are cheap to ignore; concerns are the ones worth a turn
//              (#7759 discussion). Both bracket spellings seen live are
//              accepted.
const POST_VERDICT_REVIEW_MARKERS = Object.freeze({
  omp: Object.freeze({
    note: /\bAdvisor \d+ note\b/,
    concern: /⟦blocker⟧|\[blocker\]|⟦concern⟧|\[concern\]/,
    // #7879: the strictly-stronger subset that alone can earn the SECOND
    // follow-up.
    blocker: /⟦blocker⟧|\[blocker\]/,
  }),
});

// postVerdictConcerns returns the concern lines that sit BELOW the agent's
// verdict line on the pane — the review of its final turn — in pane order.
//
// "Below" is what makes a note post-verdict: the advisor writes in transcript
// order, so anything above the verdict was posted about an earlier turn and is
// not this task's closing review. A concern only counts when a note header
// precedes it after the verdict, so a bare bracketed word in the agent's own
// prose (or in a quoted diff) cannot pass as a review note. Returns [] when the
// verdict line is not on the pane at all, since then there is no "below".
function postVerdictConcerns(lines, verdictLine, markers) {
  if (!markers || !Array.isArray(lines) || typeof verdictLine !== 'string') return [];
  const at = lines.lastIndexOf(verdictLine);
  if (at < 0) return [];
  const concerns = [];
  let inNote = false;
  for (let i = at + 1; i < lines.length; i++) {
    const line = lines[i];
    if (markers.note.test(line)) { inNote = true; continue; }
    if (inNote && markers.concern.test(line)) concerns.push(line);
  }
  return concerns;
}

// postVerdictNoteBlocks returns, for each review note BELOW the verdict line
// that carries a ⟦blocker⟧/⟦concern⟧ marker, its text — the marker line plus
// the indented continuation lines omp renders under it, with the gutter glyph
// stripped and whitespace collapsed (hivecommons/hive#7879). Same "below the
// verdict" rule as postVerdictConcerns; nits are skipped for the same reason
// they never earn a turn. This is what gets RECORDED when a task finalizes
// with notes still unaddressed: the information already exists on the pane
// and used to die with the relaunch.
function postVerdictNoteBlocks(lines, verdictLine, markers) {
  return postVerdictNoteBlockEntries(lines, verdictLine, markers).map(e => e.text);
}

// postVerdictNoteBlockEntries is postVerdictNoteBlocks with the raw marker
// lines each block was flagged by kept alongside its text:
// `{ text, markerLines }`, in pane order.
//
// #7935: the follow-up quotes the notes that earned it, and the notes that
// earned it are chosen by postVerdictConcerns() — which yields raw marker
// LINES, filtered against the previous tick's snapshot and (on the second
// round) down to ⟦blocker⟧s. Keeping the marker lines is what lets the caller
// intersect the two views and quote exactly the notes it is asking about,
// rather than every flagged block below the verdict.
function postVerdictNoteBlockEntries(lines, verdictLine, markers) {
  if (!markers || !Array.isArray(lines) || typeof verdictLine !== 'string') return [];
  const at = lines.lastIndexOf(verdictLine);
  if (at < 0) return [];
  const blocks = [];
  let current = null;
  const flush = () => {
    if (current && current.flagged && current.text.length) {
      blocks.push({
        text: current.text.join(' ').replace(/\s+/g, ' ').trim(),
        markerLines: current.markerLines,
      });
    }
    current = null;
  };
  for (let i = at + 1; i < lines.length; i++) {
    const line = lines[i];
    if (markers.note.test(line)) { flush(); current = { flagged: false, text: [], markerLines: [] }; continue; }
    if (!current) continue;
    // A note's body is indented; the first flush-left line ends it.
    if (!/^\s/.test(line) || line.trim() === '') { flush(); continue; }
    if (markers.concern.test(line)) { current.flagged = true; current.markerLines.push(line); }
    current.text.push(line.replace(/^[\s▎│|]+/, '').trim());
  }
  flush();
  return blocks;
}

// postVerdictQuotableNotes picks the note text the follow-up should quote for
// a given set of triggering concern lines (#7935).
//
// A concern line and a note block are two readings of the same pane region and
// they can disagree: postVerdictConcerns() accepts a marker line anywhere
// under a note header, while a block needs indented body lines. When the
// intersection is empty — an advisor rendering the relay has not seen — fall
// back to the concern lines themselves, which are always at least the marker
// and its first sentence. Quoting something beats quoting nothing; that is the
// whole point of #7935.
function postVerdictQuotableNotes(lines, verdictLine, markers, concernLines) {
  const wanted = new Set(Array.isArray(concernLines) ? concernLines : []);
  if (wanted.size === 0) return [];
  const matched = postVerdictNoteBlockEntries(lines, verdictLine, markers)
    .filter(e => e.markerLines.some(l => wanted.has(l)))
    .map(e => e.text)
    .filter(Boolean);
  if (matched.length) return matched;
  return Array.from(wanted).map(l => l.replace(/^[\s▎│|]+/, '').trim()).filter(Boolean);
}

// postVerdictReviewAnswered reports whether a verdict on the pane is the
// SECOND one — printed after the relay's follow-up — rather than the first
// verdict still sitting there while the agent works on the notes.
//
// The two verdict lines may be byte-identical (an agent that re-prints its
// conclusion verbatim), so line equality cannot tell them apart. The CLI's
// echo of the follow-up message can: a verdict below that echo was printed
// after it. When the echo has scrolled out of the scan window the agent has
// produced more than PR_SCAN_LINES rows of work since, and any verdict still
// in the window is by construction below it.
function postVerdictReviewAnswered(lines) {
  return followUpAnswered(lines, POST_VERDICT_REVIEW_ANCHOR);
}

// prClaimFollowUpAnswered is the #7862 twin: true once a HIVE_VERDICT line sits
// below the pane's echo of PR_CLAIM_FOLLOWUP_MESSAGE.
function prClaimFollowUpAnswered(lines) {
  return followUpAnswered(lines, PR_CLAIM_FOLLOWUP_ANCHOR);
}

function followUpAnswered(lines, anchor) {
  if (!Array.isArray(lines)) return true;
  let echoAt = -1;
  for (let i = lines.length - 1; i >= 0; i--) {
    if (lines[i].includes(anchor)) { echoAt = i; break; }
  }
  if (echoAt < 0) return true;
  return detectCompletionVerdict(lines.slice(echoAt + 1)) !== null;
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


// How long an attached tmux client must have been silent before the relay
// stops treating it as a person who owns the pane (kubestellar/hive#5277).
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
const HUMAN_PRESENCE_MAX_DEFERRALS = Number(process.env.HIVE_HUMAN_PRESENCE_MAX_DEFERRALS) || 3;

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

let lastPresencePaneFingerprint = null;
let presenceDeferralCount = 0;

function resetHumanPresenceEvidence() {
  lastPresencePaneFingerprint = null;
  presenceDeferralCount = 0;
}

function paneEditedSincePresenceCheck(tmuxLines) {
  const fingerprint = Array.isArray(tmuxLines) ? tmuxLines.join('\n') : String(tmuxLines || '');
  if (!fingerprint) return true;
  const previous = lastPresencePaneFingerprint;
  lastPresencePaneFingerprint = fingerprint;
  if (previous === null) return true;
  return fingerprint !== previous;
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
  execFileSync('tmux', ['send-keys', '-t', TMUX_SESSION, '-l', message], { timeout: 15000 });
  sleepMs(ENTER_DELAY_MS);
  tmuxSendEnters();
}

// classifyTmuxPane hands the captured text to the pure state machine
// (classifyPane in bin/lib/pane-classifier.js), injecting the one effectful
// reading it needs: whether a bob process is still alive (bobIsRunning),
// which only this relay — with process-table access — can answer.
function classifyTmuxPane(text) {
  return classifyPane(text, BACKEND, { bobIsRunning });
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
  // The pane may be wedged in bash PS2 continuation; clear it or the relaunch
  // command is swallowed as more continuation text and never runs.
  recoverWedgedShell();
  const launchCmd = typeLaunchCommand();
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

// The shape a hub-assigned repo has ('owner/name'), and the only shape
// taskCheckoutDir will turn into a path. Anything else — an empty repo, a
// bare name, a segment that could climb out of the workspace — resolves to no
// checkout at all rather than to a guess.
const TASK_REPO_SHAPE = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;

// taskCheckoutDir is where the task prompt tells the agent to put (or reuse)
// the checkout for `repo`: $HIVE_WORKSPACE_DIR/<owner>/<name>, as
// buildTaskPromptBodyForAccess in src/pkg/dashboard/contribute_task_prompt.go
// spells it. Returns '' when the relay cannot name that directory precisely.
//
// Deliberately keyed on TASK_CHECKOUT_ROOT (the explicit HIVE_WORKSPACE_DIR)
// and not on TASK_WORKSPACE_DIR's process.cwd() fallback: in local mode the
// relay's cwd is the hive checkout `just contribute-hive` was run from — the
// operator's own tree — and the one thing the hygiene below must never do is
// touch a checkout that is not the task's. The '.git' probe is the second half
// of that guarantee: `git -C <dir>` on a directory that exists but is not
// itself a repository would operate on whatever repository ENCLOSES it.
function taskCheckoutDir(repo) {
  if (!TASK_CHECKOUT_ROOT || typeof repo !== 'string' || !TASK_REPO_SHAPE.test(repo)) return '';
  if (repo.split('/').some(seg => seg === '.' || seg === '..')) return '';
  const dir = path.join(TASK_CHECKOUT_ROOT, repo);
  try {
    if (!fs.existsSync(path.join(dir, '.git'))) return '';
  } catch (_) {
    return '';
  }
  return dir;
}

// preserveTaskLeftovers sets aside whatever an exiting task left uncommitted in
// the shared checkout, so the next task on that repo starts from a clean tree
// (hivecommons/hive#7790).
//
// A relay works every task on a repo out of ONE persistent checkout under
// $HIVE_WORKSPACE_DIR, and the prompt tells each task to reuse it. A task that
// is revoked or aborted mid-edit is stopped by stopAgentForTaskExit() — two
// Ctrl-Cs and a relaunch — and nothing touches the tree: its half-done edits
// stay on its branch. Observed on projectbluefin/utah: utah#175 was revoked
// mid-edit in the #7732 hub-restart cascade and left three modified files on
// fix/kernel-module-fallback; the next four tasks on that repo (#14, #131,
// #128 …) all started from them, and each agent's advisor flagged it. None of
// the resulting PRs leaked the files only because that agent chose
// `git worktree add` on its own each time — an agent that follows the prompt
// literally (`cd` in, `git checkout -b … upstream/<base>`) carries them onto
// its branch, and one `git add -A` ships someone else's half-finished change
// under this contributor's name.
//
// STASH, never reset or clean: the leftovers are somebody's work, and an
// operator must be able to get them back (`git stash list` shows the task they
// came from). `--include-untracked` so a new file the task created is set
// aside too — that is exactly what a literal `git add -A` would pick up.
// Ignored files are left alone; they are build state, not work.
//
// Called on EVERY task-exit path, not just failure and revoke. The checkout
// stops being this task's whatever the verdict, and a completed task's stray
// uncommitted file is just as much the next task's problem. Sequenced AFTER
// the agent is stopped, so the stash is not racing an editor; a `git` the
// agent left mid-flight can still hold the index lock for a moment, in which
// case the stash fails and is logged — best-effort by design, like everything
// else on an exit path, and the prompt's own hygiene step (#7790, the same
// fix from the agent's side) is the backstop.
//
// task is the exiting task ({ repo, number, task_id }); callers that have
// already cleared currentTask pass it explicitly.
function preserveTaskLeftovers(task, reason) {
  if (!task || !task.repo) return;
  const dir = taskCheckoutDir(task.repo);
  if (!dir) return;
  const label = `${task.repo}#${task.number}`;
  let status = '';
  try {
    status = execFileSync('git', ['-C', dir, 'status', '--porcelain', '--untracked-files=all'], {
      encoding: 'utf8', timeout: 15000, stdio: ['ignore', 'pipe', 'pipe'],
    });
  } catch (e) {
    console.error(`Could not inspect the ${label} checkout at ${dir} after ${reason}: ${e.message} — a later task on ${task.repo} may start from a dirty tree`);
    return;
  }
  const paths = String(status || '').split('\n').filter(Boolean);
  if (paths.length === 0) return;
  const message = `hive leftover ${task.task_id || 'unknown-task'} (${label}, ${reason})`;
  try {
    execFileSync('git', ['-C', dir, 'stash', 'push', '--include-untracked', '-m', message], {
      encoding: 'utf8', timeout: 60000, stdio: ['ignore', 'pipe', 'pipe'],
    });
    console.log(`Set aside ${paths.length} uncommitted path(s) ${label} left in ${dir} after ${reason} as git stash "${message}" — recover with 'git -C ${dir} stash list'`);
  } catch (e) {
    console.error(`Could not stash the ${paths.length} uncommitted path(s) ${label} left in ${dir} after ${reason}: ${e.message} — a later task on ${task.repo} will start from a dirty tree`);
  }
}

// stopAgentForTaskExit ends the AGENT, not just the bookkeeping, when a task
// stops being ours (kubestellar/hive#5353 cause B).
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
// Between 2 and 3 — once the agent is stopped and before anything new can
// touch the tree — the task's shared checkout is swept for uncommitted
// leftovers (preserveTaskLeftovers, #7790). That happens on every branch of
// this function, including skipCLI and headless: the checkout stops being
// this task's however the agent went away.
//
// Re-entrancy: callers that have ALREADY stopped or relaunched the pane pass
// { skipCLI: true } and get only step 1 (plus the sweep) — nesting a second
// quit/relaunch into a relaunch already in flight is how double-launches
// happen. Headless mode has no pane at all; there the in-flight one-shot child
// is killed instead, matching what the revoke handler does.
//
// opts.reason names the exit in the relaunch log line.
//
// opts.task is the exiting task, for a caller that has already cleared
// currentTask (the revoke handler); everyone else leaves it unset and the
// sweep reads currentTask, which is still set at that point on those paths.
//
// opts.noRelaunch runs steps 1 and 2 but not step 3 — for the signal-shutdown
// path (kubestellar/hive#5655), where the PROCESS is exiting: relaunching
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
  const task = (opts && opts.task) || currentTask;
  // Step 1, always — even when the pane is deliberately left alone. A task
  // that is no longer ours must not keep its credential under any branch.
  dropTaskCredential();
  if (skipCLI) {
    // The caller already stopped the agent, so the tree is quiescent.
    preserveTaskLeftovers(task, reason);
    return;
  }
  if (CONTRIBUTOR_MODE === MODE_HEADLESS) {
    if (headlessChild) {
      try { headlessChild.kill('SIGKILL'); } catch (_) {}
      headlessChild = null;
      writeHeadlessStatus(HEADLESS_STATE_WAITING);
    }
    preserveTaskLeftovers(task, reason);
    return;
  }
  cliReady = false;
  quitLiveCLI();
  preserveTaskLeftovers(task, reason);
  if (noRelaunch) return;
  try {
    console.log(`Relaunching ${BACKEND} after ${reason}: ${relaunchCLI()}`);
  } catch (e) {
    cliReadyFailed = true;
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
// Pane fingerprint captured at the last IDLE_COMPLETE tick that credited the
// grace counter (#6775). The chrome-idle path infers "the agent finished" from
// a pane classified IDLE_COMPLETE — but classification is a per-frame read.
// A backend that renders a busy frame classifiers do not recognise as activity
// (pi's progress percentages are the observed case) satisfies IDLE_COMPLETE
// while the transcript is still growing: three consecutive misreads then end
// a task that was actively producing output. Requiring the pane to be
// byte-identical between consecutive credited ticks is what separates "still
// producing output the classifier does not see" from "actually finished with
// no verdict emitted": a pane whose fingerprint changed between ticks CANNOT
// be idle, whatever classifyPane() said about either frame in isolation.
let chromeIdleFingerprint = null;
let taskAgentActivityObserved = false;
let deliveredAgentActivityBaseline = new Map();

function agentActivityLineKey(line) {
  const s = String(line || '').trim();
  if (!s) return null;
  if (detectCompletionVerdict([s])) return s;
  if (/https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/pull\/\d+/.test(s)) return s;
  if (/^[●⏺]\s+\S/.test(s)) return s;
  if (/^[•·▸]\s+(?!(?:Working|Running|Executing|Thinking)\b)\S/i.test(s)) return s;
  return null;
}

function agentActivityCounts(lines) {
  const counts = new Map();
  for (const line of Array.isArray(lines) ? lines : String(lines || '').split('\n')) {
    const key = agentActivityLineKey(line);
    if (!key) continue;
    counts.set(key, (counts.get(key) || 0) + 1);
  }
  return counts;
}

function recordTaskAgentActivity(lines) {
  const counts = agentActivityCounts(lines);
  for (const [key, count] of counts) {
    if (count > (deliveredAgentActivityBaseline.get(key) || 0)) {
      taskAgentActivityObserved = true;
      return true;
    }
  }
  return taskAgentActivityObserved;
}

function resetTaskAgentActivity(baselineLines) {
  taskAgentActivityObserved = false;
  deliveredAgentActivityBaseline = agentActivityCounts(baselineLines || []);
}

// recordChromeIdleTick advances (or resets) the grace counter and reports
// whether chrome alone has now earned the right to end the task.
//
// PURE with respect to the pane fingerprint: it takes the already-captured
// lines and never reads the pane itself. paneStalled() is destructive — the
// first call seeing new output consumes it (#5333) — so nothing on the tick
// path may take a second reading.
//
// #6775: `currentFingerprint`, when provided, gates the increment on the pane
// being byte-identical to the last credited tick. classifyPane() is a
// per-frame reading, and pi's busy pane can render frames that read as
// IDLE_COMPLETE (a `\d+\.\d+%` progress line satisfies both hasIdlePrompt and
// hasCompletionMarker with no working-verb match). Three such frames in a row
// used to end a task in flight. Requiring the FINGERPRINT to be unchanged
// between credited ticks means a pane still producing output — however the
// classifier reads any single frame — can never fire chrome_idle: the bytes
// moved. A frame with a fresh fingerprint restarts the count at 1 and adopts
// that fingerprint as the new baseline; a matching frame advances toward the
// full window. Callers that cannot supply a fingerprint (the existing #5376
// regression test drives this directly, without a pane) omit the argument
// and get the pre-#6775 behaviour, which is safe: production always supplies
// one.
function recordChromeIdleTick(idleWithoutVerdict, currentFingerprint) {
  if (!idleWithoutVerdict) {
    chromeIdleTicks = 0;
    chromeIdleFingerprint = null;
    return false;
  }
  if (typeof currentFingerprint === 'string' && currentFingerprint !== '') {
    if (chromeIdleFingerprint !== null && currentFingerprint !== chromeIdleFingerprint) {
      // Pane content moved between two IDLE_COMPLETE readings — output was
      // produced, so this pane is not idle regardless of what classifyPane()
      // said about either frame. Restart the count with this frame as the new
      // baseline: the pane may still settle, but nothing so far has held long
      // enough to be trusted.
      //
      // #7662: say WHAT moved. A backend whose idle chrome repaints every
      // tick — a clock, a token meter, a toast — restarts this count forever,
      // and the log then shows `1/3` on every tick with no way to tell a
      // clock from work. The differing rows are what makes that one look to
      // diagnose and one small change to mask for that backend.
      logChromeIdleRestart(chromeIdleFingerprint, currentFingerprint);
      chromeIdleTicks = 1;
      chromeIdleFingerprint = currentFingerprint;
      return chromeIdleTicks >= CHROME_IDLE_GRACE_TICKS;
    }
    chromeIdleFingerprint = currentFingerprint;
  }
  chromeIdleTicks++;
  return chromeIdleTicks >= CHROME_IDLE_GRACE_TICKS;
}

function resetChromeIdleGrace() {
  chromeIdleTicks = 0;
  chromeIdleFingerprint = null;
}

// How many differing rows per side logChromeIdleRestart quotes. Enough to see
// a clock or a meter; a whole-frame repaint is summarised by count instead.
const CHROME_IDLE_DIFF_MAX_LINES = 3;

// paneFrameDiff returns the rows present in one fingerprint but not the other,
// each side capped at CHROME_IDLE_DIFF_MAX_LINES, plus the uncapped counts.
// Set difference rather than a positional diff on purpose: a single new row
// shifts every row below it, and a positional diff would then report the whole
// frame as changed — which is exactly the case this exists to see through.
function paneFrameDiff(previousFingerprint, currentFingerprint) {
  const prev = String(previousFingerprint || '').split('\n');
  const cur = String(currentFingerprint || '').split('\n');
  const prevSet = new Set(prev);
  const curSet = new Set(cur);
  const removed = prev.filter(l => !curSet.has(l));
  const added = cur.filter(l => !prevSet.has(l));
  return {
    removed: removed.slice(0, CHROME_IDLE_DIFF_MAX_LINES),
    added: added.slice(0, CHROME_IDLE_DIFF_MAX_LINES),
    removedCount: removed.length,
    addedCount: added.length,
  };
}

// logChromeIdleRestart (#7662) names the rows that differed between two
// consecutive idle-looking frames, so an operator reading `1/3` tick after
// tick can tell a repainting status line from real output without attaching
// to the pane. Rows are JSON-quoted so a change in trailing chrome or
// whitespace is visible rather than invisible.
function logChromeIdleRestart(previousFingerprint, currentFingerprint) {
  const diff = paneFrameDiff(previousFingerprint, currentFingerprint);
  const quote = (rows) => rows.map(r => JSON.stringify(r)).join(' ');
  const taskLabel = currentTask ? `Task ${currentTask.task_id}: ` : '';
  const parts = [];
  if (diff.addedCount) parts.push(`+${diff.addedCount} row(s): ${quote(diff.added)}${diff.addedCount > diff.added.length ? ' …' : ''}`);
  if (diff.removedCount) parts.push(`-${diff.removedCount} row(s): ${quote(diff.removed)}${diff.removedCount > diff.removed.length ? ' …' : ''}`);
  console.log(`${taskLabel}idle-grace counter restarted at 1/${CHROME_IDLE_GRACE_TICKS} — the pane changed between two idle-looking checks (${parts.join('; ') || 'rows reordered'}). A row that changes every check with no work behind it is chrome to mask for ${BACKEND}, not progress (#7662).`);
}

let lastPaneFingerprint = null;
let lastPaneChangeAt = 0;
// How many CONSECUTIVE ticks paneStalled() has now returned true. Distinct
// from the fingerprint clock above: that clock says "how long has it been
// unchanged", this says "how many chances has the CLI had to prove otherwise
// since we first noticed". Reset by resetPaneStallClock() and by any tick
// where paneStalled() is false (new output resets the whole stall story).
let stallConfirmCount = 0;

// Transient-API-error nudge state (kubestellar/hive#5094), scoped to the
// CURRENT task: how many retries we have typed and when the last one went out.
// Both are reset at task start — a previous task's exhausted budget must not
// deny this one its retries.
let transientNudgeCount = 0;
let lastTransientNudgeAt = 0;

function resetTransientNudgeState() {
  transientNudgeCount = 0;
  lastTransientNudgeAt = 0;
  resetHumanPresenceEvidence();
}

// Autonomy-nudge state (kubestellar/hive#5281), scoped to the CURRENT task.
// Budget of exactly one: a question the agent re-asks AFTER being told to
// proceed autonomously is a question it genuinely cannot answer itself, and
// re-nudging it would loop until the max-duration ceiling. Once spent, the pane
// reports blocked_on_human exactly as it does today.
let autonomyNudgeSent = false;

function resetAutonomyNudgeState() {
  autonomyNudgeSent = false;
}

// Post-verdict review state (hivecommons/hive#7759), scoped to the CURRENT
// task. Budget of exactly one: an advisor that reviews every turn will always
// have something new to say, so the second HIVE_VERDICT is final no matter
// what appears under it. `lastTickConcernLines` is the set of concern lines
// anywhere on the pane at the previous tick — the follow-up only fires for
// notes that were not there then, so a note from mid-task can never re-open a
// finished task.
// #7879: the boolean latch became a counter — the second follow-up is
// reserved for a NEW ⟦blocker⟧ and there is never a third
// (POST_VERDICT_REVIEW_MAX_FOLLOWUPS). `postVerdictReviewRequested` reads as
// "at least one sent", so every pending/answered check is unchanged.
let postVerdictReviewCount = 0;
let postVerdictReviewRequested = false;
let lastTickConcernLines = new Set();
// #7862: whether this task's one PR-less-complete follow-up has been spent.
let prClaimFollowUpRequested = false;
// #7907: the verdict LINE currently on the pane and when a tick first saw it.
// A different line (the second verdict after a #7759 follow-up) starts a new
// sighting; the same line ages toward VERDICT_SETTLE_MS.
let verdictFirstSeen = null;

function resetPostVerdictReviewState() {
  postVerdictReviewCount = 0;
  postVerdictReviewRequested = false;
  lastTickConcernLines = new Set();
  prClaimFollowUpRequested = false;
  verdictFirstSeen = null;
}

// noteVerdictSighting records that `line` is on the pane now and reports
// whether it has been there long enough to judge (#7907). The first sighting
// of a line is never settled: the whole point is that the tick which
// discovers the verdict is the one that captured too early. Called once per
// tick, ahead of every branch that acts on the verdict, so the follow-up
// (#7759), the PR-claim follow-up (#7862) and the finalization all read the
// settled pane rather than the streaming one.
function noteVerdictSighting(line) {
  const now = Date.now();
  if (!verdictFirstSeen || verdictFirstSeen.line !== line) {
    verdictFirstSeen = { line, at: now };
    return VERDICT_SETTLE_MS <= 0;
  }
  return now - verdictFirstSeen.at >= VERDICT_SETTLE_MS;
}

// verdictSettlePending: the fast-path glance asks this before firing again
// for a line it has already handed to a tick that deferred on it.
function verdictSettlePending(line) {
  return !!verdictFirstSeen && verdictFirstSeen.line === line &&
    Date.now() - verdictFirstSeen.at < VERDICT_SETTLE_MS;
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
// previous tick (kubestellar/hive#5321).
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
  // #7779: the queue is per-task. If the relay no longer holds the task this
  // prompt was queued for — it was revoked or failed while the CLI was coming
  // up, and a task-exit path missed the discard — typing it would put the agent
  // to work on an issue nobody has a lease for. Drop it instead; the readiness
  // callback that called us has already advertised `ready` if the relay is
  // idle, so real work follows through the normal assignment path.
  const owner = currentTask ? currentTask.task_id : null;
  if (owner !== pendingTaskId) {
    discardPendingTask(`the relay now holds ${owner || 'no task'}, not the task it was queued for`);
    return;
  }
  const t = pendingTask;
  pendingTask = null;
  pendingTaskId = null;
  tmuxSendKeys(t);
}

function checkTmuxIdle() {
  return checkTmuxPaneState() === PANE_STATE_IDLE_COMPLETE;
}

const TASK_GRACE_PERIOD_MS = RELAY_TEST_TIMING ? 0 : 180000;
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

// ── What the PR review cycle is for, and what it used to review (#6664) ──────
//
// Every PR_REVIEW_EVERY_N completions the relay stops taking new issues and
// asks its agent to answer review comments on the PRs it has filed. Two flaws
// in the ten lines that did that meant it routinely reviewed NOTHING:
//
//   1. It scoped the review to the repo of the single task that had just
//      finished. A contributor working across eleven repos had PRs in ten of
//      them permanently invisible to this mechanism — the cadence is per-five-
//      completions, not per-repo, so coverage never catches up.
//   2. It fired on COMPLETIONS, not on PRs shipped. A task that correctly
//      concludes "nothing shippable" still advances the counter and guarantees
//      that repo has no new PR.
//
// Observed: a cycle whose five triggering completions were ALL no_work_needed
// ran `gh pr list --repo projectbluefin/utah --author @me --state open` against
// a repo with zero PRs, while twenty open PRs across eleven repos went
// unreviewed.
//
// prsShippedSinceReview counts PRs this relay actually opened since the last
// review cycle. It is the precondition, and it is what makes suggestion 4 of
// the report ("skip the cycle when there is nothing to review") fall out for
// free rather than needing another API call: a cycle now runs only when we know
// from our own records that at least one PR exists to be reviewed.
//
// Deliberately a COUNT, not a latch on the last completion: a PR shipped on
// completion 3 is still worth reviewing when completion 5 is the one that
// crosses the cadence. And because the counter keeps accumulating when a cycle
// is skipped, a quiet run of no_work_needed tasks defers the review rather than
// starving it — the next multiple of PR_REVIEW_EVERY_N picks it up.
let prsShippedSinceReview = 0;
// The repos those PRs went to, most recent last. Used for the operator log line
// and to give the synthetic task an honest `repo` field; the REVIEW itself is
// account-scoped, because "which of my PRs have comments" is not a repo-scoped
// question and scoping it to one repo is the bug.
let reposShippedSinceReview = [];

// authorizedRepos is the AUTHORIZATION BOUNDARY for the review cycle (#6908):
// every repository a hub in this session actually dispatched work for. Unlike
// reposShippedSinceReview it is never reset — a PR filed on completion 2 is
// still ours to follow up on at completion 40 — and unlike the account, it
// contains nothing a hub did not hand us.
//
// Populated from task_assign rather than from completions, because assignment
// is where the hub grants authority. A task that failed, or that ended
// no_work_needed, still means "this hub sent me here", and a PR opened during
// it is still in scope.
const authorizedRepos = new Set();

// isSafeRepoName reports whether `name` has the GitHub `owner/name` shape.
// The names in authorizedRepos end up interpolated verbatim into shell command
// lines the review prompt pre-authorizes ("Run these, and only these"), so a
// hub-supplied value must be proven to be an identifier — not a command
// fragment — before it can cross from data into an executable line (#6938).
const SAFE_REPO_NAME_RE = /^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?\/[A-Za-z0-9._-]+$/;
function isSafeRepoName(name) {
  return typeof name === 'string' && SAFE_REPO_NAME_RE.test(name);
}

// isSafeAuthorLogin reports whether `login` is a plausible GitHub login (or
// `@me`), for the same reason: it is rendered into the `--author` position of
// the pre-authorized command lines (#6938).
const SAFE_AUTHOR_LOGIN_RE = /^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(\[bot\])?$/;
function isSafeAuthorLogin(login) {
  return login === '@me' || (typeof login === 'string' && SAFE_AUTHOR_LOGIN_RE.test(login));
}

// recordAuthorizedRepo notes that a hub dispatched work for `repo`. Ignores
// blanks so a malformed assignment cannot widen the boundary to "", and
// rejects anything that is not shaped like `owner/name` so a hostile hub
// cannot smuggle shell syntax into the review cycle's command list (#6938).
function recordAuthorizedRepo(repo) {
  const name = typeof repo === 'string' ? repo.trim() : '';
  if (!name) return;
  if (!isSafeRepoName(name)) {
    console.error(`Ignoring authorized-repo candidate that is not shaped like owner/name: ${JSON.stringify(name)}`);
    return;
  }
  authorizedRepos.add(name);
}

// buildReviewPrompt renders the PR review cycle's prompt (#6664, rescoped by
// #6908).
//
// SCOPED TO AUTHORIZED REPOS, NOT TO THE ACCOUNT AND NOT TO ONE REPO.
//
// #6664 correctly identified that scoping the review to the single repo of the
// last-finished task left PRs in ten of eleven repos permanently unreachable —
// the cadence is per-five-completions, not per-repo, so coverage never catches
// up. But its fix moved from too narrow straight to `gh search prs --author
// @me`, which is the whole GitHub account, skipping the correct scope in
// between.
//
// That matters because of what the next sentence of this prompt says: address
// the feedback, PUSH FIXES, and respond. An account-wide sweep therefore ends
// with an agent pushing commits to whatever the token's human owner happens to
// have open — PRs they wrote by hand, in repositories no hub in this session
// manages. Observed live (#6908): a session configured against two hubs
// enumerated 26 PRs across five unrelated repositories, and was stopped by the
// operator before it acted on them.
//
// Two mechanics made that more than a scoping preference, and both are why the
// fix is HERE rather than only in the wrapper:
//
//   1. `gh search prs` was not covered by the wrapper's --author identity
//      check, which gated on `list` (bin/gh-wrapper.sh). Moving the cycle from
//      `gh pr list` to `gh search prs` moved it off the path #3072/#3096 added
//      without that being visible at the call site. #6908 closes that hole too,
//      but a prompt that only works because a wrapper says no is a prompt that
//      asks for the wrong thing.
//   2. `@me` resolves to the TOKEN'S USER — the human operator — because
//      contributor mode deliberately keeps server-side resolution (#4044
//      rewrites to a bot identity only for staff agents). So `--author @me` is
//      precisely what surfaced human-authored PRs.
//
// So: one `gh pr list --repo <repo> --author <login> --state open` per repo a
// hub actually assigned work for. That keeps #6664's real correction (the
// review spans every repo this relay works across, not just the last one),
// bounds it by authorization rather than by account, puts the calls back on the
// wrapper's author-checked path, and costs one API call per repo instead of one
// unbounded search.
//
// shippedRepos stays a HINT for ORDERING only — the repos with work landed
// since the last cycle have the freshest comments. It is a subset of
// authorizedRepos, so naming it narrows nothing.
//
// THE VERDICT LINE IS THE OTHER HALF. The old prompt ended "just say 'No PR
// comments to address.'" — prose, not a sentinel — so this cycle could only
// ever complete through the terminal-chrome heuristic that #5376 added
// HIVE_VERDICT to replace, and that #5353 documents as having produced thirteen
// separate issues. The review cycle was the one task type that never got the
// fix, purely because its prompt is assembled here instead of by the hub. The
// wording mirrors contributorTaskPrompt in src/pkg/dashboard/contribute_ws.go;
// keep the two in step.
//
// Returns '' when no repo is authorized yet, which the caller treats as "do not
// run a review". An empty scope must mean review NOTHING; falling back to the
// account is the bug.
function buildReviewPrompt(shippedRepos, authorized, login) {
  // Defense in depth (#6938): recordAuthorizedRepo already refuses malformed
  // names, but this function renders whatever set it is HANDED into command
  // lines the prompt tells the agent to run verbatim — so re-prove the shape
  // here rather than trust every caller forever.
  const hinted = Array.isArray(shippedRepos) ? shippedRepos.filter(isSafeRepoName) : [];
  const scope = Array.from(authorized || []).filter(isSafeRepoName);
  if (!scope.length) return '';

  // Hinted repos first so the agent starts where comments are likeliest, then
  // the rest of the authorized set. Order only — every repo below is listed.
  const ordered = hinted.filter(r => scope.includes(r))
    .concat(scope.filter(r => !hinted.includes(r)));

  // The token's own login, never `@me`. HIVE_CONTRIBUTOR_USERNAME is the
  // identity the hive issued this contributor; `@me` is whoever holds the
  // token, which on a PAT-backed session is the operator (#6908). Falling back
  // to `@me` keeps the cycle working where the variable is unset, and the
  // per-repo scope still bounds what that can reach.
  let author = (login || '').trim() || '@me';
  if (!isSafeAuthorLogin(author)) {
    // A login that is not shaped like a GitHub login must not reach the
    // command line either; @me keeps the cycle working, and the per-repo
    // scope still bounds what it can touch (#6938).
    console.error(`Review prompt ignoring malformed author login ${JSON.stringify(author)}; using @me`);
    author = '@me';
  }

  const commands = ordered
    .map(repo => `GH_TOKEN=$GH_TOKEN gh pr list --repo ${repo} --author ${author} --state open`)
    .join('\n');

  return 'Check the open PRs you have filed, in the repositories this hive has assigned you work in, ' +
    'for review comments. Run these, and only these:\n' +
    commands + '\n' +
    'Those repositories are the whole scope of this task. Do not search across your account, ' +
    'and do not read, comment on, or push to a PR in any other repository — a PR outside that list ' +
    'was not opened on this hive\'s behalf, even if you have access to it. ' +
    'For each PR with review comments, read the comments, address the feedback, push fixes, and respond. ' +
    'If no PRs have comments, say so and stop — do not look for other work. ' +
    'When you HAVE finished — every PR with comments is addressed, or there were none — print, as the very ' +
    'last thing you output and on a line by itself, in plain text, no Markdown formatting: ' +
    "'HIVE_VERDICT: complete — <short reason>'. Print it exactly once, only when you are actually done.";
}

// ── Local-only tasks (kubestellar/hive#5715) ────────────────────────────────
//
// Almost every currentTask arrives in a task_assign and is backed by a
// server-issued LEASE. The PR review cycle is the one exception: after every
// PR_REVIEW_EVERY_N completions the relay builds a task for ITSELF, locally,
// with a `pr-review-` id and `number: 0`. The hub never assigned it, holds no
// lease for it, and has no work item behind it.
//
// That distinction was invisible to the reconnect path, and the cost was that a
// WebSocket flap ABORTED the review. On reconnect the relay re-asserted
// whatever was in currentTask, so it sent a task_progress for a task the hub
// had never heard of. The hub is right to refuse that — a resume is honoured
// only against a server-issued lease, the #C4 rule that stops a client claiming
// ownership of work it was not given (src/pkg/dashboard/contribute_ws.go) — so
// it answered `task_revoke: no active lease for this task`. The relay then
// treated the revoke as terminal: stop the agent, relaunch the CLI, ask for
// fresh work. The review was lost, every time, on a hive where 1006 closes are
// routine (29 in under four hours in the report).
//
// The fix is to know which tasks the hub owns. A local-only task:
//   - is never re-asserted to the hub (no task_accepted / task_progress), and
//   - cannot be ended by a revoke, because no hub ever owned it and so no other
//     contributor can have been given it.
//
// Both halves are checked at a choke point rather than per call site, so a
// future frame or a future locally-built task cannot quietly reintroduce the
// claim. task_complete and task_failed are deliberately NOT withheld: they
// assert nothing about ownership, the hub ignores them for a task it never
// assigned, and the `ready` that follows the review is what puts the
// contributor back in the rotation.
const LOCAL_TASK_ID_PREFIX = 'pr-review-';

// Frames that ASSERT this connection owns the named task. These are the only
// ones a hub can answer with a revoke, and the only ones withheld below.
const HUB_OWNERSHIP_FRAMES = new Set(['task_accepted', 'task_progress']);

// Belt and braces: the explicit marker set on the object the relay builds, and
// the id prefix. The marker alone would be lost by anything that reconstructs
// the task from the wire (or from HIVE_TASK_FILE); the prefix alone would be
// forgeable by a hub that chose that id. Requiring either, not both, keeps the
// guard working when only one survives.
function isLocalOnlyTaskId(taskID) {
  return typeof taskID === 'string' && taskID.startsWith(LOCAL_TASK_ID_PREFIX);
}

function isLocalOnlyTask(task) {
  return !!task && (task.synthetic === true || isLocalOnlyTaskId(task.task_id));
}

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
//
// opts.skipCLI (kubestellar/hive#5353) says the CALLER has already dealt with
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
  // #7779: a prompt still queued for this task must not be typed into the
  // relaunched CLI after the task has been handed back.
  discardPendingTask('the task failed');
  currentTask = null;
  releaseQuotaPoolReservation();
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

// finishCurrentTask is the completion counterpart of failCurrentTask: it stops
// the agent, reports task_complete, clears the per-task state, and either
// starts the periodic PR-review cycle or advertises `ready`. The caller has
// already decided the task is complete and with what evidence; this owns
// everything that must happen the same way whichever signal decided it.
//
// Extracted from progressTick() for #7662 so the progress-lease expiry can
// complete a task it can prove finished (a PR this task opened is on the pane)
// through exactly the same path as the tick loop, rather than a second copy of
// the bookkeeping that would drift.
//
//   completionSignal — 'verdict' or 'chrome_idle' (the hub's vocabulary).
//   summary          — one line for the hub's activity feed.
//   tmuxLines        — the display tail captured BEFORE the agent is stopped,
//                      so the hub's evidence is the agent's output and not
//                      launch chrome.
//   prURL            — the PR resolveTaskPR() attributed to this task, or ''.
//   noWork           — the no_work_needed verdict object, or null.
// postUnaddressedNotesComment leaves the advisor notes the agent did not get
// to on the PR, as ONE comment, so the reviewer sees exactly what the advisor
// saw (hivecommons/hive#7879). Authenticated with the task credential from
// GH_TOKEN_CACHE when present (it is about to be dropped), else gh's ambient
// auth. Best-effort and bounded: a gh that is missing, offline or refused
// costs a log line, never the completion.
function postUnaddressedNotesComment(prURL, notes) {
  let token = null;
  try { token = fs.readFileSync(GH_TOKEN_CACHE, 'utf8').trim() || null; } catch (_) {}
  const body = `### ${UNADDRESSED_ADVISOR_NOTES_HEADING}\n\n` +
    'The contributor\'s advisor posted these after the final verdict; the agent had no further turn to address them. Recorded here for the reviewer.\n\n' +
    notes.map(n => `- ${n}`).join('\n') +
    '\n\n<sub>— hive contributor relay (#7879)</sub>';
  try {
    execSync(`gh pr comment ${shellQuote(prURL)} --body ${shellQuote(body)} 2>/dev/null`, {
      encoding: 'utf8',
      timeout: 20000,
      env: token ? { ...process.env, GH_TOKEN: token } : process.env,
    });
    console.log(`Posted ${notes.length} unaddressed advisor note(s) as a comment on ${prURL} (#7879)`);
  } catch (e) {
    console.error(`Could not post the unaddressed advisor notes to ${prURL}: ${(e && e.message) || 'unknown error'} — they are still in the task_complete summary`);
  }
}

// BLOCKED_WORKFLOW_LABEL is the hub's canonical "waiting on something outside
// this repo" overlay: an issue carrying it is withheld from every offer
// surface until a human removes it (blockedWorkflowLabel in
// src/pkg/dashboard/contribute_admission.go). Keep the two spellings in sync.
const BLOCKED_WORKFLOW_LABEL = 'blocked';

// hubGrantsIssuesWrite reports whether the credential this hub mints for the
// task can write to issues — the hub says so on auth_ok (`permissions`, per
// trust tier). Without it the label call would only fail with a 403, so the
// relay does not try and the hub's cooldown is the whole hold.
function hubGrantsIssuesWrite(hub) {
  return !!hub && Array.isArray(hub.permissions) && hub.permissions.includes('issues:write');
}

// markIssueBlocked closes the loop on a `blocked` verdict (hivecommons/hive
// #7924): it applies BLOCKED_WORKFLOW_LABEL to the task's issue with the task
// credential, so the hub's existing admission gate takes over from the
// verdict's cooldown and the finding survives on GitHub for a human, the
// scanner, or another spoke — instead of living only in this hub's ledger.
// The agent is asked by the task prompt to leave the reason as a comment in
// its own attribution style; this is the half that needs no model.
//
// Lifting the label stays human: the relay never removes it.
//
// Only a GitHub-backed issue task qualifies (a Linear/Jira item has no GitHub
// issue to label; a review cycle has no issue at all), and only when the hub
// granted issues:write. Best-effort and bounded like postUnaddressedNotesComment:
// a gh that is missing, offline or refused costs a log line, never the
// completion. gh refuses to add a label the repository does not define, so a
// first failure creates the label and retries exactly once.
function markIssueLabel(task, reason, label, description, issueTag) {
  label = String(label || '').trim();
  if (!label) {
    console.log(`Task ${task && task.task_id ? task.task_id : '(unknown)'}: ${issueTag} verdict configured with no label — the hub's cooldown holds the issue`);
    return;
  }

  if (!task || task.kind !== 'issue' || !task.repo || !(task.number > 0) || task.external_id) return;
  const hub = task._hub || hubs[activeHubIndex];
  if (!hubGrantsIssuesWrite(hub)) {
    console.log(`Task ${task.task_id}: ${issueTag} verdict on ${task.repo}#${task.number}, but the task credential does not carry issues:write — leaving the '${label}' label to a human; the hub's cooldown holds the issue`);
    return;
  }
  let token = null;
  try { token = fs.readFileSync(GH_TOKEN_CACHE, 'utf8').trim() || null; } catch (_) {}
  const env = token ? { ...process.env, GH_TOKEN: token } : process.env;
  const issueURL = `https://github.com/${task.repo}/issues/${task.number}`;
  const addLabel = () => execSync(
    `gh issue edit ${shellQuote(issueURL)} --add-label ${shellQuote(label)} 2>&1`,
    { encoding: 'utf8', timeout: 20000, env });
  const describe = e => ((e && (e.stdout || e.message)) || 'unknown error').toString().trim();
  try {
    addLabel();
  } catch (first) {
    try {
      execSync(
        `gh label create ${shellQuote(label)} --repo ${shellQuote(task.repo)} ` +
        `--description ${shellQuote(description)} ` +
        '--color d93f0b 2>&1',
        { encoding: 'utf8', timeout: 20000, env });
      addLabel();
    } catch (second) {
      console.error(`Could not apply the '${label}' label to ${task.repo}#${task.number}: ${describe(first)}; after creating the label: ${describe(second)} — the hub's cooldown still holds the issue`);
      return;
    }
  }
  console.log(`Applied the '${label}' label to ${task.repo}#${task.number}: ${reason || '(no reason given)'} — a human lifts it when the condition clears`);
}

function markIssueBlocked(task, reason) {
  markIssueLabel(task, reason, BLOCKED_WORKFLOW_LABEL, 'Waiting on something outside this repository; not contributor work until a human clears the label', 'blocked');
}

function markIssueNeedsDecision(task, reason) {
  const hub = (task && task._hub) || hubs[activeHubIndex];
  const label = hub && typeof hub.contributeNeedsDecisionLabel === 'string' ? hub.contributeNeedsDecisionLabel : DEFAULT_NEEDS_DECISION_LABEL;
  markIssueLabel(task, reason, label, 'Waiting on a maintainer decision; not contributor work until a human clears the label', 'needs_decision');
}

function markIssueAlreadyDone(task, reason) {
  if (!task || task.kind !== 'issue' || !task.repo || !(task.number > 0) || task.external_id) return;
  const hub = task._hub || hubs[activeHubIndex];
  if (!hubGrantsIssuesWrite(hub)) {
    console.log(`Task ${task.task_id}: already-done verdict on ${task.repo}#${task.number}, but the task credential does not carry issues:write — leaving the '${ALREADY_DONE_WORKFLOW_LABEL}' label to the hub or a human; the hub's already-done hold suppresses re-offer (#8477)`);
    return;
  }
  let token = null;
  try { token = fs.readFileSync(GH_TOKEN_CACHE, 'utf8').trim() || null; } catch (_) {}
  const env = token ? { ...process.env, GH_TOKEN: token } : process.env;
  const issueURL = `https://github.com/${task.repo}/issues/${task.number}`;
  const addLabel = () => execSync(
    `gh issue edit ${shellQuote(issueURL)} --add-label ${shellQuote(ALREADY_DONE_WORKFLOW_LABEL)} 2>&1`,
    { encoding: 'utf8', timeout: 20000, env });
  const describe = e => ((e && (e.stdout || e.message)) || 'unknown error').toString().trim();
  try {
    addLabel();
  } catch (first) {
    try {
      execSync(
        `gh label create ${shellQuote(ALREADY_DONE_WORKFLOW_LABEL)} --repo ${shellQuote(task.repo)} ` +
        `--description ${shellQuote('Hive contributor found this issue already resolved; remove if work remains')} ` +
        '--color 8250df 2>&1',
        { encoding: 'utf8', timeout: 20000, env });
      addLabel();
    } catch (second) {
      console.error(`Could not apply the '${ALREADY_DONE_WORKFLOW_LABEL}' label to ${task.repo}#${task.number}: ${describe(first)}; after creating the label: ${describe(second)} — the hub's already-done hold still suppresses re-offer (#8477)`);
      return;
    }
  }
  console.log(`Applied the '${ALREADY_DONE_WORKFLOW_LABEL}' label to ${task.repo}#${task.number} (#8477): ${reason || '(no reason given)'} — a human removes it if work remains`);
}

function finishCurrentTask({ completionSignal, summary, tmuxLines, prURL, noWork, unaddressedNotes = [] }) {
  if (!currentTask) return;
  // #7879: the PR comment must go out BEFORE stopAgentForTaskExit drops the
  // task credential below — it is posted with the same token the agent used.
  if (prURL && unaddressedNotes.length) postUnaddressedNotesComment(prURL, unaddressedNotes);
  // #7924: same ordering for the blocked label — it is applied with the task
  // credential, which is about to be dropped.
  if (noWork && noWork.verdict === HIVE_VERDICT_BLOCKED) markIssueBlocked(currentTask, noWork.reason);
  if (noWork && noWork.needsDecision) markIssueNeedsDecision(currentTask, noWork.reason);
  if (isAlreadyDoneNoWork(noWork)) markIssueAlreadyDone(currentTask, noWork.reason);
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
  // is already true. tmuxLines was captured by the caller, so the evidence the
  // hub receives is still the agent's own output and not launch chrome.
  //
  // bob is exempt from the quit half: it is not a persistent REPL and has
  // already exited at the end of its turn, so the pane is a bare shell and
  // there is nothing to interrupt — sending Ctrl-C at that shell and then
  // racing the bob-specific relaunch below is how a pane ends up with two
  // launches in flight. Its credential is still dropped.
  const bobAlreadyExited = BACKEND === 'bob' && !bobIsRunning();
  stopAgentForTaskExit({ skipCLI: bobAlreadyExited });
  send({ type: 'task_complete', seq: nextSeq(), task_id: currentTask.task_id, task_gen: currentTask.task_gen, result: 'completed', summary, tmux_output: tmuxLines, pr_url: prURL, completion_signal: completionSignal, ...verdictWireFields(noWork) });
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
  // #6664: a review cycle's own completion must not count as having shipped.
  // A review pushes fixes to PRs that already exist, so any PR URL on its
  // pane is one it was READING, and counting it would let each cycle re-arm
  // the next off its own output — a review loop with no new work behind it.
  const completedWasReviewCycle = isLocalOnlyTask(currentTask);
  // #7779: nothing queued for the task that just ended may outlive it. (A task
  // completes only after its prompt was delivered, so this is normally empty;
  // the review-cycle prompt queued below is for the NEXT task and is unaffected.)
  discardPendingTask('the task completed');
  currentTask = null;
  releaseQuotaPoolReservation();
  taskAssignedAt = 0;
  if (progressInterval) { clearInterval(progressInterval); progressInterval = null; }
  if (taskTimeoutHandle) { clearTimeout(taskTimeoutHandle); taskTimeoutHandle = null; }
  tasksCompletedCount++;
  // #6664: record what this completion actually SHIPPED, which is the thing
  // the review cycle exists to follow up on. A completion is not a PR: the
  // counter used to conflate the two, so five consecutive no_work_needed
  // verdicts scheduled a review of a repo with nothing in it.
  if (prURL && !completedWasReviewCycle) {
    prsShippedSinceReview++;
    if (completedRepo && !reposShippedSinceReview.includes(completedRepo)) {
      reposShippedSinceReview.push(completedRepo);
    }
  }
  const reviewPrompt = (tasksCompletedCount % PR_REVIEW_EVERY_N === 0 && prsShippedSinceReview > 0)
    ? buildReviewPrompt(reposShippedSinceReview, authorizedRepos, CONTRIBUTOR_LOGIN)
    : '';
  if (reviewPrompt) {
    const shippedRepos = reposShippedSinceReview.slice();
    console.log(`PR review cycle (${tasksCompletedCount} tasks completed, ` +
      `${prsShippedSinceReview} PR(s) shipped since the last review in ${shippedRepos.join(', ') || 'no repo'}) — ` +
      `checking open PRs in ${authorizedRepos.size} authorized repo(s): ${Array.from(authorizedRepos).join(', ')}`);
    prsShippedSinceReview = 0;
    reposShippedSinceReview = [];
    // `synthetic: true` is the explicit half of isLocalOnlyTask() (#5715):
    // this object is built HERE, by us, and no hub holds a lease for it. The
    // `pr-review-` id prefix says the same thing and is what survives a
    // round-trip through HIVE_TASK_FILE, but stating it on the object is what
    // makes the property legible at the one place it becomes true.
    //
    // `repo` is the most recent repo we actually shipped to (#6664) rather
    // than "whichever repo the fifth task happened to be in". It is local
    // bookkeeping — taskKey() and the log line — and the review itself is not
    // scoped to it.
    const reviewRepo = shippedRepos[shippedRepos.length - 1] || completedRepo;
    currentTask = { task_id: `${LOCAL_TASK_ID_PREFIX}${Date.now()}`, kind: 'review', repo: reviewRepo, number: 0, title: 'Review open PRs for comments', synthetic: true };
    taskAssignedAt = Date.now();
    tmuxSendKeys(reviewPrompt);
    startProgressReporting();
  } else {
    if (tasksCompletedCount % PR_REVIEW_EVERY_N === 0) {
      // Say why the cycle did not run. Silence here reads as "the review
      // cadence is broken"; it is doing exactly what it should. Name the
      // actual reason: "no PRs shipped" and "no repo authorized" are
      // different states and an operator debugging a quiet cycle needs to
      // know which one they are in.
      const reason = prsShippedSinceReview > 0
        ? `no hub has assigned work for any repository in this session, so there is no authorized scope to review (#6908)`
        : `no PRs shipped since the last review, so there is nothing new to follow up on (#6664)`;
      console.log(`Skipping the PR review cycle at ${tasksCompletedCount} completions — ${reason}`);
    }
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
  // And the one-shot post-verdict review follow-up (#7759): the previous
  // task's spent budget, and the notes that were on its pane, say nothing
  // about this one.
  resetPostVerdictReviewState();

  armTaskProgressLease();

  progressInterval = setInterval(progressTick, PROGRESS_REPORT_INTERVAL_MS);
  startVerdictWatch();
}

// startVerdictWatch arms the #7841 fast path for the current task. It lives and
// dies with progressInterval: every path that stops the tick loop (task exit,
// hub disconnect, shutdown) nulls that handle, and the watch disarms itself on
// the next glance when it finds it gone — so none of those paths needs to know
// the watch exists.
function startVerdictWatch() {
  if (verdictWatchInterval) clearInterval(verdictWatchInterval);
  verdictFastPathLine = null;
  verdictWatchInterval = setInterval(verdictWatchTick, VERDICT_WATCH_INTERVAL_MS);
}

function stopVerdictWatch() {
  if (verdictWatchInterval) { clearInterval(verdictWatchInterval); verdictWatchInterval = null; }
}

// verdictWatchTick is the glance itself. Pure until it fires: one capture-pane
// read (captureTmuxLines does not touch the paneStalled() fingerprint — only
// checkTmuxPaneState()/paneStalled() do), the same detectCompletionVerdict and
// the same #5650 baseline exclusion the tick loop applies. A verdict it has not
// acted on yet runs progressTick() now and re-phases the regular interval so
// the next scheduled tick is a full period away rather than moments later. The
// verdict LINE is remembered, not a boolean: after a #7759 review follow-up the
// agent prints a second, different verdict, and that one deserves the fast
// path too. Whether the tick finalizes is entirely progressTick's call — a
// pending review follow-up or an API-error pane still refuse it exactly as on
// a scheduled tick.
function verdictWatchTick() {
  if (!progressInterval) { stopVerdictWatch(); return; }
  if (!currentTask) return;
  if (CONTRIBUTOR_MODE !== MODE_HEADLESS && !taskPromptDelivered) return;
  // Inside the task grace period progressTick() judges nothing, so firing it
  // would only spend this verdict's one fast-path run on a no-op. Leave the
  // line unconsumed and glance again once the grace period is over.
  if (Date.now() - taskAssignedAt < TASK_GRACE_PERIOD_MS) return;
  const paneVerdict = detectCompletionVerdict(captureTmuxLines(PR_SCAN_LINES));
  if (!paneVerdict || paneVerdict.line === deliveredVerdictBaseline) return;
  if (paneVerdict.line === verdictFastPathLine) return;
  // #7907: a line the tick has seen but refused to judge yet (its settle has
  // not elapsed) is glanced at again next interval, not now. The line is
  // marked consumed only on the run that gets to judge it — the first run
  // records the sighting and defers, so it must not spend the fast path.
  if (verdictSettlePending(paneVerdict.line)) return;
  const settled = !!verdictFirstSeen && verdictFirstSeen.line === paneVerdict.line;
  if (settled) verdictFastPathLine = paneVerdict.line;
  console.log(`Task ${currentTask.task_id}: HIVE_VERDICT: ${paneVerdict.verdict} is on the pane — running the progress tick now instead of waiting up to ${Math.round(PROGRESS_REPORT_INTERVAL_MS / 1000)}s for the next one (#7841)`);
  progressTick();
  if (progressInterval) {
    clearInterval(progressInterval);
    progressInterval = setInterval(progressTick, PROGRESS_REPORT_INTERVAL_MS);
  }
}

// armTaskProgressLease (re)starts the max-duration timer from NOW.
//
// Called once at task start and again from every tick that observes forward
// progress, which is what turns MAX_TASK_DURATION_MS from a wall-clock budget
// into a lease (kubestellar/hive#5321). An agent producing output keeps its
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

  // #7662: look at the pane before calling the silence a failure. A pane that
  // has not changed for the whole lease window is silent for one of two
  // reasons — the agent hung, or the agent FINISHED and nothing in the tick
  // loop credited it. Observed live on OMP: the agent printed
  // `HIVE_VERDICT: complete` and opened a PR, the sentinel scrolled out of the
  // tick's window under OMP's post-turn chrome, the chrome-idle counter never
  // accrued because that chrome repaints, and this path handed the finished
  // task back as an `environment` failure half an hour later — the #4127/#4182
  // shape the chrome-idle branch's own comment says it exists to avoid.
  //
  // The two pieces of evidence the tick loop itself accepts are checked here,
  // from one deep capture, in the same order and with the same guards:
  //
  //   1. the agent's own sentinel, attributed to THIS task by the #5650
  //      baseline exactly as progressTick() attributes it. Reaching this path
  //      with a fresh verdict on the pane means the tick loop is not running
  //      (this timer exists for that case); the verdict is still the agent's
  //      statement and still ends the task.
  //   2. a PR resolveTaskPR() CONFIRMED as this task's own — opened by this
  //      contributor after the task started, in the assigned repo. A CONFIRMED
  //      PR is the one finding strong enough to outrank a verdict on the
  //      completion path (#6662); it is more than strong enough to outrank
  //      "nothing visibly happened". An UNVERIFIED candidate (gh offline,
  //      rate-limited) is not: the task still fails, but the URL rides in the
  //      reason so the Operations feed shows what to look at instead of a bare
  //      "not visibly working".
  //
  // The completion goes through finishCurrentTask(), so the hub sees the same
  // task_complete the tick loop would have sent — pr_url for it to verify and
  // link, completion_signal so the run log records that no verdict ended it.
  const paneScanLines = captureTmuxLines(PR_SCAN_LINES);
  const tmuxLines = paneScanLines.slice(-TMUX_TAIL_LINES);
  // Same exclusion the tick loop applies: a pane parked on an API error has
  // not completed, whatever verdict line sits above the error. classifyTmuxPane
  // is the pure classifier; checkTmuxPaneState() is not used here because it
  // re-captures the pane and can type into it (the goose retry).
  const leasePaneState = classifyTmuxPane(paneScanLines.join('\n'));
  const leaseApiErrorState = leasePaneState === PANE_STATE_TRANSIENT_API_ERROR ||
    leasePaneState === PANE_STATE_UNKNOWN_API_ERROR ||
    leasePaneState === PANE_STATE_FATAL_API_ERROR;
  const paneVerdict = leaseApiErrorState ? null : detectCompletionVerdict(paneScanLines);
  const completionVerdict = paneVerdict && paneVerdict.line !== deliveredVerdictBaseline ? paneVerdict : null;
  const prFinding = resolveTaskPR(paneScanLines, {
    repo: currentTask.repo,
    taskId: currentTask.task_id,
    taskStartedAt: taskAssignedAt,
    contributorLogin: CONTRIBUTOR_LOGIN,
  });
  const prConfirmed = !!(prFinding.url && prFinding.evidence && prFinding.evidence.status === PR_ATTRIBUTION_CONFIRMED);
  if (completionVerdict || prConfirmed) {
    const evidence = completionVerdict
      ? `HIVE_VERDICT: ${completionVerdict.verdict} is on the pane`
      : `PR ${prFinding.url} was opened by this task`;
    console.warn(`Task ${currentTask.task_id}: no pane change for ${MAX_TASK_DURATION_MS / 60000}min, but ${evidence} — the agent finished and the tick loop never credited it. Completing it instead of handing it back as an environment failure (#7662).`);
    resetChromeIdleGrace();
    cliRestartCounts.delete(taskKey(currentTask));
    const noWork = !completionVerdict || prFinding.suppressesVerdict || !isNoWorkVerdict(completionVerdict)
      ? null
      : completionVerdict;
    finishCurrentTask({
      completionSignal: completionVerdict ? 'verdict' : 'chrome_idle',
      summary: completionVerdict
        ? 'Agent reported the task complete (HIVE_VERDICT; credited at progress-lease expiry)'
        : `Agent went quiet with its PR open (no verdict emitted; ${prFinding.url} opened by this task; credited at progress-lease expiry)`,
      tmuxLines,
      prURL: prFinding.url,
      noWork,
    });
    return;
  }

  failCurrentTask(
    `no observed progress for ${MAX_TASK_DURATION_MS / 60000}min — the agent CLI is not visibly working` +
      (prFinding.url ? ` (a PR is visible in the pane but could not be attributed to this task: ${prFinding.url})` : ''),
    { kind: 'environment' }
  );
}

// One iteration of the progress/completion/crash-detection loop. Extracted from
// the setInterval body so it can be driven deterministically from tests.
// handleTransientAPIError recovers a task whose turn ended in a retryable API
// failure (kubestellar/hive#5094).
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
  // mid-keystroke. Two signals must agree before a retry is withheld:
  // tmux saw input recently and the pane changed since the last presence check.
  const presence = tmuxSessionHumanPresence();
  const paneEdited = paneEditedSincePresenceCheck(tmuxLines);
  const deferralsLeft = presenceDeferralCount < HUMAN_PRESENCE_MAX_DEFERRALS;
  if (presence.active && paneEdited && deferralsLeft) {
    presenceDeferralCount++;
    const since = presence.idleMs === null
      ? 'client activity unknown'
      : `client sent input ${Math.round(presence.idleMs / 1000)}s ago`;
    console.warn(`Task ${currentTask.task_id} stopped on a retryable API error; ` +
      `${TMUX_SESSION} looks in use (${since}, and the pane changed since the ` +
      `last check), so not typing a retry ` +
      `(${presenceDeferralCount}/${HUMAN_PRESENCE_MAX_DEFERRALS} deferrals)`);
    send({
      ...progressBase,
      status: 'blocked_on_human',
      attention: true,
      summary: 'Agent stopped on a retryable API error; a human is active in the pane',
      ...progressModelFields(),
    });
    return;
  }
  if (presence.active && !paneEdited) {
    console.warn(`Task ${currentTask.task_id} stopped on a retryable API error; ` +
      `${TMUX_SESSION} reported client input but the pane is unchanged — that is ` +
      `the terminal answering the CLI's queries, not someone typing, so ` +
      `proceeding with the retry`);
  } else if (presence.active && !deferralsLeft) {
    console.warn(`Task ${currentTask.task_id} stopped on a retryable API error; ` +
      `${TMUX_SESSION} still looks in use, but ${HUMAN_PRESENCE_MAX_DEFERRALS} ` +
      `deferrals is the cap — retrying rather than parking the task on a signal ` +
      `we cannot verify`);
  } else if (presence.attached) {
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
// (kubestellar/hive#5281), and reports whether it did.
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

// maybeRequestPostVerdictReview asks the agent, once per task, to address the
// review notes its own CLI posted under the HIVE_VERDICT line and then to
// print the verdict again (hivecommons/hive#7759). Returns true when the
// follow-up went out and the tick must NOT finalize the task this time.
//
// The bound, stated once here because an advisor that reviews every turn
// would otherwise never let a task end:
//
//   - One follow-up per task, ever. The second verdict is final whatever
//     appears under it; there is no "notes arrived after the second verdict,
//     go again". The budget is spent BEFORE typing, so a send that throws is
//     not retried on the next tick — the task finalizes as it would have.
//   - Only ⟦blocker⟧/⟦concern⟧ triggers it, never ⟦nit⟧ (see POST_VERDICT_REVIEW_MARKERS).
//   - Only notes BELOW the verdict, and only ones that were not on the pane
//     at the previous tick. Nothing new since the verdict means the task
//     finalizes on this very tick, exactly as before this existed.
//   - The progress lease and the absolute deadline are untouched. The
//     follow-up neither extends nor resets either; if the agent burns the
//     remaining budget on the concern, the lease/stall paths book it exactly
//     as today — and lease expiry, which reads the pane itself (#7662),
//     completes on whichever verdict it finds.
//
// Backends without a POST_VERDICT_REVIEW_MARKERS entry never reach the send:
// for them a verdict finalizes on the tick it is read, unchanged.
function maybeRequestPostVerdictReview(paneScanLines, tmuxLines, verdict, previousConcerns) {
  if (!currentTask || !verdict) return false;
  const markers = POST_VERDICT_REVIEW_MARKERS[BACKEND];
  if (!markers) return false;
  if (postVerdictReviewCount >= POST_VERDICT_REVIEW_MAX_FOLLOWUPS) return false;

  let concerns = postVerdictConcerns(paneScanLines, verdict.line, markers)
    .filter(line => !previousConcerns.has(line));
  // #7879: the second follow-up is bought only by a NEW ⟦blocker⟧. A concern
  // or nit under the second verdict finalizes as before — a body-prose nit is
  // not worth a turn — but a late blocker of the kind that turned utah#131 /
  // testsuite#805 into real PRs gets one more, and never a third.
  const secondRound = postVerdictReviewCount > 0;
  if (secondRound) concerns = concerns.filter(line => markers.blocker && markers.blocker.test(line));
  if (concerns.length === 0) return false;

  // #7935: the notes the relay is asking about go INTO the message, so the
  // agent cannot match "advisor notes were posted" to notes it already
  // handled earlier in the turn. Both rounds get them; the second round's
  // `concerns` are already narrowed to the new ⟦blocker⟧s, so quoting them is
  // the same operation.
  const quotedNotes = postVerdictQuotableNotes(paneScanLines, verdict.line, markers, concerns);
  const message = buildPostVerdictReviewMessage(quotedNotes);

  postVerdictReviewCount++;
  postVerdictReviewRequested = true;
  console.log(secondRound
    ? `Task ${currentTask.task_id}: a NEW advisor ⟦blocker⟧ was posted under its re-printed HIVE_VERDICT — ` +
      `asking the agent a second and final time to address it and re-print the verdict (#7879): ${concerns.map(c => JSON.stringify(c.trim())).join(' ')}`
    : `Task ${currentTask.task_id}: ${concerns.length} advisor concern(s) were posted under its HIVE_VERDICT line — ` +
      `asking the agent once to address them and re-print the verdict (#7759): ${concerns.map(c => JSON.stringify(c.trim())).join(' ')}`);
  try {
    tmuxSendNudge(message);
  } catch (e) {
    console.error('Failed to send the post-verdict review follow-up; finalizing on the verdict as-is:', e.message);
    return false;
  }
  send({
    type: 'task_progress',
    seq: nextSeq(),
    task_id: currentTask.task_id,
    task_gen: currentTask.task_gen,
    status: 'working',
    summary: secondRound
      ? `Agent re-printed HIVE_VERDICT, then its advisor posted a new blocker under it; asked it a second and final time to address it and re-print the verdict`
      : `Agent printed HIVE_VERDICT, then its advisor posted ${concerns.length} concern(s) under it; asked it once to address them and re-print the verdict`,
    tmux_output: tmuxLines,
    ...progressModelFields(),
  });
  return true;
}

// ── A `complete` with no PR behind it (hivecommons/hive#7862) ───────────────
//
// The prompt defines `HIVE_VERDICT: complete` as "the PR is open". Observed
// live: a model printing `complete — PR opened` after thirty read-only tool
// calls — no branch, no commit, no push, no PR. resolveTaskPR() correctly
// found nothing, and the relay reported the completion anyway with the claim
// passed through as prose; the verdict text and the PR scan were never
// compared. The hub (which now books this shape as evidence-less) cannot fix
// the missing PR; only the agent can, and it is still sitting at its prompt.
//
// So: when a `complete` verdict whose reason CLAIMS a PR (PR_CLAIM_PATTERN)
// would finalize this tick and resolveTaskPR() attributes no PR to the task,
// tell the agent once — open the PR now, or downgrade to no_work_needed — and
// hold the finalization until it answers
// with a second HIVE_VERDICT (or goes idle, via the same pending mechanics
// as #7759). Whatever the second verdict says is final: a `complete` that
// still has no PR is reported as-is and the hub books it evidence-less.
//
// Bounds, shared with maybeRequestPostVerdictReview: once per task, budget
// spent before typing, lease and deadline untouched. It never fires for
// no_work_needed (that verdict is a conclusion in itself), never for a
// verdict with a PR, and never on an api-error pane (verdictCompletes is
// already false there).
//
// `prFinding` is the tick's single resolveTaskPR() result, shared with the
// finalization below it: the lookup has a per-tick gh budget and a log line
// per candidate, and must not run twice for one verdict.
function maybeRequestPRForClaimedComplete(tmuxLines, verdict, prFinding) {
  if (!currentTask || !verdict) return false;
  // Only an issue task's `complete` means "a PR is open". A review task
  // ("complete — no PR comments to address") legitimately ends with no new PR.
  if (currentTask.kind !== 'issue') return false;
  if (verdict.verdict !== HIVE_VERDICT_COMPLETE) return false;
  if (!PR_CLAIM_PATTERN.test(verdict.reason || '')) return false;
  if (prClaimFollowUpRequested) return false;
  if (prFinding && prFinding.url) return false;

  prClaimFollowUpRequested = true;
  console.warn(`Task ${currentTask.task_id}: HIVE_VERDICT: complete is on the pane${verdict.reason ? ` (${JSON.stringify(verdict.reason)})` : ''} but no PR for this task exists — ` +
    `asking the agent once to open it or downgrade to no_work_needed (#7862)`);
  try {
    tmuxSendNudge(PR_CLAIM_FOLLOWUP_MESSAGE);
  } catch (e) {
    console.error('Failed to send the missing-PR follow-up; finalizing on the verdict as-is:', e.message);
    return false;
  }
  send({
    type: 'task_progress',
    seq: nextSeq(),
    task_id: currentTask.task_id,
    task_gen: currentTask.task_gen,
    status: 'working',
    summary: 'Agent printed HIVE_VERDICT: complete but no PR for this task exists; asked it once to open the PR or downgrade to no_work_needed',
    tmux_output: tmuxLines,
    ...progressModelFields(),
  });
  return true;
}

function progressTick() {
  lastProgressTick = Date.now();
  if (!currentTask) return;

  // Keep this relay's claim on the shared quota pool alive while a task really
  // is in flight (kubestellar/hive#6953): the reservation is a lease so a
  // crashed relay's claim expires, but a long, healthy task must not let its
  // own lease lapse and let a peer double-book the reserve underneath it.
  refreshQuotaPoolReservation();

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

  // Never judge a task the agent has not been given (kubestellar/hive#5650).
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
  // One capture, two windows (#6667). Capturing the deep scrollback and slicing
  // its tail keeps the protocol payload byte-identical to before while giving
  // PR detection room to see a URL that has scrolled past the visible rows. It
  // deliberately does NOT add a second capture-pane call: the pane read is
  // destructive to the paneStalled() fingerprint (#5333).
  const paneScanLines = captureTmuxLines(PR_SCAN_LINES);
  const tmuxLines = paneScanLines.slice(-TMUX_TAIL_LINES);

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
  // Read from the already-captured deep scan: no second pane read, so the
  // destructive paneStalled() fingerprint (#5333) is untouched.
  //
  // #7662: the DEEP window (PR_SCAN_LINES), not the 15-row display tail. The
  // sentinel is the agent's last line, but it is not the pane's last line: a
  // backend draws its own chrome under it — OMP renders its Advisor notes, a
  // clipboard toast and the input box, thirteen-plus rows — and the sentinel
  // is out of a 15-row window before the next tick. Observed live: an OMP
  // agent printed `HIVE_VERDICT: complete` and opened a PR, the relay logged
  // "no HIVE_VERDICT yet" on every tick, and the progress lease handed the
  // finished task back as an environment failure 30 minutes later. The PR
  // scan has used this window since #6667 for the same reason. The #5650
  // baseline is captured from the same window at dispatch, so a previous
  // task's line deeper in scrollback is still recognised as stale.
  const paneVerdict = detectCompletionVerdict(paneScanLines);

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
  const hasTaskAgentActivity = paneState === PANE_STATE_WORKING
    ? (taskAgentActivityObserved = true)
    : recordTaskAgentActivity(paneScanLines);

  // #7759: the review notes a backend posts UNDER the verdict. Two things are
  // read here, every tick, whichever branch below returns:
  //
  //   - the concern lines on the pane right now, snapshotted so the NEXT tick
  //     can tell a note that just appeared from one that was already there
  //     (the previous snapshot is what the trigger below is filtered against);
  //   - whether a follow-up is outstanding and the agent has not yet answered
  //     it. While that is so, the verdict on the pane is the FIRST one — the
  //     agent is mid-turn on the notes — and finalizing on it would kill that
  //     turn. It is treated like a pane with no verdict yet: WORKING reports
  //     progress, and idle chrome accrues toward the chrome-idle completion,
  //     so an agent that addresses the notes but never re-prints the sentinel
  //     still ends through the same fallback as one that never printed it —
  //     with the first verdict's no_work_needed, if that is what it said,
  //     still carried to the hub.
  const reviewMarkers = POST_VERDICT_REVIEW_MARKERS[BACKEND];
  const previousConcerns = lastTickConcernLines;
  lastTickConcernLines = new Set(reviewMarkers ? paneScanLines.filter(l => reviewMarkers.concern.test(l)) : []);
  const secondVerdictPending = !!completionVerdict && (
    (postVerdictReviewRequested && !postVerdictReviewAnswered(paneScanLines)) ||
    (prClaimFollowUpRequested && !prClaimFollowUpAnswered(paneScanLines)));

  // Chrome-idle grace (#5376). classifyTmuxPane() saying IDLE_COMPLETE is now
  // only a hint; it must repeat across CHROME_IDLE_GRACE_TICKS ticks before it
  // may end a task on its own. A verdict short-circuits the wait entirely.
  //
  // #6775: the idle reading must also be STABLE across those ticks. Passing
  // the current pane fingerprint makes recordChromeIdleTick refuse to credit a
  // tick whose bytes differ from the previous credited one — a pane still
  // producing output cannot pretend to be idle just because classifyPane()
  // misread a busy frame (pi's progress percentages were the observed case).
  const idleWithoutVerdict = paneState === PANE_STATE_IDLE_COMPLETE && (!completionVerdict || secondVerdictPending);
  const chromeIdleGraceElapsed = recordChromeIdleTick(idleWithoutVerdict, paneFingerprint(tmuxLines));

  // ── The chrome-idle veto (#6717) ──────────────────────────────────────────
  //
  // chrome_idle infers "the agent finished" from a pane that has stopped
  // changing. That inference has one premise it never checked: that the agent
  // STARTED. When a task prompt is typed but never submitted — collapsed into
  // a paste placeholder, the Enters swallowed as content — the pane goes quiet
  // for the most conclusive reason there is, and the fallback read that silence
  // as success. Observed live in #6717: an issue booked COMPLETED with no
  // commit, no branch, no PR and the checkout untouched.
  //
  // A false completion is strictly worse than a false failure here. A failure
  // is re-offered; a completion parks the issue as done and takes it out of the
  // offer queue, where nothing will ever look at it again.
  //
  // Two independent signals are required, both of which the #6717 capture
  // shows and neither of which a real turn can produce:
  //
  //   1. the prompt is STILL collapsed in the input widget, and
  //   2. the pane has not changed by a single byte since the prompt was
  //      delivered — no spinner, no tool row, no prose, not even the CLI's own
  //      echo of the submitted prompt.
  //
  // Requiring both is what keeps this from ever failing a task that ran. A CLI
  // that echoes a submitted paste back into its transcript still satisfies (1)
  // forever, so (1) alone would fail every task on such a backend; and a pane
  // byte-identical to its pre-work state cannot belong to an agent that did
  // anything. Neither is a judgement about the WORK — only about whether any
  // work was ever started.
  //
  // Deliberately scoped to the chrome-idle path. A HIVE_VERDICT line is the
  // agent's own statement and needs no corroboration from the chrome; it also
  // cannot be on a pane that never changed, since the baseline suppression in
  // #5650 already removes the previous task's line.
  // Signal 1, from either side: the send path already failed to clear the
  // widget, or the pane still shows a collapsed paste sitting in it.
  const promptStillInWidget = !promptSubmissionConfirmed ||
    paneHoldsUnsubmittedPrompt(paneScanLines.join('\n'), BACKEND);
  // Signal 2. The deep capture is used for the widget above because
  // paneHoldsUnsubmittedPrompt() scopes itself to the input rows at its end;
  // the fingerprint compares the same TMUX_TAIL_LINES window the delivery
  // snapshot was taken from, so the two are the same pane read two ways.
  const nothingEverRan = idleWithoutVerdict &&
    promptStillInWidget &&
    !paneChangedSinceDelivery(tmuxLines);
  if (chromeIdleGraceElapsed && nothingEverRan) {
    console.error(`Task ${currentTask.task_id}: the pane has been idle for ${chromeIdleTicks} checks with the task prompt still unsubmitted in the ${BACKEND} input widget and NO output since delivery — the agent never ran this task. Reporting it FAILED so the hub re-offers the issue (#6717).`);
    resetChromeIdleGrace();
    // 'environment': this client's own runtime failed to hand the work over.
    // The agent never saw the task, so nothing about the task itself failed.
    failCurrentTask(`task prompt was never submitted to the ${BACKEND} CLI — it stayed collapsed in the input widget and the agent produced no output`, { kind: 'environment' });
    return;
  }

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
  const verdictCompletes = !!completionVerdict && !apiErrorState && !secondVerdictPending;

  // #7907: a verdict this tick is the FIRST to see is judged on a later
  // capture. The backend renders its end-of-turn advisor notes under the
  // verdict about a second after the line itself appears; a capture taken in
  // that second shows the verdict alone, and every branch below that reads
  // the notes — the #7759 follow-up, the #7879 recording — would read none.
  // Bounded at one extra tick (the #7841 glance re-fires once the settle has
  // elapsed); the verdict line is remembered, not a boolean, so a second
  // verdict after a follow-up settles on its own.
  if (verdictCompletes && !noteVerdictSighting(completionVerdict.line)) {
    // A deferred tick judged nothing, so it must not advance the #7759
    // concern snapshot either: the notes that render during the settle have
    // to read as NEW on the tick that finally looks, or the follow-up they
    // would have earned on a lucky timer phase is lost to an unlucky one.
    lastTickConcernLines = previousConcerns;
    console.log(`Task ${currentTask.task_id}: HIVE_VERDICT: ${completionVerdict.verdict} first seen on this capture — re-capturing after ${VERDICT_SETTLE_MS}ms before judging it, so notes the ${BACKEND} CLI renders at end of turn are on the pane (#7907)`);
    send({ type: 'task_progress', seq: nextSeq(), task_id: currentTask.task_id, task_gen: currentTask.task_gen, status: 'working', tmux_output: tmuxLines, ...progressModelFields() });
    return;
  }

  if (paneState === PANE_STATE_IDLE_COMPLETE && chromeIdleGraceElapsed && !verdictCompletes && !hasTaskAgentActivity) {
    resetChromeIdleGrace();
    failCurrentTask(`pane went idle before ${BACKEND} produced any task output; prompt may not have been submitted`, { kind: 'environment' });
    return;
  }

  // #7759: before a verdict ends the task, give the agent its one chance at
  // the review its CLI posted underneath it. Only a verdict that would
  // complete right now is eligible — the api-error exclusions above and the
  // stale-baseline suppression already applied — so this can never re-open a
  // task the relay would not otherwise have finalized on this tick.
  if (verdictCompletes && maybeRequestPostVerdictReview(paneScanLines, tmuxLines, completionVerdict, previousConcerns)) {
    return;
  }
  // Best-effort: the PR the agent opened, if one is attributable to this
  // task from its recent output, so the hub can distinguish "shipped a PR"
  // from "just went idle" and pick the right issue cooldown
  // (kubestellar/hive#2393 item 7). Resolved ONCE here, ahead of both the
  // #7862 follow-up and the finalization that share it. Empty when no PR is
  // found — the hub then applies the short cooldown.
  const finalizing = verdictCompletes || (paneState === PANE_STATE_IDLE_COMPLETE && chromeIdleGraceElapsed);
  const prFinding = finalizing
    ? resolveTaskPR(paneScanLines, {
      repo: currentTask.repo,
      taskId: currentTask.task_id,
      taskStartedAt: taskAssignedAt,
      contributorLogin: CONTRIBUTOR_LOGIN,
    })
    : null;

  // #7862: and its one chance to back a `complete` with the PR it implies.
  if (verdictCompletes && maybeRequestPRForClaimedComplete(tmuxLines, completionVerdict, prFinding)) {
    return;
  }

  if (finalizing) {
    // How this task ended, recorded so the hub and the operator can tell the
    // trustworthy signal from the fallback — and so per-backend sentinel
    // non-compliance is measurable rather than guessed at.
    const completionSignal = verdictCompletes ? 'verdict' : 'chrome_idle';
    console.log(`Task ${currentTask.task_id} completed — signal=${completionSignal}` +
      (verdictCompletes
        ? ` (HIVE_VERDICT: ${completionVerdict.verdict})`
        : (completionVerdict
          ? ` (pane idle for ${chromeIdleTicks} consecutive checks; the agent's HIVE_VERDICT: ${completionVerdict.verdict} was followed by a relay follow-up it was asked to answer, and it never re-printed the verdict — #7759/#7862)`
          : ` (pane idle for ${chromeIdleTicks} consecutive checks, no verdict emitted)`)));
    resetChromeIdleGrace();
    // Successful completion clears this work item's crash-retry budget.
    cliRestartCounts.delete(taskKey(currentTask));
    const prURL = prFinding.url;
    // #6717 item 4: make the weakest completion the loudest line in the log.
    // A chrome_idle completion with no verdict AND no PR is the exact shape of
    // the false completion in that issue, and the relay used to record it in
    // the same register as a clean one. It is not always wrong — an agent that
    // genuinely found nothing to do but never printed the sentinel lands here
    // too — so it is a warning to audit, not a failure: the veto above already
    // owns the cases the relay can actually prove.
    if (!verdictCompletes && !prURL) {
      console.warn(`Task ${currentTask.task_id} completed on chrome alone with no HIVE_VERDICT and no PR — nothing in the pane shows what this task produced. Audit this one (#6717).`);
    }
    // #3987: only report a no_work_needed verdict when no PR was shipped — a
    // visible PR contradicts "nothing shippable" (the hub would override the
    // claim with "shipped" anyway).
    //
    // #6662: "a visible PR" is the wrong test, and it inverted the inference
    // for the population #3987 was built for. Only a PR THIS TASK OPENED
    // contradicts "nothing shippable"; a PR a maintainer merged last month
    // corroborates it, and is in the pane precisely because the agent had to
    // cite it to justify the verdict. resolveTaskPR() draws that line.
    const noWork = prFinding.suppressesVerdict || !completionVerdict || !isNoWorkVerdict(completionVerdict)
      ? null
      : completionVerdict;
    if (noWork) console.log(`Detected ${noWork.verdict} verdict for ${currentTask.task_id}: ${noWork.reason || '(no reason)'}`);
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
    let completionSummary = noWork
      ? `Agent returned to idle (reported ${noWork.verdict})`
      : (verdictCompletes
        ? 'Agent reported the task complete (HIVE_VERDICT)'
        : `Agent returned to idle (no verdict emitted; pane idle for ${CHROME_IDLE_GRACE_TICKS} consecutive checks)`);
    // #7879: review notes under the verdict being finalized are, by
    // construction, ones the agent will not get another turn for. Record
    // them — in the summary the hub keeps, and on the PR if there is one —
    // instead of letting them die with the relaunch. Zero extra turns.
    const unaddressedNotes = verdictCompletes
      ? postVerdictNoteBlocks(paneScanLines, completionVerdict.line, reviewMarkers)
      : [];
    if (unaddressedNotes.length) {
      console.log(`Task ${currentTask.task_id}: ${unaddressedNotes.length} advisor note(s) under the final verdict were not addressed before completion — recording them (#7879)`);
      completionSummary += `\n\n${UNADDRESSED_ADVISOR_NOTES_HEADING}:\n${unaddressedNotes.map(n => `- ${n}`).join('\n')}`;
    }
    finishCurrentTask({ completionSignal, summary: completionSummary, tmuxLines, prURL, noWork, unaddressedNotes });
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
    // #7996: a login wall is blocked-on-human only while a human might turn
    // up. Unattended past the grace, it is an environment the CLI cannot work
    // in: hand the task back now (not at the 30-minute watchdog), keep the
    // pane as it is for whoever attaches, and stop advertising until they do.
    const paneText = tmuxLines.join('\n');
    if (paneShowsLoginRequiredError(paneText)) {
      loginWallTicks++;
      if (!paneHasPresentHuman() && loginWallTicks >= LOGIN_WALL_GRACE_TICKS) {
        const line = loginWallLine(paneText);
        enterLoginHold(line);
        failCurrentTask(
          `${BACKEND} CLI login expired — the CLI refuses every prompt until someone attaches and runs /login (${line}); ` +
            'not a fault of the task; this contributor is standing down until the pane shows a signed-in CLI',
          { skipReady: true, skipCLI: true, kind: 'environment' });
        return;
      }
      console.warn(`Task ${currentTask.task_id} is blocked on an expired ${BACKEND} login (${loginWallTicks}/${LOGIN_WALL_GRACE_TICKS} ticks before standing down${paneHasPresentHuman() ? '; a human is attached' : ''})`);
    } else {
      loginWallTicks = 0;
      console.warn(`Task ${currentTask.task_id} is blocked waiting for human input`);
    }
    send({
      type: 'task_progress',
      seq: nextSeq(),
      task_id: currentTask.task_id,
      task_gen: currentTask.task_gen,
      status: 'blocked_on_human',
      attention: true,
      summary: paneShowsLoginRequiredError(paneText)
        ? `Agent CLI login has expired — attach to ${TMUX_SESSION} and run /login`
        : 'Agent is waiting for human input in the tmux pane',
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
    //
    // QUOTA IS THE SEPARABLE CASE (#6541). Both halves of this bucket refuse the
    // task, but only quota refuses every OTHER task too, and only quota comes
    // with an expiry. Handing the task back and immediately advertising `ready`
    // — which is what this branch did for both — walked straight into the next
    // refusal, once per assignment, for the whole reset window. So for quota:
    // skipReady, park the loop, and say so in the reason the hub records, since
    // "an API failure a retry cannot clear" gives an operator nothing to act on.
    const quota = paneQuotaExhaustion(tmuxLines.join('\n'));
    if (quota) {
      enterQuotaHold(quota);
      failCurrentTask(
        `provider quota exhausted for ${BACKEND} — not a fault of this host; ` +
          `the relay is standing down for ${formatQuotaHoldRemaining(quotaHoldUntil - Date.now())} ` +
          `and will ask for work again after that (${quota.line})`,
        { kind: 'environment', skipReady: true }
      );
    } else {
      failCurrentTask(
        'agent stopped on an API failure a retry cannot clear (authorization or quota)',
        { kind: 'environment' }
      );
    }
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
        // #7760: the second model that reviews this contributor's work, when
        // its CLI runs one. Optional and additive; an older hub ignores it.
        ...advisorFields(),
        role: AGENT_ROLE,
        // Multi-session-per-account: additive, optional. An older hub ignores
        // this unknown field and treats the relay as a single session.
        session: AGENT_SESSION || undefined,
        ...currentKnowledgeState(),
        // #2547 declare half + #2567: additive, optional self-report of runtime
        // posture and protocol version. An older hub ignores these unknown fields.
        protocol_version: RELAY_PROTOCOL_VERSION,
        capabilities: detectCapabilities(),
      });
      break;

    case 'auth_ok':
      console.log(`Authenticated with ${hub.url} as ${sanitizeHubText(msg.contributor_id)} (tier: ${sanitizeHubText(msg.trust_tier)})`);
      recordHubSeen(hub);
      // #2567: the hub advertises its protocol version + capability set here. We
      // log them (forward-compatible: unknown/absent fields are simply skipped)
      // so a newer relay can adapt to what the deployed server supports instead
      // of probing. No behaviour is gated on them today.
      if (msg.protocol_version || (msg.server_capabilities && msg.server_capabilities.length)) {
        console.log(`Hub protocol ${sanitizeHubText(msg.protocol_version) || 'unversioned'}; capabilities: ${sanitizeHubText((msg.server_capabilities || []).join(', ')) || 'none'}`);
      }
      // #7932: the one server bound the relay cannot discover by behaving well
      // — exceeding it is answered with a connection close, not a reply. Take
      // the hub at its word when it states the limit, so a hub that raises its
      // ceiling raises the relay's with it and the two halves cannot drift.
      // A hub that says nothing keeps the 64 KiB default that has always held.
      hub.maxFrameBytes = Number.isFinite(msg.max_message_bytes) && msg.max_message_bytes > 0
        ? Math.max(msg.max_message_bytes - WS_FRAME_HEADROOM_BYTES, MIN_WS_FRAME_BYTES)
        : WS_FRAME_BYTES;
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
      printHubAnnouncementOnce(hub, msg.announcement);
      hub.authenticated = true;
      hub.authFailed = false;
      hub.connectionId = msg.connection_id || '';
      hub.serverCapabilities = Array.isArray(msg.server_capabilities) ? msg.server_capabilities.slice() : [];
      // #7924: the permission set the hub mints task credentials with, per
      // trust tier. Read by markIssueBlocked to decide whether a label call
      // can succeed at all. An older hub sends none → no label attempts.
      hub.permissions = Array.isArray(msg.permissions) ? msg.permissions.slice() : [];
      hub.contributeNeedsDecisionLabel = typeof msg.contribute_needs_decision_label === 'string' ? msg.contribute_needs_decision_label.trim() : DEFAULT_NEEDS_DECISION_LABEL;
      hub.reconnectDelay = BASE_RECONNECT_DELAY_MS;
      // #7732: a fresh session. Whatever this hub was asked before it
      // re-authenticated is not a question it is still going to answer.
      hub.readyOutstanding = false;
      // Scoped to the hub this task would have been re-asserted TO, so a
      // second, non-active hub authenticating mid-review stays as silent as it
      // was before — it was never going to resume anything either way.
      if (currentTask && isLocalOnlyTask(currentTask) && hub === currentTaskHub()) {
        // #5715: nothing to resume. The hub never leased this task, so a resume
        // can only be answered with a revoke — and the old message named
        // `${repo}#0`, an issue number that does not exist, which is the tell
        // that the frame was about a task the hub had no record of.
        //
        // Deliberately NOT calling startProgressReporting(): the local tick loop
        // is not torn down by a socket close (the close handler clears the
        // heartbeat, not progressInterval), so the review has been running
        // throughout the flap. Re-arming here would reset the stall clock and
        // the max-duration lease, silently extending a review that may be
        // wedged — a flap must not buy the agent more time.
        console.log(`Reconnected during the local ${currentTask.kind} cycle (${currentTask.task_id}) — not resuming: the hub never leased it, so it continues locally`);
      } else if (currentTask && hub === currentTaskHub()) {
        console.log(`Reconnected while working on ${currentTask.repo}#${currentTask.number} — resuming`);
        sendTo(hub, { type: 'task_accepted', seq: nextSeq(), task_id: currentTask.task_id });
        sendTo(hub, { type: 'task_progress', seq: nextSeq(), task_id: currentTask.task_id, task_gen: currentTask.task_gen, kind: currentTask.kind, repo: currentTask.repo, number: currentTask.number, title: currentTask.title, status: 'working' });
        startProgressReporting();
      } else if (!currentTask && hub === hubs[activeHubIndex]) {
        // Only the hub currently in the poll rotation asks for work. A hub
        // that authenticates while it's not its turn just sits connected
        // (heartbeating) until task_unavailable rotates the active slot to it.
        if (quotaHoldActive()) {
          // sendTo() would swallow the ready anyway; say why, or a reconnect
          // during a hold looks like the relay silently losing interest (#6541).
          console.log(`Authenticated, but the provider quota is exhausted — withholding ready for ` +
            `${formatQuotaHoldRemaining(quotaHoldUntil - Date.now())} (${quotaHoldReason})`);
        } else if (loginHoldActive()) {
          console.log(`Authenticated, but the ${BACKEND} CLI login has expired — withholding ready until someone runs /login in the pane (${loginHoldReason})`);
        } else if (CONTRIBUTOR_MODE === MODE_HEADLESS || cliReady) {
          sendTo(hub, { type: 'ready', seq: nextSeq() });
        } else if (cliReadyFailed) {
          console.log('Authenticated, but CLI readiness previously failed — withholding ready until the CLI recovers');
        } else {
          console.log('Authenticated, but CLI is not ready yet — withholding ready until the CLI reaches its prompt');
        }
      }
      break;

    case 'auth_failed':
      console.error(`Authentication with ${hub.url} failed: ${sanitizeHubText(msg.reason)}`);
      if (msg.accepted_models && msg.accepted_models.length > 0) {
        console.error('\nThis hive accepts the following models:');
        msg.accepted_models.forEach(m => console.error('  - ' + sanitizeHubText(m)));
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
      // #7732: an assignment answers the `ready` it was sent for, whatever
      // this relay does with it below.
      hub.readyOutstanding = false;
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
      // #6541: quota-blocked. Withholding `ready` stops us ASKING, but a hub can
      // still push an assignment — a queued offer, a hub that never saw the last
      // `ready` consumed, an operator forcing one. Accepting it would spend a
      // provider round-trip to be refused again and mark a hive issue failed for
      // a reason that has nothing to do with it. Decline immediately so the hub
      // can offer it to a contributor who can actually run it, and stay silent
      // afterwards rather than re-advertising.
      if (quotaHoldActive()) {
        const remaining = formatQuotaHoldRemaining(quotaHoldUntil - Date.now());
        console.log(`Declining ${taskKey(msg)} — provider quota exhausted, standing down for ${remaining}`);
        sendTo(hub, {
          type: 'task_failed',
          seq: nextSeq(),
          task_id: msg.task_id,
          reason: `provider quota exhausted for ${BACKEND} — not a fault of this host; declining work for ${remaining} (${quotaHoldReason})`,
          failure_kind: 'environment',
        });
        break;
      }
      // #7996: an expired CLI login refuses every prompt. Decline so the hub
      // offers the issue to a contributor who can run it, instead of holding
      // the lease for a 30-minute watchdog to release.
      if (loginHoldActive()) {
        console.log(`Declining ${taskKey(msg)} — the ${BACKEND} CLI login has expired; waiting for /login in the pane`);
        sendTo(hub, {
          type: 'task_failed',
          seq: nextSeq(),
          task_id: msg.task_id,
          reason: `${BACKEND} CLI login expired on this contributor — declining work until someone runs /login in the pane (${loginHoldReason})`,
          failure_kind: 'environment',
        });
        break;
      }
      loginWallTicks = 0;
      const quotaDecision = evaluateContributorQuota(msg);
      if (!quotaDecision.admit) {
        logContributorQuotaDecision(msg, quotaDecision);
        if (hubSupportsQuotaPreflight(hub)) {
          sendTo(hub, {
            type: 'task_declined',
            seq: nextSeq(),
            task_id: msg.task_id,
            task_gen: msg.task_gen,
            reason: 'local_capacity_guard',
            message: `contributor quota guard held ${BACKEND}: ${quotaDecision.reason}`,
          });
        } else {
          console.warn(`Hub ${hub.url} does not advertise quota_preflight_v1; closing instead of reporting a quota hold as task_failed.`);
          try { if (hub.ws && typeof hub.ws.close === 'function') hub.ws.close(); } catch (_) {}
        }
        break;
      }
      currentTask = msg;
      // #6908: assignment is where a hub grants authority over a repo, so this
      // is where the review cycle's scope is earned. Recorded before anything
      // below can fail, and never cleared, so a PR opened during this task
      // stays reviewable for the rest of the session.
      recordAuthorizedRepo(msg.repo);
      // A continue-once override authorized exactly this one admission; consume
      // it now so it can never become a standing authorization to keep spending
      // (kubestellar/hive#6953). And claim this relay's share of the shared pool
      // so a peer relay on the same account holds instead of double-booking the
      // same reserve.
      if (quotaDecision.override === 'continue_once') consumeQuotaOverrideOnce();
      reserveQuotaPool();
      // Non-enumerable: currentTask IS msg, and msg gets JSON.stringify'd
      // wholesale to TASK_FILE a few lines down. hub carries live
      // setInterval/setTimeout handles (heartbeatInterval, reconnectTimer),
      // which are circular — a plain assignment here crashed every task
      // (TypeError: Converting circular structure to JSON), which crashed the
      // process, on the very first task after adding multi-hub support.
      Object.defineProperty(currentTask, '_hub', { value: hub, enumerable: false, writable: true, configurable: true });
      activeHubIndex = hubs.indexOf(hub);
      console.log(`Task assigned: ${sanitizeHubText(msg.kind)} ${sanitizeHubText(msg.repo)}#${msg.number} — ${sanitizeHubText(msg.title)} (from ${hub.url})`);
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
      // (kubestellar/hive#5065).
      const { github_token: _omittedToken, ...taskFileRecord } = msg;
      // A failed write must never throw out of handleMessage
      // (hivecommons/hive#7777): this runs before task_accepted is sent, so an
      // unwritable path — a full /tmp, a stale file owned by another uid on a
      // shared host, a bad HIVE_TASK_FILE — crashed the relay on every
      // assignment: a crash loop, not a degraded mode, with currentTask already
      // set and the token already written but no task_accepted ever sent. The
      // file is observability state no task depends on, so log loudly and carry
      // on, exactly as injectGhToken above does for the token cache.
      try {
        fs.writeFileSync(TASK_FILE, JSON.stringify(taskFileRecord, null, 2), { mode: 0o600 });
        try { fs.chmodSync(TASK_FILE, 0o600); } catch (_) { /* content is already token-free */ }
      } catch (e) {
        console.error(`Failed to write task file ${TASK_FILE}: ${e.message} — continuing without it`);
      }
      send({ type: 'task_accepted', seq: nextSeq(), task_id: msg.task_id, task_gen: msg.task_gen });
      // #7925: the repository's declared tools go in before the prompt does.
      // The task stays current while this runs; a revoke that lands meanwhile
      // clears currentTask and the dispatch below is dropped.
      installRepoToolchain(msg, () => {
        if (!currentTask || currentTask.task_id !== msg.task_id || currentTask.task_gen !== msg.task_gen) {
          console.log(`Task ${msg.task_id} is no longer current after the toolchain step; not dispatching it`);
          return;
        }
        if (CONTRIBUTOR_MODE === MODE_HEADLESS) {
          // Non-interactive path (kubestellar/hive#2538): drive a one-shot CLI
          // invocation and report completion/failure from its exit status — no
          // tmux, no pane scraping, no watchdog waiting on an invisible prompt.
          runHeadlessTask(msg);
        } else {
          const taskPrompt = resolveTaskPrompt(msg);
          // tmuxSendKeys() itself queues when the CLI is not confirmed ready, so
          // there is a single gate rather than two that can disagree.
          tmuxSendKeys(taskPrompt);
          startProgressReporting();
        }
      });
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

    // token_refresh_failed (kubestellar/hive#5447): the hub could not re-mint
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
      // #5715: a revoke is terminal because the work now belongs to someone
      // else — the hub has taken the lease back and may hand it to another
      // contributor, so continuing would be two agents on one issue. None of
      // that reasoning survives for a task the hub never owned. There is no
      // lease to take back and nobody else to give it to, so stopping the
      // agent and relaunching the CLI would destroy a valid turn for nothing.
      //
      // Withholding the ownership frames in sendTo() means the relay no longer
      // PROVOKES this, but a revoke can still arrive — one already in flight
      // when the socket dropped, or a hub that revokes for its own reasons.
      // Surviving it is the second, independent half of the fix: the review
      // cycle now runs to completion regardless of why a revoke shows up.
      if (isLocalOnlyTask(currentTask)) {
        console.log(`Ignoring task_revoke for ${sanitizeHubText(msg.task_id)} (${sanitizeHubText(msg.reason)}) — locally-created ${currentTask.kind} cycle, never leased by the hub; continuing`);
        break;
      }
      console.log(`Task revoked: ${sanitizeHubText(msg.task_id)} — ${sanitizeHubText(msg.reason)}`);
      // #7779: if this task's prompt was still queued (assigned while the CLI
      // was relaunching), drop it now. stopAgentForTaskExit() below relaunches
      // the CLI, and its readiness callback would otherwise flush the revoked
      // task's prompt into the fresh CLI — an agent working an issue the hub
      // has taken back, with the relay believing it holds nothing.
      discardPendingTask('the task was revoked');
      // Held past the clear below so stopAgentForTaskExit() can still name
      // the checkout it has to sweep (#7790): this is the exit path that
      // produced the utah leftovers, and it is the one path that clears
      // currentTask before stopping the agent.
      const revokedTask = currentTask;
      currentTask = null;
      releaseQuotaPoolReservation();
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
      //
      // Interactive mode sends no `ready` here (#5042): the relaunch's
      // readiness callback advertises once — and only once (#7732) — when the
      // fresh CLI is confirmed at its prompt with the relay still idle.
      stopAgentForTaskExit({ reason: 'task revoke', task: revokedTask });
      // Stay with the hub that just revoked — it's clearly alive and reachable.
      activeHubIndex = hubs.indexOf(hub);
      if (CONTRIBUTOR_MODE === MODE_HEADLESS) sendTo(hub, { type: 'ready', seq: nextSeq() });
      break;

    case 'task_unavailable':
      // #7732: the hub's explicit "nothing for you" answers the outstanding
      // `ready`; the retry below asks again.
      hub.readyOutstanding = false;
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
      console.log(`No task assigned on ${hub.url} — reason: ${sanitizeHubText(msg.reason) || 'unspecified'}; retrying in ${TASK_UNAVAILABLE_RETRY_MS / 1000}s`);
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
      if (msg.announcement && printHubAnnouncementOnce(hub, msg.announcement)) break;
      console.log(sanitizeHubText(msg.message) || sanitizeHubText(msg.reason) || 'Notice from hub');
      break;

    case 'ping':
      sendTo(hub, { type: 'pong', seq: msg.seq });
      break;

    case 'pong':
      hub.lastPong = Date.now();
      break;

    default:
      console.log('Unknown message type:', sanitizeHubText(msg.type));
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
// THE GAP THIS FILLS (kubestellar/hive#5090): this handler used to ignore both
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

function wsCloseCorrelation(hub, now = Date.now()) {
  const lastPongAge = hub.lastPong ? Math.max(0, now - hub.lastPong) : -1;
  const lastPingAge = hub.lastPingSentAt ? Math.max(0, now - hub.lastPingSentAt) : -1;
  return `conn=${hub.connectionId || 'unknown'} ` +
    `last_pong_age_ms=${lastPongAge} last_ping_age_ms=${lastPingAge} ` +
    `reconnect_delay_ms=${hub.reconnectDelay} ` +
    `heartbeat_interval_ms=${HEARTBEAT_INTERVAL_MS} heartbeat_timeout_ms=${HEARTBEAT_TIMEOUT_MS}`;
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
    hub.lastPingSentAt = 0;
    hub.connectionId = '';

    hub.heartbeatInterval = setInterval(() => {
      if (gen !== hub.connectGeneration) { clearInterval(hub.heartbeatInterval); return; }
      if (Date.now() - hub.lastPong > HEARTBEAT_TIMEOUT_MS) {
        console.error(`Heartbeat timeout on ${hub.url} (${wsCloseCorrelation(hub)}) — reconnecting`);
        hub.ws.terminate();
        return;
      }
      hub.lastPingSentAt = Date.now();
      sendTo(hub, { type: 'ping', seq: nextSeq() });
      // Also emit a PROTOCOL-level Ping control frame (kubestellar/hive#5090).
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
  // (kubestellar/hive#5090), so a hub answering only control frames cannot trip
  // this relay's HEARTBEAT_TIMEOUT_MS sweep. An inbound Ping is likewise
  // evidence the hub is alive; `ws` auto-replies with a Pong for us.
  hub.ws.on('pong', () => {
    if (gen !== hub.connectGeneration) return;
    hub.lastPong = Date.now();
    recordHubSeen(hub, hub.lastPong);
  });
  hub.ws.on('ping', () => {
    if (gen !== hub.connectGeneration) return;
    hub.lastPong = Date.now();
    recordHubSeen(hub, hub.lastPong);
  });

  hub.ws.on('close', (code, reason) => {
    if (gen !== hub.connectGeneration) return;
    console.log(`Connection to ${hub.url} closed (${describeWsClose(code, reason)}). ` +
      `${wsCloseCorrelation(hub)}. Reconnecting in ${hub.reconnectDelay}ms...`);
    if (hub.heartbeatInterval) { clearInterval(hub.heartbeatInterval); hub.heartbeatInterval = null; }
    // #7732: a `ready` in flight on this socket died with it; the auth_ok of
    // the reconnect asks afresh.
    hub.readyOutstanding = false;
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
  if (hubsSeenWriteTimer) { clearTimeout(hubsSeenWriteTimer); hubsSeenWriteTimer = null; }
  writeHubsSeenNow();
  removeRelayPidFile();
  hubs.forEach(hub => {
    if (hub.heartbeatInterval) { clearInterval(hub.heartbeatInterval); hub.heartbeatInterval = null; }
  });
  if (progressInterval) { clearInterval(progressInterval); progressInterval = null; }
  if (knowledgeStateTimer) { clearInterval(knowledgeStateTimer); knowledgeStateTimer = null; }
  stopVerdictWatch();
  // A shutdown with a task in flight must run the same task-exit contract as
  // every other way a task stops being ours (kubestellar/hive#5655, #5353).
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
    releaseQuotaPoolReservation();
  }
}

process.on('SIGTERM', () => { cleanup(); process.exit(0); });
process.on('SIGINT', () => { cleanup(); process.exit(0); });
process.on('SIGUSR1', handleProfileSwitchSignal);

// Last-resort backstop (kubestellar/hive#5655): the scoped token must never
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
    armCLIReadyWait,
    tmuxSessionMissing,
    recreateTmuxSession,
    failCurrentTask,
    finishCurrentTask,
    startProgressReporting,
    progressTick,
    // Local-only (synthetic pr-review) task surface — kubestellar/hive#5715.
    // PR_REVIEW_EVERY_N is exported so a test can enter the REAL cycle rather
    // than hand-build the task it is meant to be asserting about.
    PR_REVIEW_EVERY_N,
    // PR review cycle scope and trigger (kubestellar/hive#6664).
    buildReviewPrompt,
    getPRsShippedSinceReview: () => prsShippedSinceReview,
    setPRsShippedSinceReview: (v) => { prsShippedSinceReview = v; },
    getAuthorizedRepos: () => Array.from(authorizedRepos),
    recordAuthorizedRepo,
    isSafeRepoName,
    isSafeAuthorLogin,
    clearAuthorizedRepos: () => authorizedRepos.clear(),
    getReposShippedSinceReview: () => reposShippedSinceReview.slice(),
    setReposShippedSinceReview: (v) => { reposShippedSinceReview = Array.isArray(v) ? v.slice() : []; },
    LOCAL_TASK_ID_PREFIX,
    isLocalOnlyTask,
    isLocalOnlyTaskId,
    classifyTmuxPane,
    paneTail,
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
    // Provider quota hold (kubestellar/hive#6541).
    paneQuotaExhaustion,
    quotaHoldActive,
    enterQuotaHold,
    releaseQuotaHold,
    getQuotaHoldUntil: () => quotaHoldUntil,
    getQuotaHoldReason: () => quotaHoldReason,
    // CLI login hold (hivecommons/hive#7996).
    loginHoldActive,
    enterLoginHold,
    releaseLoginHold,
    probeLoginHold,
    getLoginHoldReason: () => loginHoldReason,
    LOGIN_WALL_GRACE_TICKS,
    QUOTA_HOLD_FALLBACK_MS,
    QUOTA_HOLD_MAX_MS,
    QUOTA_HOLD_GRACE_MS,
    evaluateContributorQuota,
    normalizeTaskComplexity,
    quotaRequiredReserve,
    quotaWindowReserve,
    contributorReadingCapturedAtMs,
    contributorQuotaReadingWithFreshness,
    QUOTA_READING_PUBLISH_INTERVAL_MS,
    QUOTA_READING_STALE_AFTER_MS,
    readContributorQuotaReading,
    // Default-on publishing derivation (kubestellar/hive#6987).
    defaultContributorPoolDir,
    QUOTA_DEFAULT_POOL_DIR,
    quotaDefaultPublishedReadingFile,
    // Publisher presence marker — condition (b) (kubestellar/hive#6987).
    quotaDefaultPublisherMarkerFile,
    quotaPublisherMarkerFresh,
    // Guard resume + provisioning (kubestellar/hive#6951).
    retryContributorQuota,
    armContributorQuotaRetry,
    QUOTA_GUARD_RETRY_MS,
    // Cross-process pool store + scoped overrides (kubestellar/hive#6953).
    quotaPoolActive,
    reserveQuotaPool,
    releaseQuotaPoolReservation,
    refreshQuotaPoolReservation,
    quotaPoolHasPeerReservation,
    readQuotaOverride,
    consumeQuotaOverrideOnce,
    quotaOverrideApplies,
    quotaPauseOverrideActive,
    QUOTA_SESSION_ID,
    QUOTA_RELAY_ID,
    QUOTA_POOL_KEY: quotaPool ? quotaPool.poolKey : null,
    quotaPoolStore,
    setContributorQuotaPaused: (v) => { contributorQuotaPaused = !!v; },
    setContributorQuotaStayPaused: (v) => { contributorQuotaStayPaused = !!v; },
    getContributorQuotaPaused: () => contributorQuotaPaused,
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
    // Post-verdict review follow-up (hivecommons/hive#7759).
    POST_VERDICT_REVIEW_MESSAGE,
    POST_VERDICT_REVIEW_ANCHOR,
    POST_VERDICT_REVIEW_INSTRUCTION,
    POST_VERDICT_REVIEW_MARKERS,
    // The notes the follow-up quotes (hivecommons/hive#7935).
    buildPostVerdictReviewMessage,
    sanitizePostVerdictNote,
    postVerdictNoteBlockEntries,
    postVerdictQuotableNotes,
    POST_VERDICT_NOTE_MAX_CHARS,
    POST_VERDICT_NOTES_MAX_QUOTED,
    postVerdictConcerns,
    postVerdictReviewAnswered,
    maybeRequestPostVerdictReview,
    resetPostVerdictReviewState,
    getPostVerdictReviewRequested: () => postVerdictReviewRequested,
    getPostVerdictReviewCount: () => postVerdictReviewCount,
    postVerdictNoteBlocks,
    POST_VERDICT_REVIEW_MAX_FOLLOWUPS,
    UNADDRESSED_ADVISOR_NOTES_HEADING,
    getPRClaimFollowUpRequested: () => prClaimFollowUpRequested,
    PR_CLAIM_FOLLOWUP_MESSAGE,
    PR_CLAIM_FOLLOWUP_ANCHOR,
    PR_CLAIM_PATTERN,
    tmuxSessionHasAttachedClient,
    tmuxSessionHumanPresence,
    HUMAN_PRESENCE_IDLE_MS,
    HUMAN_PRESENCE_MAX_DEFERRALS,
    paneEditedSincePresenceCheck,
    resetHumanPresenceEvidence,
    getPresenceDeferralCount: () => presenceDeferralCount,
    TRANSIENT_API_ERROR_MAX_NUDGES,
    TRANSIENT_API_ERROR_NUDGE_MESSAGE,
    getTransientNudgeCount: () => transientNudgeCount,
    __clearTransientNudgeCooldown: () => { lastTransientNudgeAt = 0; },
    // Run one progress tick with the grace period already elapsed.
    __crashTick: () => { taskAssignedAt = Date.now() - TASK_GRACE_PERIOD_MS - 1; progressTick(); },
    __setVerdictSettleMs: (ms) => { VERDICT_SETTLE_MS = ms; },
    resolveTaskPrompt,
    TASK_WORKSPACE_DIR,
    paneStalled,
    paneStallConfirmed,
    paneChangedSince,
    resetPaneStallClock,
    PANE_STALL_CONFIRM_TICKS,
    // Completion-signal surface (kubestellar/hive#5376).
    CHROME_IDLE_GRACE_TICKS,
    HIVE_VERDICT_COMPLETE,
    HIVE_VERDICT_NO_WORK,
    HIVE_VERDICT_BLOCKED,
    HIVE_VERDICT_TOKENS,
    BLOCKED_WORKFLOW_LABEL,
    DEFAULT_NEEDS_DECISION_LABEL,
    ALREADY_DONE_WORKFLOW_LABEL,
    isNoWorkVerdict,
    isAlreadyDoneNoWork,
    verdictWireFields,
    alreadyDoneVerdictFields,
    markIssueBlocked,
    markIssueNeedsDecision,
    markIssueAlreadyDone,
    detectHiveVerdict,
    detectHiveVerdicts,
    detectCompletionVerdict,
    recordChromeIdleTick,
    resetChromeIdleGrace,
    getChromeIdleTicks: () => chromeIdleTicks,
    getTaskAgentActivityObserved: () => taskAgentActivityObserved,
    setTaskAgentActivityObserved: (v) => { taskAgentActivityObserved = !!v; },
    resetTaskAgentActivity,
    // Max-duration lease surface (kubestellar/hive#5321).
    MAX_TASK_DURATION_MS,
    ABSOLUTE_TASK_DEADLINE_MS,
    HEADLESS_TASK_TIMEOUT_MS,
    DEFAULT_HEADLESS_MAX_OUTPUT_BYTES,
    HEADLESS_MAX_OUTPUT_BYTES,
    HEADLESS_MAX_OUTPUT_ENV,
    // Frame-size clamping (hivecommons/hive#7932).
    DEFAULT_HUB_MAX_FRAME_BYTES,
    WS_FRAME_HEADROOM_BYTES,
    WS_FRAME_BYTES,
    MIN_WS_FRAME_BYTES,
    OUTPUT_TAIL_MAX_BYTES,
    OUTPUT_TAIL_TRUNCATED_MARKER,
    TEXT_TRUNCATED_SUFFIX,
    FRAME_TRUNCATABLE_FIELDS,
    clampFrame,
    truncateTailLines,
    truncateTextHead,
    truncateTextTail,
    hubFrameBytes,
    frameByteLength,
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
    // Task-exit checkout sweep (hivecommons/hive#7790).
    taskCheckoutDir,
    preserveTaskLeftovers,
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
    // omp primary + advisor selection (hivecommons/hive#7760).
    detectOmpSelection,
    splitOmpSelection,
    parseOmpConfig,
    // #7922: the omp credential-store gate behind getCLIState().
    ompProviderCredentialState,
    ompConfiguredProviderBlocked,
    advisorFields,
    SELECTION_DETECTORS,
    OMP_EFFORT_LEVELS,
    getDetectedEffort: () => detectedEffort,
    getDetectedAdvisor: () => ({ model: detectedAdvisorModel, effort: detectedAdvisorEffort }),
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
    // Stamps the owner from currentTask exactly as queuePendingTask does
    // (#7779), so a test that re-queues a prompt sees it flush for the task it
    // was set up under.
    setPendingTask: (v) => { pendingTask = v; pendingTaskId = (v !== null && currentTask) ? currentTask.task_id : null; },
    getPendingTaskId: () => pendingTaskId,
    // Per-task prompt-delivery surface (kubestellar/hive#5650).
    getTaskPromptDelivered: () => taskPromptDelivered,
    setTaskPromptDelivered: (v) => { taskPromptDelivered = v; },
    // Prompt-SUBMISSION surface (#6717): typed is not submitted.
    PROMPT_PASTE_SETTLE_MS,
    PROMPT_SUBMIT_RETRIES,
    confirmPromptSubmitted,
    paneHoldsUnsubmittedPrompt,
    paneChangedSinceDelivery,
    paneFingerprint,
    getPromptSubmissionConfirmed: () => promptSubmissionConfirmed,
    setPromptSubmissionConfirmed: (v) => { promptSubmissionConfirmed = v; },
    getPromptDeliveryFingerprint: () => promptDeliveryFingerprint,
    setPromptDeliveryFingerprint: (v) => { promptDeliveryFingerprint = v; },
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
    // Peer-protocol compatibility (kubestellar/hive#2547). Exported so the
    // relay-side half of "both sides can detect an incompatible peer" is tested
    // behaviourally here, not just asserted to exist from the Go side.
    RELAY_PROTOCOL_VERSION,
    // Outbound negotiated capability set (kubestellar/hive#6954). Exported so a
    // test can assert the relay advertises quota_preflight_v1 rather than the hub
    // inferring it from the protocol version.
    RELAY_CAPABILITIES,
    parseProtocolVersion,
    classifyPeerProtocol,
    warnOnProtocolDrift,
    formatHubAnnouncementLine,
    printHubAnnouncementOnce,
    sanitizeHubText,
    describeWsClose,
    wsCloseCorrelation,
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
    // #7778: the headless lease-renewal tick and whether its timer is armed.
    headlessProgressTick,
    getProgressIntervalArmed: () => progressInterval !== null,
    // #7841: the verdict fast path and whether its watch is armed.
    verdictWatchTick,
    VERDICT_WATCH_INTERVAL_MS,
    getVerdictWatchArmed: () => verdictWatchInterval !== null,
    TASK_GRACE_PERIOD_MS,
    // Attach-hint surface (kubestellar/hive#5145): the exact command the
    // needs-authentication banner tells a human to paste.
    ATTACH_COMMAND,
    loginBannerLines,
    renderBoxedBanner,
    NEEDS_LOGIN_CONFIRM_TICKS,
    CONTAINER_NAME,
    CONTAINER_RUNTIME,
    // Coverage for previously untested pure/isolated functions (#4267).
    redactTokens,
    captureTmuxLines,
    detectNoWorkVerdict,
    detectPRURL,
    // Every PR URL on the pane, ranked for verification (hivecommons/hive#7789).
    detectPRURLs,
    isHiveVerdictLine,
    // PR attribution (kubestellar/hive#6662). prAttributionEvidence is the pure
    // rule set and is where the interesting cases live; resolveTaskPR is the
    // wiring, exercised through a stubbed gh.
    prAttributionEvidence,
    verifyTaskPR,
    resolveTaskPR,
    PR_ATTRIBUTION_CONFIRMED,
    PR_ATTRIBUTION_REFUTED,
    PR_ATTRIBUTION_UNKNOWN,
    PR_ATTRIBUTION_CLOCK_SKEW_MS,
    PR_ATTRIBUTION_MAX_LOOKUPS,
    CONTRIBUTOR_LOGIN,
    TMUX_TAIL_LINES,
    PR_SCAN_LINES,
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
    tmuxSendNudge,
    GIVE_UP_MEMORY_MS,
    parseContributorEnvFile,
    hubListFromEnv,
    reloadHubsFromProjection,
    recordHubSeen,
    writeHubsSeenNow,
    handleProfileSwitchSignal,
    hubs,
    HUBS_SEEN_FILE,
    RELAY_PID_FILE,
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
  writeRelayPidFile();
  connect();
}
