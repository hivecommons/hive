#!/usr/bin/env bash
# test-extract-release-notes.sh — exercises CHANGELOG release-note extraction.
#
# Usage: src/scripts/test-extract-release-notes.sh
set -uo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
extract="$script_dir/extract-release-notes.sh"
tmp_root=${TMPDIR:-"$script_dir/../.test-tmp"}
mkdir -p "$tmp_root"
tmp=$(mktemp -d "$tmp_root/extract-notes.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

fail=0
note_fail() { echo "  FAIL: $*"; fail=1; }
note_ok()   { echo "  ok: $*"; }

cat > "$tmp/CHANGELOG.md" <<'EOF'
# Changelog

## Unreleased

## 2026-09-04 (v4.12.2)

### Fixed

- A shipped fix.

## 2026-09-04 (v4.12.1)

## 2026-09-03 (v4.12.0)

### Added

- Older entry.
EOF

out="$(bash "$extract" "$tmp/CHANGELOG.md" 4.12.2)"
want=$'### Fixed\n\n- A shipped fix.'
if [[ "$out" == "$want" ]]; then
  note_ok "extracts and trims a populated release section"
else
  note_fail "unexpected populated-section output: $out"
fi

cat > "$tmp/fallback.md" <<'EOF'
## What's Changed
- generated fallback
EOF

out="$(bash "$extract" "$tmp/CHANGELOG.md" 4.12.1 "$tmp/fallback.md")"
want=$'## What\'s Changed\n- generated fallback'
if [[ "$out" == "$want" ]]; then
  note_ok "empty release section falls back to generated notes"
else
  note_fail "empty section did not use fallback: $out"
fi

if bash "$extract" "$tmp/CHANGELOG.md" 9.9.9 >/dev/null 2>&1; then
  note_fail "missing release section without fallback should fail"
else
  note_ok "missing release section without fallback fails"
fi

exit "$fail"
