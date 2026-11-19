#!/usr/bin/env bash
# Tests the hive.yaml.bak -> hive.yaml.runtime migration in entrypoint.sh.
#
# The rename is copy-forward: readers prefer /data/hive.yaml.runtime and fall
# back to the legacy /data/hive.yaml.bak, and NOTHING removes the legacy file
# on the PVC. ~51 live hives carry only the legacy name, and on Docker/LXC
# that file is the single copy of the live config.
#
# #17862 added one deliberate write: when the NEW name is the boot source and
# parses as a config, the legacy file is refreshed from it (temp + mv) so the
# fallback is never months stale, and falling back to it logs a loud WARN.
#
# Rather than grepping the script, this extracts the real resolver function and
# executes it against real files, so it fails if the logic regresses.
#
# Run: bash src/deploy/test_entrypoint_runtime_config.sh
set -euo pipefail

# Shared skip discipline (#5388): hive_test_skip is permissive by default and
# FATAL under HIVE_TEST_REQUIRE_BEHAVIOURAL=1. Extracted from this file and its
# sibling by #5388 so every deploy suite can use the same contract.
# shellcheck source=src/deploy/test_lib.sh
. "$(cd "$(dirname "$0")" && pwd)/test_lib.sh"

ENTRYPOINT="$(cd "$(dirname "$0")" && pwd)/entrypoint.sh"

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

echo "=== entrypoint runtime-config migration tests ==="

# Extract the resolver verbatim from the entrypoint so this tests the shipped
# code, not a copy that can drift away from it.
RESOLVER="$(sed -n '/^hive_file_mtime() {/,/^}/p; /^hive_runtime_config_read() {/,/^}/p' "$ENTRYPOINT")"
if ! grep -q 'hive_runtime_config_read() {' <<<"$RESOLVER" || ! grep -q 'hive_file_mtime() {' <<<"$RESOLVER"; then
  echo "  FAIL: could not extract hive_runtime_config_read / hive_file_mtime from $ENTRYPOINT"
  exit 1
fi

# resolve <new-content> <legacy-content> — "" means the file is absent.
# Prints the path the entrypoint would read, relative-ised to new/legacy/"".
resolve() {
  local new_content="$1" legacy_content="$2"
  local dir
  dir="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '$dir'" RETURN

  HIVE_CONFIG_RUNTIME="$dir/hive.yaml.runtime"
  HIVE_CONFIG_RUNTIME_LEGACY="$dir/hive.yaml.bak"
  [ -n "$new_content" ] && printf '%s' "$new_content" > "$HIVE_CONFIG_RUNTIME"
  [ -n "$legacy_content" ] && printf '%s' "$legacy_content" > "$HIVE_CONFIG_RUNTIME_LEGACY"
  # Empty-but-present files: create with zero bytes to exercise the -s guard.
  [ "$new_content" = "EMPTY" ] && : > "$HIVE_CONFIG_RUNTIME"
  [ "$legacy_content" = "EMPTY" ] && : > "$HIVE_CONFIG_RUNTIME_LEGACY"

  export HIVE_CONFIG_RUNTIME HIVE_CONFIG_RUNTIME_LEGACY
  local out
  out="$(eval "$RESOLVER"; hive_runtime_config_read 2>/dev/null)"

  case "$out" in
    "$HIVE_CONFIG_RUNTIME") echo "new" ;;
    "$HIVE_CONFIG_RUNTIME_LEGACY") echo "legacy" ;;
    "") echo "none" ;;
    *) echo "unexpected:$out" ;;
  esac
}

# 1. Both present — the new name wins.
check "both present -> new name wins" "new" "$(resolve "new: cfg" "old: cfg")"

# 2. THE MIGRATION CASE: only the legacy file, as on all ~51 live hives at the
#    moment they first boot the new code. Must still find a config.
check "legacy only -> falls back to legacy (no boot-config loss)" \
  "legacy" "$(resolve "" "old: cfg")"

# 3. Only the new name — the steady state after the first save.
check "new only -> new name" "new" "$(resolve "new: cfg" "")"

# 4. Neither — the caller must handle 'no config', not read a bogus path.
check "neither present -> empty" "none" "$(resolve "" "")"

# 5. An empty new file must not shadow a good legacy file. Without the -s
#    guard a 0-byte hive.yaml.runtime would win and the hive would boot with
#    no config while a perfectly good legacy file sat next to it.
check "empty new + good legacy -> legacy" "legacy" "$(resolve "EMPTY" "old: cfg")"

# 6. Both empty is the same as neither.
check "empty new + empty legacy -> empty" "none" "$(resolve "EMPTY" "EMPTY")"

# 7. The legacy file must never be deleted, and the only thing allowed to
#    overwrite it is hive_refresh_legacy_fallback (#17862), which copies a
#    newer, validated runtime config over it. On Docker/LXC it can be the
#    only copy of the live config, so nothing else may touch it.
if grep -Eq 'rm[^\n]*HIVE_CONFIG_RUNTIME_LEGACY' "$ENTRYPOINT"; then
  echo "  FAIL: entrypoint removes the legacy runtime config"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: legacy runtime config is never removed"
  PASS=$((PASS + 1))
fi
REFRESH="$(sed -n '/^hive_refresh_legacy_fallback() {/,/^}/p' "$ENTRYPOINT")"
mv_total="$(grep -cE 'mv[^\n]*HIVE_CONFIG_RUNTIME_LEGACY' "$ENTRYPOINT" || true)"
mv_in_refresh="$(grep -cE 'mv[^\n]*HIVE_CONFIG_RUNTIME_LEGACY' <<<"$REFRESH" || true)"
check "legacy runtime config is only overwritten inside hive_refresh_legacy_fallback" "$mv_total" "$mv_in_refresh"

# ── #17862: the legacy fallback is refreshed on clean boots and WARNs loudly ──
#
# The incident: a kill mid-save left a 0-byte hive.yaml.runtime, the resolver
# fell back to a hive.yaml.bak frozen three months earlier, and the hive
# booted a stale ACMM level with no indication in the log. Two properties
# close it: (a) a clean boot from the new name refreshes the legacy copy so it
# is at most one boot old, and (b) a fallback is announced on stderr.

echo
echo "=== #17862: legacy fallback freshness and WARN ==="

# resolve_stderr <new-content> <legacy-content> — stderr the resolver emits.
resolve_stderr() {
  local new_content="$1" legacy_content="$2"
  local dir
  dir="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '$dir'" RETURN
  HIVE_CONFIG_RUNTIME="$dir/hive.yaml.runtime"
  HIVE_CONFIG_RUNTIME_LEGACY="$dir/hive.yaml.bak"
  [ -n "$new_content" ] && printf '%s' "$new_content" > "$HIVE_CONFIG_RUNTIME"
  [ -n "$legacy_content" ] && printf '%s' "$legacy_content" > "$HIVE_CONFIG_RUNTIME_LEGACY"
  [ "$new_content" = "EMPTY" ] && : > "$HIVE_CONFIG_RUNTIME"
  export HIVE_CONFIG_RUNTIME HIVE_CONFIG_RUNTIME_LEGACY
  (eval "$RESOLVER"; hive_runtime_config_read 2>&1 >/dev/null) || true
}

# (b) Falling back is loud; the steady state is silent.
warn_out="$(resolve_stderr "EMPTY" "old: cfg")"
if grep -q 'WARN.*FALLING BACK to legacy' <<<"$warn_out" && grep -q '0 bytes' <<<"$warn_out"; then
  echo "  PASS: empty runtime + legacy fallback logs a loud WARN naming the truncation"
  PASS=$((PASS + 1))
else
  echo "  FAIL: fallback to legacy did not WARN (got: '$warn_out')"
  FAIL=$((FAIL + 1))
fi
check "steady-state boot from the new name is silent" "" "$(resolve_stderr "new: cfg" "old: cfg")"

# (a) hive_refresh_legacy_fallback: the legacy copy is overwritten from a
#     valid runtime file, never from an invalid one, and only when the new
#     name is the boot source.
HARDEN_FN="$(sed -n '/^hive_harden_runtime_config() {/,/^}/p' "$ENTRYPOINT")"
refresh_case() {
  # refresh_case <boot-source: new|legacy> <runtime-content> <legacy-content>; prints legacy content after.
  local src="$1" rt="$2" lg="$3" dir
  dir="$(mktemp -d)"
  # shellcheck disable=SC2064
  trap "rm -rf '$dir'" RETURN
  HIVE_CONFIG_RUNTIME="$dir/hive.yaml.runtime"
  HIVE_CONFIG_RUNTIME_LEGACY="$dir/hive.yaml.bak"
  HIVE_RUNTIME_USER="$(id -un)"; HIVE_RUNTIME_GROUP="$(id -gn)"
  export HIVE_CONFIG_RUNTIME HIVE_CONFIG_RUNTIME_LEGACY HIVE_RUNTIME_USER HIVE_RUNTIME_GROUP
  printf '%s' "$rt" > "$HIVE_CONFIG_RUNTIME"
  printf '%s' "$lg" > "$HIVE_CONFIG_RUNTIME_LEGACY"
  local arg="$HIVE_CONFIG_RUNTIME"
  [ "$src" = legacy ] && arg="$HIVE_CONFIG_RUNTIME_LEGACY"
  (eval "$HARDEN_FN"; eval "$REFRESH"; hive_refresh_legacy_fallback "$arg" >/dev/null 2>&1) || true
  cat "$HIVE_CONFIG_RUNTIME_LEGACY"
}
# Contents are single-line here so $(...) newline stripping cannot bite.
GOOD_RT='{project: {org: acme}, acmm_level: 6}'
STALE_LG='{acmm_level: 3}'
check "clean boot from new name refreshes the legacy fallback" \
  "$GOOD_RT" "$(refresh_case new "$GOOD_RT" "$STALE_LG")"
check "boot from the legacy name leaves it untouched" \
  "$STALE_LG" "$(refresh_case legacy "$GOOD_RT" "$STALE_LG")"
if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' 2>/dev/null; then
  check "unparsable runtime config never overwrites the fallback" \
    "$STALE_LG" "$(refresh_case new 'project: [unterminated' "$STALE_LG")"
  check "runtime config without a project section never overwrites the fallback" \
    "$STALE_LG" "$(refresh_case new '{acmm_level: 0}' "$STALE_LG")"
else
  hive_test_skip "python3+yaml unavailable — cannot exercise the parse guard in hive_refresh_legacy_fallback"
fi

# 8. Every write of the config path to the PVC targets the NEW name. Counted,
#    not just grepped: there are two such writes (the K8s post-merge snapshot
#    and the Docker first-boot seed) and both must use HIVE_CONFIG_RUNTIME.
#    A single grep would let one of them regress to the legacy name unnoticed.
writes_new="$(grep -c 'cp "\$HIVE_CONFIG_PATH" "\$HIVE_CONFIG_RUNTIME"' "$ENTRYPOINT" || true)"
check "both PVC writes target the new name" "2" "$writes_new"

writes_legacy="$(grep -c 'cp "\$HIVE_CONFIG_PATH" "\$HIVE_CONFIG_RUNTIME_LEGACY"' "$ENTRYPOINT" || true)"
check "no PVC write targets the legacy name" "0" "$writes_legacy"

# ── #5360: hardening must leave the file READABLE BY THE READING USER ──
#
# The regression this closes: #5342 asserted only that the mode was 0600 and
# passed while the product was broken. 0600 is OWNER-only, the `cp` calls that
# create the file run as root, and the hive process drops to dev (uid 1001)
# before it opens the config — so a root:root 0600 file is mode-correct and
# unreadable, and startup died with `permission denied` on the arm64 lane.
#
# So the assertion is not "mode is 0600". It is "the mode is 0600 AND uid 1001
# can actually open it" — the property the product needs, checked by really
# opening the file as that uid rather than by inspecting metadata.

echo
echo "=== #5360: hardened config is readable by the runtime user ==="

# Every site that creates or hardens a PVC config copy must route through the
# helper, so a new `cp` cannot reintroduce the bug by hardening inline.
inline_chmod="$(grep -cE '^[[:space:]]*chmod 600 "\$(HIVE_CONFIG_RUNTIME|_cfg)' "$ENTRYPOINT" || true)"
check "no site chmods a PVC config copy outside the helper" "0" "$inline_chmod"

# The helper must chown, not merely chmod. A helper that only chmods is
# exactly the #5360 shape.
HARDEN="$(sed -n '/^hive_harden_runtime_config() {/,/^}/p' "$ENTRYPOINT")"
if [ -z "$HARDEN" ]; then
  echo "  FAIL: could not extract hive_harden_runtime_config from $ENTRYPOINT"
  FAIL=$((FAIL + 1))
elif ! grep -q 'chown' <<<"$HARDEN"; then
  echo "  FAIL: hive_harden_runtime_config does not chown — 0600 on a root-owned"
  echo "        file is unreadable to the dev uid that reads it (#5360)"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: hive_harden_runtime_config chowns as well as chmods"
  PASS=$((PASS + 1))
fi

# The behavioural test. Requires root (to own a file as root and then drop to
# another uid) and a uid-1001 account. CI's arm64/container lanes have both;
# a developer laptop generally has neither, so skip loudly rather than fake a
# pass — a silent skip here is how the original gap shipped.
RUNTIME_UID=1001
if [ "$(id -u)" != "0" ]; then
  hive_test_skip "not root — cannot exercise the root-creates/dev-reads path" \
       "(this is the case CI must run; see #5360)"
elif ! id -u dev >/dev/null 2>&1; then
  hive_test_skip "no 'dev' account on this host — cannot exercise the drop"
else
  tmpd="$(mktemp -d)"
  trap 'rm -rf "$tmpd"' EXIT
  # /data is world-traversable in the image; mirror that so the only thing
  # under test is the file's own mode and ownership.
  chmod 755 "$tmpd"

  target="$tmpd/hive.yaml.runtime"
  # Reproduce the failing shape exactly: root creates the file 0644 (the mode
  # `cp` inherits from the 0644 ConfigMap seed / bind-mounted hive.yaml).
  printf 'dashboard:\n  auth_token: probe-not-a-real-token\n' > "$target"
  chown root:root "$target"
  chmod 644 "$target"

  HIVE_RUNTIME_USER="dev"
  HIVE_RUNTIME_GROUP="node"
  export HIVE_RUNTIME_USER HIVE_RUNTIME_GROUP
  # shellcheck disable=SC1090
  eval "$HARDEN"
  hive_harden_runtime_config "$target" >/dev/null

  mode="$(stat -c '%a' "$target" 2>/dev/null)"
  owner="$(stat -c '%u' "$target" 2>/dev/null)"

  # 1. Still owner-only. The security fix must not be weakened to buy back
  #    readability — the file holds dashboard.auth_token (#5331).
  check "hardened config is still mode 0600" "600" "$mode"

  # 2. Owned by the uid that reads it. This is the half #5342 was missing.
  check "hardened config is owned by the runtime uid" "$RUNTIME_UID" "$owner"

  # 3. THE ASSERTION THAT MATTERS: really open it as that uid. Mode and owner
  #    are metadata; this is the syscall the hive binary makes at startup,
  #    and it is what returned EACCES in #5360.
  if su -s /bin/sh dev -c "cat '$target' >/dev/null 2>&1"; then
    echo "  PASS: the runtime user can actually read the hardened config"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: the runtime user CANNOT read the hardened config"
    echo "        this is #5360 — hive aborts with 'permission denied' at startup"
    FAIL=$((FAIL + 1))
  fi

  # 4. And nobody else can. Confirms we bought readability with ownership,
  #    not by widening the mode. 'nobody' exists on every image variant here.
  if id -u nobody >/dev/null 2>&1; then
    if su -s /bin/sh nobody -c "cat '$target' >/dev/null 2>&1"; then
      echo "  FAIL: an unrelated uid can read the hardened config"
      echo "        the token in this file must not be world-readable (#5331)"
      FAIL=$((FAIL + 1))
    else
      echo "  PASS: an unrelated uid cannot read the hardened config"
      PASS=$((PASS + 1))
    fi
  fi

  # 5. A file already owned by the runtime user still gets tightened. This is
  #    the steady state after Config.Save() and every non-root boot.
  printf 'dashboard:\n  auth_token: probe-not-a-real-token\n' > "$target"
  chown dev:node "$target"
  chmod 644 "$target"
  hive_harden_runtime_config "$target" >/dev/null
  check "already dev-owned config is still tightened to 0600" \
    "600" "$(stat -c '%a' "$target" 2>/dev/null)"

  rm -rf "$tmpd"
  trap - EXIT
fi

hive_test_report
