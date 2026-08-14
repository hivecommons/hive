#!/bin/bash
# git-credential-hive.sh — Git credential helper that uses the GitHub App token.
# Reads from the cached token file, refreshing if stale (>55 min old).
#
# Install (github.com): git config --global credential.https://github.com.helper /usr/local/bin/git-credential-hive.sh
# Install (GHE):         git config --global credential.https://<ghe-host>.helper /usr/local/bin/git-credential-hive.sh
#
# GHE-aware host matching: git invokes credential helpers once per remote host
# and feeds `protocol=...` / `host=...` (and optionally `path=...`) on stdin
# for the `get` operation (see git-credential(1)). A helper that ignores this
# and always answers "host=github.com" — as this script did before — silently
# returns NO credential for any other host git actually asked about, and git
# then falls through to an interactive prompt, which fails hard in a
# non-interactive agent shell with "terminal prompts disabled". This version
# echoes back whatever host git asked for (once it has confirmed a token
# exists for it) instead of a hardcoded "github.com", so the same script and
# the same underlying App-installation token work for github.com AND any
# configured GitHub Enterprise host (e.g. github.ibm.com) — entrypoint.sh
# wires the credential.helper entry for the hive's actual configured host
# (github.HostLabel() in the Go config), so this script only ever gets
# invoked for a host it is actually supposed to answer for.

set -euo pipefail

# ── Read git's credential request off stdin (protocol=... / host=... lines,
# terminated by a blank line) so we can echo the SAME host back. Git passes
# this on `get`; other ops (store/erase) are no-ops below and don't need it,
# but reading it unconditionally is harmless and keeps the parsing in one
# place. See git-credential(1) "Credential Helpers" for the wire format.
REQUEST_PROTOCOL=""
REQUESTED_HOST=""
while IFS='=' read -r _cred_key _cred_val; do
  [ -z "$_cred_key" ] && break
  case "$_cred_key" in
    protocol) REQUEST_PROTOCOL="$_cred_val" ;;
    host) REQUESTED_HOST="$_cred_val" ;;
  esac
done

# ── UID-based identity verification (defense-in-depth) ──
# When running under a per-agent UID (>= 2001), derive the agent name from
# the UID map instead of trusting the HIVE_AGENT env var (which the agent
# could unset or spoof).
UID_MAP_FILE="/var/run/hive/uid-map.json"
CURRENT_UID=$(id -u)
AGENT_UID_BASE=2001

if [ "$CURRENT_UID" -ge "$AGENT_UID_BASE" ] && [ -f "$UID_MAP_FILE" ]; then
  UID_AGENT=$(python3 -c "
import json, sys
with open('$UID_MAP_FILE') as f:
    m = json.load(f)
for name, uid in m.get('agents', {}).items():
    if uid == $CURRENT_UID:
        print(name)
        sys.exit(0)
sys.exit(1)
" 2>/dev/null) || true
  if [ -n "$UID_AGENT" ]; then
    AGENT="$UID_AGENT"
  else
    echo "⛔ git push blocked: unknown agent UID ${CURRENT_UID}" >&2
    exit 1
  fi
else
  AGENT="${HIVE_AGENT:-}"
fi

# ── Mode-based enforcement: block git push for agents without push capability ──

# Read mode from file first (hot-reloadable), fallback to env var
MODE_FILE="/tmp/.hive-mode-${AGENT}"
if [ -f "$MODE_FILE" ] && [ -r "$MODE_FILE" ]; then
  AGENT_MODE="$(cat "$MODE_FILE")"
else
  AGENT_MODE="${HIVE_AGENT_MODE:-}"
fi

if [ -n "$AGENT_MODE" ]; then
  case "$AGENT_MODE" in
    NO_GITHUB|ADVISORY|ISSUES_ONLY)
      echo "⛔ git push blocked: ${AGENT} is in ${AGENT_MODE} mode" >&2
      echo "⛔ TERMINAL: this block is mode-enforced and retrying can NEVER succeed. Do NOT retry, loop, or seek workarounds. Record your intended change as an advisory finding (bd create / [FINDING]) and end this task." >&2
      exit 1
      ;;
  esac
else
  # Fallback: level-based enforcement
  ACMM="${HIVE_ACMM_LEVEL:-0}"
  if [ -n "$AGENT" ] && [ "$ACMM" -gt 0 ]; then
    if [ "$ACMM" -lt 3 ]; then
      echo "⛔ git push blocked: ACMM L${ACMM} agents are advisory-only" >&2
      echo "⛔ TERMINAL: this block is mode-enforced and retrying can NEVER succeed. Do NOT retry, loop, or seek workarounds. Record your intended change as an advisory finding (bd create / [FINDING]) and end this task." >&2
      exit 1
    fi
    if [ "$ACMM" -eq 3 ] && [ "$AGENT" != "quality" ]; then
      echo "⛔ git push blocked: only quality agent can push at ACMM L3" >&2
      echo "⛔ TERMINAL: this block is mode-enforced and retrying can NEVER succeed. Do NOT retry, loop, or seek workarounds. Record your intended change as an advisory finding (bd create / [FINDING]) and end this task." >&2
      exit 1
    fi
  fi
fi

TOKEN_FILE="/var/run/hive-metrics/gh-app-token.cache"
CACHE_MAX_AGE_SECONDS=3300

refresh_token() {
  if [ -x /usr/local/bin/gh-app-token.sh ]; then
    /usr/local/bin/gh-app-token.sh >/dev/null 2>&1 || true
  fi
}

# Per-agent scoped token takes priority (Phase 4 — least-privilege).
AGENT_TOKEN_FILE="${HIVE_AGENT_TOKEN_CACHE:-}"
if [ -n "$AGENT_TOKEN_FILE" ] && [ -f "$AGENT_TOKEN_FILE" ]; then
  TOKEN_FILE="$AGENT_TOKEN_FILE"
else
  if [ ! -f "$TOKEN_FILE" ]; then
    refresh_token
  fi

  if [ -f "$TOKEN_FILE" ]; then
    cache_age=$(( $(date +%s) - $(stat -c %Y "$TOKEN_FILE" 2>/dev/null || echo 0) ))
    if [ "$cache_age" -gt "$CACHE_MAX_AGE_SECONDS" ]; then
      refresh_token
    fi
  fi
fi

TOKEN=$(cat "$TOKEN_FILE" 2>/dev/null || true)
if [ -z "$TOKEN" ]; then
  exit 1
fi

TOKEN_ACCESS_LOG="/var/run/hive-metrics/token-access.jsonl"

case "${1:-}" in
  get)
    # Only ever answer for https — this script has no business supplying a
    # credential for git+ssh or any other protocol, on any host.
    if [ -n "$REQUEST_PROTOCOL" ] && [ "$REQUEST_PROTOCOL" != "https" ]; then
      exit 0
    fi
    printf '{"ts":"%s","agent":"%s","uid":%d,"op":"git-credential","host":"%s"}\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${AGENT:-unknown}" "$(id -u)" "${REQUESTED_HOST:-unknown}" \
      >> "$TOKEN_ACCESS_LOG" 2>/dev/null || true
    # Echo back the SAME host git asked about (github.com, github.ibm.com, or
    # any other configured GitHub Enterprise host) rather than a hardcoded
    # "github.com" — see the file header. entrypoint.sh only registers this
    # helper for `credential.https://<configured-host>.helper`, so by the time
    # git invokes it for `get`, REQUESTED_HOST is already the one host this
    # hive is authorized to answer for. Falling back to "github.com" when git
    # sends no host at all (older git, or a manual invocation) preserves the
    # previous behavior for the public-GitHub case.
    echo "protocol=https"
    echo "host=${REQUESTED_HOST:-github.com}"
    echo "username=x-access-token"
    echo "password=$TOKEN"
    ;;
esac
