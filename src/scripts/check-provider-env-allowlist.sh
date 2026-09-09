#!/usr/bin/env bash
# check-provider-env-allowlist.sh — static contract check for the provider
# environment allowlist in the Justfile's contribute-hive recipe.
#
# Regression coverage for hivecommons/hive#6400: the Goose backend forwarded
# GOOSE_PROVIDER, GOOSE_MODEL and OPENAI_API_KEY from the host into the
# contributor container, but not OPENAI_HOST or OPENAI_BASE_PATH — so Goose
# configured against an OpenAI-compatible local inference server fell back to
# api.openai.com and failed with 401. This is a grep-based static check, not a
# live container run: it asserts the two variables remain in the allowlist
# `for name in ...` loop that feeds add_provider_env (which already forwards a
# name only when it is set on the host — `[[ -n "${!name:-}" ]]` — and omits
# it otherwise, so covering membership in the loop covers both the "forwarded
# when set" and "absent when unset" behaviors for these vars).
#
# Usage: src/scripts/check-provider-env-allowlist.sh [path-to-Justfile]
set -euo pipefail

JUSTFILE="${1:-Justfile}"

if [[ ! -f "$JUSTFILE" ]]; then
  echo "ERROR: Justfile not found at ${JUSTFILE}" >&2
  exit 1
fi

fail=0

check() {
  local desc="$1" pattern="$2"
  if grep -qE "$pattern" "$JUSTFILE"; then
    echo "  ok: ${desc}"
  else
    echo "  FAIL: ${desc} (expected to find pattern: ${pattern})"
    fail=1
  fi
}

echo "== Provider env allowlist contract check (${JUSTFILE}) =="

# The allowlist loop must still exist and use the conditional forwarder —
# add_provider_env only appends "-e NAME" (never a literal value) when the
# host has the variable set, so a name missing from this list is silently
# dropped rather than forwarded.
check "conditional forwarder (add_provider_env) still present" \
  'add_provider_env\(\) \{'
check "allowlist loop guards on host-set value only" \
  '\[\[ -n "\$\{!name:-\}" \]\]; then PROVIDER_ENV_ARGS'

# OPENAI_HOST and OPENAI_BASE_PATH must be members of the allowlist `for
# name in ...` loop alongside the existing GOOSE_PROVIDER/GOOSE_MODEL/
# OPENAI_API_KEY entries (#6400).
check "OPENAI_HOST is forwarded to the contributor container" \
  'for name in [^$]*\bOPENAI_HOST\b'
check "OPENAI_BASE_PATH is forwarded to the contributor container" \
  'for name in [^$]*\bOPENAI_BASE_PATH\b'

if [[ "$fail" -ne 0 ]]; then
  echo ""
  echo "Provider env allowlist contract check FAILED — see hivecommons/hive#6400"
  exit 1
fi

echo ""
echo "Provider env allowlist contract check passed."
