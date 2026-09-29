#!/usr/bin/env bash
# Deterministic changelog.d fragment guard shared by CI and the PR-open precheck.
set -euo pipefail

base_ref="${BASE_REF:-${1:-}}"
if [ -z "$base_ref" ]; then
  echo "BASE_REF (or first argument) is required" >&2
  exit 2
fi
has_no_changelog_label="${HAS_NO_CHANGELOG_LABEL:-false}"

# Two views of the diff: ALL changed paths (deletions included — a PR that only
# removes code is still a code change an operator can see), and paths still
# present at HEAD (a deleted fragment can be neither validated nor counted).
git diff --name-only "origin/${base_ref}...HEAD" > changed-files.txt
git diff --name-only --diff-filter=d "origin/${base_ref}...HEAD" > present-files.txt
echo "Changed files vs merge base with origin/${base_ref}:"
sed 's/^/  /' changed-files.txt

grep -E '^changelog\.d/[^/]+\.md$' present-files.txt | grep -v '^changelog\.d/README\.md$' > fragments.txt || true
bad=0
while IFS= read -r f; do
  [ -z "$f" ] && continue
  base="$(basename "$f")"
  if ! printf '%s' "$base" | grep -qE '^(added|changed|deprecated|fixed|security)-[A-Za-z0-9][A-Za-z0-9._-]*\.md$'; then
    echo "::error file=$f::fragment name must be <category>-<pr-or-slug>.md with category one of added/changed/deprecated/fixed/security (see changelog.d/README.md)"
    bad=1
    continue
  fi
  first_line="$(grep -m1 '[^[:space:]]' "$f" || true)"
  case "$first_line" in
    '- '*|'<!-- release:'*|'<!--release:'*) ;;
    *)
      echo "::error file=$f::a fragment must start with a '- ' entry bullet (or a '<!-- release: ... -->' marker) — it IS the entry; the compiler owns the ### headings"
      bad=1
      ;;
  esac
done < fragments.txt
if [ "$bad" -ne 0 ]; then
  exit 1
fi

grep -E '^src/' changed-files.txt \
  | grep -vE '^src/docs/' \
  | grep -vE '_test\.go$' \
  | grep -vE '^src/(deploy|scripts)/test[-_][^/]+\.sh$' \
  > code-files.txt || true

if [ ! -s code-files.txt ]; then
  echo "OK: no non-exempt code changes under src/ — no fragment needed."
  exit 0
fi
echo "Code files that make this PR changelog-relevant:"
sed 's/^/  /' code-files.txt

if [ -s fragments.txt ]; then
  echo "OK: the PR adds a changelog.d fragment."
  exit 0
fi

if [ "$has_no_changelog_label" = "true" ]; then
  echo "OK: the PR carries the no-changelog label — author judged it not user-visible."
  exit 0
fi

if grep -qx 'CHANGELOG.md' changed-files.txt; then
  echo "::error::This PR edits CHANGELOG.md's Unreleased section directly. Move the entry into a changelog.d/<category>-<slug>.md fragment (see below; #5675)."
fi

echo "::error::src/ code changed without a changelog.d fragment or a no-changelog label (see job log; #5675)"
cat <<'GUIDANCE'

CHANGELOG FRAGMENT GUARD (#5675):

This PR changes code under src/ and neither adds a changelog fragment nor carries
the `no-changelog` label.

If the change is user-visible, add ONE file:

  changelog.d/<category>-<pr-or-slug>.md

category: added | changed | deprecated | fixed | security
content:  exactly your entry, as a single '- ' bullet.

If it is a refactor, test-only, or dependency churn, ask a maintainer to add the
`no-changelog` label instead. Do NOT append to CHANGELOG.md's Unreleased section.
GUIDANCE
exit 1
