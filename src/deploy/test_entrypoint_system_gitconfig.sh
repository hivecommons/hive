#!/usr/bin/env bash
# Regression test for kubestellar/hive#5343 — agents cannot push because the
# git credential helper was only in /home/dev/.gitconfig.
#
# WHAT THIS ASSERTS, and why it is not a grep test:
#
# The #5343 defect was invisible to a grep test by construction. The entrypoint
# DID run `git config --global credential.https://github.com.helper ...` and it
# DID succeed — a grep for that line would have passed on the broken build. The
# defect was that `--global` is per-$HOME and the entrypoint runs with
# HOME=/home/dev, while every per-agent UID runs with a different $HOME and no
# .gitconfig of its own. There was no /etc/gitconfig, so agents resolved NO
# helper at all.
#
# So this test EXECUTES the entrypoint's system-config writer against real git,
# with $HOME pointed at an empty directory — the faithful stand-in for an agent
# UID — and asserts that git still resolves the helper. That is the invariant:
# not "the config was written", but "an agent can reach it".
#
# Run: bash src/deploy/test_entrypoint_system_gitconfig.sh
set -uo pipefail

PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "        $2"; FAIL=$((FAIL + 1)); }

ENTRYPOINT="$(cd "$(dirname "$0")" && pwd)/entrypoint.sh"
HELPER_PATH="/usr/local/bin/git-credential-hive.sh"

echo "=== Entrypoint system gitconfig (#5343) ==="

if ! command -v git >/dev/null 2>&1; then
  echo "  SKIP: git not available"
  exit 0
fi

TMP="$(mktemp -d)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

# ── Extract the two functions under test out of the entrypoint and source them
# in isolation. Running the whole entrypoint is not possible (it drops
# privileges and execs the hive binary); running the real function is the
# entire point, so we take the function bodies verbatim rather than
# reimplementing them here — a reimplementation could not catch a drift bug.
sed -n '/^hive_ghe_git_host() {/,/^}/p;/^hive_write_system_gitconfig() {/,/^}/p' \
  "$ENTRYPOINT" > "$TMP/funcs.sh"

if grep -q '^hive_write_system_gitconfig() {' "$TMP/funcs.sh" \
   && grep -q '^hive_ghe_git_host() {' "$TMP/funcs.sh"; then
  pass "entrypoint defines hive_ghe_git_host and hive_write_system_gitconfig"
else
  fail "entrypoint defines hive_ghe_git_host and hive_write_system_gitconfig" \
       "extraction from $ENTRYPOINT produced no matching function"
  echo "=== $PASS passed, $FAIL failed ==="
  exit 1
fi

# The writer targets the literal path /etc/gitconfig, which a test must not
# touch. Redirect it to a temp path by overriding the one variable it uses.
# shellcheck disable=SC1090
. "$TMP/funcs.sh"

run_writer() {
  # $1 = destination path, $2 = optional hive.yaml. HIVE_SYSTEM_GITCONFIG is
  # the writer's own test seam, so the REAL function runs — no reimplementation
  # that could drift from the code being tested.
  HIVE_SYSTEM_GITCONFIG="$1" HIVE_CONFIG="${2:-}" \
    bash -c ". '$TMP/funcs.sh'; hive_write_system_gitconfig" >/dev/null 2>&1
}

# ── Case 1: plain github.com hive (no GHE host configured) ─────────────────
SYS1="$TMP/gitconfig-plain"
run_writer "$SYS1"

if [ -f "$SYS1" ]; then
  pass "writer produced a system gitconfig"
else
  fail "writer produced a system gitconfig" "no file at $SYS1"
  echo "=== $PASS passed, $FAIL failed ==="
  exit 1
fi

# THE INVARIANT: an empty $HOME (an agent UID) still resolves the helper.
AGENT_HOME="$TMP/agent-home"
mkdir -p "$AGENT_HOME"

resolved="$(HOME="$AGENT_HOME" XDG_CONFIG_HOME="$AGENT_HOME" GIT_CONFIG_SYSTEM="$SYS1" \
  git config --get-urlmatch credential.helper https://github.com/o/r 2>/dev/null)"
if [ "$resolved" = "$HELPER_PATH" ]; then
  pass "agent UID with an empty \$HOME resolves the helper for github.com"
else
  fail "agent UID with an empty \$HOME resolves the helper for github.com" \
       "got '${resolved:-<nothing>}', want '$HELPER_PATH'"
fi

# The file must be parseable by git at all — a malformed system config makes
# EVERY git invocation in the container fail, which would be far worse than
# the bug being fixed.
if HOME="$AGENT_HOME" GIT_CONFIG_SYSTEM="$SYS1" git config --list >/dev/null 2>&1; then
  pass "generated system gitconfig parses cleanly"
else
  fail "generated system gitconfig parses cleanly" "git config --list rejected $SYS1"
fi

# Identity must be present system-wide too: an agent whose $HOME has no
# .gitconfig cannot commit without user.name/user.email either.
name="$(HOME="$AGENT_HOME" XDG_CONFIG_HOME="$AGENT_HOME" GIT_CONFIG_SYSTEM="$SYS1" \
  git config --get user.name 2>/dev/null)"
if [ -n "$name" ]; then
  pass "git identity is set system-wide (user.name='$name')"
else
  fail "git identity is set system-wide" "user.name unset with an empty \$HOME"
fi

# NO SECRET. The file names a helper; the helper mints the token. A token
# committed here would be readable by every UID in the container.
if grep -qiE 'ghs_|ghp_|github_pat_|BEGIN [A-Z ]*PRIVATE KEY|password[[:space:]]*=' "$SYS1"; then
  fail "system gitconfig contains no credential material" \
       "a token-shaped or password entry appears in the generated file"
else
  pass "system gitconfig contains no credential material"
fi

# ── Case 2: GHE hive — the helper must be wired for the enterprise host too.
if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' 2>/dev/null; then
  cat > "$TMP/hive.yaml" <<'YAML'
github:
  base_url: https://github.example-enterprise.com
YAML
  SYS2="$TMP/gitconfig-ghe"
  run_writer "$SYS2" "$TMP/hive.yaml"

  ghe_resolved="$(HOME="$AGENT_HOME" XDG_CONFIG_HOME="$AGENT_HOME" GIT_CONFIG_SYSTEM="$SYS2" \
    git config --get-urlmatch credential.helper \
    https://github.example-enterprise.com/o/r 2>/dev/null)"
  if [ "$ghe_resolved" = "$HELPER_PATH" ]; then
    pass "agent UID resolves the helper for the configured GHE host"
  else
    fail "agent UID resolves the helper for the configured GHE host" \
         "got '${ghe_resolved:-<nothing>}', want '$HELPER_PATH'"
  fi

  # github.com must keep working on a GHE hive (the App may still reach it).
  gh_resolved="$(HOME="$AGENT_HOME" XDG_CONFIG_HOME="$AGENT_HOME" GIT_CONFIG_SYSTEM="$SYS2" \
    git config --get-urlmatch credential.helper https://github.com/o/r 2>/dev/null)"
  if [ "$gh_resolved" = "$HELPER_PATH" ]; then
    pass "GHE hive still wires github.com as well"
  else
    fail "GHE hive still wires github.com as well" \
         "got '${gh_resolved:-<nothing>}', want '$HELPER_PATH'"
  fi
else
  echo "  SKIP: python3+pyyaml unavailable — GHE host derivation not exercised"
fi

# ── Case 3: the writer must be invoked from the ROOT phase. /etc is root-owned;
# calling it after the drop to dev would silently fail to write and leave every
# agent exactly as broken as before. Assert the call site precedes the exec.
root_call_line="$(grep -n '^  hive_write_system_gitconfig' "$ENTRYPOINT" | head -1 | cut -d: -f1)"
drop_line="$(grep -n 'exec gosu dev' "$ENTRYPOINT" | head -1 | cut -d: -f1)"
if [ -n "$root_call_line" ] && [ -n "$drop_line" ] && [ "$root_call_line" -lt "$drop_line" ]; then
  pass "hive_write_system_gitconfig runs in the root phase, before the drop to dev"
else
  fail "hive_write_system_gitconfig runs in the root phase, before the drop to dev" \
       "call at line ${root_call_line:-<none>}, privilege drop at line ${drop_line:-<none>}"
fi

# ── Case 4: the dev-phase global config must NOT have been removed. The
# contributor-relay / local-mode paths and interactive shells read it, and a
# non-root boot never reaches the system writer at all.
if grep -q 'git config --global --replace-all "credential.https://github.com.helper"' "$ENTRYPOINT"; then
  pass "dev-user global credential wiring is retained (contributor-relay / non-root boot)"
else
  fail "dev-user global credential wiring is retained (contributor-relay / non-root boot)" \
       "the --global helper line is gone; local-mode and non-root boots lose their credential"
fi

echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
