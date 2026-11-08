#!/usr/bin/env bash
# test-release-notes-cap.sh — exercises the release-notes size cap that keeps
# `gh release create` under GitHub's 125000-character body limit (v6.0.0's
# first-on-the-line notes exceeded it and the Release step failed after the
# tag and image retags had already happened).
#
# Usage: src/scripts/test-release-notes-cap.sh
set -uo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
cap="$script_dir/release-notes-cap.sh"
tmp_root=${TMPDIR:-"$script_dir/../.test-tmp"}
mkdir -p "$tmp_root"
tmp=$(mktemp -d "$tmp_root/notes-cap.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

fail=0
note_fail() { echo "  FAIL: $*"; fail=1; }
note_ok()   { echo "  ok: $*"; }

cat > "$tmp/CHANGELOG.md" <<'EOF'
# Changelog

## Unreleased

## 2026-11-08 (v6.0.0)

### Added

- entry
EOF

# --- fits: untouched, silent ------------------------------------------------
printf '### Fixed\n\n- small fix (#1)\n' > "$tmp/small.md"
cp "$tmp/small.md" "$tmp/small.before"
out="$(RELEASE_NOTES_MAX_CHARS=1000 bash "$cap" "$tmp/small.md" 6.0.0 hivecommons/hive "$tmp/CHANGELOG.md")"
if cmp -s "$tmp/small.md" "$tmp/small.before" && [[ -z "$out" ]]; then
  note_ok "body under the cap is left untouched"
else
  note_fail "body under the cap was modified or produced output: $out"
fi

# --- oversized: cut at a bullet boundary, marker appended --------------------
{
  echo "### Added"
  echo
  for i in $(seq 1 200); do
    printf -- '- added entry %03d with some padding text to make it longer (#%d)\n' "$i" "$i"
    printf '  continuation line for entry %03d\n' "$i"
  done
  echo
  echo "### Fixed"
  echo
  for i in $(seq 1 50); do
    printf -- '- fixed entry %03d (#%d)\n' "$i" "$((1000 + i))"
  done
} > "$tmp/big.md"
total_bullets=250
limit=6000
out="$(RELEASE_NOTES_MAX_CHARS=$limit bash "$cap" "$tmp/big.md" 6.0.0 hivecommons/hive "$tmp/CHANGELOG.md")"
rc=$?
size="$(wc -c < "$tmp/big.md" | tr -d ' ')"
if [[ "$rc" -eq 0 && "$size" -le "$limit" ]]; then
  note_ok "oversized body is capped to ${size} <= ${limit} bytes"
else
  note_fail "cap failed (rc=${rc}) or body still ${size} bytes: $out"
fi
last="$(tail -n1 "$tmp/big.md")"
kept="$(grep -c '^- ' "$tmp/big.md")"
dropped=$(( total_bullets - kept ))
want="… (${dropped} more entries) — full notes in [CHANGELOG.md](https://github.com/hivecommons/hive/blob/v6.0.0/CHANGELOG.md#2026-11-08-v600)"
if [[ "$last" == "$want" ]]; then
  note_ok "marker counts dropped bullets and links the tag's CHANGELOG section anchor"
else
  note_fail "unexpected marker line: ${last} (want: ${want})"
fi
# The line before the blank separator must be the end of a whole entry: the
# last kept bullet's continuation line, never a bullet split from its body.
before_marker="$(tail -n3 "$tmp/big.md" | head -n1)"
if [[ "$before_marker" == "  continuation line for entry "* ]]; then
  note_ok "cut lands on a bullet boundary (continuation kept with its bullet)"
else
  note_fail "cut split an entry: '${before_marker}'"
fi
if ! grep -q '^### Fixed' "$tmp/big.md"; then
  note_ok "subsections past the cut are dropped with their entries"
else
  note_fail "### Fixed should have been cut"
fi

# --- deterministic: same input, same output ---------------------------------
{
  echo "### Added"
  echo
  for i in $(seq 1 200); do
    printf -- '- added entry %03d with some padding text to make it longer (#%d)\n' "$i" "$i"
    printf '  continuation line for entry %03d\n' "$i"
  done
  echo
  echo "### Fixed"
  echo
  for i in $(seq 1 50); do
    printf -- '- fixed entry %03d (#%d)\n' "$i" "$((1000 + i))"
  done
} > "$tmp/big2.md"
RELEASE_NOTES_MAX_CHARS=$limit bash "$cap" "$tmp/big2.md" 6.0.0 hivecommons/hive "$tmp/CHANGELOG.md" >/dev/null
if cmp -s "$tmp/big.md" "$tmp/big2.md"; then
  note_ok "capping is deterministic"
else
  note_fail "two runs over identical input differ"
fi

# --- no changelog: link has no anchor ----------------------------------------
cp "$tmp/big2.md" "$tmp/big3.md"
printf '%s\n' "- one more" >> "$tmp/big3.md"
RELEASE_NOTES_MAX_CHARS=3000 bash "$cap" "$tmp/big3.md" 6.0.0 hivecommons/hive >/dev/null
if tail -n1 "$tmp/big3.md" | grep -q 'blob/v6.0.0/CHANGELOG.md)$'; then
  note_ok "without a changelog the link falls back to the file"
else
  note_fail "unexpected fallback link: $(tail -n1 "$tmp/big3.md")"
fi

# --- default cap leaves room for the workflow trailer under 125000 -----------
default_cap="$(grep -o 'RELEASE_NOTES_MAX_CHARS:-[0-9]*' "$cap" | cut -d- -f2)"
if [[ -n "$default_cap" && "$default_cap" -le 124000 ]]; then
  note_ok "default RELEASE_NOTES_MAX_CHARS=${default_cap} leaves headroom under GitHub's 125000"
else
  note_fail "default cap ${default_cap} leaves no room for the SBOM/NOTICE trailer"
fi

exit "$fail"
