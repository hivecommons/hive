#!/usr/bin/env bash
# test_backend_smoke.sh — end-to-end smoke for the contributor agent backends.
#
# The vendor coding-agent CLIs hive drives (claude, codex, …) are an
# integration surface hive does not control: vendors ship CLI updates on their
# own schedule, and what breaks is the seam — completion detection (#5376),
# CODEX_HOME handling (#5335), readiness regexes that matched nothing on real
# output (see the codex arm of getCLIState in bin/contributor-relay.sh). Every
# existing test pins that seam against captured fixtures, so a vendor change
# ships green here and fails in production. This suite is the live complement:
# it drives the REAL relay against a fake hub and, where credentials exist,
# the REAL backend CLI on a one-line task, and asserts the machine-checkable
# contract — the task_complete/task_failed wire shape, the HIVE_VERDICT
# sentinel, and completion_signal=verdict rather than the chrome_idle fallback.
#
# Sections:
#   A  static drift checks — keyless, deterministic, always run:
#      A1 HEADLESS_BACKENDS (relay) vs KNOWN_BACKENDS (backends.conf), a pair
#         kept in sync by comment only before this test existed;
#      A2/A3 codex + claude version pins agree across src/Dockerfile and
#         src/Dockerfile.contributor.
#   S  stub wire-contract scenarios — keyless, deterministic: a stub backend
#      binary on PATH drives the full relay↔hub loop, locking the wire shape
#      the live scenarios (and the hub) rely on, with zero API spend.
#   B  live per-backend scenarios (needs the CLI + a credential; skips
#      otherwise — fatally under HIVE_TEST_REQUIRE_BACKEND_SMOKE=1):
#      B0 detect_cli health probe (contributor-agent.sh's own seam);
#      B1 headless end-to-end: real relay, real CLI one-shot, fake hub;
#      B2 interactive end-to-end: real CLI in tmux, the relay scraping the
#         pane — the exact surface the thirteen chrome issues lived on.
#
# Env knobs:
#   HIVE_SMOKE_BACKENDS              space-separated, default "claude codex"
#   HIVE_SMOKE_MODEL_CLAUDE          default claude-haiku-4-5 (cheapest tier —
#                                    the plumbing is under test, not the model)
#   HIVE_SMOKE_MODEL_CODEX           default gpt-5.4-mini
#   HIVE_TEST_REQUIRE_BACKEND_SMOKE  1 = skips become failures (the CI lane
#                                    inversion, same shape as
#                                    HIVE_TEST_REQUIRE_BEHAVIOURAL in
#                                    src/deploy/test_entrypoint_*.sh)
#   HIVE_SMOKE_CLAUDE_CREDENTIALS_B64 / HIVE_SMOKE_CODEX_AUTH_B64
#                                    base64 subscription-login blobs, used
#                                    when no API key is set — see
#                                    seed_backend_auth for sources, order,
#                                    and the staleness caveat
#
# Run: bash bin/test_backend_smoke.sh          (keyless: A + S run, B skips)
#      ANTHROPIC_API_KEY=... bash bin/test_backend_smoke.sh   (full claude arm)
set -uo pipefail

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && [ -n "${2:-}" ] && echo "        $2"
  FAIL=$((FAIL + 1))
}
check() {
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then pass "$label"; else fail "$label" "want: '$want'  got: '$got'"; fi
}
contains() {
  local label="$1" haystack="$2" needle="$3"
  if printf '%s' "$haystack" | grep -qF -- "$needle"; then pass "$label"
  else fail "$label" "missing: '$needle'"; fi
}

REQUIRE="${HIVE_TEST_REQUIRE_BACKEND_SMOKE:-0}"
# skip(): green-but-loud on a laptop, a real failure in the scheduled lane.
# Without the inversion this suite is the guard-that-cannot-fail: a runner
# missing every credential would skip every live scenario and stay green,
# which is exactly the state the scheduled workflow exists to rule out.
skip() {
  if [ "$REQUIRE" = "1" ]; then
    fail "SKIP escalated (HIVE_TEST_REQUIRE_BACKEND_SMOKE=1): $1"
  else
    echo "  SKIP: $1"
  fi
}

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RELAY="$ROOT/bin/contributor-relay.sh"
REAL_HOME="$HOME"

SMOKE_BACKENDS="${HIVE_SMOKE_BACKENDS:-claude codex}"
MODEL_CLAUDE="${HIVE_SMOKE_MODEL_CLAUDE:-claude-haiku-4-5}"
MODEL_CODEX="${HIVE_SMOKE_MODEL_CODEX:-gpt-5.4-mini}"

# shellcheck source=../config/backends.conf disable=SC1091
source "$ROOT/config/backends.conf"

echo "=== backend smoke: contributor CLI integration (A: drift, S: wire, B: live) ==="

# ── A. Static drift checks ───────────────────────────────────────────────────
echo ""
echo "-- A1: HEADLESS_BACKENDS (relay) agrees with KNOWN_BACKENDS (backends.conf) --"
# Extract the real table from the shipped relay source, the same
# read-the-shipped-artifact technique src/deploy/test_entrypoint_*.sh use on
# entrypoint.sh. A backend added to either list without a headless decision
# now fails a test instead of drifting silently.
HEADLESS_KEYS="$(sed -n '/^const HEADLESS_BACKENDS = {$/,/^};$/p' "$RELAY" \
  | grep -E '^  [a-z]+: \{' | sed -E 's/^  ([a-z]+):.*/\1/')"
if [ -z "$HEADLESS_KEYS" ]; then
  fail "HEADLESS_BACKENDS table extracted from bin/contributor-relay.sh" \
       "the sed/grep anchors matched nothing — did the table's formatting change?"
else
  pass "HEADLESS_BACKENDS table extracted ($(echo "$HEADLESS_KEYS" | wc -l) backends)"
  for k in $HEADLESS_KEYS; do
    case " $KNOWN_BACKENDS " in
      *" $k "*) pass "headless backend '$k' is in KNOWN_BACKENDS" ;;
      *) fail "headless backend '$k' is in KNOWN_BACKENDS" \
              "relay lists '$k' but config/backends.conf KNOWN_BACKENDS does not" ;;
    esac
  done
  # The complement is a decision, not an accident: bob and aider drive an
  # interactive TUI with no one-shot entry point (see the HEADLESS_BACKENDS
  # header comment). A new backend landing in this list means someone added it
  # to backends.conf without deciding its headless story.
  NON_HEADLESS=""
  for k in $KNOWN_BACKENDS; do
    case "
$HEADLESS_KEYS
" in
      *"
$k
"*) ;;
      *) NON_HEADLESS="$NON_HEADLESS $k" ;;
    esac
  done
  check "backends without a headless mode are exactly the documented pair" \
        "aider bob" "$(echo "$NON_HEADLESS" | tr ' ' '\n' | grep -v '^$' | sort | tr '\n' ' ' | sed 's/ $//')"
fi

echo ""
echo "-- A2/A3: CLI version pins agree across both Dockerfiles --"
pin() { grep -m1 "^ARG $2=" "$ROOT/$1" | cut -d= -f2; }
check "codex pin: src/Dockerfile == src/Dockerfile.contributor" \
      "$(pin src/Dockerfile CODEX_VERSION)" "$(pin src/Dockerfile.contributor CODEX_VERSION)"
check "claude pin: src/Dockerfile == src/Dockerfile.contributor" \
      "$(pin src/Dockerfile CLAUDE_CODE_VERSION)" "$(pin src/Dockerfile.contributor CLAUDE_CODE_VERSION)"

# ── Shared rig for S and B ───────────────────────────────────────────────────
RIG_OK=1
for tool in node npm jq python3; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    skip "'$tool' not installed; wire-contract and live scenarios cannot run"
    RIG_OK=0
  fi
done

WORK=""
FAKEHUB_PID=""
RELAY_PID=""
TMUX_SESS=""
cleanup() {
  [ -n "$RELAY_PID" ] && kill "$RELAY_PID" 2>/dev/null
  [ -n "$FAKEHUB_PID" ] && kill "$FAKEHUB_PID" 2>/dev/null
  [ -n "$TMUX_SESS" ] && tmux kill-session -t "$TMUX_SESS" 2>/dev/null
  [ -n "$WORK" ] && rm -rf "$WORK"
}
trap cleanup EXIT

if [ "$RIG_OK" = "1" ]; then
  WORK="$(mktemp -d)"

  # The relay's one npm dependency. Same install-into-scratch + NODE_PATH shape
  # the Justfile relay recipe and src/Dockerfile.contributor use. Resolution is
  # checked FROM $WORK: the fake hub script lives there, and a ws that only
  # resolves from the repo root (a stray node_modules in the checkout) would
  # pass a cwd-based check and still leave the hub unable to start.
  if ! (cd "$WORK" && node -e "require('ws')" 2>/dev/null); then
    (cd "$WORK" && npm install --no-fund --no-audit ws >/dev/null 2>&1)
    export NODE_PATH="$WORK/node_modules${NODE_PATH:+:$NODE_PATH}"
  fi
  if ! (cd "$WORK" && node -e "require('ws')" 2>/dev/null); then
    skip "npm install ws failed; wire-contract and live scenarios cannot run"
    RIG_OK=0
  fi
fi

if [ "$RIG_OK" = "1" ]; then
  # A fake hub speaking the five message types the relay needs (the ws port of
  # src/deploy/test_contribute_move.sh's fakehub.py): challenge, accept auth,
  # assign one task with SMOKE_PROMPT verbatim, answer pings, and append every
  # inbound relay message as JSONL for the suite to assert on.
  cat > "$WORK/fakehub-ws.js" <<'JS'
const WebSocket = require('ws');
const fs = require('fs');
const portFile = process.argv[2];
const logFile = process.argv[3];
const prompt = process.env.SMOKE_PROMPT || 'missing SMOKE_PROMPT';
let done = false;
const wss = new WebSocket.Server({ host: '127.0.0.1', port: 0 }, () => {
  fs.writeFileSync(portFile, String(wss.address().port));
});
wss.on('connection', (ws) => {
  const send = (o) => { try { ws.send(JSON.stringify(o)); } catch (_) {} };
  let assigned = false;
  send({ type: 'auth_challenge' });
  ws.on('message', (data) => {
    let msg;
    try { msg = JSON.parse(data.toString()); } catch (_) { return; }
    fs.appendFileSync(logFile, JSON.stringify(msg) + '\n');
    if (msg.type === 'auth_response') {
      send({ type: 'auth_ok', contributor_id: 'smoke', trust_tier: 'contributor' });
    } else if (msg.type === 'ready' && !assigned) {
      assigned = true;
      send({ type: 'task_assign', task_id: 'smoke-1', task_gen: 1, kind: 'issue',
             repo: 'hivecommons/hive', number: 0, title: 'backend smoke', prompt });
    } else if (msg.type === 'ping') {
      send({ type: 'pong' });
    } else if (msg.type === 'task_complete' || msg.type === 'task_failed') {
      done = true;
      setTimeout(() => process.exit(0), 500);
    }
  });
});
setTimeout(() => process.exit(done ? 0 : 3),
           Number(process.env.SMOKE_HUB_TIMEOUT_MS || 600000));
JS
fi

# start_fakehub NAME — starts a hub instance; sets HUB_PORT/HUB_LOG/FAKEHUB_PID.
start_fakehub() {
  local name="$1" portfile="$WORK/hub-$1.port"
  HUB_LOG="$WORK/hub-$name.jsonl"
  rm -f "$portfile" "$HUB_LOG"
  SMOKE_PROMPT="$SMOKE_PROMPT" SMOKE_HUB_TIMEOUT_MS="${SMOKE_HUB_TIMEOUT_MS:-600000}" \
    node "$WORK/fakehub-ws.js" "$portfile" "$HUB_LOG" >"$WORK/hub-$name.out" 2>&1 &
  FAKEHUB_PID=$!
  for _ in $(seq 1 50); do
    [ -s "$portfile" ] && { HUB_PORT="$(cat "$portfile")"; return 0; }
    sleep 0.1
  done
  echo "  fake hub '$name' never wrote its port" >&2
  return 1
}

# wait_for_terminal LOG DEADLINE_S — waits for a task_complete or task_failed
# to land in the hub's JSONL.
wait_for_terminal() {
  local log="$1" deadline="$2" start=$SECONDS
  while [ $((SECONDS - start)) -lt "$deadline" ]; do
    if [ -f "$log" ] && jq -e -s \
         'map(select(.type == "task_complete" or .type == "task_failed")) | length > 0' \
         "$log" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

# msg_field LOG TYPE JQ_EXPR — extracts a field from the first message of TYPE.
msg_field() {
  jq -r -s --arg t "$2" "map(select(.type == \$t)) | first | $3" "$1" 2>/dev/null
}

msg_seen() {
  jq -e -s --arg t "$2" 'map(select(.type == $t)) | length > 0' "$1" >/dev/null 2>&1
}

# dump_evidence NAME RELAY_LOG — the escalation-package rule: a red run must
# carry its own raw evidence into the workflow log, so the filed issue's linked
# run answers "what did it actually print" without a rerun.
dump_evidence() {
  echo "  ---- evidence: $1 ----"
  [ -n "${2:-}" ] && [ -f "$2" ] && tail -n 30 "$2" | sed 's/^/  relay| /'
  [ -f "${HUB_LOG:-}" ] && tail -n 10 "$HUB_LOG" | sed 's/^/  hub  | /'
  if [ -n "$TMUX_SESS" ]; then
    tmux capture-pane -t "$TMUX_SESS" -p 2>/dev/null | tail -n 20 | sed 's/^/  pane | /'
  fi
  echo "  ---- end evidence ----"
}

stop_scenario() {
  [ -n "$RELAY_PID" ] && kill "$RELAY_PID" 2>/dev/null
  [ -n "$FAKEHUB_PID" ] && kill "$FAKEHUB_PID" 2>/dev/null
  RELAY_PID=""
  FAKEHUB_PID=""
  if [ -n "$TMUX_SESS" ]; then
    tmux kill-session -t "$TMUX_SESS" 2>/dev/null
    TMUX_SESS=""
  fi
  wait 2>/dev/null
}

# The one prompt every live scenario sends. It IS the sentinel contract from
# buildTaskPromptBody (src/pkg/dashboard/contribute_ws.go) in miniature: one
# model turn, no file access, and the exact HIVE_VERDICT line
# detectHiveVerdict() parses. A backend that cannot follow this has broken the
# completion contract — precisely what the suite exists to catch.
SMOKE_PROMPT='This is an automated integration check. Do not read, create, or modify any files. Reply with exactly this single line and nothing else: HIVE_VERDICT: no_work_needed — backend smoke'

# run_relay_headless BACKEND MODEL HOME_DIR EXTRA_PATH — starts the real relay
# in headless mode against the current fake hub. EXTRA_PATH prepends a stub
# directory (S scenarios); empty for real CLIs.
run_relay_headless() {
  local b="$1" model="$2" home="$3" extra_path="$4"
  RELAY_LOG="$WORK/relay-$b-headless.log"
  mkdir -p "$home" "$WORK/ws-$b"
  (
    cd "$ROOT" || exit 1
    [ -n "$extra_path" ] && export PATH="$extra_path:$PATH"
    HOME="$home" \
    AGENT_BACKEND="$b" \
    AGENT_MODEL="$model" \
    CONTRIBUTOR_MODE=headless \
    HIVE_HUB="ws://127.0.0.1:$HUB_PORT/contribute" \
    HIVE_REGISTRATION_TOKEN=smoke-token \
    HIVE_WORKSPACE_DIR="$WORK/ws-$b" \
    HIVE_HEADLESS_STATUS_FILE="$WORK/status-$b.json" \
    HIVE_HEADLESS_TASK_TIMEOUT_MS=300000 \
    HIVE_TASK_FILE="$WORK/task-$b.json" \
    HIVE_GH_TOKEN_CACHE="$WORK/gh-$b.cache" \
    exec node "$RELAY"
  ) >"$RELAY_LOG" 2>&1 &
  RELAY_PID=$!
}

# ── S. Stub wire-contract scenarios (keyless, no API spend) ──────────────────
if [ "$RIG_OK" = "1" ]; then
  echo ""
  echo "-- S1: stub backend, happy path — task_complete carries the verdict --"
  STUB="$WORK/stub-ok"
  mkdir -p "$STUB"
  # A stand-in "claude" that behaves like a compliant one-shot CLI: prints the
  # sentinel and exits 0. Everything else in the loop — handshake, assignment,
  # execFile, verdict parsing, wire reporting — is the real relay.
  printf '#!/bin/sh\necho "HIVE_VERDICT: no_work_needed — stub smoke"\nexit 0\n' > "$STUB/claude"
  chmod +x "$STUB/claude"
  if start_fakehub s1; then
    run_relay_headless claude "" "$WORK/home-s1" "$STUB"
    if wait_for_terminal "$HUB_LOG" 60; then
      if msg_seen "$HUB_LOG" task_accepted; then pass "relay accepted the task"; else fail "relay accepted the task"; fi
      check "task_complete result" "completed" "$(msg_field "$HUB_LOG" task_complete .result)"
      check "verdict parsed off the one-shot output" "no_work_needed" \
            "$(msg_field "$HUB_LOG" task_complete .verdict)"
      check "relay re-advertised ready after completing" "ready" \
            "$(jq -r -s 'last | .type' "$HUB_LOG")"
      check "headless status file settled" "waiting" \
            "$(jq -r '.state' "$WORK/status-claude.json" 2>/dev/null)"
    else
      fail "stub happy path reached a terminal message within 60s"
      dump_evidence "S1" "$RELAY_LOG"
    fi
  else
    fail "fake hub started (S1)"
  fi
  stop_scenario

  echo ""
  echo "-- S2: stub backend, failure path — task_failed carries the exit code --"
  STUB_BAD="$WORK/stub-bad"
  mkdir -p "$STUB_BAD"
  printf '#!/bin/sh\necho "stub detonation" >&2\nexit 42\n' > "$STUB_BAD/claude"
  chmod +x "$STUB_BAD/claude"
  if start_fakehub s2; then
    run_relay_headless claude "" "$WORK/home-s2" "$STUB_BAD"
    if wait_for_terminal "$HUB_LOG" 60; then
      check "task_failed result" "failed" "$(msg_field "$HUB_LOG" task_failed .result)"
      check "failure is not permanent (retryable elsewhere)" "false" \
            "$(msg_field "$HUB_LOG" task_failed .permanent)"
      contains "reason names the exit code" \
               "$(msg_field "$HUB_LOG" task_failed .reason)" "code 42"
      contains "reason preserves the CLI's own last line" \
               "$(msg_field "$HUB_LOG" task_failed .reason)" "stub detonation"
    else
      fail "stub failure path reached a terminal message within 60s"
      dump_evidence "S2" "$RELAY_LOG"
    fi
  else
    fail "fake hub started (S2)"
  fi
  stop_scenario
fi

# ── B. Live per-backend scenarios ────────────────────────────────────────────

# seed_backend_auth BACKEND HOME_DIR — puts a credential into a throwaway HOME.
# Three sources, in order of preference:
#   1. an API key (ANTHROPIC_API_KEY / OPENAI_API_KEY) — the scheduled lane's
#      first choice: keys never expire mid-schedule;
#   2. a base64-encoded subscription login blob
#      (HIVE_SMOKE_CLAUDE_CREDENTIALS_B64 = ~/.claude/.credentials.json,
#      HIVE_SMOKE_CODEX_AUTH_B64 = ~/.codex/auth.json) — for projects running
#      the lane on a Claude Pro/Max or ChatGPT account instead of metered
#      keys. Same shape as HIVE_CLAUDE_CREDENTIALS_B64 (#5103). CAVEAT: these
#      are OAuth tokens with rotating refresh chains, so the stored secret
#      goes stale after a while and must be re-captured from a fresh login —
#      a stale one surfaces as NOT_AUTHED-style live-scenario failures;
#   3. copying the operator's own logged-in credential from the real HOME, so
#      a maintainer's laptop run exercises the full arm with zero setup.
# Echoes "ok" or "missing".

# decode_b64_credential B64_VALUE DEST — the careful-decode shape from
# contributor-agent.sh (#5103): decode to a temp file first and discard on
# failure, so a corrupt secret leaves no half-written credential behind.
decode_b64_credential() {
  local b64="$1" dest="$2" tmp
  mkdir -p "$(dirname "$dest")"
  tmp="$(mktemp "$dest.tmp.XXXXXX")"
  if printf '%s' "$b64" | base64 -d > "$tmp" 2>/dev/null && [ -s "$tmp" ]; then
    chmod 600 "$tmp"
    mv "$tmp" "$dest"
    return 0
  fi
  rm -f "$tmp"
  # stderr: callers run inside command substitution, which would swallow (and
  # worse, capture) a stdout note into the ok/missing result.
  echo "  note: a base64 credential for $dest did not decode to a non-empty file; ignoring it" >&2
  return 1
}
seed_backend_auth() {
  local b="$1" home="$2"
  mkdir -p "$home"
  case "$b" in
    claude)
      if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
        seeded=ok
      elif [ -n "${HIVE_SMOKE_CLAUDE_CREDENTIALS_B64:-}" ] \
          && decode_b64_credential "$HIVE_SMOKE_CLAUDE_CREDENTIALS_B64" "$home/.claude/.credentials.json"; then
        seeded=ok
      elif [ -f "$REAL_HOME/.claude/.credentials.json" ]; then
        mkdir -p "$home/.claude"
        cp "$REAL_HOME/.claude/.credentials.json" "$home/.claude/"
        chmod 600 "$home/.claude/.credentials.json" 2>/dev/null || true
        seeded=ok
      else
        seeded=missing
      fi
      if [ "$seeded" = "ok" ]; then
        # Pre-answer every first-run gate a fresh HOME raises, the way the
        # hub does (inferenceUserConfigSeed / inferenceSettingsSeed in
        # src/pkg/agent/manager.go): onboarding + custom-API-key approval in
        # .claude.json, and — crucially — skipDangerousModePermissionPrompt
        # in .claude/settings.json, the only key that suppresses the "Bypass
        # Permissions mode" consent menu, whose default selection is
        # "No, exit". Without it, any dismissal loop that answers with a bare
        # Enter makes claude EXIT and the pane degrade to bash.
        HOME="$home" python3 - <<'PYEOF' 2>/dev/null || true
import json, os
home = os.path.expanduser('~')
d = {'hasCompletedOnboarding': True, 'autoUpdates': False, 'installMethod': 'npm',
     'bypassPermissionsModeAccepted': True}
key = os.environ.get('ANTHROPIC_API_KEY', '')
if key:
    d['customApiKeyResponses'] = {'approved': [k for k in (key, key[-20:]) if k], 'rejected': []}
with open(os.path.join(home, '.claude.json'), 'w') as f:
    json.dump(d, f, indent=2)
os.makedirs(os.path.join(home, '.claude'), exist_ok=True)
with open(os.path.join(home, '.claude', 'settings.json'), 'w') as f:
    json.dump({'permissions': {'allow': [], 'deny': []},
               'hasCompletedOnboarding': True,
               'bypassPermissions': True,
               'hasAcknowledgedDisclaimer': True,
               'skipDangerousModePermissionPrompt': True}, f, indent=2)
PYEOF
        chmod 600 "$home/.claude.json" 2>/dev/null || true
      fi
      echo "$seeded"
      ;;
    codex)
      # A minimal CODEX_HOME under the throwaway HOME. This deliberately walks
      # the fresh-CODEX_HOME surface the #5335 healing work covers, and the
      # auth.json shape matches codex_auth_json_has_credentials in
      # bin/contributor-agent.sh.
      if [ -n "${OPENAI_API_KEY:-}" ]; then
        mkdir -p "$home/.codex"
        printf '{"OPENAI_API_KEY": "%s"}\n' "$OPENAI_API_KEY" > "$home/.codex/auth.json"
        chmod 600 "$home/.codex/auth.json"
        echo ok
      elif [ -n "${HIVE_SMOKE_CODEX_AUTH_B64:-}" ] \
          && decode_b64_credential "$HIVE_SMOKE_CODEX_AUTH_B64" "$home/.codex/auth.json"; then
        echo ok
      elif [ -f "$REAL_HOME/.codex/auth.json" ]; then
        mkdir -p "$home/.codex"
        cp "$REAL_HOME/.codex/auth.json" "$home/.codex/"
        chmod 600 "$home/.codex/auth.json" 2>/dev/null || true
        echo ok
      else
        echo missing
      fi
      ;;
    *)
      echo missing
      ;;
  esac
}

smoke_model_for() {
  case "$1" in
    claude) normalize_model_for_backend claude "$MODEL_CLAUDE" ;;
    codex)  echo "$MODEL_CODEX" ;;
    *)      echo "" ;;
  esac
}

if [ "$RIG_OK" = "1" ]; then
  for b in $SMOKE_BACKENDS; do
    model="$(smoke_model_for "$b")"
    bhome="$WORK/home-$b"

    echo ""
    echo "-- B0 [$b]: detect_cli health probe --"
    # contributor-agent.sh's own detection, through its own test seam — the
    # probe every backend gets, normalizing the old asymmetry where only the
    # claude arm of `just contribute-check` did a real check.
    st="$(HOME="$bhome" HIVE_REGISTRATION_TOKEN=smoke-token \
          HIVE_CONTRIBUTOR_AGENT_TEST_DETECT_CLI=1 AGENT_BACKEND="$b" \
          bash "$ROOT/bin/contributor-agent.sh" 2>/dev/null | tail -1)"
    case "$st" in
      OK) pass "detect_cli($b) = OK" ;;
      NOT_INSTALLED)
        skip "$b CLI not installed — live scenarios for '$b' cannot run"
        continue
        ;;
      *)
        # NOT_AUTHED/BROKEN: report it, and let the credential check below
        # decide whether the live scenarios can still run (detect_cli reads
        # the real HOME's login state; the scenarios bring their own key).
        echo "  note: detect_cli($b) = ${st:-<empty>}"
        ;;
    esac

    if [ "$(seed_backend_auth "$b" "$bhome")" != "ok" ]; then
      skip "no credential for '$b' (need ANTHROPIC_API_KEY / OPENAI_API_KEY or an existing login) — live scenarios skipped"
      continue
    fi

    echo ""
    echo "-- B1 [$b]: headless end-to-end (real CLI one-shot, $model) --"
    if start_fakehub "b1-$b"; then
      run_relay_headless "$b" "$model" "$bhome" ""
      if wait_for_terminal "$HUB_LOG" 360; then
        if msg_seen "$HUB_LOG" task_accepted; then pass "relay accepted the task"; else fail "relay accepted the task"; fi
        if msg_seen "$HUB_LOG" task_complete; then
          check "task_complete result" "completed" "$(msg_field "$HUB_LOG" task_complete .result)"
          check "the $b CLI followed the sentinel contract" "no_work_needed" \
                "$(msg_field "$HUB_LOG" task_complete .verdict)"
        else
          fail "headless run completed" \
               "task_failed: $(msg_field "$HUB_LOG" task_failed .reason)"
          dump_evidence "B1-$b" "$RELAY_LOG"
        fi
      else
        fail "headless run reached a terminal message within 360s"
        dump_evidence "B1-$b" "$RELAY_LOG"
      fi
    else
      fail "fake hub started (B1-$b)"
    fi
    stop_scenario

    if ! command -v tmux >/dev/null 2>&1; then
      skip "tmux not installed — interactive scenario for '$b' skipped"
      continue
    fi

    echo ""
    echo "-- B2 [$b]: interactive end-to-end (tmux pane, completion_signal) --"
    # The drift surface: readiness regexes against a REAL current pane,
    # first-run dialog auto-dismissal, and the #5376 completion contract —
    # completion_signal must be 'verdict'. chrome_idle here means the task
    # completed only because the fallback saved it: the sentinel contract is
    # broken for this backend and the fleet is one chrome restyle away from
    # the next #4127.
    TMUX_SESS="hive-smoke-$b"
    tmux kill-session -t "$TMUX_SESS" 2>/dev/null
    if start_fakehub "b2-$b" && tmux new-session -d -s "$TMUX_SESS" -c "$WORK/ws-$b"; then
      CMD="$(backend_binary "$b")"
      PERM_FLAG="$(backend_perm_flag_shell "$b")"
      MODEL_FLAG=""
      case "$b" in goose|bob) ;; *) [ -n "$model" ] && MODEL_FLAG="--model $model" ;; esac
      # Same launch line contributor-agent.sh types, into the same kind of
      # fresh-HOME pane a new contributor gets.
      tmux send-keys -t "$TMUX_SESS" \
        "cd $(printf %q "$WORK/ws-$b") && HOME=$(printf %q "$bhome") CODEX_HOME=$(printf %q "$bhome/.codex") $CMD $PERM_FLAG $MODEL_FLAG" Enter

      # contributor-agent.sh's auto-dismiss loop, abbreviated: first-run
      # trust/theme/API-key dialogs must be cleared for readiness to be
      # reachable at all — their patterns going stale is itself a drift
      # failure this scenario would surface as a readiness timeout.
      (
        for _ in $(seq 1 10); do
          sleep 3
          PANE="$(tmux capture-pane -t "$TMUX_SESS" -p -S -10 2>/dev/null || true)"
          if echo "$PANE" | grep -q "trust this folder\|trust the files\|Confirm folder trust\|Enter to confirm"; then
            tmux send-keys -t "$TMUX_SESS" Enter 2>/dev/null || true
          elif echo "$PANE" | grep -q "Do you trust the contents of this directory"; then
            tmux send-keys -t "$TMUX_SESS" "1" Enter 2>/dev/null || true
          elif echo "$PANE" | grep -q "Choose the text style"; then
            tmux send-keys -t "$TMUX_SESS" "1" Enter 2>/dev/null || true
          elif echo "$PANE" | grep -q "Bypass Permissions mode"; then
            # Fallback only — the settings seed suppresses this menu. Its
            # default selection is "No, exit", so a bare Enter kills the CLI.
            tmux send-keys -t "$TMUX_SESS" "2" Enter 2>/dev/null || true
          elif echo "$PANE" | grep -qi "custom API key"; then
            tmux send-keys -t "$TMUX_SESS" "1" Enter 2>/dev/null || true
          elif echo "$PANE" | grep -q "bypass permissions\|❯\|›\|/ commands\|> *$"; then
            break
          fi
        done
      ) &
      DISMISS_PID=$!

      RELAY_LOG="$WORK/relay-$b-interactive.log"
      (
        cd "$ROOT" || exit 1
        HOME="$bhome" \
        CODEX_HOME="$bhome/.codex" \
        AGENT_BACKEND="$b" \
        AGENT_MODEL="$model" \
        HIVE_AGENT_SESSION="$TMUX_SESS" \
        HIVE_AGENT_CWD="$WORK/ws-$b" \
        HIVE_HUB="ws://127.0.0.1:$HUB_PORT/contribute" \
        HIVE_REGISTRATION_TOKEN=smoke-token \
        HIVE_WORKSPACE_DIR="$WORK/ws-$b" \
        HIVE_TASK_FILE="$WORK/task-$b.json" \
        HIVE_GH_TOKEN_CACHE="$WORK/gh-$b.cache" \
        exec node "$RELAY"
      ) >"$RELAY_LOG" 2>&1 &
      RELAY_PID=$!

      # The interactive completion check runs on the relay's 120s progress
      # tick, so the floor here is ~2.5 minutes even for an instant reply.
      if wait_for_terminal "$HUB_LOG" 480; then
        if msg_seen "$HUB_LOG" task_complete; then
          sig="$(msg_field "$HUB_LOG" task_complete .completion_signal)"
          if [ "$sig" = "verdict" ]; then
            pass "completion_signal=verdict — the $b CLI honored the sentinel contract"
          else
            fail "completion_signal=verdict — the $b CLI honored the sentinel contract" \
                 "got '$sig': the task completed but only the chrome-idle fallback saved it; the HIVE_VERDICT contract is broken for $b"
            dump_evidence "B2-$b" "$RELAY_LOG"
          fi
          check "verdict on the wire" "no_work_needed" \
                "$(msg_field "$HUB_LOG" task_complete .verdict)"
        else
          fail "interactive run completed" \
               "task_failed: $(msg_field "$HUB_LOG" task_failed .reason)"
          dump_evidence "B2-$b" "$RELAY_LOG"
        fi
      else
        fail "interactive run reached a terminal message within 480s (readiness regexes may no longer match the real $b pane)"
        dump_evidence "B2-$b" "$RELAY_LOG"
      fi
      kill "$DISMISS_PID" 2>/dev/null
    else
      fail "fake hub + tmux session started (B2-$b)"
    fi
    stop_scenario
  done
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
