#!/usr/bin/env bash
# Tests the hive.yaml.bak -> hive.yaml.runtime migration in entrypoint.sh.
#
# The rename is copy-forward: readers prefer /data/hive.yaml.runtime and fall
# back to the legacy /data/hive.yaml.bak, and NOTHING renames or removes the
# legacy file on the PVC. ~51 live hives carry only the legacy name, and on
# Docker/LXC that file is the single copy of the live config — mutating it at
# boot could lose owner customisations with no warning.
#
# Rather than grepping the script, this extracts the real resolver function and
# executes it against real files, so it fails if the logic regresses.
#
# Run: bash src/deploy/test_entrypoint_runtime_config.sh
set -euo pipefail

PASS=0
FAIL=0

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
RESOLVER="$(sed -n '/^hive_runtime_config_read() {/,/^}/p' "$ENTRYPOINT")"
if [ -z "$RESOLVER" ]; then
  echo "  FAIL: could not extract hive_runtime_config_read from $ENTRYPOINT"
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
  out="$(eval "$RESOLVER"; hive_runtime_config_read)"

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

# 7. The legacy file must never be renamed or deleted. On Docker/LXC it is the
#    only copy of the live config, so a destructive migration could lose owner
#    customisations silently.
if grep -Eq '(mv|rm)[^\n]*HIVE_CONFIG_RUNTIME_LEGACY' "$ENTRYPOINT"; then
  echo "  FAIL: entrypoint moves or removes the legacy runtime config"
  echo "        the migration must be copy-forward only"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: legacy runtime config is never moved or removed"
  PASS=$((PASS + 1))
fi

# 8. Every write of the config path to the PVC targets the NEW name. Counted,
#    not just grepped: there are two such writes (the K8s post-merge snapshot
#    and the Docker first-boot seed) and both must use HIVE_CONFIG_RUNTIME.
#    A single grep would let one of them regress to the legacy name unnoticed.
writes_new="$(grep -c 'cp "\$HIVE_CONFIG_PATH" "\$HIVE_CONFIG_RUNTIME"' "$ENTRYPOINT" || true)"
check "both PVC writes target the new name" "2" "$writes_new"

writes_legacy="$(grep -c 'cp "\$HIVE_CONFIG_PATH" "\$HIVE_CONFIG_RUNTIME_LEGACY"' "$ENTRYPOINT" || true)"
check "no PVC write targets the legacy name" "0" "$writes_legacy"

# 9. A read-only ConfigMap fallback must reach the Go process itself. The image
# CMD still contains --config /etc/hive/hive.yaml, so merely exporting
# HIVE_CONFIG is insufficient: the explicit old flag wins. The shipped launch
# appends the selected PVC path last, preserving all other arguments.
check "read-only runtime fallback reaches hive as final config flag" \
  'hive "$@" --config "$HIVE_CONFIG" &' \
  "$(grep -F 'hive "$@" --config "$HIVE_CONFIG" &' "$ENTRYPOINT" | sed 's/^  //' || true)"

# 10. The ordinary writable/seed path remains unchanged and does not gain a
# second config flag.
check "ordinary writable config launch remains unchanged" \
  'hive "$@" &' \
  "$(grep -F '  hive "$@" &' "$ENTRYPOINT" | sed 's/^  //' || true)"

# 11. The root setup re-execs the entrypoint as dev. The effective HIVE_CONFIG
# may already point at the PVC by then, so the original seed path must survive
# in a distinct exported variable or the final-config comparison becomes a
# false no-op and the image CMD wins again.
check "entrypoint preserves original config path across privilege re-exec" \
  'HIVE_ENTRYPOINT_CONFIG_PATH="${HIVE_CONFIG:-/etc/hive/hive.yaml}"' \
  "$(grep -F '  HIVE_ENTRYPOINT_CONFIG_PATH="${HIVE_CONFIG:-/etc/hive/hive.yaml}"' "$ENTRYPOINT" | sed 's/^  //' || true)"

check "effective config comparison uses preserved original path" \
  'HIVE_CONFIG_PATH="$HIVE_ENTRYPOINT_CONFIG_PATH"' \
  "$(grep -F 'HIVE_CONFIG_PATH="$HIVE_ENTRYPOINT_CONFIG_PATH"' "$ENTRYPOINT" || true)"

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
