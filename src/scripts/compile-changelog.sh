#!/usr/bin/env bash
# compile-changelog.sh — fold changelog.d/ fragment files into CHANGELOG.md's
# `## Unreleased` section, then delete the fragments (#5675).
#
# WHY FRAGMENTS. Every PR used to append its entry directly under
# `## Unreleased`, so any two PRs landing near each other edited the same lines
# under the same heading and the second one to merge hit a structural conflict
# — six rebases of unrelated PRs in one day. Each PR now writes its own file
# under changelog.d/ (see changelog.d/README.md for the format), and this
# script is the single point where those files become CHANGELOG.md content.
#
# WHO RUNS IT. tagged-release.yml, twice per release:
#   - in the `decide` job, working-tree only, so derive-release-version.sh
#     sees fragment content when judging release-worthiness and the bump;
#   - in the `release` job, immediately before the existing "move Unreleased
#     into a dated section" step, with the release commit carrying both the
#     CHANGELOG.md update and the fragment deletions.
# Running it locally is also fine — it is deliberately safe to preview with a
# dirty tree and `git checkout -- CHANGELOG.md changelog.d` to undo.
#
# BEHAVIOUR:
#   - No changelog.d/ directory, or no fragments in it => exact no-op:
#     CHANGELOG.md is not rewritten (not even byte-identically), exit 0. This
#     preserves tagged-release.yml's idempotency loop — after the release
#     commit consumes the fragments, the re-triggered run sees an empty
#     Unreleased AND an empty changelog.d/ and derives release=false.
#   - Fragments are gathered from changelog.d/*.md (README.md excluded),
#     sorted by filename within each category for a deterministic order.
#   - The filename prefix picks the `###` subsection: added- / changed- /
#     deprecated- / fixed- / security-. An unrecognized prefix is a LOUD
#     failure, never a silent guess — a miscategorized entry changes the
#     derived semver bump.
#   - Entries are appended to an existing `### <Category>` subsection under
#     Unreleased if one exists (after the entries already there), else the
#     subsection is created at the end of the Unreleased section. Direct
#     CHANGELOG.md edits therefore keep working alongside fragments, which is
#     what the 7-day transition window relies on.
#   - A fragment's first non-blank line must start with `- ` (an entry bullet)
#     or be a `<!-- release: ... -->` marker, so the escape hatch
#     derive-release-version.sh honours can also ride in a fragment.
#   - Each fragment is deleted after a successful compile, so `git add -A
#     changelog.d` in the release commit records the consumption.
#
# Usage: src/scripts/compile-changelog.sh [changelog-path] [fragments-dir]
#   changelog-path  default CHANGELOG.md
#   fragments-dir   default changelog.d
set -euo pipefail

CHANGELOG="${1:-CHANGELOG.md}"
FRAGDIR="${2:-changelog.d}"

# Canonical subsection order for newly created subsections (keep-a-changelog
# order; existing subsections keep their position, entries are only appended).
CATEGORIES=(added changed deprecated fixed security)

category_heading() {
  case "$1" in
    added)      echo "### Added" ;;
    changed)    echo "### Changed" ;;
    deprecated) echo "### Deprecated" ;;
    fixed)      echo "### Fixed" ;;
    security)   echo "### Security" ;;
    *)          return 1 ;;
  esac
}

if [[ ! -f "$CHANGELOG" ]]; then
  echo "::error::changelog not found at ${CHANGELOG}" >&2
  exit 1
fi

if [[ ! -d "$FRAGDIR" ]]; then
  echo "No ${FRAGDIR}/ directory — nothing to compile."
  exit 0
fi

shopt -s nullglob
fragments=()
for f in "$FRAGDIR"/*.md; do
  base="$(basename "$f")"
  [[ "$base" == "README.md" ]] && continue
  fragments+=("$f")
done
shopt -u nullglob

if [[ ${#fragments[@]} -eq 0 ]]; then
  echo "No fragments in ${FRAGDIR}/ — nothing to compile."
  exit 0
fi

# The Unreleased heading must exist, or the awk below would pass the file
# through unchanged and the fragments would be deleted without ever landing
# anywhere — silent data loss, the one failure mode a compiler cannot have.
if ! awk '
  /^[[:space:]]*(```|~~~)/ { in_fence = !in_fence }
  !in_fence && /^## Unreleased[[:space:]]*$/ { found = 1 }
  END { exit found ? 0 : 1 }
' "$CHANGELOG"; then
  echo "::error::${CHANGELOG} has no '## Unreleased' heading — refusing to compile fragments into nowhere." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Validate every fragment BEFORE touching anything, so a bad fragment fails
# the whole run with the changelog untouched and every fragment still on disk.
# ---------------------------------------------------------------------------
bad=0
for f in "${fragments[@]}"; do
  base="$(basename "$f")"
  if ! [[ "$base" =~ ^(added|changed|deprecated|fixed|security)-[A-Za-z0-9][A-Za-z0-9._-]*\.md$ ]]; then
    echo "::error::${f}: fragment name must be <category>-<pr-or-slug>.md with category one of added/changed/deprecated/fixed/security (see ${FRAGDIR}/README.md)" >&2
    bad=1
    continue
  fi
  first_line="$(grep -m1 '[^[:space:]]' "$f" || true)"
  if [[ -z "$first_line" ]]; then
    echo "::error::${f}: fragment is empty" >&2
    bad=1
  elif ! [[ "$first_line" == "- "* || "$first_line" == "<!-- release:"* || "$first_line" == "<!--release:"* ]]; then
    echo "::error::${f}: a fragment must start with a '- ' entry bullet (or a '<!-- release: ... -->' marker); it is the entry itself, not a section with its own headings" >&2
    bad=1
  fi
done
if [[ "$bad" -ne 0 ]]; then
  exit 1
fi

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

# ---------------------------------------------------------------------------
# Group fragment bodies by category, trailing blank lines stripped, sorted by
# filename within the category so the compiled order is deterministic.
# ---------------------------------------------------------------------------
for cat in "${CATEGORIES[@]}"; do
  catfile="${workdir}/${cat}.entries"
  while IFS= read -r f; do
    [[ -z "$f" ]] && continue
    # Strip trailing blank lines; keep internal continuation lines verbatim.
    awk 'NF { last = NR } { lines[NR] = $0 } END { for (i = 1; i <= last; i++) print lines[i] }' "$f" >> "$catfile"
  done < <(printf '%s\n' "${fragments[@]}" | grep -E "/${cat}-[^/]+\.md$" | LC_ALL=C sort || true)
done

# ---------------------------------------------------------------------------
# Insert each category's entries into the Unreleased section: appended to the
# existing `### <Category>` subsection when present, else as a new subsection
# at the end of Unreleased. Blank lines inside the section are buffered so new
# entries land directly after the last existing entry, with the surrounding
# blank-line hygiene (one blank line before the next `##`/`###` heading)
# preserved — the shape tagged-release.yml's move step and
# derive-release-version.sh both parse.
# ---------------------------------------------------------------------------
compiled="${workdir}/CHANGELOG.compiled"
cp "$CHANGELOG" "$compiled"

for cat in "${CATEGORIES[@]}"; do
  catfile="${workdir}/${cat}.entries"
  [[ -s "$catfile" ]] || continue
  heading="$(category_heading "$cat")"
  next="${workdir}/CHANGELOG.next"
  awk -v heading="$heading" -v ef="$catfile" '
    function dump(   line) {
      while ((getline line < ef) > 0) print line
      close(ef)
    }
    function flushblanks(   i) {
      for (i = 0; i < nb; i++) print ""
      nb = 0
    }
    BEGIN { in_unrel = 0; in_cat = 0; inserted = 0; nb = 0; in_fence = 0 }
    {
      if ($0 ~ /^[[:space:]]*(```|~~~)/) in_fence = !in_fence
      if (!in_unrel) {
        print
        if (!in_fence && $0 ~ /^## Unreleased[[:space:]]*$/) in_unrel = 1
        next
      }
      if (!in_fence && $0 ~ /^## /) {
        # Unreleased ends here. Land the entries first, then exactly one
        # blank line before this next release heading.
        if (in_cat && !inserted) { dump(); inserted = 1; in_cat = 0 }
        else if (!inserted) { print ""; print heading; print ""; dump(); inserted = 1 }
        print ""
        nb = 0
        in_unrel = 0
        print
        next
      }
      if ($0 ~ /^[[:space:]]*$/) { nb++; next }
      if (in_cat && !in_fence && $0 ~ /^### /) {
        # The target subsection ends at the next subsection: append the new
        # entries right after its last existing line, then restore the
        # buffered blank line(s) that preceded this heading.
        dump(); inserted = 1; in_cat = 0
        flushblanks()
        print
        next
      }
      flushblanks()
      print
      if (!inserted && !in_fence && $0 == heading) in_cat = 1
    }
    END {
      if (in_unrel) {
        if (in_cat && !inserted) { dump(); inserted = 1 }
        else if (!inserted) { print ""; print heading; print ""; dump(); inserted = 1 }
        print ""
      }
    }
  ' "$compiled" > "$next"
  mv "$next" "$compiled"
done

mv "$compiled" "$CHANGELOG"

for f in "${fragments[@]}"; do
  rm "$f"
done

echo "Compiled ${#fragments[@]} fragment(s) from ${FRAGDIR}/ into ${CHANGELOG}'s Unreleased section and deleted them."
