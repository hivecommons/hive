#!/usr/bin/env bash
# Mutation report for the Go packages a PR touches (#17349).
# Runs gremlins on the lines each changed package's diff adds (#17463), then
# upserts ONE <!-- hive-mutation --> PR comment listing surviving (LIVED) and
# uncovered (NOT_COVERED) mutants in changed files.
#
# Three phases so the token never shares an environment with PR-controlled code:
#   mutation-report.sh run      runs gremlins (no GH_TOKEN) and writes
#                               $out_dir/comment.md and $out_dir/verdict.
#                               Always exits 0. A wall-clock budget
#                               (MUTATION_BUDGET_SECONDS, default 900, under the
#                               job's timeout-minutes) stops the run cleanly:
#                               packages it did not reach, or cut short, are
#                               listed in the comment instead of the job being
#                               cancelled and losing the report (#17605).
#   mutation-report.sh post     reads comment.md and upserts the PR comment
#                               (needs GH_TOKEN, PR_NUMBER, GITHUB_REPOSITORY)
#   mutation-report.sh verdict  exits 1 when $out_dir/verdict is `error`,
#                               `lived` or `misrouted`, or when the file is
#                               missing (the run phase did not finish);
#                               `uncovered` only warns (silenced by a
#                               `mutation/acknowledged` label in PR_LABELS,
#                               comma-separated). Phase 2 of #17349: a workflow
#                               that wants to block runs this phase as its own
#                               step.
#
# Verdicts, in precedence order:
#   error      gremlins exited non-zero or wrote no report for a package, so
#              there is nothing to judge. Never silently `pass` (#17349, item 1).
#   misrouted  a mutant on a line the PR added was SKIPPED, so gremlins did not
#              recognise the diff for that file. Mutants on unchanged lines are
#              always SKIPPED under --diff, so that count alone proves nothing:
#              only a SKIPPED mutant inside a range gremlins honours does. A
#              diff with no mutable operator at all (a unary `!`, a bare
#              `return`) produces zero mutants legitimately and is `pass`.
#              gremlins v0.6.0 approximates each default-context hunk as
#              start = +position + leading context, end = start + lines added
#              - 1 (internal/diff/diff.go), so a hunk whose added lines are not
#              contiguous (edits <= 6 lines apart share one hunk) only honours
#              the first `lines added` lines. SKIPPED mutants on added lines
#              outside that approximation are gremlins' own doing and are
#              reported as a non-blocking note, not as `misrouted`.
#   lived      a mutant in a changed file survived the tests.
#   uncovered  a mutant in a changed file is not reached by any test.
#   pass       everything else, including "nothing to mutate".
set -uo pipefail

phase="${1:-run}"
marker='<!-- hive-mutation -->'
out_dir="${MUTATION_OUT_DIR:-mutation-out}"
body="$out_dir/comment.md"
verdict_file="$out_dir/verdict"

has_label() {
  local want="$1" labels="${PR_LABELS:-}" l arr
  IFS=',' read -r -a arr <<< "$labels"
  for l in "${arr[@]}"; do
    [ "$(printf '%s' "$l" | sed 's/^ *//; s/ *$//')" = "$want" ] && return 0
  done
  return 1
}

verdict_phase() {
  local v
  if [ ! -f "$verdict_file" ]; then
    echo "::error::mutation: no verdict at $verdict_file; the run phase did not finish (see its log)"
    return 1
  fi
  v=$(cat "$verdict_file")
  case "$v" in
    error)
      echo "::error::mutation: gremlins failed or wrote no report for a changed package, so no verdict could be derived (see the run log and the <!-- hive-mutation --> comment)"
      return 1 ;;
    misrouted)
      echo "::error::mutation: a mutant on a line this PR added was SKIPPED; gremlins did not match the diff for that file (see the <!-- hive-mutation --> comment)"
      return 1 ;;
    lived)
      echo "::error::mutation: surviving mutant(s) in changed files; the code ran under tests but no test fails when it changes (see the <!-- hive-mutation --> comment)"
      return 1 ;;
    uncovered)
      if has_label 'mutation/acknowledged'; then
        echo "mutation: uncovered mutant(s) in changed files acknowledged via the mutation/acknowledged label."
      else
        echo "::warning::mutation: mutant(s) in changed files are not reached by any test (see the <!-- hive-mutation --> comment); a maintainer may apply mutation/acknowledged"
      fi
      return 0 ;;
    *)
      echo "mutation: $v"
      return 0 ;;
  esac
}

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
  # The report always lands in the step summary; the PR comment is reserved
  # for findings (any verdict but `pass`). A clean run never creates a
  # comment, and one that follows an earlier finding edits that comment down
  # to a one-line resolved note instead of posting again (#17863).
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    cat "$body" >> "$GITHUB_STEP_SUMMARY"
  fi
  local verdict="" resolved=""
  [ -f "$verdict_file" ] && verdict=$(cat "$verdict_file")
  if [ "$verdict" = pass ]; then
    # shellcheck disable=SC2016 # backticks are literal Markdown
    resolved="$(printf '%s\n### Mutation testing\n\n✅ Resolved: no surviving or uncovered mutants in changed files on the current head.\n\n_Verdict: `pass` (#17349)._\n' "$marker")"
  fi
  # Only upsert the bot's own report so another participant cannot pre-seat or
  # get overwritten by a comment that merely starts with the marker.
  local id
  id=$(gh api --paginate "repos/${repo}/issues/${pr_number}/comments" \
    --jq ".[] | select(.user.login == \"github-actions[bot]\" and (.body | startswith(\"${marker}\"))) | .id" 2>/dev/null | head -n 1 || true)
  if [ -n "$resolved" ]; then
    if [ -z "$id" ]; then
      echo "mutation: verdict pass and no earlier comment; not posting (report is in the step summary)."
      return 0
    fi
    gh api -X PATCH "repos/${repo}/issues/comments/${id}" -f "body=${resolved}" >/dev/null \
      || echo "::warning::could not edit mutation comment"
    return 0
  fi
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
  verdict) verdict_phase; exit $? ;;
  run) ;;
  *) echo "usage: $0 [run|post|verdict]" >&2; exit 0 ;;
esac

if [ -n "${GH_TOKEN:-}${GITHUB_TOKEN:-}" ]; then
  echo "::warning::GH_TOKEN/GITHUB_TOKEN is set during the mutation run; the token must only be given to the post phase"
fi

base_ref="${BASE_REF:?BASE_REF is required}"
coefficient="${MUTATION_TIMEOUT_COEFFICIENT:-3}"
budget="${MUTATION_BUDGET_SECONDS:-900}"
case "$budget" in ''|*[!0-9]*) budget=900 ;; esac
run_start=$SECONDS
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
  echo pass > "$verdict_file"
  exit 0
fi

printf '%s\n' "$changed" > "$out_dir/changed.txt"

# Added-line ranges per changed file (file<TAB>start<TAB>end), from the same
# merge-base diff gremlins reads: added.tsv holds the exact -U0 added lines;
# gremlins.tsv holds the range gremlins v0.6.0 actually honours per
# default-context hunk (+position + leading context, for `lines added` lines).
# A SKIPPED mutant inside a gremlins.tsv range means gremlins did not recognise
# the file as changed; one inside added.tsv only is its hunk approximation.
added="$out_dir/added.tsv"
honoured="$out_dir/gremlins.tsv"
: > "$added"
: > "$honoured"
while IFS= read -r f; do
  [ -n "$f" ] || continue
  git diff -U0 "origin/${base_ref}...HEAD" -- "$f" \
    | awk -v f="$f" '/^@@ / { split($3, p, ","); s = substr(p[1], 2) + 0; n = (p[2] == "" ? 1 : p[2] + 0); if (n > 0) printf "%s\t%d\t%d\n", f, s, s + n - 1 }' >> "$added"
  # No -U: gremlins runs `git diff --merge-base` with the default context.
  git diff "origin/${base_ref}...HEAD" -- "$f" \
    | awk -v f="$f" '
      function flush() { if (hunk && add > 0) printf "%s\t%d\t%d\n", f, start + lead, start + lead + add - 1 }
      /^@@ / { flush(); split($3, p, ","); start = substr(p[1], 2) + 0; lead = 0; add = 0; seen = 0; hunk = 1; next }
      !hunk { next }
      /^ / { if (!seen) lead++; next }
      /^\+/ { seen = 1; add++; next }
      /^-/ { seen = 1; next }
      END { flush() }' >> "$honoured"
done <<< "$changed"
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
errors="$out_dir/errors.tsv"
budgeted="$out_dir/budget.tsv"
: > "$errors"
: > "$budgeted"
i=0
while IFS= read -r pkg; do
  [ -n "$pkg" ] || continue
  i=$((i + 1))
  printf '%s\n' "$pkg" > "$out_dir/pkg-$i.dir"
  remaining=$((budget - (SECONDS - run_start)))
  if [ "$remaining" -le 0 ]; then
    echo "::warning::mutation budget of ${budget}s exhausted; skipping $pkg"
    printf '%s\tskipped (not started)\n' "$pkg" >> "$budgeted"
    continue
  fi
  echo "::group::gremlins unleash ./$pkg --diff origin/${base_ref}"
  rc=0
  (cd "src/$pkg" && GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=diff.relative GIT_CONFIG_VALUE_0=true \
    timeout --kill-after=10s "${remaining}s" \
    gremlins unleash --timeout-coefficient "$coefficient" --diff "origin/${base_ref}" \
    --output "$out_abs/pkg-$i.json") || rc=$?
  echo "::endgroup::"
  # gremlins writes no report when it errors or is cancelled, so either sign
  # means this package was not judged: record it rather than pass by default.
  if [ "$rc" -eq 124 ]; then
    echo "::warning::mutation budget of ${budget}s exhausted while running $pkg"
    printf '%s\tcut short (partial, no report)\n' "$pkg" >> "$budgeted"
  elif [ "$rc" -ne 0 ]; then
    echo "::error::gremlins exited $rc for $pkg"
    printf '%s\texited %d\n' "$pkg" "$rc" >> "$errors"
  elif [ ! -s "$out_abs/pkg-$i.json" ]; then
    echo "::error::gremlins wrote no report for $pkg"
    printf '%s\twrote no report\n' "$pkg" >> "$errors"
  fi
done <<< "$pkgs"

# gremlins reports file_name relative to the package dir it was run on, so
# re-anchor every mutant to a repo-relative path before matching changed.txt.
# Only mutants in files the PR changed are kept: file<TAB>line<TAB>type<TAB>status.
mutants="$out_dir/mutants-changed.tsv"
: > "$mutants"
for j in "$out_dir"/pkg-*.json; do
  [ -f "$j" ] || continue
  pkg=$(cat "${j%.json}.dir" 2>/dev/null || true)
  jq -r --arg prefix "src/${pkg:+$pkg/}" \
     '.files[]? | .file_name as $f | .mutations[]?
      | [($prefix + ($f | ltrimstr("./"))), .line, .type, .status] | @tsv' "$j" 2>/dev/null \
  | while IFS=$'\t' read -r f line type status; do
      [ -n "$f" ] || continue
      grep -qxF "$f" "$out_dir/changed.txt" && printf '%s\t%s\t%s\t%s\n' "$f" "$line" "$type" "$status"
    done >> "$mutants"
done

filtered="$out_dir/survivors-changed.tsv"
uncovered="$out_dir/uncovered-changed.tsv"
misrouted="$out_dir/misrouted-changed.tsv"
unhonoured="$out_dir/unhonoured-changed.tsv"
awk -F'\t' '$4=="LIVED" {print $1"\t"$2"\t"$3}' "$mutants" > "$filtered"
awk -F'\t' '$4=="NOT COVERED" {print $1"\t"$2"\t"$3}' "$mutants" > "$uncovered"
# in_ranges <ranges.tsv> <mutants.tsv>: SKIPPED mutants whose line falls in a range.
in_ranges() {
  awk -F'\t' 'NR==FNR { if ($1 != "") { n[$1]++; s[$1,n[$1]]=$2; e[$1,n[$1]]=$3 } ; next }
    $4=="SKIPPED" { for (k=1; k<=n[$1]; k++) if ($2+0 >= s[$1,k]+0 && $2+0 <= e[$1,k]+0) { print $1"\t"$2"\t"$3; break } }' "$1" "$2"
}
# SKIPPED inside a range gremlins honours: it saw the file but not its diff.
in_ranges "$honoured" "$mutants" > "$misrouted"
# SKIPPED on an added line gremlins' hunk approximation leaves out: advisory.
in_ranges "$added" "$mutants" | grep -vxF -f "$misrouted" > "$unhonoured" || true
mutated=$(awk -F'\t' '$4=="LIVED" || $4=="KILLED" || $4=="NOT COVERED" || $4=="TIMED OUT" {c++} END {print c+0}' "$mutants")
skipped=$(awk -F'\t' '$4=="SKIPPED" {c++} END {print c+0}' "$mutants")
added_lines=$(awk -F'\t' '{c += $3 - $2 + 1} END {print c+0}' "$added")

if [ -s "$errors" ]; then verdict=error
elif [ -s "$misrouted" ]; then verdict=misrouted
elif [ -s "$filtered" ]; then verdict=lived
elif [ -s "$uncovered" ]; then verdict=uncovered
else verdict=pass
fi
echo "$verdict" > "$verdict_file"

table() { sort -t$'\t' -k1,1 -k2,2n "$1" | head -n 100 | awk -F'\t' '{printf "| `%s` | %s | %s |\n", $1, $2, $3}'; }

{
  echo "$marker"
  echo "### Mutation testing"
  echo
  echo "Head \`${head_sha}\`."
  echo
  if [ -s "$budgeted" ]; then
    echo "⏱️ **The ${budget}s mutation budget ran out**, so $(wc -l < "$budgeted" | tr -d ' ') package(s) were not fully mutated and the results below do not cover them:"
    echo
    awk -F'\t' '{printf "- `%s`: %s\n", $1, $2}' "$budgeted"
    echo
  fi
  if [ -s "$errors" ]; then
    echo "❌ **gremlins did not produce a report for $(wc -l < "$errors" | tr -d ' ') package(s)**, so no verdict can be derived from this run (see the job log):"
    echo
    awk -F'\t' '{printf "- `%s`: gremlins %s\n", $1, $2}' "$errors"
    echo
  fi
  if [ -s "$misrouted" ]; then
    echo "⚠️ **$(wc -l < "$misrouted" | tr -d ' ') mutant(s) on lines this PR added were SKIPPED**: gremlins did not recognise the diff for these files, so nothing below can be trusted. This is a tooling fault (diff path matching), not a test gap."
    echo
    echo "| File | Line | Mutation |"
    echo "|---|---|---|"
    table "$misrouted"
    echo
  fi
  if [ -s "$filtered" ]; then
    echo "gremlins found **$(wc -l < "$filtered" | tr -d ' ') surviving mutant(s)** in changed files: the code ran under tests, but changing it did not fail any test."
    echo
    echo "| File | Line | Mutation |"
    echo "|---|---|---|"
    table "$filtered"
    echo
  fi
  if [ -s "$uncovered" ]; then
    echo "**$(wc -l < "$uncovered" | tr -d ' ') mutant(s) in changed files are not reached by any test** (NOT COVERED). A maintainer may apply the \`mutation/acknowledged\` label to accept this."
    echo
    echo "| File | Line | Mutation |"
    echo "|---|---|---|"
    table "$uncovered"
    echo
  fi
  if [ -s "$unhonoured" ]; then
    echo "ℹ️ $(wc -l < "$unhonoured" | tr -d ' ') mutant(s) on added lines were SKIPPED because gremlins v0.6.0 only honours the first \`lines added\` lines of each default-context hunk, so non-contiguous edits in one hunk are partly missed. This does not affect the verdict."
    echo
    echo "| File | Line | Mutation |"
    echo "|---|---|---|"
    table "$unhonoured"
    echo
  fi
  if [ -s "$errors" ]; then
    :
  elif [ "$mutated" -eq 0 ] && [ ! -s "$misrouted" ]; then
    echo "No mutants in changed files: the $added_lines added line(s) contain no operator gremlins can mutate ($skipped mutant(s) on unchanged lines skipped as expected)."
  elif [ ! -s "$filtered" ] && [ ! -s "$uncovered" ] && [ ! -s "$misrouted" ]; then
    echo "All $mutated mutant(s) in changed files were killed."
  fi
  echo
  echo "_Verdict: \`$verdict\` (#17349)._"
} > "$body"
cat "$body"
exit 0
