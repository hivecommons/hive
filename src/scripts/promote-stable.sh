#!/usr/bin/env bash
# Evaluate and publish v5 soaked-build -> stable promotions by manifest digest.
set -euo pipefail

SOAK_HOURS_DEFAULT=24
RELEASE_BRANCH_DEFAULT=v5
CANDIDATE_CHANNEL_DEFAULT=candidate
STABLE_CHANNEL_DEFAULT=stable
IMAGE_NAMES_DEFAULT="hive hive-contributor hive-hub"
BLOCKER_LABEL_DEFAULT=release-blocker
SMOKE_VAR_DEFAULT=STABLE_SMOKE_EVIDENCE
RUN_LABEL=io.kubestellar.hive.github-actions-run-number
REVISION_LABEL=org.opencontainers.image.revision
DOCKER_WORKFLOW_DEFAULT=docker.yml
REQUIRED_WORKFLOWS_DEFAULT="v2-ci.yml v2-tests.yml"
STABLE_PROMOTION_URL_DEFAULT="https://hive.hivecommons.dev/api/hub/release/stable-promotion"
DOCKER_RUN_LOOKBACK_DEFAULT=500

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
  local current_candidate=${CURRENT_CANDIDATE:-true}
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
    reason="build age ${candidate_age_seconds}s < required ${soak_seconds}s (${soak_hours}h)"
  elif [[ $green_evidence != true ]]; then
    reason="required v2 CI / v2 Tests evidence is not green"
  elif (( blocker_count != 0 )); then
    reason="$blocker_count open release blocker(s) labelled release-blocker"
  elif [[ -z $smoke_evidence ]]; then
    reason="operator smoke signal is missing"
  else
    decision=promote
    reason="all stable chases-candidate promotion gates passed"
  fi

  printf 'decision=%s\nreason=%s\n' "$decision" "$reason"
  write_output decision "$decision"
  write_output reason "$reason"
}

# hold_with_reason emits a hold decision in the same shape as
# normalized_decision for conditions the gate learns about before it has a full
# set of facts (for example a candidate whose publishing run is still running).
hold_with_reason() {
  local reason=$1
  printf 'decision=hold\nreason=%s\n' "$reason"
  write_output decision hold
  write_output reason "$reason"
}

inspect_raw() {
  docker buildx imagetools inspect "$@"
}

manifest_digest() {
  local ref=$1 digest
  if ! digest=$(inspect_raw --format '{{.Manifest.Digest}}' "$ref" 2>&1); then
    echo "$digest" >&2
    return 1
  fi
  if ! [[ $digest == sha256:* ]]; then
    digest=$(sed -n 's/^Digest:[[:space:]]*//p' <<<"$digest" | head -n 1)
  fi
  [[ $digest == sha256:* ]] || return 1
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
  value=$(platform_json "$ref" | jq -r --arg label "$label" '(.config.Labels // .Config.Labels // {})[$label] // empty')
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
  value=$(jq -r --arg label "$RUN_LABEL" '(.config.Labels // .Config.Labels // {})[$label] // "0"' <<<"$inspect")
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

# EVIDENCE_ANCESTOR_DEPTH bounds how far back a required workflow's evidence may
# be inherited from. Ancestors are only consulted when the workflow did not run
# at all on the candidate; a run that FAILED is never inherited past.
EVIDENCE_ANCESTOR_DEPTH=${EVIDENCE_ANCESTOR_DEPTH:-10}

# workflow_ran_on reports how the workflow concluded on exactly this SHA:
# "success", "failure" (any completed non-success), or "none" when it did not
# run. The three cases are NOT interchangeable, which is the bug this replaces -
# treating "did not run" as "failed" made every docs-only or workflow-only merge
# permanently unpromotable, because docker.yml publishes a candidate on every
# push while v2-ci.yml and v2-tests.yml are path-filtered to src/**.
workflow_ran_on() {
  local repo=$1 workflow=$2 sha=$3 runs
  # Deliberately NOT branch-filtered. A run of this workflow on this exact
  # commit is evidence about that commit wherever it ran, and a branch filter
  # hides a genuine FAILURE (reporting it as "did not run"), which the caller
  # would then inherit past. Matching on head_sha alone keeps a failure visible.
  runs=$(unset GITHUB_TOKEN && gh api -H "Accept: application/vnd.github+json" \
    "/repos/${repo}/actions/workflows/${workflow}/runs?head_sha=${sha}&per_page=20" \
    --jq '[.workflow_runs[] | select(.status == "completed") | .conclusion]' 2>/dev/null || echo '[]')
  if grep -q '"success"' <<<"$runs"; then
    printf 'success'
  elif [[ $runs == "[]" || -z $runs ]]; then
    printf 'none'
  else
    printf 'failure'
  fi
}

# workflow_success accepts evidence from the candidate, or - when the workflow
# was path-filtered out of the candidate entirely - from the nearest ancestor
# that did run it. A commit that changes no code the suite covers inherits the
# verdict of the last commit that did, which is what "this code is tested" means
# for a tree the suite never looked at.
#
# It stops at the first ancestor where the workflow actually ran: if that run
# FAILED, the answer is failure. Inheriting past a failure would promote code a
# required suite rejected.
workflow_success() {
  local repo=$1 workflow=$2 sha=$3 resolved verdict candidate i
  resolved=$(full_sha "$repo" "$sha")

  verdict=$(workflow_ran_on "$repo" "$workflow" "$resolved")
  case $verdict in
    success) return 0 ;;
    failure) return 1 ;;
  esac

  for (( i = 1; i <= EVIDENCE_ANCESTOR_DEPTH; i++ )); do
    candidate=$(unset GITHUB_TOKEN && gh api -H "Accept: application/vnd.github+json" \
      "/repos/${repo}/commits/${resolved}~${i}" --jq '.sha' 2>/dev/null || true)
    [[ $candidate =~ ^[0-9a-f]{40}$ ]] || return 1
    verdict=$(workflow_ran_on "$repo" "$workflow" "$candidate")
    case $verdict in
      success)
        echo "::notice::${workflow} did not run on ${resolved:0:7}; inheriting success from ancestor ${candidate:0:7}" >&2
        return 0
        ;;
      failure) return 1 ;;
    esac
  done
  return 1
}

# workflow_run_completed_at is when the candidate's docker.yml run FINISHED,
# which is when that build became a soakable candidate. The run's createdAt is
# when it was queued: a multi-arch build takes tens of minutes, so measuring the
# soak from createdAt credits the candidate with time before its digest existed
# and promotes it short of the full window (#10042).
workflow_run_completed_at() {
  local repo=$1 run_number=$2 result
  result=$(unset GITHUB_TOKEN && gh run list -R "$repo" --workflow "${DOCKER_WORKFLOW:-$DOCKER_WORKFLOW_DEFAULT}" \
    --branch "${RELEASE_BRANCH:-$RELEASE_BRANCH_DEFAULT}" --json number,createdAt,updatedAt,status,conclusion --limit 100 \
    --jq ".[] | select(.number == ${run_number}) | select(.status == \"completed\") | if (.updatedAt // \"\") == \"\" then .createdAt else .updatedAt end" | head -n 1)
  [[ -n $result ]] || return 1
  echo "$result"
}

docker_success_runs() {
  local repo=$1 branch=${RELEASE_BRANCH:-$RELEASE_BRANCH_DEFAULT}
  local lookback=${DOCKER_RUN_LOOKBACK:-$DOCKER_RUN_LOOKBACK_DEFAULT}
  local page pages
  pages=$(( (lookback + 99) / 100 ))
  {
    gh run list -R "$repo" --workflow "${DOCKER_WORKFLOW:-$DOCKER_WORKFLOW_DEFAULT}" \
      --branch "$branch" --json number,headSha,updatedAt,status,conclusion --limit "$lookback" \
      --jq '.[] | select(.status == "completed" and .conclusion == "success") | [.number, .headSha, .updatedAt] | @tsv' || true
    for (( page = 1; page <= pages; page++ )); do
      gh api -H "Accept: application/vnd.github+json" \
        "/repos/${repo}/actions/workflows/${DOCKER_WORKFLOW:-$DOCKER_WORKFLOW_DEFAULT}/runs?branch=${branch}&per_page=100&page=${page}" \
        --jq '.workflow_runs[] | select(.status == "completed" and .conclusion == "success") | [.run_number, .head_sha, (.updated_at // .created_at)] | @tsv' || true
      curl -fsSL "https://api.github.com/repos/${repo}/actions/workflows/${DOCKER_WORKFLOW:-$DOCKER_WORKFLOW_DEFAULT}/runs?branch=${branch}&per_page=100&page=${page}" \
        | jq -r '.workflow_runs[] | select(.status == "completed" and .conclusion == "success") | [.run_number, .head_sha, (.updated_at // .created_at)] | @tsv' || true
    done
  } | awk -F '\t' 'NF >= 3 && !seen[$1]++ { print }' | sort -t $'\t' -k1,1nr
}

blocker_count() {
  unset GITHUB_TOKEN && gh issue list -R "$1" --state open --label "${BLOCKER_LABEL:-$BLOCKER_LABEL_DEFAULT}" --json number --limit 100 --jq 'length'
}

fetch_stable_promotion_state() {
  local url=${STABLE_PROMOTION_URL:-${STABLE_PROMOTION_URL_DEFAULT:-https://hive.hivecommons.dev/api/hub/release/stable-promotion}}
  curl -fsS --connect-timeout 5 --max-time 20 --retry 2 "$url"
}

stable_promotion_preflight() {
  local state
  if [[ -n ${STABLE_PROMOTION_STATE_JSON:-} ]]; then
    state=$STABLE_PROMOTION_STATE_JSON
  elif ! state=$(fetch_stable_promotion_state 2>/dev/null); then
    echo "::warning::could not reach stable auto-promotion state at ${STABLE_PROMOTION_URL:-${STABLE_PROMOTION_URL_DEFAULT:-https://hive.hivecommons.dev/api/hub/release/stable-promotion}}; skipping promotion fail-closed"
    return 1
  fi
  if ! jq -e 'type == "object" and has("auto_promote")' <<<"$state" >/dev/null 2>&1; then
    echo "::warning::could not read valid stable auto-promotion state at ${STABLE_PROMOTION_URL:-${STABLE_PROMOTION_URL_DEFAULT:-https://hive.hivecommons.dev/api/hub/release/stable-promotion}}; skipping promotion fail-closed"
    return 1
  fi
  local auto paused_by paused_at
  auto=$(jq -r '.auto_promote // false' <<<"$state")
  if [[ $auto != true ]]; then
    paused_by=$(jq -r '.paused_by // ""' <<<"$state")
    paused_at=$(jq -r '.paused_at // ""' <<<"$state")
    echo "::notice::stable auto-promotion paused by ${paused_by:-operator} at ${paused_at:-unknown}; skipping"
    return 1
  fi
  STABLE_PROMOTION_STATE_JSON=$state
  export STABLE_PROMOTION_STATE_JSON
  return 0
}

# stable_smoke_from_hub <sha> <digest> <soak_hours> [min_generation]
# A maintained hive is smoke evidence for the build when it runs that exact
# build, or — when min_generation is given and the hub reports the hive's
# build generation — any build at or after it in the same monotonic v5
# lineage (#10042: candidate moves faster than spokes update on busy days).
stable_smoke_from_hub() {
  local candidate_sha=$1 candidate_digest=$2 soak_hours=${3:-$SOAK_HOURS_DEFAULT} min_generation=${4:-0}
  local state=${STABLE_PROMOTION_STATE_JSON:-}
  [[ -n $state ]] || return 1
  STABLE_PROMOTION_STATE_JSON="$state" python3 - "$candidate_sha" "$candidate_digest" "$soak_hours" "$min_generation" <<'PY'
import json
import os
import sys
from datetime import datetime, timezone, timedelta

sha, digest, soak_hours = sys.argv[1], sys.argv[2], float(sys.argv[3])
min_generation = int(sys.argv[4] or 0)
try:
    state = json.loads(os.environ["STABLE_PROMOTION_STATE_JSON"])
except Exception:
    raise SystemExit(1)

now = datetime.now(timezone.utc)
window = timedelta(hours=soak_hours)
matches = []
for hive in state.get("maintained_hives") or []:
    hsha = str(hive.get("git_hash") or "")
    image = str(hive.get("image_ref") or "")
    generation = int(hive.get("generation") or 0)
    exact = bool(sha and hsha and (hsha.startswith(sha) or sha.startswith(hsha)))
    later = min_generation > 0 and generation >= min_generation
    if sha and hsha and not (exact or later):
        continue
    if not hsha and digest and digest not in image:
        continue
    if not hive.get("healthy"):
        continue
    if int(hive.get("crash_restarts_24h") or 0) != 0:
        continue
    hb = str(hive.get("last_heartbeat_at") or "").replace("Z", "+00:00")
    try:
        seen = datetime.fromisoformat(hb).astimezone(timezone.utc)
    except Exception:
        continue
    if now - seen > window:
        continue
    matches.append((hive.get("id") or "unknown", seen.isoformat().replace("+00:00", "Z"), hsha or image, exact, generation))

if not matches:
    raise SystemExit(1)
# Prefer a hive on the exact build; otherwise the oldest later build.
matches.sort(key=lambda m: (not m[3], m[4]))
hid, seen, ref, exact, generation = matches[0]
lineage = "" if exact else f" (later candidate generation {generation} >= {min_generation})"
print(f"hub candidate smoke: maintained hive {hid} healthy on {ref}{lineage} with heartbeat {seen}, 0 crash restarts/{int(soak_hours)}h")
PY
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
  local image_names=${IMAGE_NAMES:-$IMAGE_NAMES_DEFAULT}
  local images
  read -r -a images <<< "$image_names"
  local soak_seconds now blockers current_candidate_digest current_candidate_revision current_candidate_generation current_smoke
  local max_stable_generation=0 min_stable_generation="" stable_all_chosen=true stable_digest="mixed-or-not-promoted"
  local best_digest="" best_revision="" best_generation="" best_completed="" best_age="" best_smoke="" best_evidence="" best_reason=""
  local next_unsoaked_at="" next_unsoaked_sha="" next_unsoaked_generation="" next_unsoaked_completed=""
  local decision reason image run_number run_sha run_completed run_epoch age green evidence_text smoke digest revision generation stable_generation image_stable_digest
  declare -A chosen_digests stable_generations

  if ! stable_promotion_preflight; then
    write_output promoted false
    return 0
  fi

  now=$(now_epoch)
  soak_seconds=$(hours_to_seconds "${SOAK_HOURS:-$SOAK_HOURS_DEFAULT}")
  blockers=$(blocker_count "$repo")
  [[ $blockers =~ ^[0-9]+$ ]] || blockers=0

  for image in "${images[@]}"; do
    stable_generation=$(generation_for_ref "${image_prefix}/${image}:${stable_channel}")
    stable_generations[$image]=$stable_generation
    if (( stable_generation > max_stable_generation )); then
      max_stable_generation=$stable_generation
    fi
    if [[ -z $min_stable_generation || stable_generation -lt min_stable_generation ]]; then
      min_stable_generation=$stable_generation
    fi
  done
  stable_generation=${min_stable_generation:-0}

  # Rule 5 smoke evidence may be attached to the exact build or to the current
  # candidate when it is a later build in the same v5 line. A maintained hive
  # surviving a later candidate is conservative evidence for older builds in the
  # same monotonic docker.yml lineage; it is never used for a younger build.
  if current_candidate_digest=$(manifest_digest "${image_prefix}/${images[0]}:${candidate_channel}" 2>/dev/null); then
    current_candidate_revision=$(revision_for_ref "${image_prefix}/${images[0]}@${current_candidate_digest}" 2>/dev/null || true)
    current_candidate_generation=$(generation_for_ref "${image_prefix}/${images[0]}@${current_candidate_digest}" 2>/dev/null || echo 0)
    if [[ -z ${SMOKE_EVIDENCE:-} ]]; then
      current_smoke=$(stable_smoke_from_hub "$current_candidate_revision" "$current_candidate_digest" "${SOAK_HOURS:-$SOAK_HOURS_DEFAULT}" 2>/dev/null || true)
    fi
  fi

  while IFS=$'\t' read -r run_number run_sha run_completed; do
    [[ $run_number =~ ^[0-9]+$ ]] || continue
    (( run_number > max_stable_generation )) || continue
    run_epoch=$(iso_to_epoch "$run_completed")
    age=$((now - run_epoch))
    (( age >= 0 )) || age=0
    if (( age < soak_seconds )); then
      local eligible
      eligible=$(python3 - "$run_completed" "${SOAK_HOURS:-$SOAK_HOURS_DEFAULT}" <<'PYEOF'
from datetime import datetime, timezone, timedelta
import sys
built = datetime.fromisoformat(sys.argv[1].replace('Z', '+00:00')).astimezone(timezone.utc)
print((built + timedelta(hours=float(sys.argv[2]))).isoformat().replace('+00:00', 'Z'))
PYEOF
)
      if [[ -z $next_unsoaked_at || $eligible < $next_unsoaked_at ]]; then
        next_unsoaked_at=$eligible
        next_unsoaked_sha=${run_sha:0:7}
        next_unsoaked_generation=$run_number
        next_unsoaked_completed=$run_completed
      fi
      continue
    fi

    local same=true missing=false first_digest="" first_revision="" first_generation=""
    for image in "${images[@]}"; do
      local ref="${image_prefix}/${image}:${run_sha:0:7}"
      if ! digest=$(manifest_digest "$ref" 2>/dev/null); then
        missing=true
        break
      fi
      revision=$(revision_for_ref "${image_prefix}/${image}@${digest}" 2>/dev/null || true)
      generation=$(generation_for_ref "${image_prefix}/${image}@${digest}" 2>/dev/null || echo 0)
      if [[ $revision != "$run_sha" && $revision != "${run_sha:0:7}" ]]; then
        same=false
      fi
      if (( generation != run_number )); then
        same=false
      fi
      chosen_digests[$image]=$digest
      if [[ -z $first_digest ]]; then
        first_digest=$digest; first_revision=$revision; first_generation=$generation
      elif [[ $revision != "$first_revision" || $generation != "$first_generation" ]]; then
        same=false
      fi
    done
    if [[ $missing == true || $same != true ]]; then
      best_reason="digest integrity gate is holding build ${run_sha:0:7} generation ${run_number}: image digests are missing or metadata does not match"
      break
    fi

    evidence_text=$(collect_required_workflows "$repo" "$run_sha" 2>&1) && green=true || green=false
    if [[ $green != true ]]; then
      best_reason="green evidence gate is holding build ${run_sha:0:7} generation ${run_number}: required v2 CI / v2 Tests evidence is not green"
      break
    fi
    if (( blockers != 0 )); then
      best_reason="release-blocker gate is holding build ${run_sha:0:7} generation ${run_number}: ${blockers} open issue(s) labelled ${BLOCKER_LABEL:-$BLOCKER_LABEL_DEFAULT}"
      break
    fi

    smoke=${SMOKE_EVIDENCE:-}
    if [[ -z $smoke ]]; then
      smoke=${!SMOKE_VAR_DEFAULT:-}
    fi
    if [[ -z $smoke ]]; then
      smoke=$(stable_smoke_from_hub "$run_sha" "$first_digest" "${SOAK_HOURS:-$SOAK_HOURS_DEFAULT}" "$run_number" 2>/dev/null || true)
    fi
    if [[ -z $smoke && -n ${current_smoke:-} && ${current_candidate_generation:-0} =~ ^[0-9]+$ ]] && (( current_candidate_generation >= run_number )); then
      smoke="hub later-candidate smoke: ${current_smoke}"
    fi
    if [[ -z $smoke ]]; then
      best_reason="smoke signal gate is holding build ${run_sha:0:7} generation ${run_number}: operator smoke signal is missing"
      break
    fi

    best_digest=$first_digest
    best_revision=$run_sha
    best_generation=$run_number
    best_completed=$run_completed
    best_age=$age
    best_smoke=$smoke
    best_evidence=$evidence_text
    break
  done < <(docker_success_runs "$repo")

  if [[ -z $best_digest ]]; then
    if [[ -n $best_reason ]]; then
      hold_with_reason "$best_reason"
    elif [[ -n $next_unsoaked_at ]]; then
      hold_with_reason "no eligible build has completed the ${SOAK_HOURS:-$SOAK_HOURS_DEFAULT}h soak yet; next unsoaked build ${next_unsoaked_sha} generation ${next_unsoaked_generation} completed ${next_unsoaked_completed} and is eligible_at ${next_unsoaked_at}"
    else
      hold_with_reason "no docker.yml build newer than stable generation ${max_stable_generation} has crossed the ${SOAK_HOURS:-$SOAK_HOURS_DEFAULT}h line"
    fi
    write_output promoted false
    return 0
  fi

  stable_all_chosen=true
  for image in "${images[@]}"; do
    image_stable_digest=$(manifest_digest "${image_prefix}/${image}:${stable_channel}" 2>/dev/null || true)
    if [[ $image_stable_digest != "${chosen_digests[$image]}" ]]; then
      stable_all_chosen=false
    fi
  done
  if [[ $stable_all_chosen == true ]]; then
    stable_digest=$best_digest
  fi

  local out
  out=$(CANDIDATE_DIGEST="$best_digest" STABLE_DIGEST="$stable_digest" CANDIDATE_AGE_SECONDS="$best_age" \
    CURRENT_CANDIDATE=true GREEN_EVIDENCE=true BLOCKER_COUNT="$blockers" SMOKE_EVIDENCE="$best_smoke" \
    CANDIDATE_GENERATION="$best_generation" STABLE_GENERATION="$stable_generation" \
    EMERGENCY_EXCEPTION_REASON="${EMERGENCY_EXCEPTION_REASON:-}" EMERGENCY_FOLLOWUP_ISSUE="${EMERGENCY_FOLLOWUP_ISSUE:-}" normalized_decision)
  decision=$(awk -F= '/^decision=/{print $2}' <<<"$out")
  reason=$(awk -F= '/^reason=/{sub(/^reason=/,""); print}' <<<"$out")

  if [[ $decision == promote ]]; then
    for image in "${images[@]}"; do
      local recheck_generation
      recheck_generation=$(generation_for_ref "${image_prefix}/${image}:${stable_channel}")
      if [[ $recheck_generation != "${stable_generations[$image]}" ]]; then
        decision=hold
        reason="stable generation changed for ${image} from ${stable_generations[$image]} to ${recheck_generation} before the tag move; re-evaluate on the next schedule"
        write_output decision "$decision"
        write_output reason "$reason"
        break
      fi
      if (( recheck_generation >= best_generation )); then
        decision=hold
        reason="stable generation ${recheck_generation} is already >= selected build generation ${best_generation}; re-evaluate on the next schedule"
        write_output decision "$decision"
        write_output reason "$reason"
        break
      fi
    done
  fi

  printf 'decision=%s\nreason=%s\n' "$decision" "$reason"
  local promoted=false
  if [[ $decision == promote ]] && ! bool "$dry_run"; then
    promoted=true
  fi
  write_output promoted "$promoted"
  write_output candidate_digest "$best_digest"
  write_output candidate_sha "$best_revision"

  {
    echo "## Stable promotion decision"
    echo
    echo "- Decision: ${decision}"
    echo "- Reason: ${reason}"
    echo "- Selected build digest: ${best_digest}"
    echo "- Selected build SHA: ${best_revision}"
    echo "- Selected build generation: ${best_generation}"
    echo "- Selected build completed: ${best_completed}"
    echo "- Selected build age: ${best_age}s"
    echo "- Required soak: ${SOAK_HOURS:-$SOAK_HOURS_DEFAULT}h"
    echo "- Stable generation at decision: ${stable_generation}"
    echo "- Open ${BLOCKER_LABEL:-$BLOCKER_LABEL_DEFAULT} blockers: ${blockers}"
    echo "- Smoke evidence: ${best_smoke:-missing}"
    if [[ -n ${EMERGENCY_EXCEPTION_REASON:-} ]]; then
      echo "- Emergency exception reason: ${EMERGENCY_EXCEPTION_REASON}"
      echo "- Emergency follow-up issue: ${EMERGENCY_FOLLOWUP_ISSUE:-missing}"
    fi
    echo
    echo "### Checks consulted"
    echo "$best_evidence"
  } | append_summary

  if [[ $decision == promote ]]; then
    for image in "${images[@]}"; do
      publish_stable "${image_prefix}/${image}" "${chosen_digests[$image]}" "$dry_run"
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
