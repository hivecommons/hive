#!/usr/bin/env bash
# Regression tests for kubestellar/hive#1861: the kick messages must never
# instruct an agent to read the shared GitHub App token cache.
#
# /var/run/hive-metrics/gh-app-token.cache holds the FULL, unscoped
# installation token. Reading it skips the credential helper's UID/mode gate
# and hands any agent full privilege — the escalation #1861 exists to close
# (audit H3, CWE-522/732). The file is 0600 today, so the old instruction can
# no longer succeed for a non-root agent, but an agent TOLD to reach for it
# will try the same path with curl or git, where no wrapper intervenes.
#
# bin/gh-wrapper.sh already refuses that fallback and fails loud, and the v2
# scheduler (v2/pkg/scheduler/scheduler.go ghAuthInstructions) already tells
# agents the right thing. These tests hold the v1 kick lane to the same rule so
# the two lanes cannot drift apart again.
#
# Run: bash bin/kick-agents-gh-auth.test.sh

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KICK="${ROOT_DIR}/bin/kick-agents.sh"
SHARED_CACHE="/var/run/hive-metrics/gh-app-token.cache"
PASSED=0
FAILED=0

pass() { printf '  ✓ %s\n' "$1"; PASSED=$((PASSED + 1)); }
fail() { printf '  ✗ %s\n' "$1"; printf '    %s\n' "${2:-}"; FAILED=$((FAILED + 1)); }

# The agents whose kick messages carry a GH AUTH line. Kept explicit rather
# than derived, so ADDING an agent without an auth line is a visible diff here.
AGENTS=(scanner ci-maintainer architect outreach supervisor)

printf '\n=== kick-agents.sh GH AUTH instruction (#1861) ===\n\n'

# --- 1. the script is valid bash -------------------------------------------
if bash -n "$KICK" 2>/dev/null; then
  pass "kick-agents.sh parses"
else
  fail "kick-agents.sh parses" "$(bash -n "$KICK" 2>&1 | head -3)"
fi

# --- 2. no agent-facing instruction names the shared cache ------------------
# Scoped to the auth helper and the kick message bodies. The script may still
# reference the shared cache in hive-side plumbing that runs as the hive, which
# is legitimate; what must never happen is TELLING AN AGENT to read it.
if grep -n "cat ${SHARED_CACHE}" "$KICK" >/dev/null 2>&1; then
  fail "no kick instruction tells an agent to cat the shared App token cache" \
    "found: $(grep -n "cat ${SHARED_CACHE}" "$KICK" | head -3)"
else
  pass "no kick instruction tells an agent to cat the shared App token cache"
fi

if grep -n "GH_TOKEN=\\\$(cat ${SHARED_CACHE}" "$KICK" >/dev/null 2>&1; then
  fail "no kick instruction exports GH_TOKEN from the shared cache" \
    "found: $(grep -n "GH_TOKEN=\\\$(cat ${SHARED_CACHE}" "$KICK" | head -3)"
else
  pass "no kick instruction exports GH_TOKEN from the shared cache"
fi

# --- 3. the auth helper renders a per-agent path ----------------------------
# Extract just _gh_auth_instr rather than sourcing kick-agents.sh, which does
# real work (reads metrics, talks to tmux) at load time.
HELPER="$(mktemp)"
# shellcheck disable=SC2329 # invoked via EXIT trap
cleanup() { rm -f "$HELPER"; }
trap cleanup EXIT
sed -n '/^_gh_auth_instr() {$/,/^}$/p' "$KICK" >"$HELPER"

if [ -s "$HELPER" ]; then
  pass "_gh_auth_instr helper found in kick-agents.sh"
else
  fail "_gh_auth_instr helper found in kick-agents.sh" "extraction produced nothing"
  printf '\nResult: %d passed, %d failed\n' "$PASSED" "$FAILED"
  exit 1
fi

# shellcheck source=/dev/null
source "$HELPER"

for agent in "${AGENTS[@]}"; do
  rendered="$(_gh_auth_instr "$agent")"

  # The agent's OWN token file, and no other agent's.
  want="/var/run/hive-metrics/agent-tokens/gh-token-${agent}.cache"
  if [[ "$rendered" == *"$want"* ]]; then
    pass "${agent}: instruction names its own token file"
  else
    fail "${agent}: instruction names its own token file" "want substring: $want"
  fi

  # The shared cache may be NAMED in a prohibition ("NEVER read ..."), but must
  # never appear as a path the agent is told to read.
  if [[ "$rendered" == *"cat ${SHARED_CACHE}"* || "$rendered" == *"cat \$(${SHARED_CACHE}"* ]]; then
    fail "${agent}: instruction does not tell the agent to read the shared cache" \
      "rendered: $rendered"
  else
    pass "${agent}: instruction does not tell the agent to read the shared cache"
  fi

  # And it must actively warn against it — silence would let an agent that
  # remembers the old prompt keep using it.
  if [[ "$rendered" == *"NEVER read"*"gh-app-token.cache"* ]]; then
    pass "${agent}: instruction explicitly forbids the shared cache"
  else
    fail "${agent}: instruction explicitly forbids the shared cache" "rendered: $rendered"
  fi
done

# --- 4. the emitted token path is a literal for the agent, not expanded here -
# The kick builder must hand the agent the command text `$(cat ...)`. If the
# substitution ran while BUILDING the kick, the hive would read the file as
# root and paste a live token into a tmux pane and the kick audit log.
rendered_scanner="$(_gh_auth_instr scanner)"
if [[ "$rendered_scanner" == *'$(cat /var/run/hive-metrics/agent-tokens/gh-token-scanner.cache)'* ]]; then
  pass "token path stays a literal command for the agent (no token pasted into the kick)"
else
  fail "token path stays a literal command for the agent (no token pasted into the kick)" \
    "rendered: $rendered_scanner"
fi

# --- 5. every call site passes an agent name --------------------------------
# A bare ${_GH_AUTH_INSTR} left behind would expand to the empty string under
# `set -u`-less bash, silently dropping the auth line from that agent's kick.
if grep -n '_GH_AUTH_INSTR' "$KICK" >/dev/null 2>&1; then
  fail "no stale _GH_AUTH_INSTR references remain" \
    "found: $(grep -n '_GH_AUTH_INSTR' "$KICK" | head -3)"
else
  pass "no stale _GH_AUTH_INSTR references remain"
fi

call_sites="$(grep -c '\$(_gh_auth_instr ' "$KICK" || true)"
if [ "$call_sites" -eq "${#AGENTS[@]}" ]; then
  pass "all ${#AGENTS[@]} kick messages carry a GH AUTH line"
else
  fail "all ${#AGENTS[@]} kick messages carry a GH AUTH line" \
    "found ${call_sites} call sites, expected ${#AGENTS[@]}"
fi

# --- 6. each kick passes ITS OWN agent name ---------------------------------
# The call sites are hand-maintained, one per message, so the natural error is a
# copy-paste that leaves an agent pointing at ANOTHER agent's token file. That
# is a cross-agent credential leak — the agent is handed a path it must never
# read (and, if tiers differ, one that escalates it) — and every check above
# still passes, because the helper itself renders correctly for whatever name
# it is given. Tie each call site back to the [agent:NAME] tag of the message
# it sits inside.
mismatches=""
while IFS=: read -r lineno _; do
  [ -n "$lineno" ] || continue
  passed_agent="$(sed -n "${lineno}p" "$KICK" | sed -n 's/.*_gh_auth_instr \([a-z0-9-]*\)).*/\1/p')"
  # The owning message's tag is the nearest [agent:NAME] at or above this line.
  owning_agent="$(head -n "$lineno" "$KICK" | grep -o '\[agent:[a-z0-9-]*\]' | tail -1 | sed 's/\[agent:\(.*\)\]/\1/')"
  if [ -z "$owning_agent" ]; then
    mismatches="${mismatches}line ${lineno}: no [agent:...] tag found above the call site; "
  elif [ "$passed_agent" != "$owning_agent" ]; then
    mismatches="${mismatches}line ${lineno}: [agent:${owning_agent}] kick passes '${passed_agent}'; "
  fi
done < <(grep -n '\$(_gh_auth_instr ' "$KICK")

if [ -z "$mismatches" ]; then
  pass "every kick passes its own agent name (no cross-agent token path)"
else
  fail "every kick passes its own agent name (no cross-agent token path)" "$mismatches"
fi

printf '\nResult: %d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
