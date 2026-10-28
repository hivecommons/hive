#!/usr/bin/env bash
# test-crux-publish-keep.sh — proves crux-publish.sh leaves the hand-maintained
# scaffolding in hivecommons/crux (notably runbooks/) alone while clearing
# stale synced files. A sync once deleted runbooks/ because it was not in KEEP.
#
# Usage: src/scripts/test-crux-publish-keep.sh
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail=0
pass() { echo "  ok: $*"; }
bad()  { echo "  FAIL: $*"; fail=1; }

command -v rsync >/dev/null || { echo "  skip: rsync not installed"; exit 0; }

HIVE="$TMP/hive"; DEST="$TMP/dest"
mkdir -p "$HIVE/src/crux" "$DEST/runbooks" "$DEST/stale"
git -C "$HIVE" init -q
git -C "$HIVE" -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m init
printf 'module github.com/hivecommons/hive/crux\n\ngo 1.22\n' > "$HIVE/src/crux/go.mod"
echo '# module' > "$HIVE/src/crux/README.md"
mkdir -p "$HIVE/src/crux/docs"
echo x > "$DEST/runbooks/release-rollback.md"
echo x > "$DEST/stale/old.txt"
echo x > "$DEST/README.md"

if ! out="$(CRUX_SKIP_BUILD=1 bash "$HERE/crux-publish.sh" "$HIVE" "$DEST" 2>&1)"; then
  bad "crux-publish.sh failed"; echo "$out" | sed 's/^/      | /'
fi

[[ -f "$DEST/runbooks/release-rollback.md" ]] && pass "runbooks/ survives a sync" || bad "runbooks/ was deleted"
[[ -f "$DEST/README.md" ]] && pass "README.md survives a sync" || bad "README.md was deleted"
[[ ! -e "$DEST/stale" ]] && pass "stale synced entries are cleared" || bad "stale/ was not cleared"

exit $fail
