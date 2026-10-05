#!/usr/bin/env bash
# Deterministic changelog.d fragment guard shared by CI and the PR-open precheck.
set -euo pipefail

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

base_ref="${BASE_REF:-${1:-}}"
if [ -z "$base_ref" ]; then
  echo "BASE_REF (or first argument) is required" >&2
  exit 2
fi
has_no_changelog_label="${HAS_NO_CHANGELOG_LABEL:-false}"

# Forward-merge sync PRs (v5-topup.yml / v6-topup.yml and the hand-rolled ones
# opened when those conflict) carry the source line's whole history, so the
# three-dot diff below re-attributes every carried src/ change and the
# release-time CHANGELOG.md compilation edit to the sync PR — it can never
# satisfy this guard, and each carried change already passed it on its own PR
# (#7783). Human-opened top-ups live on sync/*; agent-opened ones are forced
# onto <lane>/sync-vN-to-vM by the push broker, which the workflow's own
# sync/* exemption missed and then autofixed with a bogus fragment (#10475).
# Exempting here too keeps the autofix (gated on this script failing) from
# ever pushing to a sync branch, whatever the workflow-level check does.
head_ref="${HEAD_REF:-${GITHUB_HEAD_REF:-}}"
case "$head_ref" in
  sync/*|*/sync-v[0-9]*-to-v[0-9]*)
    echo "OK: forward-merge sync branch '$head_ref' — carried changes passed the guard on their own PRs; no fragment needed."
    exit 0
    ;;
esac

# Two views of the diff: ALL changed paths (deletions included — a PR that only
# removes code is still a code change an operator can see), and paths still
# present at HEAD (a deleted fragment can be neither validated nor counted).
git diff --name-only "origin/${base_ref}...HEAD" > "$work_dir"/changed-files.txt
git diff --name-only --diff-filter=d "origin/${base_ref}...HEAD" > "$work_dir"/present-files.txt
echo "Changed files vs merge base with origin/${base_ref}:"
sed 's/^/  /' "$work_dir"/changed-files.txt

grep -E '^changelog\.d/[^/]+\.md$' "$work_dir"/present-files.txt | grep -v '^changelog\.d/README\.md$' > "$work_dir"/fragments.txt || true
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
done < "$work_dir"/fragments.txt
if [ "$bad" -ne 0 ]; then
  exit 1
fi

grep -E '^src/' "$work_dir"/changed-files.txt \
  | grep -vE '^src/docs/' \
  | grep -vE '_test\.go$' \
  | grep -vE '^src/(deploy|scripts)/test[-_][^/]+\.sh$' \
  > "$work_dir"/code-files.txt || true

if [ ! -s "$work_dir"/code-files.txt ]; then
  echo "OK: no non-exempt code changes under src/ — no fragment needed."
  exit 0
fi
echo "Code files that make this PR changelog-relevant:"
sed 's/^/  /' "$work_dir"/code-files.txt

if [ -s "$work_dir"/fragments.txt ]; then
  echo "OK: the PR adds a changelog.d fragment."
  exit 0
fi

if [ "$has_no_changelog_label" = "true" ]; then
  echo "OK: the PR carries the no-changelog label — author judged it not user-visible."
  exit 0
fi

if grep -qx 'CHANGELOG.md' "$work_dir"/changed-files.txt; then
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
