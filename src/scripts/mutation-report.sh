#!/usr/bin/env bash
# Advisory mutation report for the Go packages a PR touches (#17349, Phase 1).
# Runs gremlins on the lines each changed package's diff adds (#17463), then
# upserts ONE <!-- hive-mutation --> PR comment listing surviving (LIVED)
# mutants in changed files. Never fails.
#
# The run phase keeps its own time budget (MUTATION_BUDGET_SECONDS, default
# 17 min) under the job's timeout-minutes so comment.md is always written: a
# job killed by the runner writes nothing, and the PR keeps the comment from
# an earlier head (or none) with no sign the report is stale or partial.
#
# Two phases so the token never shares an environment with PR-controlled code:
#   mutation-report.sh run    runs gremlins (no GH_TOKEN) and writes
#                             $out_dir/comment.md
#   mutation-report.sh post   reads comment.md and upserts the PR comment
#                             (needs GH_TOKEN, PR_NUMBER, GITHUB_REPOSITORY)
set -uo pipefail

phase="${1:-run}"
marker='<!-- hive-mutation -->'
out_dir="${MUTATION_OUT_DIR:-mutation-out}"
body="$out_dir/comment.md"

post_comment() {
  local pr_number="${PR_NUMBER:-}" repo="${GITHUB_REPOSITORY:-}"
  if [ ! -s "$body" ]; then
    echo "No mutation report at $body; nothing to post."
    return 0
  fi
  if [ -z "$pr_number" ] || [ -z "$repo" ]; then
    echo "PR_NUMBER/GITHUB_REPOSITORY unset; not posting a comment."
    return 0
  fi
  # Only upsert the bot's own report so another participant cannot pre-seat or
  # get overwritten by a comment that merely starts with the marker.
  local id
  id=$(gh api --paginate "repos/${repo}/issues/${pr_number}/comments" \
    --jq ".[] | select(.user.login == \"github-actions[bot]\" and (.body | startswith(\"${marker}\"))) | .id" 2>/dev/null | head -n 1 || true)
  if [ -n "$id" ]; then
    gh api -X PATCH "repos/${repo}/issues/comments/${id}" -F "body=@${body}" >/dev/null \
      || echo "::warning::could not edit mutation comment"
  else
    gh api -X POST "repos/${repo}/issues/${pr_number}/comments" -F "body=@${body}" >/dev/null \
      || echo "::warning::could not post mutation comment"
  fi
}

case "$phase" in
  post) post_comment; exit 0 ;;
  run) ;;
  *) echo "usage: $0 [run|post]" >&2; exit 0 ;;
esac

if [ -n "${GH_TOKEN:-}${GITHUB_TOKEN:-}" ]; then
  echo "::warning::GH_TOKEN/GITHUB_TOKEN is set during the mutation run; the token must only be given to the post phase"
fi

base_ref="${BASE_REF:?BASE_REF is required}"
coefficient="${MUTATION_TIMEOUT_COEFFICIENT:-3}"
budget="${MUTATION_BUDGET_SECONDS:-1020}"
deadline=$(( $(date +%s) + budget ))
mkdir -p "$out_dir"
out_abs=$(cd "$out_dir" && pwd)
# On a pull_request checkout HEAD is the synthetic merge commit; HEAD^2 is the
# PR head the comment should name so a reader can tell a stale report apart.
head_sha=$(git rev-parse --short HEAD^2 2>/dev/null || git rev-parse --short HEAD)

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

# gremlins mutates the whole subtree under a package, so drop packages that
# have an ancestor already in the list to avoid mutating them twice.
roots=""
while IFS= read -r pkg; do
  [ -n "$pkg" ] || continue
  covered=0
  while IFS= read -r r; do
    [ -n "$r" ] || continue
    case "$pkg" in "$r"/*) covered=1; break ;; esac
  done <<< "$roots"
  [ "$covered" -eq 1 ] || roots+="$pkg"$'\n'
done <<< "$pkgs"
pkgs="${roots%$'\n'}"

# gremlins --diff reads `git diff --merge-base <ref>` from its own working
# directory and matches those paths against files relative to the package it
# mutates. Run it from inside the package with diff.relative so both sides use
# package-relative paths; otherwise every mutant would be skipped, or (with no
# --diff) the whole package is mutated and the job times out (#17463). Coverage
# chdirs to the module root, so the output path must be absolute.
# Each package gets whatever is left of the budget; one that outlives it is
# killed (exit 124) and recorded as unfinished, and the packages after it are
# recorded as not started, so the comment says what was and was not mutated.
unfinished="$out_dir/unfinished.txt"
: > "$unfinished"
i=0
while IFS= read -r pkg; do
  [ -n "$pkg" ] || continue
  i=$((i + 1))
  printf '%s\n' "$pkg" > "$out_dir/pkg-$i.dir"
  remaining=$(( deadline - $(date +%s) ))
  if [ "$remaining" -le 0 ]; then
    echo "time budget exhausted before $pkg; not started"
    printf '%s\tnot started\n' "$pkg" >> "$unfinished"
    continue
  fi
  echo "::group::gremlins unleash ./$pkg --diff origin/${base_ref} (budget ${remaining}s)"
  (cd "src/$pkg" && GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=diff.relative GIT_CONFIG_VALUE_0=true \
    timeout --kill-after=10 "$remaining" \
    gremlins unleash --timeout-coefficient "$coefficient" --diff "origin/${base_ref}" \
    --output "$out_abs/pkg-$i.json")
  rc=$?
  if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
    echo "gremlins hit the time budget in $pkg after ${remaining}s; results for it are partial or missing"
    printf '%s\tunfinished\n' "$pkg" >> "$unfinished"
  elif [ "$rc" -ne 0 ]; then
    echo "gremlins exited non-zero ($rc) for $pkg (advisory)"
  fi
  echo "::endgroup::"
done <<< "$pkgs"

# gremlins reports file_name relative to the package dir it was run on, so
# re-anchor each survivor to a repo-relative path before matching changed.txt.
survivors="$out_dir/survivors.tsv"
: > "$survivors"
for j in "$out_dir"/pkg-*.json; do
  [ -f "$j" ] || continue
  pkg=$(cat "${j%.json}.dir" 2>/dev/null || true)
  jq -r --arg prefix "src/${pkg:+$pkg/}" \
     '.files[]? | .file_name as $f | .mutations[]? | select(.status=="LIVED")
      | [($prefix + ($f | ltrimstr("./"))), .line, .type] | @tsv' "$j" >> "$survivors" 2>/dev/null || true
done

# Keep only survivors in files the PR changed.
filtered="$out_dir/survivors-changed.tsv"
: > "$filtered"
while IFS=$'\t' read -r f line type; do
  [ -n "$f" ] || continue
  if grep -qxF "$f" "$out_dir/changed.txt"; then
    printf '%s\t%s\t%s\n' "$f" "$line" "$type" >> "$filtered"
  fi
done < "$survivors"

{
  echo "$marker"
  echo "### Mutation testing (advisory)"
  echo
  echo "Head \`${head_sha}\`."
  echo
  if [ -s "$filtered" ]; then
    echo "gremlins found **$(wc -l < "$filtered" | tr -d ' ') surviving mutant(s)** in changed files: the code ran under tests, but changing it did not fail any test."
    echo
    echo "| File | Line | Mutation |"
    echo "|---|---|---|"
    sort -t$'\t' -k1,1 -k2,2n "$filtered" | head -n 100 | awk -F'\t' '{printf "| `%s` | %s | %s |\n", $1, $2, $3}'
  elif [ -s "$unfinished" ]; then
    echo "No surviving mutants reported in the packages that finished."
  else
    echo "No surviving mutants in changed files."
  fi
  if [ -s "$unfinished" ]; then
    echo
    echo "⏱ **Time budget (${budget}s) exhausted: $(wc -l < "$unfinished" | tr -d ' ') of ${i} changed package(s) were not fully mutated**, so this report is partial:"
    echo
    awk -F'\t' '{printf "- `src/%s` — %s\n", $1, $2}' "$unfinished"
  fi
  echo
  echo "_Advisory only; this job never blocks the PR (#17349)._"
} > "$body"
cat "$body"
exit 0
