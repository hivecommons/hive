#!/usr/bin/env bash
# ci-flake-tally.sh — rank tests and packages that have failed in recent CI runs.
#
# Usage:
#   src/scripts/ci-flake-tally.sh [--repo owner/name] [--workflow file.yml]
#     [--branch name] [-n limit] [--include-cancelled] [--json]
#     [--min-count n] [--keep-logs]
#
# The script scans recent GitHub Actions runs, downloads failed job logs, extracts
# Go test failure markers, and aggregates the distinct workflow runs in which each
# test/package failed. Jobs with no `--- FAIL:` test marker are reported as
# build/infra failures instead of flake candidates.
set -euo pipefail

unset GITHUB_TOKEN

DEFAULT_REPO="hivecommons/hive"
DEFAULT_WORKFLOW="v2-tests.yml"
DEFAULT_BRANCH="v5"
DEFAULT_LIMIT="80"
DEFAULT_MIN_COUNT="1"

repo="$DEFAULT_REPO"
workflow="$DEFAULT_WORKFLOW"
branch="$DEFAULT_BRANCH"
limit="$DEFAULT_LIMIT"
min_count="$DEFAULT_MIN_COUNT"
include_cancelled=0
json_output=0
keep_logs=0

usage() {
  sed -n '2,16p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

fail_usage() {
  echo "ERROR: $*" >&2
  echo >&2
  usage >&2
  exit 2
}

need_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "ERROR: required command not found: $1" >&2
    exit 2
  fi
}

is_positive_integer() {
  [[ "$1" =~ ^[1-9][0-9]*$ ]]
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)
      [[ $# -ge 2 ]] || fail_usage "--repo requires owner/name"
      repo="$2"
      shift 2
      ;;
    --workflow)
      [[ $# -ge 2 ]] || fail_usage "--workflow requires a workflow file or name"
      workflow="$2"
      shift 2
      ;;
    --branch)
      [[ $# -ge 2 ]] || fail_usage "--branch requires a branch name"
      branch="$2"
      shift 2
      ;;
    -n|--limit)
      [[ $# -ge 2 ]] || fail_usage "$1 requires a positive integer"
      limit="$2"
      shift 2
      ;;
    --include-cancelled)
      include_cancelled=1
      shift
      ;;
    --json)
      json_output=1
      shift
      ;;
    --min-count)
      [[ $# -ge 2 ]] || fail_usage "--min-count requires a positive integer"
      min_count="$2"
      shift 2
      ;;
    --keep-logs)
      keep_logs=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail_usage "unknown argument: $1"
      ;;
  esac
done

is_positive_integer "$limit" || fail_usage "--limit must be a positive integer"
is_positive_integer "$min_count" || fail_usage "--min-count must be a positive integer"

need_command gh
need_command jq

cache_parent="${HIVE_CI_FLAKE_CACHE_PARENT:-${PWD}/.ci-flake-tally-cache}"
mkdir -p "$cache_parent"
log_dir="$(TMPDIR="$cache_parent" mktemp -d "${cache_parent%/}/run.XXXXXX")"
cleanup() {
  if [[ "$keep_logs" -eq 0 ]]; then
    rm -rf "$log_dir"
  else
    echo "kept logs: $log_dir" >&2
  fi
}
trap cleanup EXIT

runs_json="$log_dir/runs.json"
records_tsv="$log_dir/records.tsv"
infra_tsv="$log_dir/infra.tsv"
: > "$records_tsv"
: > "$infra_tsv"

if ! gh run list \
  --repo "$repo" \
  --workflow "$workflow" \
  --branch "$branch" \
  -L "$limit" \
  --json databaseId,conclusion,createdAt,headSha,event \
  > "$runs_json"; then
  echo "ERROR: failed to list workflow runs for ${repo}/${workflow} on ${branch}" >&2
  exit 1
fi

runs_scanned="$(jq 'length' "$runs_json")"
success_count="$(jq '[.[] | select(.conclusion == "success")] | length' "$runs_json")"
failure_count="$(jq '[.[] | select(.conclusion == "failure")] | length' "$runs_json")"
cancelled_count="$(jq '[.[] | select(.conclusion == "cancelled" or .conclusion == "timed_out")] | length' "$runs_json")"
failed_runs_total="$failure_count"
if [[ "$include_cancelled" -eq 1 ]]; then
  failed_runs_total="$(jq '[.[] | select(.conclusion == "failure" or .conclusion == "cancelled" or .conclusion == "timed_out")] | length' "$runs_json")"
fi

run_selector='.[] | select(.conclusion == "failure")'
if [[ "$include_cancelled" -eq 1 ]]; then
  run_selector='.[] | select(.conclusion == "failure" or .conclusion == "cancelled" or .conclusion == "timed_out")'
fi

log_unavailable_count=0
run_jobs_with_unavailable_logs=0

strip_log_timestamp() {
  sed -E 's/^[^[:space:]]+[[:space:]]+//'
}

fetch_job_log() {
  local job_id="$1" outfile="$2" status_file="$3"
  if [[ -s "$outfile" || -f "$status_file" ]]; then
    return 0
  fi

  if gh api "repos/${repo}/actions/jobs/${job_id}/logs" > "$outfile" 2> "$status_file"; then
    return 0
  fi

  if grep -Eq '(^|[^0-9])(404|410)([^0-9]|$)' "$status_file"; then
    rm -f "$outfile"
    return 1
  fi

  cat "$status_file" >&2
  rm -f "$outfile"
  return 1
}

while IFS=$'\t' read -r run_id conclusion created_at head_sha event; do
  [[ -n "$run_id" ]] || continue

  jobs_json="$log_dir/jobs-${run_id}.json"
  if ! gh api "repos/${repo}/actions/runs/${run_id}/jobs" --paginate > "$jobs_json"; then
    echo "WARN: failed to list jobs for run ${run_id}; skipping" >&2
    continue
  fi

  failed_jobs_count=0
  run_had_test_fail=0
  run_had_unavailable_log=0
  run_url="https://github.com/${repo}/actions/runs/${run_id}"

  while IFS=$'\t' read -r job_id job_name job_conclusion; do
    [[ -n "$job_id" ]] || continue
    [[ "$job_conclusion" == "failure" || "$job_conclusion" == "cancelled" || "$job_conclusion" == "timed_out" ]] || continue
    failed_jobs_count=$((failed_jobs_count + 1))

    log_file="$log_dir/job-${job_id}.log"
    status_file="$log_dir/job-${job_id}.err"
    if ! fetch_job_log "$job_id" "$log_file" "$status_file"; then
      log_unavailable_count=$((log_unavailable_count + 1))
      run_had_unavailable_log=1
      continue
    fi

    parsed_failures="$log_dir/job-${job_id}.failures.tsv"
    strip_log_timestamp < "$log_file" | awk '
      function flush_tests(pkg,  i, label) {
        if (pkg == "") pkg = "package unknown"
        for (i = 1; i <= pending_count; i++) {
          label = pending_tests[i] " (" pkg ")"
          if (!(label in emitted_tests)) {
            emitted_tests[label] = 1
            printf "test\t%s\n", label
          }
        }
        delete pending_tests
        delete pending_seen
        pending_count = 0
      }
      /^[[:space:]]*--- FAIL: [^[:space:](]+/ {
        test_name = $0
        sub(/^[[:space:]]*--- FAIL: /, "", test_name)
        sub(/[[:space:](].*$/, "", test_name)
        if (!(test_name in pending_seen)) {
          pending_tests[++pending_count] = test_name
          pending_seen[test_name] = 1
        }
      }
      /^FAIL[[:space:]]+github\.com\// {
        package_name = $0
        sub(/^FAIL[[:space:]]+/, "", package_name)
        sub(/[[:space:]].*$/, "", package_name)
        if (!(package_name in emitted_packages)) {
          emitted_packages[package_name] = 1
          printf "package\t%s\n", package_name
        }
        flush_tests(package_name)
      }
      END { flush_tests("") }
    ' > "$parsed_failures"

    if grep -q $'^test\t' "$parsed_failures"; then
      run_had_test_fail=1
    fi

    while IFS=$'\t' read -r kind failure_name; do
      [[ -n "$kind" && -n "$failure_name" ]] || continue
      printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
        "$kind" "$failure_name" "$run_id" "$run_url" "$job_name" "$conclusion" "$created_at" "$head_sha" >> "$records_tsv"
    done < "$parsed_failures"
  done < <(jq -r '.jobs[]? | [.id, .name, (.conclusion // "")] | @tsv' "$jobs_json")

  if [[ "$failed_jobs_count" -gt 0 && "$run_had_test_fail" -eq 0 ]]; then
    if [[ "$run_had_unavailable_log" -eq 1 ]]; then
      run_jobs_with_unavailable_logs=$((run_jobs_with_unavailable_logs + 1))
    fi
    printf '%s\t%s\t%s\t%s\t%s\n' "$run_id" "$run_url" "$conclusion" "$created_at" "$event" >> "$infra_tsv"
  fi
done < <(jq -r "${run_selector} | [.databaseId, .conclusion, .createdAt, .headSha, .event] | @tsv" "$runs_json")

summary_json="$log_dir/summary.json"
awk -F '\t' -v min_count="$min_count" -v failed_runs_total="$failed_runs_total" '
  BEGIN { OFS = "\t" }
  {
    key = $1 SUBSEP $2
    seen = key SUBSEP $3
    if (!(seen in run_seen)) {
      run_seen[seen] = 1
      count[key]++
      runs[key] = runs[key] (runs[key] ? "," : "") $3
      urls[key] = urls[key] (urls[key] ? "," : "") $4
    }
    job_key = key SUBSEP $5
    if (!(job_key in job_seen)) {
      job_seen[job_key] = 1
      jobs[key] = jobs[key] (jobs[key] ? "," : "") $5
    }
  }
  END {
    for (key in count) {
      split(key, parts, SUBSEP)
      if (count[key] < min_count) continue
      is_candidate = (parts[1] == "test" && count[key] >= 1 && count[key] < failed_runs_total) ? 1 : 0
      printf "%s\t%s\t%d\t%s\t%s\t%d\t%s\n", parts[1], parts[2], count[key], runs[key], urls[key], is_candidate, jobs[key]
    }
  }
' "$records_tsv" | LC_ALL=C sort -t $'\t' -k3,3nr -k1,1 -k2,2 > "$log_dir/aggregate.tsv"

flake_candidates="$(awk -F '\t' '$6 == 1 { n++ } END { print n + 0 }' "$log_dir/aggregate.tsv")"
infra_count="$(awk 'END { print NR + 0 }' "$infra_tsv")"

jq -Rn \
  --argjson runs_scanned "$runs_scanned" \
  --argjson success_count "$success_count" \
  --argjson failure_count "$failure_count" \
  --argjson cancelled_count "$cancelled_count" \
  --argjson included_failed_runs "$failed_runs_total" \
  --argjson flake_candidates "$flake_candidates" \
  --argjson infra_count "$infra_count" \
  --argjson log_unavailable_count "$log_unavailable_count" \
  --argjson run_jobs_with_unavailable_logs "$run_jobs_with_unavailable_logs" \
  --arg repo "$repo" \
  --arg workflow "$workflow" \
  --arg branch "$branch" \
  --argjson limit "$limit" \
  --argjson include_cancelled "$include_cancelled" \
  --slurpfile aggregate <(jq -Rn '[inputs | split("\t") | select(length == 7) | {
    type: .[0],
    name: .[1],
    count: (.[2] | tonumber),
    runs: (.[3] | split(",") | map(select(. != ""))),
    urls: (.[4] | split(",") | map(select(. != ""))),
    flakeCandidate: (.[5] == "1"),
    jobs: (.[6] | split(",") | map(select(. != "")))
  }]' < "$log_dir/aggregate.tsv") \
  --slurpfile infra <(jq -Rn '[inputs | split("\t") | select(length == 5) | {
    run: .[0],
    url: .[1],
    conclusion: .[2],
    createdAt: .[3],
    event: .[4]
  }]' < "$infra_tsv") \
  '{
    repo: $repo,
    workflow: $workflow,
    branch: $branch,
    limit: $limit,
    includeCancelled: ($include_cancelled == 1),
    summary: {
      runsScanned: $runs_scanned,
      success: $success_count,
      failure: $failure_count,
      cancelledOrTimedOut: $cancelled_count,
      includedFailedRuns: $included_failed_runs,
      flakeCandidates: $flake_candidates,
      buildInfraFailures: $infra_count,
      logsUnavailable: $log_unavailable_count,
      runsWithUnavailableFailedJobLogs: $run_jobs_with_unavailable_logs
    },
    failures: $aggregate[0],
    buildInfraFailures: $infra[0]
  }' > "$summary_json"

if [[ "$json_output" -eq 1 ]]; then
  cat "$summary_json"
  exit 0
fi

printf 'scanned %s runs for %s/%s on %s (limit %s): success=%s failure=%s cancelled/timed_out=%s; flake candidates=%s; build/infra failures=%s; logs unavailable=%s\n' \
  "$runs_scanned" "$repo" "$workflow" "$branch" "$limit" "$success_count" "$failure_count" "$cancelled_count" "$flake_candidates" "$infra_count" "$log_unavailable_count"
printf '\n'
printf '%-8s %5s  %-55s  %s\n' "TYPE" "RUNS" "NAME" "RUN IDS / URLS / JOBS"
printf '%-8s %5s  %-55s  %s\n' "--------" "-----" "-------------------------------------------------------" "---------------------"
if [[ -s "$log_dir/aggregate.tsv" ]]; then
  while IFS=$'\t' read -r kind name count run_ids urls _candidate jobs; do
    printf '%-8s %5s  %-55s  runs=%s urls=%s jobs=%s\n' "$kind" "$count" "$name" "$run_ids" "$urls" "$jobs"
  done < "$log_dir/aggregate.tsv"
else
  echo "(no Go test/package failures found)"
fi

if [[ -s "$infra_tsv" ]]; then
  printf '\nBuild/infra failures (failed jobs had no --- FAIL lines):\n'
  while IFS=$'\t' read -r run_id run_url conclusion created_at event; do
    printf '  %s  %s  %s  %s  %s\n' "$run_id" "$conclusion" "$created_at" "$event" "$run_url"
  done < "$infra_tsv"
fi
