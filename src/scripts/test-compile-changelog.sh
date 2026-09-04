#!/usr/bin/env bash
# test-compile-changelog.sh — exercises src/scripts/compile-changelog.sh
# against fixture fragments and fixture CHANGELOG.md files, asserting the
# PROPERTIES the release pipeline depends on rather than the script's shape
# (#5675):
#
#   - fragments land under the right `###` subsection of `## Unreleased`,
#     appended AFTER existing entries, with released sections byte-untouched;
#   - a category with no existing subsection gets one created inside
#     Unreleased, never leaking past the next `## ` release heading —
#     tagged-release.yml's move step and derive-release-version.sh both parse
#     that boundary;
#   - an empty / absent fragments dir is an exact no-op (byte-identical file,
#     exit 0) — the idempotency of tagged-release.yml's self-retrigger loop
#     rides on this;
#   - a second run after a compile is that same no-op (fragments consumed);
#   - an unrecognized category prefix fails LOUDLY with the changelog
#     untouched and every fragment still on disk (a miscategorized entry
#     would change the derived semver bump);
#   - the compiled result is exactly what derive-release-version.sh needs:
#     an added- fragment flips an empty Unreleased to a minor release.
#
# Usage: src/scripts/test-compile-changelog.sh
set -uo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
compile="$script_dir/compile-changelog.sh"
derive="$script_dir/derive-release-version.sh"
tmp_root=${TMPDIR:-"$script_dir/../.test-tmp"}
mkdir -p "$tmp_root"
tmp=$(mktemp -d "$tmp_root/compile-changelog.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

fail=0
note_fail() { echo "  FAIL: $*"; fail=1; }
note_ok()   { echo "  ok: $*"; }

# A fresh case directory with a fixture CHANGELOG carrying an existing
# Unreleased Fixed entry and one released section — the live file's shape.
new_case() {
  local dir="$tmp/case-$1"
  mkdir -p "$dir/changelog.d"
  cat > "$dir/CHANGELOG.md" <<'EOF'
# Changelog

Intro prose that must never move.

## Unreleased

### Fixed

- an existing unreleased fix ([#1](https://example.invalid/1)).

## 2026-01-01 (v1.0.0)

### Added

- a released thing that must stay byte-identical.
EOF
  echo "$dir"
}

# ---------------------------------------------------------------------------
# Case 1: fixture fragments => expected CHANGELOG, exactly.
# One fragment appends to the existing Fixed subsection; one creates a new
# Added subsection; the entries stay inside Unreleased.
# ---------------------------------------------------------------------------
dir=$(new_case 1)
printf -- '- squashed a new bug ([#2](https://example.invalid/2)).\n' > "$dir/changelog.d/fixed-2-new-bug.md"
printf -- '- grew a new feature ([#3](https://example.invalid/3)).\n' > "$dir/changelog.d/added-3-feature.md"
printf -- 'this is documentation, not an entry\n' > "$dir/changelog.d/README.md"

if ( cd "$dir" && bash "$compile" CHANGELOG.md changelog.d ) > "$tmp/case1.out" 2>&1; then
  note_ok "compile succeeds on well-formed fragments"
else
  note_fail "compile failed on well-formed fragments: $(cat "$tmp/case1.out")"
fi

cat > "$tmp/case1.expected" <<'EOF'
# Changelog

Intro prose that must never move.

## Unreleased

### Fixed

- an existing unreleased fix ([#1](https://example.invalid/1)).
- squashed a new bug ([#2](https://example.invalid/2)).

### Added

- grew a new feature ([#3](https://example.invalid/3)).

## 2026-01-01 (v1.0.0)

### Added

- a released thing that must stay byte-identical.
EOF
if diff -u "$tmp/case1.expected" "$dir/CHANGELOG.md" > "$tmp/case1.diff"; then
  note_ok "fragments compile to exactly the expected CHANGELOG (append to existing Fixed, new Added subsection, released section untouched)"
else
  note_fail "compiled CHANGELOG differs from expected:"$'\n'"$(cat "$tmp/case1.diff")"
fi

if [[ ! -e "$dir/changelog.d/fixed-2-new-bug.md" && ! -e "$dir/changelog.d/added-3-feature.md" ]]; then
  note_ok "consumed fragments are deleted"
else
  note_fail "fragments should be deleted after a compile"
fi
if [[ -f "$dir/changelog.d/README.md" ]]; then
  note_ok "changelog.d/README.md survives the compile"
else
  note_fail "README.md must never be consumed as a fragment"
fi

# ---------------------------------------------------------------------------
# Case 2: idempotency — a second run right after the compile is a no-op.
# ---------------------------------------------------------------------------
cp "$dir/CHANGELOG.md" "$tmp/case2.before"
if ( cd "$dir" && bash "$compile" CHANGELOG.md changelog.d ) > /dev/null 2>&1 \
    && cmp -s "$tmp/case2.before" "$dir/CHANGELOG.md"; then
  note_ok "second run after a compile is a byte-identical no-op"
else
  note_fail "second run must exit 0 and leave the changelog byte-identical"
fi

# ---------------------------------------------------------------------------
# Case 3: empty dir and absent dir are exact no-ops. tagged-release.yml's
# self-retrigger loop depends on this: the release commit empties both
# Unreleased and changelog.d/, and the follow-up run must derive nothing.
# ---------------------------------------------------------------------------
dir=$(new_case 3)
cp "$dir/CHANGELOG.md" "$tmp/case3.before"
if ( cd "$dir" && bash "$compile" CHANGELOG.md changelog.d ) > /dev/null 2>&1 \
    && cmp -s "$tmp/case3.before" "$dir/CHANGELOG.md"; then
  note_ok "empty changelog.d/ is a byte-identical no-op"
else
  note_fail "empty changelog.d/ must exit 0 and leave the changelog byte-identical"
fi
rmdir "$dir/changelog.d"
if ( cd "$dir" && bash "$compile" CHANGELOG.md changelog.d ) > /dev/null 2>&1 \
    && cmp -s "$tmp/case3.before" "$dir/CHANGELOG.md"; then
  note_ok "absent changelog.d/ is a byte-identical no-op"
else
  note_fail "absent changelog.d/ must exit 0 and leave the changelog byte-identical"
fi

# ---------------------------------------------------------------------------
# Case 4: unrecognized category prefix fails loudly, changelog untouched,
# every fragment still on disk (including the valid one — all-or-nothing).
# ---------------------------------------------------------------------------
dir=$(new_case 4)
printf -- '- entry under a made-up category.\n' > "$dir/changelog.d/misc-oops.md"
printf -- '- a valid entry riding along.\n' > "$dir/changelog.d/fixed-9-valid.md"
cp "$dir/CHANGELOG.md" "$tmp/case4.before"
if ( cd "$dir" && bash "$compile" CHANGELOG.md changelog.d ) > "$tmp/case4.out" 2>&1; then
  note_fail "an unrecognized category prefix must fail, but the script exited 0"
else
  note_ok "unrecognized category prefix fails loudly"
fi
if cmp -s "$tmp/case4.before" "$dir/CHANGELOG.md" \
    && [[ -f "$dir/changelog.d/misc-oops.md" && -f "$dir/changelog.d/fixed-9-valid.md" ]]; then
  note_ok "a failed compile touches nothing: changelog byte-identical, all fragments still on disk"
else
  note_fail "a failed compile must leave the changelog and every fragment untouched"
fi
if grep -q "misc-oops" "$tmp/case4.out"; then
  note_ok "the failure names the offending fragment"
else
  note_fail "the failure should name the offending fragment, got: $(cat "$tmp/case4.out")"
fi

# ---------------------------------------------------------------------------
# Case 5: a fragment that is not an entry bullet (its own heading, prose)
# fails — a fragment IS the entry, not a section.
# ---------------------------------------------------------------------------
dir=$(new_case 5)
printf -- '### Fixed\n\n- an entry hiding under its own heading.\n' > "$dir/changelog.d/fixed-10-heading.md"
if ( cd "$dir" && bash "$compile" CHANGELOG.md changelog.d ) > /dev/null 2>&1; then
  note_fail "a fragment carrying its own heading must be rejected"
else
  note_ok "a fragment that is not a '- ' entry bullet is rejected"
fi

# ---------------------------------------------------------------------------
# Case 6: fenced prose that mentions `## Unreleased` must not be treated as
# the target section. This pins the #5918/#5939 bug class for both the
# existence check and the insertion transformer.
# ---------------------------------------------------------------------------
dir="$tmp/case-6"
mkdir -p "$dir/changelog.d"
cat > "$dir/CHANGELOG.md" <<'EOF'
# Changelog

```markdown
Example prose that is not the changelog section:
## Unreleased
```

## Unreleased

### Fixed

- the real existing entry.

## 2026-01-01 (v1.0.0)

### Added

- old.
EOF
printf -- '- landed in the real section.\n' > "$dir/changelog.d/fixed-12-fence.md"
( cd "$dir" && bash "$compile" CHANGELOG.md changelog.d ) > "$tmp/case6.out" 2>&1
if grep -A2 'Example prose' "$dir/CHANGELOG.md" | grep -q -- '- landed in the real section'; then
  note_fail "fenced ## Unreleased example must not receive fragment entries"
elif awk '/^## Unreleased$/ { real=1; next } real && /^## 2026-01-01/ { exit } real && /landed in the real section/ { found=1 } END { exit found ? 0 : 1 }' "$dir/CHANGELOG.md"; then
  note_ok "fenced prose mention of ## Unreleased is ignored by compile"
else
  note_fail "fragment did not land in the real Unreleased section: $(cat "$tmp/case6.out")"
fi

# ---------------------------------------------------------------------------
# Case 7: end-to-end property the release pipeline depends on — compiling an
# added- fragment into an EMPTY Unreleased makes derive-release-version.sh
# derive a minor release, where before the compile it derived none. This is
# the decide-job ordering (compile before derive) proven as a property.
# ---------------------------------------------------------------------------
dir="$tmp/case-7"
mkdir -p "$dir/changelog.d"
cat > "$dir/CHANGELOG.md" <<'EOF'
# Changelog

## Unreleased

## 2026-01-01 (v1.0.0)

### Added

- a released thing.
EOF
git init -q "$dir"
git -C "$dir" config user.email test@example.com
git -C "$dir" config user.name "Test"
git -C "$dir" -c commit.gpgsign=false commit -q --allow-empty -m init
git -C "$dir" tag v1.0.0
before=$( cd "$dir" && GITHUB_OUTPUT="" bash "$derive" CHANGELOG.md )
printf -- '- a fragment-borne feature.\n' > "$dir/changelog.d/added-11-feature.md"
( cd "$dir" && bash "$compile" CHANGELOG.md changelog.d ) > /dev/null 2>&1
after=$( cd "$dir" && GITHUB_OUTPUT="" bash "$derive" CHANGELOG.md )
if printf '%s\n' "$before" | grep -q '^release=false$' \
    && printf '%s\n' "$after" | grep -q '^release=true$' \
    && printf '%s\n' "$after" | grep -q '^bump=minor$' \
    && printf '%s\n' "$after" | grep -q '^version=1.1.0$'; then
  note_ok "compiled added- fragment flips derive-release-version.sh from no-release to a 1.1.0 minor release"
else
  note_fail "derive before/after compile mismatch. before: [$before] after: [$after]"
fi

echo ""
if [[ "$fail" -eq 0 ]]; then
  echo "test-compile-changelog: all cases passed"
else
  echo "test-compile-changelog: FAILURES above"
fi
exit "$fail"
