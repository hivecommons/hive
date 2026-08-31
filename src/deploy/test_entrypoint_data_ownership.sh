#!/usr/bin/env bash
# #5369: /data ownership must be an INVARIANT, not a boot-time snapshot.
#
# The fault this closes: entrypoint.sh's `chown -R dev:node /data` is guarded on
# `[ "$DATA_OWNER" != "1001" ]`, and src/Dockerfile already ships /data owned by
# dev:node — so on a normal boot the guard is FALSE and the recursive chown never
# runs. Everything the root phase creates under /data afterwards keeps root:root,
# and the hive process (uid 1001, after the setpriv/gosu drop) cannot read it.
#
# #5360 was one instance: /data/hive.yaml.runtime, chmod 600 with no chown, in
# the root phase, on a /data that was already uid 1001. #5368 fixed that one
# file. This tests the CLASS.
#
# The standard here is #5368's, and it is deliberate: assert READABILITY BY THE
# READING USER, not permission bits. #5342 asserted only the mode and passed
# while the product was broken — mode 0600 is perfectly correct and perfectly
# unreadable when the owner is not the reader.
#
# Run: bash src/deploy/test_entrypoint_data_ownership.sh
set -uo pipefail

PASS=0
FAIL=0

ENTRYPOINT="$(cd "$(dirname "$0")" && pwd)/entrypoint.sh"
RUNTIME_UID=1001

check() {
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label"
    echo "        want: '$want'"
    echo "        got:  '$got'"
    FAIL=$((FAIL + 1))
  fi
}

ok()   { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad()  { echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "        $2"; FAIL=$((FAIL + 1)); }

echo "=== #5369: /data ownership invariant ==="

# ── Structural: the guard must SURVIVE ───────────────────────────────────
#
# The fix must not be "delete the guard". A recursive chown over an NFS-backed
# PVC with thousands of files costs minutes of startup; removing the guard
# trades a permissions bug for a boot-time one. Assert the protection is still
# there, so a future "simplification" that removes it fails here.
if grep -q 'DATA_OWNER=\$(stat -c' "$ENTRYPOINT" \
   && grep -q 'if \[ "\$DATA_OWNER" != "1001" \]; then' "$ENTRYPOINT"; then
  ok "the DATA_OWNER guard still gates the recursive chown (NFS protection intact)"
else
  bad "the DATA_OWNER guard is gone" \
      "removing it reintroduces multi-minute NFS startup delays; the fix for #5369 is a targeted sweep, not a walk"
fi

# The sweep that replaces it must NOT itself be a recursive walk, or it has
# reintroduced exactly the cost the guard exists to avoid.
SWEEP="$(sed -n '/^hive_sweep_root_phase_paths() {/,/^}/p' "$ENTRYPOINT")"
if [ -z "$SWEEP" ]; then
  bad "could not extract hive_sweep_root_phase_paths from $ENTRYPOINT"
elif grep -qE 'chown[[:space:]]+-R|chown[[:space:]]+-[a-zA-Z]*R' <<<"$SWEEP"; then
  bad "the ownership sweep recurses" \
      "a recursive chown here costs the same multi-minute NFS walk the DATA_OWNER guard prevents"
else
  ok "the ownership sweep is non-recursive (cost does not scale with PVC size)"
fi

# ── Structural: the path list must be wired to BOTH consumers ────────────
for fn in hive_sweep_root_phase_paths hive_assert_runtime_readable; do
  if grep -q "^${fn}() {" "$ENTRYPOINT"; then
    ok "$fn is defined"
  else
    bad "$fn is not defined in $ENTRYPOINT"
  fi
done

if grep -q '^HIVE_DATA_ROOT_PHASE_PATHS="' "$ENTRYPOINT"; then
  ok "HIVE_DATA_ROOT_PHASE_PATHS is defined"
else
  bad "HIVE_DATA_ROOT_PHASE_PATHS is not defined"
fi

# The sweep must actually be CALLED in the root phase. A helper that is defined
# and never invoked is the #5369 shape all over again — code that looks like a
# fix and runs on no boot at all.
sweep_calls="$(grep -cE '^[[:space:]]*hive_sweep_root_phase_paths[[:space:]]*$' "$ENTRYPOINT" || true)"
check "the sweep is invoked exactly once" "1" "$sweep_calls"

# ── Structural: the assertion must run BEFORE the privilege drop ─────────
#
# After the exec we are uid 1001 and can no longer chown or meaningfully
# diagnose. An assertion placed after the drop would never run at all.
assert_line="$(grep -n 'hive_assert_runtime_readable \$HIVE_DATA_ROOT_PHASE_PATHS' "$ENTRYPOINT" | head -1 | cut -d: -f1)"
drop_line="$(grep -nE '^[[:space:]]*exec (setpriv|gosu) ' "$ENTRYPOINT" | head -1 | cut -d: -f1)"
if [ -z "$assert_line" ]; then
  bad "hive_assert_runtime_readable is never called on the path list"
elif [ -z "$drop_line" ]; then
  bad "could not locate the privilege drop (exec setpriv/gosu)"
elif [ "$assert_line" -lt "$drop_line" ]; then
  ok "the readability assertion runs before the privilege drop (line $assert_line < $drop_line)"
else
  bad "the readability assertion runs AFTER the privilege drop" \
      "at that point the process is already uid 1001 — the check cannot fire or fix anything"
fi

# The assertion must cover the config paths, which are the ones whose failure
# is fatal. #5360 was exactly /data/hive.yaml.runtime.
if grep -q 'hive_assert_runtime_readable \$HIVE_DATA_ROOT_PHASE_PATHS' "$ENTRYPOINT" \
   && sed -n "${assert_line},$((assert_line + 2))p" "$ENTRYPOINT" | grep -q 'HIVE_CONFIG_RUNTIME'; then
  ok "the assertion covers the runtime config paths (the #5360 file)"
else
  bad "the assertion does not cover HIVE_CONFIG_RUNTIME" \
      "that is the file whose unreadability was #5360; it is the one that must be named"
fi

# ── Structural: every root-phase /data creator is in the list ────────────
#
# This is the maintenance rule enforced mechanically. Extract the root phase
# (from `if [ "$(id -u)" = "0" ]` to the privilege drop) and find every mkdir
# that creates a path under /data. Each such path must either appear in
# HIVE_DATA_ROOT_PHASE_PATHS or be explicitly chowned at its own site.
#
# Without this, the list silently goes stale the first time someone adds a
# write — which is precisely the failure mode the issue describes.
echo
echo "=== every root-phase /data creator is covered ==="

root_start="$(grep -n 'if \[ "\$(id -u)" = "0" \]; then' "$ENTRYPOINT" | head -1 | cut -d: -f1)"
if [ -z "$root_start" ] || [ -z "$drop_line" ]; then
  bad "could not delimit the root phase"
else
  ROOT_PHASE="$(sed -n "${root_start},${drop_line}p" "$ENTRYPOINT")"
  LIST="$(sed -n '/^HIVE_DATA_ROOT_PHASE_PATHS="/,/^"$/p' "$ENTRYPOINT")"

  # Paths intentionally excluded, with the reason recorded in the entrypoint:
  #   /data/agents/*, /data/beads/*  -> chowned to per-agent hive-<name> UIDs
  #   /data/vaults                   -> created in the DEV phase, already dev-owned
  uncovered=""
  # shellcheck disable=SC2016
  mkdir_paths="$(printf '%s' "$ROOT_PHASE" \
    | grep -oE 'mkdir -p [^&|;]*' \
    | tr ' ' '\n' \
    | grep -E '^/data/[A-Za-z0-9._/-]+$' \
    | grep -vE '^/data/(agents|beads|vaults)(/|$)' \
    | sort -u)"

  for p in $mkdir_paths; do
    # Covered if it is in the list...
    if grep -qxF "$p" <<<"$LIST"; then
      continue
    fi
    # ...or if the site chowns that exact path...
    if grep -qE "chown( -R)? dev:node [^&|;]*${p}( |$|/)" <<<"$ROOT_PHASE"; then
      continue
    fi
    # ...or if an ANCESTOR is chowned RECURSIVELY, which does cover it. E.g.
    # `mkdir -p /data/home/.claude/session-env` is covered by
    # `chown -R dev:node /data/home/.claude`. Only -R counts here: a
    # non-recursive chown of the parent does NOT reach the child.
    covered_by_ancestor=""
    anc="$p"
    while [ "$anc" != "/data" ] && [ "$anc" != "/" ]; do
      anc="$(dirname "$anc")"
      [ "$anc" = "/data" ] && break
      if grep -qE "chown -R dev:node [^&|;]*${anc}( |$)" <<<"$ROOT_PHASE"; then
        covered_by_ancestor=yes
        break
      fi
    done
    [ -n "$covered_by_ancestor" ] && continue
    uncovered="$uncovered $p"
  done

  if [ -n "$uncovered" ]; then
    bad "root-phase mkdir under /data not covered by the sweep list or an inline chown:$uncovered" \
        "add each to HIVE_DATA_ROOT_PHASE_PATHS, or chown it at its creation site (#5369)"
  else
    ok "every root-phase mkdir under /data is swept or chowned at its site"
  fi

  # The two files the root phase WRITES (not mkdirs) under /data. These were
  # the concrete gaps found for #5369: created by `cat >` / `printf >` as root,
  # chmod 644 applied, no chown — so root:root on every boot.
  for f in /data/home/.bashrc /data/home/.profile; do
    if grep -qE "chown dev:node ${f}( |$)" <<<"$ROOT_PHASE"; then
      ok "$f is chowned at its creation site"
    elif grep -qxF "$f" <<<"$LIST"; then
      ok "$f is covered by the sweep list"
    else
      bad "$f is written by root and never handed to dev" \
          "agent shells source it; a root-owned copy is the #5369 class"
    fi
  done
fi

# ── Behavioural: the part that mode-checking could never catch ───────────
#
# Everything above is structure. This is the product property: a root-created
# path, run through the sweep, must be genuinely OPENABLE by uid 1001 — proven
# by really opening it as that uid, which is the syscall the hive binary makes.
echo
echo "=== behavioural: swept paths are readable by the runtime user ==="

if [ "$(id -u)" != "0" ]; then
  echo "  SKIP: not root — cannot create root-owned files or drop to another uid"
  echo "        (this is the case a container lane must run; see #5360/#5369)"
elif ! id -u dev >/dev/null 2>&1; then
  echo "  SKIP: no 'dev' account on this host — cannot exercise the drop"
elif ! stat -c '%u' / >/dev/null 2>&1; then
  echo "  SKIP: no GNU stat -c on this host — the helpers require it"
else
  SWEEP_FN="$SWEEP"
  ASSERT_FN="$(sed -n '/^hive_assert_runtime_readable() {/,/^}/p' "$ENTRYPOINT")"

  tmpd="$(mktemp -d)"
  trap 'rm -rf "$tmpd"' EXIT
  chmod 755 "$tmpd"

  # Reproduce the failing shape: root creates a directory and a file under it
  # exactly as the root phase does, and never chowns them.
  mkdir -p "$tmpd/home"
  printf 'export SSL_CERT_FILE=/data/proxy-ca.pem\n' > "$tmpd/home/.bashrc"
  chown -R root:root "$tmpd/home"
  chmod 700 "$tmpd/home"          # root-only dir: dev cannot even traverse it
  chmod 600 "$tmpd/home/.bashrc"

  HIVE_RUNTIME_USER="dev"
  HIVE_RUNTIME_GROUP="node"
  export HIVE_RUNTIME_USER HIVE_RUNTIME_GROUP

  # shellcheck disable=SC1090
  eval "$ASSERT_FN"
  # shellcheck disable=SC1090
  eval "$SWEEP_FN"

  # 1. BEFORE the sweep, the runtime user must NOT be able to read it. If this
  #    fails the fixture is wrong and every assertion below is vacuous — the
  #    way a test can pass while proving nothing.
  if su -s /bin/sh dev -c "cat '$tmpd/home/.bashrc' >/dev/null 2>&1"; then
    bad "fixture invalid: dev could already read the root-owned file before the sweep" \
        "the rest of this block would pass vacuously"
  else
    ok "fixture: the runtime user cannot read the root-created path (the #5369 fault)"
  fi

  # 2. The assertion must NAME that path while it is still broken. This is the
  #    diagnostic #5360 lacked: a silent EACCES from the Go binary versus a
  #    line of output identifying the file.
  assert_out="$(HIVE_RUNTIME_USER=dev hive_assert_runtime_readable "$tmpd/home" "$tmpd/home/.bashrc" 2>&1 || true)"
  if grep -qF "$tmpd/home" <<<"$assert_out"; then
    ok "the assertion names the unreadable path before the privilege drop"
  else
    bad "the assertion did not name the unreadable path" \
        "got: $assert_out"
  fi

  # 3. Run the sweep over that list, then re-check. THE ASSERTION THAT MATTERS:
  #    a real open() as uid 1001, not a stat of the mode bits.
  HIVE_DATA_ROOT_PHASE_PATHS="$tmpd/home
$tmpd/home/.bashrc"
  hive_sweep_root_phase_paths >/dev/null 2>&1

  check "swept directory is owned by the runtime uid" \
    "$RUNTIME_UID" "$(stat -c '%u' "$tmpd/home" 2>/dev/null)"
  check "swept file is owned by the runtime uid" \
    "$RUNTIME_UID" "$(stat -c '%u' "$tmpd/home/.bashrc" 2>/dev/null)"

  if su -s /bin/sh dev -c "cat '$tmpd/home/.bashrc' >/dev/null 2>&1"; then
    ok "the runtime user can actually read the swept path (real open() as uid 1001)"
  else
    bad "the runtime user STILL cannot read the swept path" \
        "this is #5369 — root-phase writes stay unreadable after the privilege drop"
  fi

  # 4. The sweep must not have bought readability by WIDENING the mode. #5331
  #    exists because a file holding dashboard.auth_token was world-readable;
  #    the fix for #5369 is ownership, never permissions.
  check "the sweep did not widen the file mode" \
    "600" "$(stat -c '%a' "$tmpd/home/.bashrc" 2>/dev/null)"
  check "the sweep did not widen the directory mode" \
    "700" "$(stat -c '%a' "$tmpd/home" 2>/dev/null)"

  if id -u nobody >/dev/null 2>&1; then
    if su -s /bin/sh nobody -c "cat '$tmpd/home/.bashrc' >/dev/null 2>&1"; then
      bad "an unrelated uid can read the swept file" \
          "readability must come from ownership, not from a widened mode (#5331)"
    else
      ok "an unrelated uid still cannot read the swept file"
    fi
  fi

  # 5. After the sweep the assertion must fall SILENT. A check that warns even
  #    once things are correct is noise, and noise is what makes the real
  #    warning ignorable.
  assert_out2="$(hive_assert_runtime_readable "$tmpd/home" "$tmpd/home/.bashrc" 2>&1 || true)"
  if [ -z "$assert_out2" ]; then
    ok "the assertion is silent once ownership is correct"
  else
    bad "the assertion still warns after a successful sweep" \
        "got: $assert_out2"
  fi

  # 6. FAIL OPEN, not closed (#5368's lesson). When the chown is impossible the
  #    sweep must WARN and continue — never abort the boot, and never leave a
  #    foreign-owned file locked down. A hive that boots degraded and says so
  #    beats one that will not boot.
  HIVE_DATA_ROOT_PHASE_PATHS="/proc/1/mem-does-not-exist
$tmpd/home"
  if hive_sweep_root_phase_paths >/dev/null 2>&1; then
    ok "the sweep fails open (returns success when a path cannot be chowned)"
  else
    bad "the sweep returned non-zero" \
        "under 'set -e' in the entrypoint this aborts the boot — chown must fail open (#5368)"
  fi

  rm -rf "$tmpd"
  trap - EXIT
fi

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
