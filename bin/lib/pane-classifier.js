// bin/lib/pane-classifier.js — pure pane-classification for the contributor
// relay (kubestellar/hive#6429).
//
// This module holds every DETECTOR the relay uses to read a tmux pane capture
// and decide what state the backend CLI is in: readiness/login/onboarding
// (classifyReadiness, formerly getCLIState's body), the busy/idle/blocked/
// error state machine (classifyPane, formerly classifyTmuxPane), the blocked-
// on-human reason breakdown (classifyBlockedOnHumanReason), the modal-dismiss
// keystroke table (blockingPromptKey), and the pane-tail / API-error line
// detectors they all share.
//
// Deliberately PURE: no `process`, no `child_process`, no tmux, no filesystem.
// Every function here takes a pane-capture string (and, where a backend's
// rules differ, an explicit `backend` argument) and returns a plain value.
// bin/contributor-relay.js is the caller: it captures pane text via tmux
// (capturePaneText()) and passes it in here, keeping the CAPTURE and the
// CLASSIFICATION as two separate concerns (kubestellar/hive#6429, kubestellar/
// hive#6427). Kept in sync test-for-test with src/pkg/agent/manager.go's Go
// port of the same rules — see bin/testdata/pane-fixtures/ and
// src/pkg/agent/pane_fixtures_test.go for the shared golden fixtures
// (kubestellar/hive#6427).
//
// `deps` on classifyPane() is the one place an effectful reading leaks in: the
// bob backend's readiness depends on whether a bob PROCESS is still alive,
// which only the caller (with process-table access) can answer. Everything
// else here is a function of the pane text alone.

'use strict';

// Interactive pane classifier states. Keep this vocabulary small and explicit:
// "not complete" splits into active work vs. human input needed so the relay
// never reports success for a turn that is actually sitting at a question.
const PANE_STATE_WORKING = 'WORKING';
const PANE_STATE_BLOCKED_ON_HUMAN = 'BLOCKED_ON_HUMAN';
const PANE_STATE_IDLE_COMPLETE = 'IDLE_COMPLETE';
// A retryable API failure left the CLI parked at its idle prompt with the
// response truncated (kubestellar/hive#5094). This is NOT completion and NOT a
// stall: the turn ended, but it ended in an error, and the same request can
// succeed on a retry.
const PANE_STATE_TRANSIENT_API_ERROR = 'TRANSIENT_API_ERROR';
// An API failure a retry CANNOT clear — an authorization refusal or an exhausted
// quota — left the CLI parked at its idle prompt. Also not completion: the turn
// ended having shipped nothing. Retrying it would loop the agent against a wall,
// so this is failed at once rather than nudged.
const PANE_STATE_FATAL_API_ERROR = 'FATAL_API_ERROR';
// An API failure matching NEITHER curated list ended the turn at the idle
// prompt (kubestellar/hive#5121). Still not completion: the turn shipped
// nothing. Nobody can say from a pattern table whether a retry clears it, so
// it takes the bounded transient path — if it was retryable the retry wins,
// and if not the budget runs out and the task is handed back as an honest
// environment failure. Either way, never a fabricated completion.
const PANE_STATE_UNKNOWN_API_ERROR = 'UNKNOWN_API_ERROR';

// ── Transient API-error recovery (kubestellar/hive#5094) ─────────────────────
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
  'individual quota reached',
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
// agy renders provider quota exhaustion WITHOUT the "API Error:" chrome — a
// bare banner the chrome gate below rejects before any pattern is consulted
// (kubestellar/hive#6541, the third chrome-less case the #5121 residual note
// predicted):
//
//   ⚠ Individual quota reached. Please upgrade your subscription to increase
//     your limits. Resets in 38m29s.
//
// Left undetected, the relay kept dispatching tasks into the quota-blocked CLI
// and booked each one as an [environment] failure 20 minutes later. The anchor
// here is agy's own error glyph at line start — the analogue of Claude's
// "● API Error:" bullet — paired with the quota wording, so two independent
// signals are still required: an agent whose completed-turn PROSE merely
// mentions "individual quota reached" (this repo contains the string) does not
// start its line with the ⚠ chrome and is not tripped.
const CHROMELESS_QUOTA_BANNER_RE = /^\s*⚠\s[^\n]*\bquota reached\b/i;
// The visible tail the error must appear in. Matching the whole pane would let
// an error the agent already recovered from read as current.
const TRANSIENT_API_ERROR_TAIL_LINES = 12;

function blockingPromptKey(text, backend) {
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
  if (backend === 'agy' && /Terms of Service & Data Use/.test(recent) && /\[(?:Previous|Back)\]\s+\[Done\]/.test(recent)) return 'Down Right';
  // agy: post-error feedback survey — "How's the CLI experience so far? Help us
  // improve: [1] Good [2] Fine [3] Bad [0] Skip" (#6541, rendered right after
  // the quota banner). Skip it: answering a satisfaction survey is not the
  // relay's call to make, and 0 persists nothing. Both halves are required so
  // a transcript that merely QUOTES the survey (this comment does) cannot
  // match without the numbered option row.
  if (backend === 'agy' && /How'?s the CLI experience so far/i.test(recent) && /\[0\]\s*Skip/.test(recent)) return '0';
  return null;
}


function classifyReadiness(text, backend) {
  if (backend === 'claude') {
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
      // with "CLI never became ready" (kubestellar/hive#5156, seen again in
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
    } else if (backend === 'copilot') {
      if (/copilot login|gh auth login/.test(text)) return 'needs-login';
      if (/Confirm folder trust|trust the files|Do you trust/.test(text)) return 'onboarding';
      if (/\/ commands.*help/.test(text)) return 'ready';
    } else if (backend === 'gemini') {
      if (/not authenticated|login required/i.test(text)) return 'needs-login';
      if (/>\s*$|❯/.test(text)) return 'ready';
    } else if (backend === 'goose') {
      if (/goose is ready|> Enter to send|>\s*$|goose>|G\s*>/.test(text)) return 'ready';
    } else if (backend === 'bob') {
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
    } else if (backend === 'codex') {
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
    } else if (backend === 'pi') {
      if (/pi v\d|0\.0%|auto\)|\d+\.\d+%/.test(text)) return 'ready';
    } else if (backend === 'agy') {
      // Antigravity gates first run behind login plus a three-step wizard, and
      // every agent that shares a $HOME can re-enter it whenever another agent
      // writes antigravity-cli/cache/onboarding.json mode 600. Check the
      // visible tail only: old task output may quote the wizard text, and a
      // stale quote must not make a live prompt look blocked.
      //
      // paneTail: agy renders inline at the TOP of the pane (banner, input box
      // and its "? for shortcuts" footer all land in rows 1-16 of a 50-row
      // capture), so a plain last-15-rows slice of the raw text is rows 36-50
      // — always blank on a healthy, idle agy pane. That made getCLIState()
      // return 'starting' forever, waitForCLI() time out at
      // CLI_READY_TIMEOUT_MS, and every task handed back with "CLI never
      // became ready" (#6413). paneTail already trims trailing blank rows
      // before slicing, which keeps the "recent output only" intent above
      // intact while making the window track actual content instead of the
      // pane's fixed height.
      const recent = paneTail(text, 15);
      if (/not signed in|Select login method/i.test(recent)) return 'needs-login';
      // The post-error feedback survey ("How's the CLI experience so far?
      // [1] Good … [0] Skip", #6541) parks the input behind a modal exactly
      // like the first-run wizard does — and it outlives the turn that raised
      // it, so a fresh readiness wait would otherwise sit at 'starting' until
      // CLI_READY_TIMEOUT_MS. Classify it 'onboarding' so waitForCLI()'s
      // auto-dismiss path consults blockingPromptKey, which answers it with
      // "0" (Skip).
      if (/How'?s the CLI experience so far/i.test(recent) && /\[0\]\s*Skip/.test(recent)) return 'onboarding';
      if (/Choose your color scheme|Terms of Service & Data Use|Do you trust the contents|I trust this (?:folder|directory)|Welcome to (?:the )?Antigravity/i.test(recent)) return 'onboarding';
      // agy shows "? for shortcuts" at the bottom when its interactive prompt
      // is ready. The generic />\s*$/ fires too early during splash, and the
      // wizard's selection cursor is also ❯.
      if (/\? for shortcuts/.test(recent)) return 'ready';
  } else {
    if (/>\s*$|❯|\$\s*$/.test(text)) return 'ready';
  }
  return 'starting';
}

function recentPaneLines(text, limit = 12) {
  return text
    .split('\n')
    .map(line => line.trim())
    .filter(Boolean)
    .slice(-limit);
}

// Why a blocked pane is blocked (kubestellar/hive#5281). BLOCKED_ON_HUMAN
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

// paneTail returns the last n NON-BLANK lines of a pane capture: blank
// (whitespace-only) lines are dropped WHEREVER they occur, not just at the
// end, and the n most recent real rows are kept in their original order.
// tmux capture-pane -p always pads its output to the pane's full height, so a
// UI that renders inline near the top (agy's banner + input box land in rows
// 1-16 of a 50-row pane) leaves a plain last-n-rows window that is entirely
// blank rows the terminal never touched. Dropping blanks first makes the
// window track actual content instead of the pane's fixed geometry, while
// still preserving the "recent output only" intent this exists for: a stale
// quote of wizard text further up the (non-blank) history is still excluded
// once it falls outside the last n real rows.
//
// This is the ONE tail semantics for pane captures in this file — it matches
// Go's paneTail in src/pkg/agent/manager.go (manager.go:~3527), which has
// always filtered blank lines this same way (scanning from the end, skipping
// any blank line, collecting until n non-blank rows are found, then
// reversing). A second, blank-including variant (paneTailNonBlank's
// predecessor) used to live beside this one; the divergence between that
// blank-including JS behavior and Go's non-blank one was the direct root
// cause of kubestellar/hive#6413 (agy contributors stuck at 'starting'
// forever, because a plain last-15-rows slice of a 50-row pane is always
// blank for a UI that renders near the top). Pure, so the detectors below
// are table-testable without tmux.
function paneTail(text, n) {
  const lines = String(text || '').split('\n');
  const kept = [];
  for (let i = lines.length - 1; i >= 0 && kept.length < n; i--) {
    if (lines[i].trim() === '') continue;
    kept.push(lines[i]);
  }
  kept.reverse();
  return kept.join('\n');
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
// part of the documented #5121 residual — EXCEPT the agy banner, which carries
// its own ⚠ line-start chrome and is matched by CHROMELESS_QUOTA_BANNER_RE
// above (#6541).
function paneShowsUnretryableAPIError(text) {
  const lines = paneTail(text, TRANSIENT_API_ERROR_TAIL_LINES).split('\n');
  return lines.some((line) => {
    if (CHROMELESS_QUOTA_BANNER_RE.test(line)) return true;
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

// paneUnknownAPIErrorLine returns the first line of the visible tail that
// carries Claude Code's own error rendering — a line-leading "● API Error:" —
// or null. Reached only after the three curated detectors above have NOT
// matched (classifyTmuxPane's ordering), so a hit here is an API failure the
// tables cannot name (kubestellar/hive#5121): a 400, a 404, a 429 phrased in a
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

function classifyPane(text, backend, deps = {}) {
  let hasIdlePrompt, hasCompletionMarker, isWorking;

  if (backend === 'claude') {
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
  } else if (backend === 'copilot') {
    hasIdlePrompt = /\/ commands.*help/.test(text);
    hasCompletionMarker = true;
    isWorking = /esc cancel/.test(text);
  } else if (backend === 'gemini') {
    hasIdlePrompt = />\s*$|❯\s*$/.test(text);
    hasCompletionMarker = /completed|Done|finished/i.test(text);
    isWorking = /Thinking|Running|Searching/i.test(text);
  } else if (backend === 'goose') {
    hasIdlePrompt = /goose is ready|> Enter to send|>\s*$|goose>|G\s*>/.test(text);
    hasCompletionMarker = true;
    isWorking = /working|running|executing|calling/i.test(text);
  } else if (backend === 'bob') {
    const BOB_IDLE_CHROME = /Enter your prompt, \/ for commands|Auto-approve:|Tokens left:/;
    const BOB_SPINNER = /\(esc to cancel/;
    const bobRunning = deps.bobIsRunning ? deps.bobIsRunning() : false;
    hasIdlePrompt = BOB_IDLE_CHROME.test(text) || !bobRunning;
    hasCompletionMarker = true;
    isWorking = bobRunning && BOB_SPINNER.test(text);
  } else if (backend === 'codex') {
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
  } else if (backend === 'pi') {
    hasIdlePrompt = /pi v\d|0\.0%|auto\)|\d+\.\d+%/.test(text);
    hasCompletionMarker = /completed|done|finished|tokens\)|\d+\.\d+%/i.test(text);
    isWorking = /Reading|Writing|Bash|Editing|thinking|running/i.test(text);
  } else if (backend === 'agy') {
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
    // paneTail (non-blank tail), not a raw slice(-15): tmux capture-pane -p pads its
    // output to the pane's full height, and agy renders inline near the top, so
    // on a short transcript a plain last-15-rows window is nothing but blank
    // padding (#6438 — the same defect #6413 fixed in getCLIState()).
    //
    // hasIdlePrompt's FIRST alternative reads the full text and so survives
    // that; its second — the one for builds that no longer print
    // "? for shortcuts", which is the current Gemini rendering described below
    // — reads this window and cannot match when it is blank. A finished turn
    // then classifies WORKING and stays there until progressTick()'s stall
    // backstop fails it as `environment`, which is exactly the #4127 incident
    // recorded below: that fix widened the regex, but left the window it is
    // applied to padding-blind.
    const agyTail = paneTail(text, 15);
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
  // A turn that ended in a RETRYABLE API failure is not a completed turn
  // (kubestellar/hive#5094). This must sit above the completion test: the
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

module.exports = {
  PANE_STATE_WORKING,
  PANE_STATE_BLOCKED_ON_HUMAN,
  PANE_STATE_IDLE_COMPLETE,
  PANE_STATE_TRANSIENT_API_ERROR,
  PANE_STATE_FATAL_API_ERROR,
  PANE_STATE_UNKNOWN_API_ERROR,
  TRANSIENT_API_ERROR_TAIL_LINES,
  BLOCKED_REASON_QUESTION,
  BLOCKED_REASON_MENU,
  BLOCKED_REASON_HUMAN_REQUIRED,
  blockingPromptKey,
  classifyReadiness,
  recentPaneLines,
  classifyBlockedOnHumanReason,
  paneLooksBlockedOnHuman,
  paneTail,
  paneShowsTransientAPIError,
  paneShowsUnretryableAPIError,
  paneShowsLoginRequiredError,
  paneUnknownAPIErrorLine,
  classifyPane,
};
