#!/usr/bin/env bash
# Asserts the N14 (#3842) fix: a native/systemd-install agent (kicked via
# kick-agents.sh, no Go AgentManager, no per-agent HIVE_AGENT_TOKEN_CACHE)
# must never be instructed to read the shared, fleet-wide, full-privilege
# GitHub App token cache into its own reasoning/output.
#
# Before this fix, kick-agents.sh's _GH_AUTH_INSTR told every kicked agent to
# `cat /var/run/hive-metrics/gh-app-token.cache` and prefix gh calls with the
# result — the exact anti-pattern audit H3 already removed from gh-wrapper.sh
# (a shared credential reaching agent-controlled text is one prompt injection
# from exfiltration), just reintroduced at the prompt-text layer instead of
# in code.
#
# This is source-asserting rather than executing (the fix's actual effect —
# `gh` working without env-var handling — needs a real `gh` auth state and a
# native install to observe directly), matching the style of
# check-suid-contract.sh and test_supply_chain_pins.sh: it tests the
# INVARIANT text is present/absent, so a future edit that reintroduces the
# cat/prefix instruction (or silently drops the gh-auth-login replacement)
# fails CI instead of shipping quietly.
#
# Run: bash bin/test_gh_auth_native_no_cat.sh
set -euo pipefail

PASS=0
FAIL=0

BIN_DIR="$(cd "$(dirname "$0")" && pwd)"
KICK_AGENTS="$BIN_DIR/kick-agents.sh"
AGENT_LAUNCH="$BIN_DIR/agent-launch.sh"

pass() {
  echo "  PASS: $1"
  PASS=$((PASS + 1))
}

fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

echo "=== N14 (#3842): no shared-token-cat instruction to agents ==="

# --- 1. kick-agents.sh must not instruct agents to cat the shared cache -----
if [ ! -f "$KICK_AGENTS" ]; then
  fail "kick-agents.sh exists" "not found at $KICK_AGENTS"
else
  if grep -qE 'cat[[:space:]]+/var/run/hive-metrics/gh-app-token\.cache' "$KICK_AGENTS"; then
    fail "kick-agents.sh does not instruct agents to cat the shared token cache" \
         "found a 'cat /var/run/hive-metrics/gh-app-token.cache' reference — this puts the raw fleet-wide token in agent-visible text"
  else
    pass "kick-agents.sh does not instruct agents to cat the shared token cache"
  fi

  if grep -qE 'GH_TOKEN=\\\$\(cat' "$KICK_AGENTS"; then
    fail "kick-agents.sh does not build a GH_TOKEN=\$(cat ...) prefix instruction" \
         "found the literal prefix pattern the agent would be told to type"
  else
    pass "kick-agents.sh does not build a GH_TOKEN=\$(cat ...) prefix instruction"
  fi

  if grep -qE '_GH_AUTH_INSTR=' "$KICK_AGENTS"; then
    pass "_GH_AUTH_INSTR is still defined (instruction exists, just not the cat pattern)"
  else
    fail "_GH_AUTH_INSTR is still defined" "the gh-auth instruction variable was removed entirely, not just fixed"
  fi
fi

# --- 2. agent-launch.sh must authenticate gh itself instead ------------------
if [ ! -f "$AGENT_LAUNCH" ]; then
  fail "agent-launch.sh exists" "not found at $AGENT_LAUNCH"
else
  if grep -qE "gh auth login --with-token" "$AGENT_LAUNCH"; then
    pass "agent-launch.sh authenticates gh's own credential store (gh auth login --with-token)"
  else
    fail "agent-launch.sh authenticates gh's own credential store" \
         "expected a 'gh auth login --with-token' step so native agents never handle the raw token themselves"
  fi

  # The fallback must be gated on the ABSENCE of the per-agent scoped cache —
  # otherwise it would also fire (redundantly, or worse, in a way that could
  # conflict) for the container-hosted model, where gh-wrapper.sh already
  # authenticates every gh call per-invocation from the per-agent cache.
  if grep -qE '\-z[[:space:]]+"\$\{HIVE_AGENT_TOKEN_CACHE:-\}"' "$AGENT_LAUNCH"; then
    pass "the gh-auth-login fallback is gated on HIVE_AGENT_TOKEN_CACHE being absent"
  else
    fail "the gh-auth-login fallback is gated on HIVE_AGENT_TOKEN_CACHE being absent" \
         "without this gate the step could run redundantly (or conflict) on the container-hosted model, which already has per-agent auth via gh-wrapper.sh"
  fi

  # The unset lines are the OTHER half of the H3 follow-up (GH_TOKEN/
  # HIVE_GITHUB_TOKEN must never reach the agent CLI's own environment,
  # partly because Copilot CLI overloads GH_TOKEN for its own API auth) and
  # must survive this change untouched.
  if grep -qE '^unset GH_TOKEN$' "$AGENT_LAUNCH" && grep -qE '^unset HIVE_GITHUB_TOKEN$' "$AGENT_LAUNCH"; then
    pass "GH_TOKEN and HIVE_GITHUB_TOKEN are still unset before the agent CLI launches"
  else
    fail "GH_TOKEN and HIVE_GITHUB_TOKEN are still unset before the agent CLI launches" \
         "the N14 fix must not reintroduce the full token into the agent's own environment"
  fi
fi

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
