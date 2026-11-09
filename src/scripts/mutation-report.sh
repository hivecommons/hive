#!/usr/bin/env bash
# Advisory mutation report for the Go packages a PR touches (#17349, Phase 1).
# Runs gremlins on each changed package, then upserts ONE <!-- hive-mutation -->
# PR comment listing surviving (LIVED) mutants in changed files. Never fails.
set -uo pipefail

base_ref="${BASE_REF:?BASE_REF is required}"
pr_number="${PR_NUMBER:-}"
repo="${GITHUB_REPOSITORY:-}"
coefficient="${MUTATION_TIMEOUT_COEFFICIENT:-3}"
marker='<!-- hive-mutation -->'
out_dir="${MUTATION_OUT_DIR:-mutation-out}"
mkdir -p "$out_dir"

changed=$(git diff --name-only --diff-filter=d "origin/${base_ref}...HEAD" -- 'src/*.go' \
  | grep -vE '(_test\.go$|(^|/)testdata/|\.pb\.go$|_gen\.go$|_generated\.go$|(^|/)mock)' || true)
# Skip files that declare themselves generated.
kept=""
while IFS= read -r f; do
  [ -n "$f" ] && [ -f "$f" ] || continue
  head -n 20 "$f" | grep -qE '^// Code generated .* DO NOT EDIT\.' && continue
  kept+="$f"$'\n'
done <<< "$changed"
changed="${kept%$'\n'}"

if [ -z "$changed" ]; then
  echo "No changed non-test Go files; skipping mutation run."
  exit 0
fi

printf '%s\n' "$changed" > "$out_dir/changed.txt"
pkgs=$(sed 's|^src/||; s|/[^/]*$||' "$out_dir/changed.txt" | sort -u)

i=0
while IFS= read -r pkg; do
  [ -n "$pkg" ] || continue
  i=$((i + 1))
  echo "::group::gremlins unleash ./$pkg"
  (cd src && gremlins unleash --timeout-coefficient "$coefficient" \
    --output "../$out_dir/pkg-$i.json" "./$pkg") || echo "gremlins exited non-zero for $pkg (advisory)"
  echo "::endgroup::"
done <<< "$pkgs"

survivors="$out_dir/survivors.tsv"
: > "$survivors"
for j in "$out_dir"/pkg-*.json; do
  [ -f "$j" ] || continue
  jq -r '.files[]? | .file_name as $f | .mutations[]? | select(.status=="LIVED")
         | [$f, .line, .type] | @tsv' "$j" >> "$survivors" 2>/dev/null || true
done

# Keep only survivors in files the PR changed (gremlins reports module-relative paths).
filtered="$out_dir/survivors-changed.tsv"
: > "$filtered"
while IFS=$'\t' read -r f line type; do
  [ -n "$f" ] || continue
  if grep -qxF "src/${f#./}" "$out_dir/changed.txt" || grep -qxF "$f" "$out_dir/changed.txt"; then
    printf '%s\t%s\t%s\n' "$f" "$line" "$type" >> "$filtered"
  fi
done < "$survivors"

body="$out_dir/comment.md"
{
  echo "$marker"
  echo "### Mutation testing (advisory)"
  echo
  if [ -s "$filtered" ]; then
    echo "gremlins found **$(wc -l < "$filtered" | tr -d ' ') surviving mutant(s)** in changed files: the code ran under tests, but changing it did not fail any test."
    echo
    echo "| File | Line | Mutation |"
    echo "|---|---|---|"
    sort -t$'\t' -k1,1 -k2,2n "$filtered" | head -n 100 | awk -F'\t' '{printf "| `%s` | %s | %s |\n", $1, $2, $3}'
  else
    echo "No surviving mutants in changed files (or the run hit its time budget before reporting any)."
  fi
  echo
  echo "_Advisory only; this job never blocks the PR (#17349)._"
} > "$body"
cat "$body"

if [ -z "$pr_number" ] || [ -z "$repo" ]; then
  echo "PR_NUMBER/GITHUB_REPOSITORY unset; not posting a comment."
  exit 0
fi

id=$(gh api --paginate "repos/${repo}/issues/${pr_number}/comments" \
  --jq ".[] | select(.body | startswith(\"${marker}\")) | .id" 2>/dev/null | head -n 1 || true)
if [ -n "$id" ]; then
  gh api -X PATCH "repos/${repo}/issues/comments/${id}" -F "body=@${body}" >/dev/null \
    || echo "::warning::could not edit mutation comment"
else
  gh api -X POST "repos/${repo}/issues/${pr_number}/comments" -F "body=@${body}" >/dev/null \
    || echo "::warning::could not post mutation comment"
fi
exit 0
