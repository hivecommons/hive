#!/bin/bash
# git-credential-hive.sh — Git credential helper that uses the GitHub App token.
# Reads from the cached token file, refreshing if stale (>55 min old).
#
# MODE SEMANTICS (#4289): mode blocks NO LONGER suppress read credentials.
# Git uses the same credential helper for fetch/clone AND push — the
# credential protocol ("get") cannot distinguish them — so exiting 1 in
# ADVISORY/ISSUES_ONLY/NO_GITHUB modes blocked all git reads, leaving
# advisory agents with no repo read path at all. This helper now always
# answers `get` with the per-agent SCOPED token. Write-prevention is
# enforced by token scope server-side: sub-L3 tiers ("advisor",
# "newcomer") are minted WITHOUT contents:write (src/pkg/github/app.go),
# so a push authenticates but is rejected by GitHub with 403. The helper
# still refuses loudly when the per-agent token is absent — it NEVER
# falls back to the shared full-privilege App token (audit H3), which is
# the one path that would hand an advisory agent a write-capable
# credential.
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

# ── Mode awareness: NOTE, do not block (#4289) ──
# Git invokes this helper for fetch/clone as well as push, and the credential
# `get` operation cannot tell them apart. Exiting 1 here (as this script did
# before) therefore blocked ALL git network operations for sub-push-capable
# agents — including the reads that are an advisory agent's entire job.
# Push-prevention does not need this block: the per-agent token at these
# tiers is minted without contents:write, so GitHub itself rejects any push
# with 403 (server-side enforcement that the agent cannot bypass). We emit a
# one-line notice so an agent whose push just failed sees WHY and does not
# loop retrying.

# Read mode from file first (hot-reloadable), fallback to env var.
# -r as well as -f: this script runs under `set -e`, so a mode file that exists
# but is unreadable by the agent UID (owner-only perms, #3679/#3881) would kill
# the helper mid-script and block every push with no credential and no error.
# Fall back to the env var instead — the same mode value the Manager exported.
MODE_FILE="/tmp/.hive-mode-${AGENT}"
if [ -f "$MODE_FILE" ] && [ -r "$MODE_FILE" ]; then
  AGENT_MODE="$(cat "$MODE_FILE" 2>/dev/null || true)"
else
  AGENT_MODE="${HIVE_AGENT_MODE:-}"
fi

if [ -n "$AGENT_MODE" ]; then
  case "$AGENT_MODE" in
    NO_GITHUB|ADVISORY|ISSUES_ONLY)
      echo "ℹ️  hive credential: ${AGENT} is in ${AGENT_MODE} mode — this credential is READ-ONLY for repo contents. git fetch/clone work; git push will be rejected by GitHub (403). Do NOT retry a failed push: record your intended change as an advisory finding (bd create / [FINDING]) instead." >&2
      ;;
  esac
else
  # Fallback: level-based awareness (same non-blocking semantics as above)
  ACMM="${HIVE_ACMM_LEVEL:-0}"
  if [ -n "$AGENT" ] && [ "$ACMM" -gt 0 ]; then
    if [ "$ACMM" -lt 3 ]; then
      echo "ℹ️  hive credential: ACMM L${ACMM} agents are advisory-only — this credential is READ-ONLY for repo contents. git fetch/clone work; git push will be rejected by GitHub (403). Do NOT retry a failed push: record your intended change as an advisory finding (bd create / [FINDING]) instead." >&2
    elif [ "$ACMM" -eq 3 ] && [ "$AGENT" != "quality" ]; then
      echo "ℹ️  hive credential: only the quality agent can push at ACMM L3 — this credential is READ-ONLY for repo contents. git fetch/clone work; git push will be rejected by GitHub (403). Do NOT retry a failed push: record your intended change as an advisory finding (bd create / [FINDING]) instead." >&2
    fi
  fi
fi

# Per-agent scoped token ONLY (Phase 4 — least-privilege).
#
# SECURITY (audit H3, CWE-522/732): this helper deliberately has NO fallback to
# the shared full-privilege installation-token cache
# (/var/run/hive-metrics/gh-app-token.cache). That cache holds the FULL
# installation token; handing it to an agent's git push would silently escalate
# the agent to full privilege and defeat per-agent tier scoping. When the
# per-agent scoped token is absent we FAIL LOUD (exit 1 → git falls through to
# its normal "no credential" error) so the operator repairs token delivery,
# rather than quietly escalating. The scoped cache is minted and kept fresh by
# the hive, so this helper only reads it — it never refreshes the shared cache.
AGENT_TOKEN_FILE="${HIVE_AGENT_TOKEN_CACHE:-}"
if [ -z "$AGENT_TOKEN_FILE" ] || [ ! -f "$AGENT_TOKEN_FILE" ]; then
  echo "⛔ git push blocked: per-agent scoped GitHub token not available (${AGENT_TOKEN_FILE:-HIVE_AGENT_TOKEN_CACHE unset})." >&2
  echo "⛔ Refusing to fall back to the shared full-privilege App token — that would defeat per-agent tier scoping (audit H3). Report this to the operator so token delivery is repaired." >&2
  exit 1
fi
TOKEN_FILE="$AGENT_TOKEN_FILE"

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
    # The group wraps the append so a failed REDIRECTION is silenced too, the
    # same shape gh-wrapper.sh uses (#4043): `>> f 2>/dev/null` only mutes the
    # printf, and when the log cannot be opened by the agent UID — its
    # directory is 0755 dev:node by design (#4044) and the file does not
    # exist until the entrypoint pre-creates it — the shell's own
    # "line N: .../token-access.jsonl: Permission denied" leaked into the
    # agent's pane on every clone, fetch, and push.
    { printf '{"ts":"%s","agent":"%s","uid":%d,"op":"git-credential","host":"%s"}\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${AGENT:-unknown}" "$(id -u)" "${REQUESTED_HOST:-unknown}" \
      >> "$TOKEN_ACCESS_LOG"; } 2>/dev/null || true
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
