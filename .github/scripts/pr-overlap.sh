#!/usr/bin/env bash
set -euo pipefail

MARKER='<!-- hive-pr-overlap -->'
LABEL='conflicts-with-open-pr'
LABEL_COLOR='d93f0b'
MAX_OPEN_PRS=60
DRY_RUN=0
TARGET_PR=''
REPO="${GITHUB_REPOSITORY:-hivecommons/hive}"
EVENT_NAME="${GITHUB_EVENT_NAME:-}"
BASE_BRANCH="${GITHUB_BASE_REF:-${GITHUB_REF_NAME:-}}"

usage() {
  cat <<USAGE
Usage: $0 [--dry-run] [--repo owner/repo] [--pr number] [--base branch]
USAGE
}

while (($#)); do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --repo) REPO="$2"; shift 2 ;;
    --pr) TARGET_PR="$2"; EVENT_NAME='pull_request'; shift 2 ;;
    --base) BASE_BRANCH="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required command: $1" >&2; exit 1; }; }
need gh
need git
need jq

if [[ -z "$EVENT_NAME" ]]; then
  EVENT_NAME='pull_request'
fi

api() { gh api "$@"; }

ensure_base() {
  local base=$1
  git fetch --no-tags origin "+refs/heads/${base}:refs/remotes/origin/${base}" >/dev/null
}

fetch_pr_ref() {
  local number=$1
  if ! git show-ref --verify --quiet "refs/heads/pr-${number}"; then
    git fetch --no-tags origin "+pull/${number}/head:refs/heads/pr-${number}" >/dev/null
  else
    git fetch --no-tags origin "+pull/${number}/head:refs/heads/pr-${number}" >/dev/null
  fi
}

pr_field() {
  local number=$1 field=$2
  api "repos/${REPO}/pulls/${number}" --jq ".${field}"
}

list_open_prs() {
  local base=$1
  api "repos/${REPO}/pulls?base=${base}&state=open&sort=updated&direction=desc&per_page=100" --paginate \
    --jq '.[] | [.number, (.title | gsub("[\t\r\n]"; " ")), .head.repo.full_name, .updated_at] | @tsv'
}

changed_files() {
  local base=$1 ref=$2
  local mb
  mb=$(git merge-base "origin/${base}" "$ref")
  git diff --name-only "${mb}...${ref}" | sort -u
}

join_files() {
  local sep=${1:-'<br>'}; shift || true
  local out='' f
  for f in "$@"; do
    [[ -n "$f" ]] || continue
    f=${f//'|'/'\|'}
    if [[ -z "$out" ]]; then out="\`${f}\`"; else out+="${sep}\`${f}\`"; fi
  done
  [[ -n "$out" ]] && printf '%s' "$out" || printf '—'
}

intersect_files() {
  local -n left=$1
  local -n right=$2
  local f g
  for f in "${left[@]}"; do
    for g in "${right[@]}"; do
      [[ "$f" == "$g" ]] && printf '%s\n' "$f"
    done
  done | sort -u
}

merge_conflicts() {
  local a=$1 b=$2 out status
  set +e
  out=$(git merge-tree --write-tree --name-only "$a" "$b" 2>&1)
  status=$?
  set -e
  if [[ $status -eq 1 ]]; then
    local line path
    while IFS= read -r line; do
      [[ -n "$line" ]] || continue
      [[ "$line" =~ ^[0-9a-f]{40}$ ]] && continue
      path=$line
      if [[ "$path" == Auto-merging[[:space:]]* ]]; then
        path=${path#Auto-merging }
      elif [[ "$path" == CONFLICT*' in '* ]]; then
        path=${path##* in }
      fi
      if git cat-file -e "${a}:${path}" 2>/dev/null || git cat-file -e "${b}:${path}" 2>/dev/null; then
        printf '%s\n' "$path"
      fi
    done <<<"$out" | sort -u
  elif [[ $status -ne 0 ]]; then
    printf '%s\n' "$out" >&2
    return "$status"
  fi
}

make_report_for_pr() {
  local target=$1 base=$2 rows_var=$3 has_conflicts_var=$4 has_any_var=$5
  local target_ref="pr-${target}"
  fetch_pr_ref "$target"
  # shellcheck disable=SC2034 # consumed by intersect_files via nameref
  mapfile -t target_files < <(changed_files "$base" "$target_ref")

  local report_rows='' report_has_conflicts=0 report_has_any=0
  local number title other_ref relation files_md
  while IFS=$'\t' read -r number title _head_repo _updated; do
    [[ -n "${number:-}" ]] || continue
    [[ "$number" == "$target" ]] && continue
    if [[ -n "${CHECK_PR_SET:-}" && "${CHECK_PR_SET}" != *" ${number} "* ]]; then
      continue
    fi
    other_ref="pr-${number}"
    fetch_pr_ref "$number"
    mapfile -t conflict_files < <(merge_conflicts "$target_ref" "$other_ref")
    if ((${#conflict_files[@]})); then
      relation='CONFLICT'
      report_has_conflicts=1
      report_has_any=1
      files_md=$(join_files '<br>' "${conflict_files[@]}")
      report_rows+="| #${number} | ${title//'|'/'\|'} | ${relation} | ${files_md} |"$'\n'
      continue
    fi
    # shellcheck disable=SC2034 # consumed by intersect_files via nameref
    mapfile -t other_files < <(changed_files "$base" "$other_ref")
    mapfile -t overlap_files < <(intersect_files target_files other_files)
    if ((${#overlap_files[@]})); then
      relation='same files'
      report_has_any=1
      files_md=$(join_files '<br>' "${overlap_files[@]}")
      report_rows+="| #${number} | ${title//'|'/'\|'} | ${relation} | ${files_md} |"$'\n'
    fi
  done < <(list_open_prs "$base")

  printf -v "$rows_var" '%s' "$report_rows"
  printf -v "$has_conflicts_var" '%s' "$report_has_conflicts"
  printf -v "$has_any_var" '%s' "$report_has_any"
}

report_body() {
  local pr=$1 base=$2 rows=$3 truncated=${4:-0}
  {
    printf '%s\n\n' "$MARKER"
    printf '### Open PR overlap check\n\n'
    printf '%s%s%s\n\n' 'Base branch: `' "$base" '`'
    if [[ "$truncated" == '1' ]]; then
      printf '> More than %d open PRs target %s%s%s; checked only the %d most recently updated.\n\n' "$MAX_OPEN_PRS" '`' "$base" '`' "$MAX_OPEN_PRS"
    fi
    if [[ -z "$rows" ]]; then
      printf 'No conflicting open PRs or same-file overlaps were found.\n'
    else
      printf '| PR | Title | Relation | Files |\n'
      printf '| --- | --- | --- | --- |\n'
      printf '%s' "$rows"
    fi
  }
}

upsert_comment_and_label() {
  local pr=$1 body=$2 has_conflicts=$3
  if ((DRY_RUN)); then
    printf 'DRY RUN: would update PR #%s comment and label=%s\n' "$pr" "$has_conflicts"
    printf '%s\n' "$body"
    return 0
  fi

  local comment_id
  comment_id=$(api "repos/${REPO}/issues/${pr}/comments?per_page=100" --paginate \
    --jq ".[] | select(.body | contains(\"${MARKER}\")) | .id" | head -n1)
  if [[ -n "$comment_id" ]]; then
    api -X PATCH "repos/${REPO}/issues/comments/${comment_id}" -f body="$body" >/dev/null
  else
    api -X POST "repos/${REPO}/issues/${pr}/comments" -f body="$body" >/dev/null
  fi

  if [[ "$has_conflicts" == '1' ]]; then
    api -X POST "repos/${REPO}/labels" -f name="$LABEL" -f color="$LABEL_COLOR" -f description='Open PR has a merge conflict with another open PR' >/dev/null 2>&1 || true
    api -X POST "repos/${REPO}/issues/${pr}/labels" -f labels[]="$LABEL" >/dev/null
  else
    api -X DELETE "repos/${REPO}/issues/${pr}/labels/${LABEL}" >/dev/null 2>&1 || true
  fi
}

write_summary() {
  local text=$1
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    printf '%s\n' "$text" >> "$GITHUB_STEP_SUMMARY"
  else
    printf '%s\n' "$text"
  fi
}

run_for_one_pr() {
  local pr=$1 base=$2 writable=$3 truncated=${4:-0}
  local rows has_conflicts _has_any body
  make_report_for_pr "$pr" "$base" rows has_conflicts _has_any
  body=$(report_body "$pr" "$base" "$rows" "$truncated")
  write_summary "$body"
  if [[ "$writable" == '1' ]]; then
    upsert_comment_and_label "$pr" "$body" "$has_conflicts"
  fi
}

if [[ "$EVENT_NAME" == 'pull_request' ]]; then
  TARGET_PR="${TARGET_PR:-${GITHUB_EVENT_PULL_REQUEST_NUMBER:-}}"
  if [[ -z "$TARGET_PR" && -n "${GITHUB_EVENT_PATH:-}" && -f "${GITHUB_EVENT_PATH}" ]]; then
    TARGET_PR=$(jq -r '.pull_request.number' "$GITHUB_EVENT_PATH")
    BASE_BRANCH=${BASE_BRANCH:-$(jq -r '.pull_request.base.ref' "$GITHUB_EVENT_PATH")}
  fi
  if [[ -z "$TARGET_PR" ]]; then
    echo 'pull_request event requires a PR number' >&2
    exit 1
  fi
  BASE_BRANCH=${BASE_BRANCH:-$(pr_field "$TARGET_PR" 'base.ref')}
  ensure_base "$BASE_BRANCH"
  head_repo=$(pr_field "$TARGET_PR" 'head.repo.full_name')
  writable=0
  if [[ "$head_repo" == "$REPO" && $DRY_RUN -eq 0 ]]; then
    writable=1
  fi
  run_for_one_pr "$TARGET_PR" "$BASE_BRANCH" "$writable"
  exit 0
fi

if [[ "$EVENT_NAME" == 'push' ]]; then
  BASE_BRANCH=${BASE_BRANCH:-${GITHUB_REF_NAME:-}}
  if [[ -z "$BASE_BRANCH" ]]; then
    echo 'push event requires GITHUB_REF_NAME or --base' >&2
    exit 1
  fi
  ensure_base "$BASE_BRANCH"
  mapfile -t all_prs < <(list_open_prs "$BASE_BRANCH")
  truncated=0
  if ((${#all_prs[@]} > MAX_OPEN_PRS)); then
    truncated=1
    all_prs=("${all_prs[@]:0:MAX_OPEN_PRS}")
    write_summary "Checked only the ${MAX_OPEN_PRS} most recently updated open PRs for ${BASE_BRANCH}."
  fi
  CHECK_PR_SET=' '
  for line in "${all_prs[@]}"; do
    IFS=$'\t' read -r number _ <<<"$line"
    CHECK_PR_SET+="${number} "
  done
  export CHECK_PR_SET
  for line in "${all_prs[@]}"; do
    IFS=$'\t' read -r number _ <<<"$line"
    run_for_one_pr "$number" "$BASE_BRANCH" '1' "$truncated"
  done
  exit 0
fi

echo "event ${EVENT_NAME} is not handled" >&2
exit 1
