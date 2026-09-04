#!/usr/bin/env bash
# Evaluate and publish v4 candidate -> stable promotions by manifest digest.
set -euo pipefail

SOAK_HOURS_DEFAULT=24
RELEASE_BRANCH_DEFAULT=v4
CANDIDATE_CHANNEL_DEFAULT=candidate
STABLE_CHANNEL_DEFAULT=stable
IMAGE_NAMES_DEFAULT="hive hive-contributor hive-hub"
BLOCKER_LABEL_DEFAULT=release-blocker
SMOKE_VAR_DEFAULT=STABLE_SMOKE_EVIDENCE
RUN_LABEL=io.kubestellar.hive.github-actions-run-number
REVISION_LABEL=org.opencontainers.image.revision
DOCKER_WORKFLOW_DEFAULT=docker.yml
REQUIRED_WORKFLOWS_DEFAULT="v2-ci.yml v2-tests.yml"

usage() {
  cat >&2 <<USAGE
usage: $0 decide|promote|publish-stable

Subcommands:
  decide          Evaluate env-provided gate facts and print decision=<promote|hold>.
  promote         Resolve GHCR/GitHub evidence, evaluate, and optionally move stable.
  publish-stable  Move one IMAGE:stable to DIGEST after the monotonic guard passes.
  mirror-stable   Move TARGET_IMAGE:stable from SOURCE_REF after the same guard passes.
USAGE
}

bool() { [[ ${1:-} == true || ${1:-} == 1 || ${1:-} == yes ]]; }

hours_to_seconds() {
  local hours=${1:-$SOAK_HOURS_DEFAULT}
  python3 - "$hours" <<'PY'
import decimal, sys
h = decimal.Decimal(sys.argv[1])
if h < 0:
    raise SystemExit("negative soak hours")
print(int(h * decimal.Decimal(3600)))
PY
}

iso_to_epoch() {
  python3 - "$1" <<'PY'
from datetime import datetime, timezone
import sys
s = sys.argv[1].replace('Z', '+00:00')
print(int(datetime.fromisoformat(s).astimezone(timezone.utc).timestamp()))
PY
}

now_epoch() {
  if [[ -n ${NOW_EPOCH:-} ]]; then
    echo "$NOW_EPOCH"
  else
    date -u +%s
  fi
}

write_output() {
  local key=$1 value=$2
  if [[ -n ${GITHUB_OUTPUT:-} ]]; then
    printf '%s=%s\n' "$key" "$value" >> "$GITHUB_OUTPUT"
  fi
}

append_summary() {
  if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
    cat >> "$GITHUB_STEP_SUMMARY"
  else
    cat
  fi
}

normalized_decision() {
  local candidate_digest=${CANDIDATE_DIGEST:-}
  local stable_digest=${STABLE_DIGEST:-}
  local candidate_age_seconds=${CANDIDATE_AGE_SECONDS:-0}
  local soak_hours=${SOAK_HOURS:-$SOAK_HOURS_DEFAULT}
  local current_candidate=${CURRENT_CANDIDATE:-false}
  local green_evidence=${GREEN_EVIDENCE:-false}
  local blocker_count=${BLOCKER_COUNT:-0}
  local smoke_evidence=${SMOKE_EVIDENCE:-}
  local emergency_reason=${EMERGENCY_EXCEPTION_REASON:-}
  local emergency_followup=${EMERGENCY_FOLLOWUP_ISSUE:-}
  local candidate_generation=${CANDIDATE_GENERATION:-0}
  local stable_generation=${STABLE_GENERATION:-0}
  local soak_seconds reason decision

  soak_seconds=$(hours_to_seconds "$soak_hours")
  decision=hold

  if [[ -z $candidate_digest ]]; then
    reason="candidate digest is unavailable"
  elif [[ $candidate_digest == "$stable_digest" ]]; then
    reason="stable already points at candidate digest $candidate_digest"
  elif ! [[ $candidate_age_seconds =~ ^[0-9]+$ ]]; then
    reason="candidate age is invalid: $candidate_age_seconds"
  elif ! [[ $candidate_generation =~ ^[0-9]+$ && $stable_generation =~ ^[0-9]+$ ]]; then
    reason="generation metadata is invalid"
  elif (( candidate_generation <= stable_generation )); then
    reason="monotonic guard held: candidate generation $candidate_generation <= stable generation $stable_generation"
  elif [[ -n $emergency_reason ]]; then
    if [[ -z $emergency_followup ]]; then
      reason="emergency exception requires a follow-up issue"
    elif [[ $green_evidence != true ]]; then
      reason="emergency exception cannot bypass missing green release evidence"
    elif (( blocker_count != 0 )); then
      reason="emergency exception cannot bypass $blocker_count open release blocker(s)"
    elif [[ $current_candidate != true ]]; then
      reason="emergency exception candidate was superseded before publication"
    else
      decision=promote
      reason="emergency exception recorded: $emergency_reason"
    fi
  elif (( candidate_age_seconds < soak_seconds )); then
    reason="candidate age ${candidate_age_seconds}s < required ${soak_seconds}s (${soak_hours}h)"
  elif [[ $current_candidate != true ]]; then
    reason="newer candidate superseded this digest before the soak window completed"
  elif [[ $green_evidence != true ]]; then
    reason="required v2 CI / v2 Tests evidence is not green"
  elif (( blocker_count != 0 )); then
    reason="$blocker_count open release blocker(s) labelled release-blocker"
  elif [[ -z $smoke_evidence ]]; then
    reason="operator smoke signal is missing"
  else
    decision=promote
    reason="all stable soak promotion conditions passed"
  fi

  printf 'decision=%s\nreason=%s\n' "$decision" "$reason"
  write_output decision "$decision"
  write_output reason "$reason"
}

inspect_raw() {
  docker buildx imagetools inspect "$@"
}

manifest_digest() {
  local ref=$1 digest
  if ! digest=$(inspect_raw "$ref" --format '{{.Manifest.Digest}}' 2>&1); then
    echo "$digest" >&2
    return 1
  fi
  echo "$digest"
}

platform_json() {
  local ref=$1 out
  if ! out=$(inspect_raw --format '{{json (index .Image "linux/amd64")}}' "$ref" 2>&1); then
    echo "$out" >&2
    return 1
  fi
  if [[ $out == null || -z $out ]]; then
    echo "linux/amd64 manifest is missing for $ref" >&2
    return 1
  fi
  echo "$out"
}

label_value() {
  local ref=$1 label=$2 value
  value=$(platform_json "$ref" | jq -r --arg label "$label" '.config.Labels[$label] // empty')
  [[ -n $value ]] || return 1
  echo "$value"
}

is_missing_manifest_error() {
  grep -Eqi 'manifest unknown|manifest.*not found|not found.*manifest|no such manifest|(^|[[:space:]:])not found$'
}

generation_for_ref() {
  local ref=$1 inspect value
  if ! inspect=$(platform_json "$ref" 2>&1); then
    if is_missing_manifest_error <<<"$inspect"; then
      echo 0
      return 0
    fi
    echo "$inspect" >&2
    return 1
  fi
  value=$(jq -r --arg label "$RUN_LABEL" '.config.Labels[$label] // "0"' <<<"$inspect")
  if [[ $value =~ ^[0-9]+$ ]]; then
    echo "$value"
  else
    echo "invalid $RUN_LABEL label on $ref: $value" >&2
    return 1
  fi
}

revision_for_ref() {
  label_value "$1" "$REVISION_LABEL"
}

# full_sha expands a short revision to the 40-character SHA. The Actions runs
# API matches head_sha EXACTLY: a 7-character value returns zero runs rather
# than an error, so a short SHA silently reads as "no green evidence" for every
# commit. The candidate revision comes from an image label, which carries the
# short form, so the expansion has to happen before any runs query.
full_sha() {
  local repo=$1 sha=$2 result
  if [[ $sha =~ ^[0-9a-f]{40}$ ]]; then
    printf '%s' "$sha"
    return 0
  fi
  result=$(unset GITHUB_TOKEN && gh api -H "Accept: application/vnd.github+json" \
    "/repos/${repo}/commits/${sha}" --jq '.sha' 2>/dev/null || true)
  if [[ $result =~ ^[0-9a-f]{40}$ ]]; then
    printf '%s' "$result"
    return 0
  fi
  # Fall back to the input. The caller then queries with a short SHA and gets
  # no runs, which is the pre-existing conservative behaviour: hold, never
  # promote on evidence we could not confirm.
  printf '%s' "$sha"
}

workflow_success() {
  local repo=$1 workflow=$2 sha=$3 result resolved
  resolved=$(full_sha "$repo" "$sha")
  result=$(unset GITHUB_TOKEN && gh api -H "Accept: application/vnd.github+json" \
    "/repos/${repo}/actions/workflows/${workflow}/runs?branch=${RELEASE_BRANCH:-$RELEASE_BRANCH_DEFAULT}&head_sha=${resolved}&per_page=20" \
    --jq '[.workflow_runs[] | select(.conclusion == "success")] | length' 2>/dev/null || echo 0)
  [[ $result =~ ^[0-9]+$ && $result -gt 0 ]]
}

workflow_run_created_at() {
  local repo=$1 run_number=$2 result
  result=$(unset GITHUB_TOKEN && gh run list -R "$repo" --workflow "${DOCKER_WORKFLOW:-$DOCKER_WORKFLOW_DEFAULT}" \
    --branch "${RELEASE_BRANCH:-$RELEASE_BRANCH_DEFAULT}" --json number,createdAt,status,conclusion --limit 100 \
    --jq ".[] | select(.number == ${run_number}) | select(.status == \"completed\") | .createdAt" | head -n 1)
  [[ -n $result ]] || return 1
  echo "$result"
}

blocker_count() {
  unset GITHUB_TOKEN && gh issue list -R "$1" --state open --label "${BLOCKER_LABEL:-$BLOCKER_LABEL_DEFAULT}" --json number --limit 100 --jq 'length'
}

collect_required_workflows() {
  local repo=$1 sha=$2 workflow ok=true lines=()
  for workflow in ${REQUIRED_WORKFLOWS:-$REQUIRED_WORKFLOWS_DEFAULT}; do
    if workflow_success "$repo" "$workflow" "$sha"; then
      lines+=("- ${workflow}: success")
    else
      lines+=("- ${workflow}: missing success for ${sha}")
      ok=false
    fi
  done
  printf '%s\n' "${lines[@]}"
  [[ $ok == true ]]
}

publish_stable_from_source() {
  local image=$1 source_ref=$2 dry_run=${3:-true}
  local stable_channel=${STABLE_CHANNEL:-$STABLE_CHANNEL_DEFAULT}
  local stable_ref="${image}:${stable_channel}"
  local candidate_generation stable_generation
  candidate_generation=$(generation_for_ref "$source_ref")
  if (( candidate_generation == 0 )); then
    echo "::error::source ${source_ref} has no ${RUN_LABEL} metadata; refusing to move ${stable_ref}" >&2
    return 1
  fi
  stable_generation=$(generation_for_ref "$stable_ref")
  if (( candidate_generation <= stable_generation )); then
    echo "::warning::not moving ${stable_ref}: candidate generation ${candidate_generation} <= stable generation ${stable_generation}"
    return 0
  fi
  if bool "$dry_run"; then
    echo "DRY-RUN: would promote ${stable_ref} to ${source_ref} (generation ${candidate_generation} > ${stable_generation})"
  else
    echo "Promoting ${stable_ref} to ${source_ref} (generation ${candidate_generation} > ${stable_generation})"
    docker buildx imagetools create -t "$stable_ref" "$source_ref"
  fi
}

publish_stable() {
  local image=$1 digest=$2 dry_run=${3:-true}
  publish_stable_from_source "$image" "${image}@${digest}" "$dry_run"
}

promote() {
  local repo=${GITHUB_REPOSITORY:-${REPO:-hivecommons/hive}}
  local owner=${GITHUB_REPOSITORY_OWNER:-${OWNER:-hivecommons}}
  local image_prefix=${IMAGE_PREFIX:-ghcr.io/${owner}}
  local dry_run=${DRY_RUN:-true}
  local candidate_channel=${CANDIDATE_CHANNEL:-$CANDIDATE_CHANNEL_DEFAULT}
  local stable_channel=${STABLE_CHANNEL:-$STABLE_CHANNEL_DEFAULT}
  local images=( ${IMAGE_NAMES:-$IMAGE_NAMES_DEFAULT} )
  local candidate_digest stable_digest candidate_generation stable_generation revision run_created run_epoch age now
  local first_digest= first_revision= first_generation= evidence_text green=true blockers smoke decision reason image image_stable_digest
  local max_stable_generation=0 min_stable_generation= stable_all_candidate=true newer_stable=false
  declare -A candidate_digests

  for image in "${images[@]}"; do
    local ref="${image_prefix}/${image}:${candidate_channel}"
    candidate_digest=$(manifest_digest "$ref")
    revision=$(revision_for_ref "${image_prefix}/${image}@${candidate_digest}")
    candidate_generation=$(generation_for_ref "${image_prefix}/${image}@${candidate_digest}")
    candidate_digests[$image]=$candidate_digest
    image_stable_digest=$(manifest_digest "${image_prefix}/${image}:${stable_channel}" 2>/dev/null || true)
    if [[ $image_stable_digest != "$candidate_digest" ]]; then
      stable_all_candidate=false
    fi
    stable_generation=$(generation_for_ref "${image_prefix}/${image}:${stable_channel}")
    if (( stable_generation > candidate_generation )); then
      newer_stable=true
    fi
    if (( stable_generation > max_stable_generation )); then
      max_stable_generation=$stable_generation
    fi
    if [[ -z $min_stable_generation || stable_generation -lt min_stable_generation ]]; then
      min_stable_generation=$stable_generation
    fi
    if [[ -z $first_digest ]]; then
      first_digest=$candidate_digest; first_revision=$revision; first_generation=$candidate_generation
    elif [[ $revision != "$first_revision" || $candidate_generation != "$first_generation" ]]; then
      CANDIDATE_DIGEST=$candidate_digest STABLE_DIGEST= CANDIDATE_AGE_SECONDS=0 CURRENT_CANDIDATE=false \
        GREEN_EVIDENCE=false BLOCKER_COUNT=0 SMOKE_EVIDENCE= CANDIDATE_GENERATION=$candidate_generation STABLE_GENERATION=0 normalized_decision
      return 0
    fi
  done

  if [[ $stable_all_candidate == true ]]; then
    stable_digest=$first_digest
  else
    stable_digest="mixed-or-not-promoted"
  fi
  if [[ $newer_stable == true ]]; then
    stable_generation=$max_stable_generation
  else
    stable_generation=${min_stable_generation:-0}
  fi
  run_created=$(workflow_run_created_at "$repo" "$first_generation")
  run_epoch=$(iso_to_epoch "$run_created")
  now=$(now_epoch)
  age=$((now - run_epoch))
  (( age >= 0 )) || age=0

  evidence_text=$(collect_required_workflows "$repo" "$first_revision" 2>&1) || green=false
  blockers=$(blocker_count "$repo")
  smoke=${SMOKE_EVIDENCE:-}
  if [[ -z $smoke ]]; then
    smoke=${!SMOKE_VAR_DEFAULT:-}
  fi

  local out
  out=$(CANDIDATE_DIGEST="$first_digest" STABLE_DIGEST="$stable_digest" CANDIDATE_AGE_SECONDS="$age" \
    CURRENT_CANDIDATE=true GREEN_EVIDENCE="$green" BLOCKER_COUNT="$blockers" SMOKE_EVIDENCE="$smoke" \
    CANDIDATE_GENERATION="$first_generation" STABLE_GENERATION="$stable_generation" \
    EMERGENCY_EXCEPTION_REASON="${EMERGENCY_EXCEPTION_REASON:-}" EMERGENCY_FOLLOWUP_ISSUE="${EMERGENCY_FOLLOWUP_ISSUE:-}" normalized_decision)
  decision=$(awk -F= '/^decision=/{print $2}' <<<"$out")
  reason=$(awk -F= '/^reason=/{sub(/^reason=/,""); print}' <<<"$out")
  echo "$out"
  local promoted=false
  if [[ $decision == promote ]] && ! bool "$dry_run"; then
    promoted=true
  fi
  write_output promoted "$promoted"
  write_output candidate_digest "$first_digest"
  write_output candidate_sha "$first_revision"

  {
    echo "## Stable promotion decision"
    echo
    echo "- Decision: ${decision}"
    echo "- Reason: ${reason}"
    echo "- Candidate digest: ${first_digest}"
    echo "- Candidate SHA: ${first_revision}"
    echo "- Candidate generation: ${first_generation}"
    echo "- Candidate first seen: ${run_created}"
    echo "- Candidate age: ${age}s"
    echo "- Required soak: ${SOAK_HOURS:-$SOAK_HOURS_DEFAULT}h"
    echo "- Stable digest: ${stable_digest:-missing}"
    echo "- Stable generation: ${stable_generation}"
    echo "- Open ${BLOCKER_LABEL:-$BLOCKER_LABEL_DEFAULT} blockers: ${blockers}"
    echo "- Smoke evidence: ${smoke:-missing}"
    if [[ -n ${EMERGENCY_EXCEPTION_REASON:-} ]]; then
      echo "- Emergency exception reason: ${EMERGENCY_EXCEPTION_REASON}"
      echo "- Emergency follow-up issue: ${EMERGENCY_FOLLOWUP_ISSUE:-missing}"
    fi
    echo
    echo "### Checks consulted"
    echo "$evidence_text"
  } | append_summary

  if [[ $decision == promote ]]; then
    for image in "${images[@]}"; do
      candidate_digest=$(manifest_digest "${image_prefix}/${image}:${candidate_channel}")
      if [[ $candidate_digest != "${candidate_digests[$image]}" ]]; then
        echo "::error::${image_prefix}/${image}:${candidate_channel} changed during evaluation; refusing to promote a superseded candidate" >&2
        exit 1
      fi
      publish_stable "${image_prefix}/${image}" "$candidate_digest" "$dry_run"
    done
  fi
}

case ${1:-} in
  decide) normalized_decision ;;
  promote) promote ;;
  publish-stable) shift; [[ $# -eq 3 ]] || { usage; exit 2; }; publish_stable "$@" ;;
  mirror-stable) shift; [[ $# -eq 3 ]] || { usage; exit 2; }; publish_stable_from_source "$@" ;;
  *) usage; exit 2 ;;
esac
