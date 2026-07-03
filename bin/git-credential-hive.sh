#!/bin/bash
# git-credential-hive.sh — Git credential helper that uses the GitHub App token.
# Reads from the cached token file, refreshing if stale (>55 min old).
# Install: git config --global credential.https://github.com.helper /usr/local/bin/git-credential-hive.sh

set -euo pipefail

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
if [ -f "$MODE_FILE" ]; then
  AGENT_MODE="$(cat "$MODE_FILE")"
else
  AGENT_MODE="${HIVE_AGENT_MODE:-}"
fi

if [ -n "$AGENT_MODE" ]; then
  case "$AGENT_MODE" in
    NO_GITHUB|ADVISORY|ISSUES_ONLY)
      echo "⛔ git push blocked: ${AGENT} is in ${AGENT_MODE} mode" >&2
      exit 1
      ;;
  esac
else
  # Fallback: level-based enforcement
  ACMM="${HIVE_ACMM_LEVEL:-0}"
  if [ -n "$AGENT" ] && [ "$ACMM" -gt 0 ]; then
    if [ "$ACMM" -lt 3 ]; then
      echo "⛔ git push blocked: ACMM L${ACMM} agents are advisory-only" >&2
      exit 1
    fi
    if [ "$ACMM" -eq 3 ] && [ "$AGENT" != "quality" ]; then
      echo "⛔ git push blocked: only quality agent can push at ACMM L3" >&2
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
    printf '{"ts":"%s","agent":"%s","uid":%d,"op":"git-credential"}\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${AGENT:-unknown}" "$(id -u)" \
      >> "$TOKEN_ACCESS_LOG" 2>/dev/null || true
    echo "protocol=https"
    echo "host=github.com"
    echo "username=x-access-token"
    echo "password=$TOKEN"
    ;;
esac
