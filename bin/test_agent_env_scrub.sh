#!/usr/bin/env bash
# Behavioural + source tests for the #4045 fix: backend CLIs re-export live
# GitHub credentials (GITHUB_TOKEN et al.) into the tool shells they spawn for
# agents, bypassing every gh-wrapper control. bin/agent-env-scrub.sh, sourced
# via BASH_ENV at CHILD shell startup, must neutralize the token no matter how
# it arrived — inherited or set explicitly in the spawn env by the CLI (the
# observed mechanism, which parent-side scrubbing like #3931 cannot reach).
#
# Doctrine (audit 6/7): every block-assertion here has a positive control that
# proves the probe can detect the leak, and the sanctioned paths (gh-wrapper
# auth, backend-process auth) are asserted ALIVE — a scrub that broke them
# would get reverted wholesale, which is worse than the vulnerability.
#
# No token material is real anywhere in this file; probes assert presence/
# absence, never values.
#
# Run: bash bin/test_agent_env_scrub.sh
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRUB="${REPO_ROOT}/bin/agent-env-scrub.sh"
AGENT_LAUNCH="${REPO_ROOT}/bin/agent-launch.sh"
WRAPPER="${REPO_ROOT}/bin/gh-wrapper.sh"
DOCKERFILE="${REPO_ROOT}/src/Dockerfile"

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

# The single source of truth for what the scrub must cover. Adding a var here
# without adding it to agent-env-scrub.sh fails the source assertions below.
SCRUBBED_VARS=(
  GITHUB_TOKEN
  GH_TOKEN
  GH_ENTERPRISE_TOKEN
  GITHUB_ENTERPRISE_TOKEN
  COPILOT_GITHUB_TOKEN
  GITHUB_COPILOT_TOKEN
  HIVE_GITHUB_TOKEN
)

# probe_child <BASH_ENV-value or ""> <extra env assignments...>
# Spawns a bash -c child the way a backend CLI spawns a tool shell — with the
# given vars set in the SPAWN env (env(1), exactly how the Copilot CLI hands
# GITHUB_TOKEN to its shell tool) — and reports each scrubbed var as
# name=present|absent.
probe_child() {
  local bash_env="$1"; shift
  local probe='out=""; for v in GITHUB_TOKEN GH_TOKEN GH_ENTERPRISE_TOKEN GITHUB_ENTERPRISE_TOKEN COPILOT_GITHUB_TOKEN GITHUB_COPILOT_TOKEN HIVE_GITHUB_TOKEN; do if [ -n "$(eval "printf %s \"\${$v:-}\"")" ]; then out="$out $v=present"; else out="$out $v=absent"; fi; done; printf "%s" "$out"'
  if [ -n "$bash_env" ]; then
    env "$@" BASH_ENV="$bash_env" bash -c "$probe"
  else
    # -u BASH_ENV: the runner's own env must not accidentally scrub the
    # positive control.
    env -u BASH_ENV "$@" bash -c "$probe"
  fi
}

FAKE_ENV=(
  GITHUB_TOKEN="fake-github-token-for-test"
  GH_TOKEN="fake-gh-token-for-test"
  GH_ENTERPRISE_TOKEN="fake-ghe-token-for-test"
  GITHUB_ENTERPRISE_TOKEN="fake-github-ent-token-for-test"
  COPILOT_GITHUB_TOKEN="fake-copilot-token-for-test"
  GITHUB_COPILOT_TOKEN="fake-copilot2-token-for-test"
  HIVE_GITHUB_TOKEN="fake-hive-token-for-test"
)

echo "=== #4045: backend-exported tokens must not survive into agent tool shells ==="

# ── POSITIVE CONTROL first: the probe must be able to SEE a leak ─────────────
# Without this, a broken probe (or a probe run without the vars) would make
# every absence-assertion below pass vacuously — the exact failure mode audit 6
# kept finding.
echo "-- positive control: unscrubbed shell shows every token --"
result="$(probe_child "" "${FAKE_ENV[@]}")"
if [[ "$result" != *"=absent"* && "$result" == *"GITHUB_TOKEN=present"* ]]; then
  pass "probe detects all injected tokens when no scrub is wired"
else
  fail "probe detects all injected tokens when no scrub is wired" "got:$result"
fi

# ── The #4045 replay: CLI sets tokens in the tool shell's spawn env ──────────
echo "-- scrubbed tool shell: every known token var is absent --"
result="$(probe_child "$SCRUB" "${FAKE_ENV[@]}")"
if [[ "$result" != *"=present"* ]]; then
  pass "all ${#SCRUBBED_VARS[@]} token vars absent in a BASH_ENV-scrubbed child"
else
  fail "all ${#SCRUBBED_VARS[@]} token vars absent in a BASH_ENV-scrubbed child" "got:$result"
fi

# The literal incident shape: the shell expansion the bypass relied on must
# come up empty, so the raw-curl fallback sends "Bearer " and 401s.
result="$(env "${FAKE_ENV[@]}" BASH_ENV="$SCRUB" bash -c 'printf "Bearer %s" "${GITHUB_TOKEN:-}"')"
if [ "$result" = "Bearer " ]; then
  pass "incident replay: 'Authorization: Bearer \$GITHUB_TOKEN' expands empty"
else
  fail "incident replay: 'Authorization: Bearer \$GITHUB_TOKEN' expands empty" "expansion was non-empty"
fi

# Nested re-export: even a shell whose PARENT was scrubbed gets the token
# re-injected by the CLI layer per spawn — each new bash must re-scrub because
# BASH_ENV stays exported down the tree.
result="$(env "${FAKE_ENV[@]}" BASH_ENV="$SCRUB" bash -c 'GITHUB_TOKEN=fake-reexported-by-cli bash -c "printf %s \"\${GITHUB_TOKEN:+present}\""')"
if [ -z "$result" ]; then
  pass "a token re-exported into a NESTED shell is scrubbed again"
else
  fail "a token re-exported into a NESTED shell is scrubbed again" "nested shell saw the token"
fi

# ── Scrub must not strip what the sanctioned paths need ──────────────────────
echo "-- non-credential env survives the scrub --"
result="$(env HIVE_AGENT_TOKEN_CACHE="/var/run/x" HIVE_ACMM_LEVEL=3 HTTPS_PROXY="http://127.0.0.1:3128" BASH_ENV="$SCRUB" bash -c 'printf "%s %s %s" "${HIVE_AGENT_TOKEN_CACHE:+cache}" "${HIVE_ACMM_LEVEL:+acmm}" "${HTTPS_PROXY:+proxy}"')"
if [ "$result" = "cache acmm proxy" ]; then
  pass "HIVE_AGENT_TOKEN_CACHE / ACMM / proxy vars pass through untouched"
else
  fail "HIVE_AGENT_TOKEN_CACHE / ACMM / proxy vars pass through untouched" "got '$result'"
fi

# ── SANCTIONED-PATH CONTROL: gh-wrapper still authenticates under the scrub ──
# The wrapper sources the scrub at startup (it is a bash script under the
# agent's BASH_ENV), losing any inherited token env — which it must never trust
# anyway (H3) — then exports GH_TOKEN itself from HIVE_AGENT_TOKEN_CACHE. The
# stub asserts the token it sees is exactly the CACHE one, proving (a) the
# wrapper path is alive and (b) the stale inherited token was replaced.
echo "-- gh-wrapper authenticates from the per-agent cache under the scrub --"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
TOKEN_CACHE="${WORK}/token"
echo "ghs_fake_cache_token_for_test" >"$TOKEN_CACHE"
STUB="${WORK}/gh-stub"
STUB_LOG="${WORK}/stub.log"
# The stub is /bin/sh ON PURPOSE: a bash stub would itself source BASH_ENV at
# startup and scrub the GH_TOKEN the wrapper just exported — the REAL gh is a
# Go binary and sources nothing, so /bin/sh (which reads neither BASH_ENV nor
# non-interactive ENV) models it correctly.
cat >"$STUB" <<'STUBEOF'
#!/bin/sh
if [ "${GH_TOKEN:-}" = "ghs_fake_cache_token_for_test" ]; then
  echo "CACHE_TOKEN_OK" >> "${STUB_LOG}"
elif [ -n "${GH_TOKEN:-}" ]; then
  echo "WRONG_TOKEN" >> "${STUB_LOG}"
else
  echo "UNAUTHENTICATED" >> "${STUB_LOG}"
fi
exit 0
STUBEOF
chmod +x "$STUB"
: >"$STUB_LOG"
out="$(
  env "${FAKE_ENV[@]}" \
    BASH_ENV="$SCRUB" \
    HIVE_GH_WRAPPER_REAL_GH="$STUB" \
    STUB_LOG="$STUB_LOG" \
    HIVE_AGENT="testagent" \
    HIVE_AGENT_ID="testagent" \
    HIVE_AGENT_MODE="ISSUES_PRS_MERGE" \
    HIVE_ACMM_LEVEL=5 \
    HIVE_AGENT_TOKEN_CACHE="$TOKEN_CACHE" \
    HIVE_CONTRIBUTOR_MODE="false" \
    bash "$WRAPPER" pr view 1 --repo owner/repo 2>&1
)"
rc=$?
stub_saw="$(cat "$STUB_LOG" 2>/dev/null | tail -n1)"
if [ "$rc" -eq 0 ] && [ "$stub_saw" = "CACHE_TOKEN_OK" ]; then
  pass "wrapper reached gh with the CACHE token (inherited fakes discarded)"
else
  fail "wrapper reached gh with the CACHE token (inherited fakes discarded)" "rc=$rc stub_saw='${stub_saw:-nothing}' out=$(echo "$out" | head -n2 | tr '\n' ' ')"
fi

# ── The scrub file must be POSIX-sh safe (tool shells may be /bin/sh) ────────
echo "-- scrub is sh-compatible --"
if sh -c ". '$SCRUB'" 2>/dev/null; then
  pass "agent-env-scrub.sh sources cleanly under sh"
else
  fail "agent-env-scrub.sh sources cleanly under sh"
fi

# ── Source-level invariants (drift guards) ───────────────────────────────────
echo "-- source invariants --"
for v in "${SCRUBBED_VARS[@]}"; do
  if grep -qE "^unset[[:space:]]+${v}\$" "$SCRUB"; then
    pass "scrub unsets ${v}"
  else
    fail "scrub unsets ${v}" "add 'unset ${v}' to bin/agent-env-scrub.sh"
  fi
done
if grep -qE 'unset[[:space:]].*HIVE_AGENT_TOKEN_CACHE' "$SCRUB"; then
  fail "scrub must NOT unset HIVE_AGENT_TOKEN_CACHE (it is a path the wrapper and git credential helper depend on)"
else
  pass "scrub leaves HIVE_AGENT_TOKEN_CACHE alone"
fi
if grep -qE '^\s*export BASH_ENV="\$AGENT_ENV_SCRUB"' "$AGENT_LAUNCH" && grep -qE '^\s*export ENV="\$AGENT_ENV_SCRUB"' "$AGENT_LAUNCH"; then
  pass "agent-launch.sh wires BASH_ENV and ENV to the scrub"
else
  fail "agent-launch.sh wires BASH_ENV and ENV to the scrub" "the #4045 boundary is not installed for tool shells"
fi
if grep -q 'bin/agent-env-scrub.sh /usr/local/bin/agent-env-scrub.sh' "$DOCKERFILE"; then
  pass "Dockerfile ships agent-env-scrub.sh"
else
  fail "Dockerfile ships agent-env-scrub.sh" "the launcher's fallback path /usr/local/bin/agent-env-scrub.sh would be empty in the image"
fi
if grep -q 'agent-env-scrub.sh' "$DOCKERFILE" && grep -q 'bash.bashrc' "$DOCKERFILE"; then
  pass "Dockerfile installs the interactive-shell (/etc/bash.bashrc) arm"
else
  fail "Dockerfile installs the interactive-shell (/etc/bash.bashrc) arm"
fi

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1
