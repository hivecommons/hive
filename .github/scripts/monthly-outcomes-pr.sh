#!/usr/bin/env bash
# Only sanitized renderer outputs enter this branch; never stage an input tree.
set -euo pipefail
unset GITHUB_TOKEN
: "${MONTH:?missing validated month}"
: "${GITHUB_REPOSITORY:?missing repository}"
[[ "$MONTH" =~ ^[0-9]{4}-[0-9]{2}$ ]] || exit 1
branch="automation/monthly-outcomes-$MONTH"
pr="$(gh pr list --repo "$GITHUB_REPOSITORY" --head "$branch" --state all --limit 2 --json number,state)"
if [ "$(printf '%s' "$pr" | jq length)" -gt 1 ] || printf '%s' "$pr" | jq -e 'any(.[]; .state != "OPEN")' >/dev/null; then
  echo "::error::A closed/merged monthly PR already exists; resolve it explicitly rather than reposting."
  exit 1
fi
if git ls-remote --exit-code --heads origin "$branch" >/dev/null; then
  git fetch origin "$branch"
  git checkout -b "$branch" FETCH_HEAD
else
  git checkout -b "$branch"
fi
dest="src/docs/benchmarks"
# Keep the original renderer revision when the approved input is unchanged,
# even if unrelated v5 commits landed while this month's PR was open.
provenance="outcomes-$MONTH.provenance.json"
if [ -f "$dest/$provenance" ] &&
   [ "$(jq -er .input_sha256 "$dest/$provenance")" = "$(jq -er .input_sha256 ".monthly-outcomes/$provenance")" ]; then
  echo "Approved input unchanged; preserving the existing monthly PR without a repost."
  exit 0
fi
# Replace only this month's outputs, including obsolete cards.
git ls-files -z "$dest/outcomes-$MONTH.*" "$dest/assets/outcomes-$MONTH-*" | xargs -0 -r git rm -q --
cp ".monthly-outcomes/outcomes-$MONTH.json" "$dest/"
cp ".monthly-outcomes/outcomes-$MONTH.md" "$dest/"
cp ".monthly-outcomes/outcomes-$MONTH.provenance.json" "$dest/"
cp .monthly-outcomes/assets/"outcomes-$MONTH-"* "$dest/assets/"
git add -- "$dest/outcomes-$MONTH.json" "$dest/outcomes-$MONTH.md" "$dest/outcomes-$MONTH.provenance.json" "$dest/assets/outcomes-$MONTH-"*
if git diff --cached --quiet; then
  echo "No monthly report diff; no commit, push or stale PR repost."
  exit 0
fi
git config user.name 'github-actions[bot]'
git config user.email '41898282+github-actions[bot]@users.noreply.github.com'
git commit -s -m "📖 docs: publish outcomes for $MONTH"
git push origin "$branch"
if [ "$(printf '%s' "$pr" | jq length)" -eq 0 ]; then
  gh pr create --repo "$GITHUB_REPOSITORY" --base v5 --head "$branch" \
    --title "📖 docs: monthly outcomes $MONTH" \
    --body "Owner-supplied, sanitized monthly aggregates. Review the cohort, observation time, cost denominator and provenance before merging. No raw evidence, stories or deployment identities are published. Collection completeness and publication approval are owner attestations, not independently verified by this workflow. CI and normal review gates must pass; this workflow does not merge."
fi
