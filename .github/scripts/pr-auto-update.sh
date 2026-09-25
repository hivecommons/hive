#!/usr/bin/env bash
set -euo pipefail

REPO="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
BASE_BRANCH="${GITHUB_REF_NAME:?GITHUB_REF_NAME is required}"
BEFORE="${GITHUB_EVENT_BEFORE:-${GITHUB_SHA_BEFORE:-}}"
AFTER="${GITHUB_SHA:?GITHUB_SHA is required}"
ZERO_SHA='0000000000000000000000000000000000000000'

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required command: $1" >&2; exit 1; }; }
need gh
need git
need jq

api() { gh api "$@"; }
summary_rows=''

if [[ -z "$BEFORE" || "$BEFORE" == "$ZERO_SHA" ]]; then
  msg="Skipping auto-update for ${BASE_BRANCH}: push has no comparable before SHA."
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then printf '%s\n' "$msg" >> "$GITHUB_STEP_SUMMARY"; else printf '%s\n' "$msg"; fi
  exit 0
fi

git fetch --no-tags origin "+refs/heads/${BASE_BRANCH}:refs/remotes/origin/${BASE_BRANCH}" >/dev/null
mapfile -t push_files < <(git diff --name-only "$BEFORE" "$AFTER" | sort -u)
if ((${#push_files[@]} == 0)); then
  msg="No changed files in push ${BEFORE}..${AFTER}; no PR branches updated."
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then printf '%s\n' "$msg" >> "$GITHUB_STEP_SUMMARY"; else printf '%s\n' "$msg"; fi
  exit 0
fi

intersects_push() {
  local pr=$1 file
  while IFS= read -r file; do
    for changed in "${push_files[@]}"; do
      [[ "$file" == "$changed" ]] && return 0
    done
  done < <(api "repos/${REPO}/pulls/${pr}/files?per_page=100" --paginate --jq '.[].filename' | sort -u)
  return 1
}

pr_mergeable_state() {
  local pr=$1 mergeable state
  for _attempt in 1 2 3; do
    IFS=$'\t' read -r mergeable state < <(api "repos/${REPO}/pulls/${pr}" --jq '[.mergeable, .mergeable_state] | @tsv')
    if [[ "$mergeable" != 'null' && -n "$mergeable" ]]; then
      printf '%s\n' "$state"
      return 0
    fi
    sleep 5
  done
  printf '%s\n' "$state"
}

add_row() {
  local pr=$1 title=$2 status=$3 note=$4
  title=${title//'|'/'\|'}
  note=${note//'|'/'\|'}
  summary_rows+="| #${pr} | ${title} | ${status} | ${note} |"$'\n'
}

while IFS=$'\t' read -r number title _head_repo; do
  [[ -n "${number:-}" ]] || continue
  if ! intersects_push "$number"; then
    add_row "$number" "$title" 'skipped' 'no changed-file intersection'
    continue
  fi
  state=$(pr_mergeable_state "$number")
  if [[ "$state" != 'behind' ]]; then
    add_row "$number" "$title" 'skipped' "mergeable_state=${state}"
    continue
  fi
  set +e
  out=$(api -X PUT "repos/${REPO}/pulls/${number}/update-branch" 2>&1)
  status=$?
  set -e
  if [[ $status -eq 0 ]]; then
    add_row "$number" "$title" 'updated' 'update-branch requested'
  elif grep -q 'HTTP 422' <<<"$out"; then
    add_row "$number" "$title" 'skipped' 'update-branch reported conflict (422)'
  elif grep -q 'HTTP 403' <<<"$out"; then
    add_row "$number" "$title" 'skipped' 'no permission to update head branch (403)'
  else
    printf '%s\n' "$out" >&2
    exit "$status"
  fi
done < <(api "repos/${REPO}/pulls?base=${BASE_BRANCH}&state=open&per_page=100" --paginate \
  --jq '.[] | [.number, (.title | gsub("[\t\r\n]"; " ")), .head.repo.full_name] | @tsv')

{
  printf '### Post-merge PR auto-update\n\n'
  printf '%s%s%s\n\n' 'Base branch: `' "$BASE_BRANCH" '`'
  printf 'Changed files in push: %d\n\n' "${#push_files[@]}"
  if [[ -z "$summary_rows" ]]; then
    printf 'No open PRs target this branch.\n'
  else
    printf '| PR | Title | Status | Notes |\n'
    printf '| --- | --- | --- | --- |\n'
    printf '%s' "$summary_rows"
  fi
} | { if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then cat >> "$GITHUB_STEP_SUMMARY"; else cat; fi; }
