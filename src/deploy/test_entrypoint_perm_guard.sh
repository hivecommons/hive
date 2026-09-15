#!/usr/bin/env bash
# The entrypoint's permission guards must survive their own repairs
# (kubestellar/hive#5730).
#
# WHAT BROKE. entrypoint.sh runs under `set -e`, and a `( ... ) &` subshell
# inherits it. `while inotifywait ...; do` is exempt from `set -e` in its
# CONDITION only — the body is not. So one non-zero command in a guard body
# ended that guard permanently, for the life of the container, and `-qq` plus
# `2>/dev/null` left no trace.
#
# Measured on a live standalone hive nine hours after boot (2026-09-02): the
# .copilot, .codex and .gemini inotify guards were still running as root, the
# .claude guard was gone, and the polling guard was gone. The two dead loops
# were exactly the two that walked /data/home/.claude — 161 MB, 8413 entries —
# with `chmod -R`, over a tree Claude Code writes into constantly. Five of six
# claude agents dropped to a login prompt within 30 minutes of a token refresh
# rewriting the shared credential 0600, while the credential itself held a live
# access token and a valid refresh grant.
#
# These tests EXECUTE the shipped guard functions rather than grepping for
# them: the functions between the `hive perm guard functions` markers in
# entrypoint.sh are extracted verbatim, repointed at a temp tree, and driven
# directly. A grep-only test could not have caught this bug, because the old
# code looked perfectly reasonable.
#
# Run: bash src/deploy/test_entrypoint_perm_guard.sh
# Exit codes: 0 all guards hold, 1 at least one regression.
set -uo pipefail

PASS=0
FAIL=0

ok() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL: $1"; [ $# -lt 2 ] || echo "        $2"; FAIL=$((FAIL + 1)); }

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRYPOINT="$HERE/entrypoint.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "=== Entrypoint permission-guard regression tests (#5730) ==="

# ── Extract the shipped guard functions ─────────────────────────────────────
# The markers exist so this suite tests the real code. If they are gone, the
# suite must fail loudly rather than silently test nothing — the exact failure
# mode #5730 is about.
GUARDS="$WORK/guards.sh"
awk '/^  # >>> hive perm guard functions$/{f=1;next} /^  # <<< hive perm guard functions$/{f=0} f' \
  "$ENTRYPOINT" > "$GUARDS"

if [ ! -s "$GUARDS" ]; then
  bad "guard-function markers present in entrypoint.sh" \
      "expected '# >>> hive perm guard functions' ... '# <<< hive perm guard functions'"
  echo ""
  echo "Results: $PASS passed, $FAIL failed"
  exit 1
fi
ok "guard functions extracted from entrypoint.sh ($(grep -c '^  hive_.*() {' "$GUARDS") functions)"

for fn in hive_fix_shared_credential hive_fix_copilot_config hive_fix_tree \
          hive_fix_claude_instant hive_fix_credentials_fast hive_guard_forever \
          hive_watch_once; do
  if grep -q "^  ${fn}() {" "$GUARDS" || grep -q "^  ${fn}() " "$GUARDS"; then
    ok "guard function $fn is defined"
  else
    bad "guard function $fn is defined" "not found in the extracted block"
  fi
done

# Repoint at a temp tree. `chown dev:node` is left in place on purpose: it FAILS
# here (no dev user, not root), which is precisely the kind of non-zero command
# that used to kill a guard, so leaving it exercises the fix.
HOMEDIR="$WORK/home"
sed "s#/data/home#${HOMEDIR}#g" "$GUARDS" > "$WORK/guards.local.sh"

mkdir -p "$HOMEDIR/.claude" "$HOMEDIR/.copilot" "$HOMEDIR/.codex" \
         "$HOMEDIR/.gemini/antigravity-cli" "$HOMEDIR/.cache" "$HOMEDIR/.bob"

# ── 1. The mechanism: chmod -R over a churning tree really does fail ─────────
mkdir -p "$WORK/churn/projects"
for i in $(seq 1 400); do echo x > "$WORK/churn/projects/f$i.jsonl"; done
( for i in $(seq 1 400); do rm -f "$WORK/churn/projects/f$i.jsonl"; done ) &
CHURN=$!
chmod -R g+rwX "$WORK/churn" 2>/dev/null
CHMOD_RC=$?
wait "$CHURN" 2>/dev/null
if [ "$CHMOD_RC" -ne 0 ]; then
  ok "chmod -R over a tree being written to returns non-zero (rc=$CHMOD_RC) — the killer under set -e"
else
  # Not a failure of the fix: the race just did not land this run. The guards
  # must survive it either way, which tests 2 and 3 prove directly.
  ok "chmod -R over a churning tree returned 0 this run (race did not land; guards still asserted below)"
fi

# ── 2. The old idiom dies; that is the regression ───────────────────────────
OLD_OUT="$(
  sh -c '
    set -e
    ( while [ "${N:-0}" -lt 3 ]; do
        N=$((${N:-0} + 1))
        chmod 660 /nonexistent/config.json 2>/dev/null
        echo "iteration $N"
      done ) &
    wait
  ' 2>/dev/null
)"
if [ -z "$OLD_OUT" ]; then
  ok "the pre-fix idiom (bare command in a set -e guard body) dies on its first failure"
else
  bad "the pre-fix idiom dies on its first failure" "unexpectedly produced: $OLD_OUT"
fi

# ── 3. The shipped repair functions cannot fail, under set -e ────────────────
# The files must EXIST for this to be a real test: every repair short-circuits
# on an absent path, so without these the failing chown below is never reached
# and the case that killed the guards is never exercised.
printf '{"claudeAiOauth":{}}' > "$HOMEDIR/.claude/.credentials.json"
chmod 600 "$HOMEDIR/.claude/.credentials.json"
printf '{}' > "$HOMEDIR/.copilot/config.json"
printf 'token' > "$HOMEDIR/.gemini/antigravity-cli/antigravity-oauth-token"
chmod 600 "$HOMEDIR/.gemini/antigravity-cli/antigravity-oauth-token"

GUARD_OUT="$WORK/guard-run.log"
sh -c '
  set -e
  . "$1"
  # Every one of these runs a chown that CANNOT succeed here.
  hive_fix_shared_credential "$2/.claude/.credentials.json"
  hive_fix_copilot_config
  hive_fix_tree "$2/.claude"
  hive_fix_claude_instant
  hive_fix_credentials_fast
  hive_fix_slow_cycle
  hive_fix_shared_credential "$2/does/not/exist.json"
  hive_fix_tree "$2/does/not/exist"
  echo GUARDS_SURVIVED
' sh "$WORK/guards.local.sh" "$HOMEDIR" >"$GUARD_OUT" 2>&1
RC=$?
if [ "$RC" -eq 0 ] && grep -q GUARDS_SURVIVED "$GUARD_OUT"; then
  ok "every repair function returns 0 under set -e, including on absent paths and a failing chown"
else
  bad "repair functions survive set -e" "rc=$RC, output: $(tr '\n' ' ' < "$GUARD_OUT")"
fi

# ── 4. The repair actually reopens the credential ───────────────────────────
CRED="$HOMEDIR/.claude/.credentials.json"
printf '{"claudeAiOauth":{}}' > "$CRED"
chmod 600 "$CRED"   # re-tighten: test 3 has already run a repair over it
sh -c '
  set -e
  . "$1"
  hive_fix_shared_credential "$2"
' sh "$WORK/guards.local.sh" "$CRED" >/dev/null 2>&1
MODE="$(stat -c '%a' "$CRED" 2>/dev/null || stat -f '%Lp' "$CRED")"
case "$MODE" in
  *4*|*5*|*6*|*7*)
    # Group digit is the middle one; check it explicitly rather than by glob.
    GROUP_DIGIT="${MODE:1:1}"
    if [ $(( GROUP_DIGIT & 4 )) -ne 0 ]; then
      ok "a 0600 credential is reopened to group-read (now 0$MODE)"
    else
      bad "a 0600 credential is reopened to group-read" "mode is 0$MODE, group cannot read"
    fi
    ;;
  *) bad "a 0600 credential is reopened to group-read" "mode is 0$MODE" ;;
esac

# Group WRITE must not be granted: the CLIs replace this file by rename in a
# group-writable directory, and it is an OAuth token.
GROUP_DIGIT="${MODE:1:1}"
if [ $(( GROUP_DIGIT & 2 )) -eq 0 ]; then
  ok "the credential repair grants group read only, never group write"
else
  bad "the credential repair grants group read only" "mode 0$MODE has group write"
fi

# Idempotent: a second pass must not change an already-correct file.
BEFORE="$(stat -c '%a' "$CRED")"
sh -c 'set -e; . "$1"; hive_fix_shared_credential "$2"' sh "$WORK/guards.local.sh" "$CRED" >/dev/null 2>&1
if [ "$(stat -c '%a' "$CRED")" = "$BEFORE" ]; then
  ok "repairing an already-group-readable credential is a no-op"
else
  bad "repairing an already-correct credential is a no-op" "mode changed from 0$BEFORE"
fi

# ── 5. The instant .claude path must not walk the tree ──────────────────────
# The walk is what killed the guard. Assert the instant path names the
# credential and does not recurse.
CLAUDE_BODY="$(awk '/^  hive_fix_claude_instant\(\) \{/{f=1} f{print} /^  \}$/{if(f)exit}' "$GUARDS")"
if printf '%s' "$CLAUDE_BODY" | grep -q '\.credentials\.json'; then
  ok "the instant .claude repair targets the credential file"
else
  bad "the instant .claude repair targets the credential file" "$CLAUDE_BODY"
fi
if printf '%s' "$CLAUDE_BODY" | grep -qE 'chmod -R|chown -R|find '; then
  bad "the instant .claude repair does not walk the tree" \
      "a recursive walk on every write event is what killed this guard: $CLAUDE_BODY"
else
  ok "the instant .claude repair does not walk the tree on every write event"
fi

# ── 6. A failed watcher restarts, loudly, instead of ending the guard ───────
STUB="$WORK/stub"
mkdir -p "$STUB"
cat > "$STUB/inotifywait" <<'STUBEOF'
#!/bin/sh
exit 1
STUBEOF
chmod +x "$STUB/inotifywait"

WATCH_LOG="$WORK/watch.log"
MARKER="$WORK/body-ran"
cat > "$WORK/drive.sh" <<DRIVEEOF
set -e
. "$WORK/guards.local.sh"
body_fn() { echo tick >> "$MARKER"; return 0; }
PATH="$STUB:\$PATH"
hive_guard_forever probe "$HOMEDIR/.claude/" close_write body_fn
DRIVEEOF

sh "$WORK/drive.sh" >"$WATCH_LOG" 2>&1 &
DRIVER=$!
sleep 3
kill "$DRIVER" 2>/dev/null
wait "$DRIVER" 2>/dev/null

WARNS="$(grep -c "perm guard 'probe' watcher exited" "$WATCH_LOG" 2>/dev/null || echo 0)"
if [ "$WARNS" -ge 2 ]; then
  ok "a failing inotifywait is logged and RETRIED ($WARNS warnings in 3s), not treated as the end of the guard"
else
  bad "a failing inotifywait is logged and retried" \
      "saw $WARNS warning(s); log: $(tr '\n' ' ' < "$WATCH_LOG")"
fi
if [ -s "$MARKER" ]; then
  ok "the guard still repairs while its watcher is unavailable ($(wc -l < "$MARKER" | tr -d ' ') passes)"
else
  bad "the guard still repairs while its watcher is unavailable" "body never ran"
fi

# ── 6b. The watch depth reaches the credential (#5734) ──────────────────────
# agy keeps its OAuth token one directory BELOW the watched dir, and
# `inotifywait` without -r reports events only for entries directly inside the
# watched directory — so the .gemini guard could never fire, for the entire life
# of every container. It read as protection while doing nothing.
#
# Asserted by running hive_watch_once against a stub that records its argv,
# because the whole failure was a flag that was not there.
ARGV_LOG="$WORK/inotify-argv.log"
cat > "$STUB/inotifywait" <<STUBEOF
#!/bin/sh
echo "\$@" >> "$ARGV_LOG"
exit 0
STUBEOF
chmod +x "$STUB/inotifywait"

: > "$ARGV_LOG"
sh -c '
  set -e
  . "$1"
  PATH="$2:$PATH"
  hive_watch_once "$3" close_write,create -r
  hive_watch_once "$3" close_write,create ""
' sh "$WORK/guards.local.sh" "$STUB" "$HOMEDIR/.gemini/" >/dev/null 2>&1

RECURSIVE_CALL="$(sed -n 1p "$ARGV_LOG")"
FLAT_CALL="$(sed -n 2p "$ARGV_LOG")"

case "$RECURSIVE_CALL" in
  *" -r "*) ok "hive_watch_once passes -r through to inotifywait when asked" ;;
  *) bad "hive_watch_once passes -r through to inotifywait when asked" \
         "argv was: $RECURSIVE_CALL" ;;
esac
case "$FLAT_CALL" in
  *" -r "*) bad "a non-recursive guard stays non-recursive" \
                "argv was: $FLAT_CALL — .claude is 161 MB / 8413 entries; -r there costs a watch per subdirectory" ;;
  *) ok "a non-recursive guard stays non-recursive" ;;
esac

# The dispatch itself: .gemini must be the recursive one, .claude must not be.
GEMINI_DISPATCH="$(grep -E '^ *hive_guard_forever gemini ' "$ENTRYPOINT" || true)"
case "$GEMINI_DISPATCH" in
  *" -r "*|*" -r&"*|*" -r &"*) ok "the .gemini guard is dispatched recursively" ;;
  *) bad "the .gemini guard is dispatched recursively" \
         "agy's token is at .gemini/antigravity-cli/, one level below the watch: $GEMINI_DISPATCH" ;;
esac
CLAUDE_DISPATCH="$(grep -E '^ *hive_guard_forever claude ' "$ENTRYPOINT" || true)"
case "$CLAUDE_DISPATCH" in
  *" -r"*) bad "the .claude guard is NOT recursive" \
               "that tree is 161 MB / 8413 entries on a working hive: $CLAUDE_DISPATCH" ;;
  *) ok "the .claude guard is not recursive (its credential is at depth 1)" ;;
esac

# And the directory must exist before any watch is established: a watch cannot
# be placed on a directory that is not there, so agy creating it after boot
# would leave the guard covering nothing.
if grep -qE 'mkdir -p[^&|;]*/data/home/\.gemini/antigravity-cli' "$ENTRYPOINT"; then
  ok "the credential's directory is pre-created at boot, before the watch is set up"
else
  bad "the credential's directory is pre-created at boot" \
      "a recursive watch established before agy creates .gemini/antigravity-cli/ may never cover it"
fi

# Restore the always-failing stub for any later test.
cat > "$STUB/inotifywait" <<'STUBEOF'
#!/bin/sh
exit 1
STUBEOF
chmod +x "$STUB/inotifywait"

# ── 7. Structural: no unguarded failure-prone command in the guard block ────
UNGUARDED="$(grep -nE '^\s+(chmod|chown|chgrp|find|sleep) ' "$GUARDS" | grep -v '|| true' || true)"
if [ -z "$UNGUARDED" ]; then
  ok "no chmod/chown/chgrp/find/sleep in the guard block lacks '|| true'"
else
  bad "no chmod/chown/chgrp/find/sleep in the guard block lacks '|| true'" \
      "under set -e each of these ends the guard permanently:
$UNGUARDED"
fi

# ── 8. The polling guard's fast path stays cheap ────────────────────────────
FAST_BODY="$(awk '/^  hive_fix_credentials_fast\(\) \{/{f=1} f{print} /^  \}$/{if(f)exit}' "$GUARDS")"
if printf '%s' "$FAST_BODY" | grep -qE 'chmod -R|chown -R|find '; then
  bad "the 5s polling path does no tree walk" "$FAST_BODY"
else
  ok "the 5s polling path does no tree walk"
fi

# ── 9. The 5-minute sweep is bounded by mtime (#7105 follow-up) ─────────────
# hive_fix_tree is O(everything ever created). On a live spoke the full sweep
# of /data/home/.copilot took 75s at 970 session directories, and that spoke had
# reached 9,449 — where the same sweep runs ~12 minutes against a 5-minute
# cycle. A repair pass that cannot finish within its own period stops being a
# repair pass, and because agent UIDs differ while the CLI creates files under a
# 022 umask, peers get EACCES on each other's session files for the whole
# backlog. The 5-minute path must therefore repair only recent entries.
SLOW_BODY="$(awk '/^  hive_fix_slow_cycle\(\) \{/{f=1} f{print} /^  \}$/{if(f)exit}' "$GUARDS")"
if printf '%s' "$SLOW_BODY" | grep -qE 'hive_fix_tree "'; then
  bad "the 5-minute sweep is bounded by mtime" \
      "hive_fix_slow_cycle calls the unbounded hive_fix_tree:
$SLOW_BODY"
else
  ok "the 5-minute sweep is bounded by mtime"
fi

# The unbounded sweep must still exist as a backstop, or anything written during
# a window the poller missed stays broken until its session is pruned.
FULL_BODY="$(awk '/^  hive_fix_full_cycle\(\) \{/{f=1} f{print} /^  \}$/{if(f)exit}' "$GUARDS")"
if printf '%s' "$FULL_BODY" | grep -qE 'hive_fix_tree "'; then
  ok "the hourly backstop still runs the unbounded sweep"
else
  bad "the hourly backstop still runs the unbounded sweep" \
      "hive_fix_full_cycle must call hive_fix_tree:
$FULL_BODY"
fi

# ── 10. Functional: the bounded repair fixes new files and skips old ones ────
# Executes the shipped function, like the rest of this suite. chown to dev:node
# fails as an unprivileged test user and is expected to — it carries '|| true',
# and the mode repair is what peers actually need.
RECENT_TREE="$WORK/recent-tree"
mkdir -p "$RECENT_TREE/session-new" "$RECENT_TREE/session-old"
NEWFILE="$RECENT_TREE/session-new/events.jsonl"
OLDFILE="$RECENT_TREE/session-old/events.jsonl"
printf 'x\n' > "$NEWFILE"
printf 'x\n' > "$OLDFILE"
chmod 0644 "$NEWFILE"
chmod 0644 "$OLDFILE"
# Age the old session well beyond the window. touch -t takes [[CC]YY]MMDDhhmm.
touch -t 202001010000 "$OLDFILE" "$RECENT_TREE/session-old"

hive_fix_tree_recent_rc=0
sh -c 'set -e; . "$1"; hive_fix_tree_recent "$2" 10' sh "$WORK/guards.local.sh" "$RECENT_TREE" \
  >/dev/null 2>&1 || hive_fix_tree_recent_rc=$?

new_mode="$(stat -c '%a' "$NEWFILE" 2>/dev/null || stat -f '%Lp' "$NEWFILE")"
old_mode="$(stat -c '%a' "$OLDFILE" 2>/dev/null || stat -f '%Lp' "$OLDFILE")"

# Check the GROUP digit specifically. A glob like *6* would match 644 and pass
# while the file is still group-read-only, which is the bug being tested for.
new_group_digit="$(printf '%s' "$new_mode" | tail -c 2 | head -c 1)"
case "$new_group_digit" in
  2|3|6|7) ok "the bounded repair makes a recent file group-writable (mode $new_mode)" ;;
  *) bad "the bounded repair makes a recent file group-writable" "mode is $new_mode, wanted group write" ;;
esac

if [ "$old_mode" = "644" ]; then
  ok "the bounded repair leaves files outside the window alone (mode $old_mode)"
else
  bad "the bounded repair leaves files outside the window alone" \
      "mode changed to $old_mode; the whole point is not to walk the backlog"
fi

# Absent paths must stay non-fatal under set -e, like every other repair here.
if sh -c 'set -e; . "$1"; hive_fix_tree_recent "$2" 10' sh "$WORK/guards.local.sh" \
     "$WORK/definitely-not-there" >/dev/null 2>&1; then
  ok "the bounded repair returns 0 on an absent path"
else
  bad "the bounded repair returns 0 on an absent path" "it must not end the guard"
fi

echo ""
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1
