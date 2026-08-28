#!/usr/bin/env bash
# Publish one multi-arch image without starving moving tags during merge bursts.
#
# Every successful build receives its immutable short-SHA tag. Moving tags are
# guarded by the workflow run number stored in each platform image's config:
# an older queued run may fill a gap, but can never move a tag backwards over a
# newer run that finished first.
set -euo pipefail

if [[ $# -lt 7 || $# -gt 8 ]]; then
  echo "usage: $0 IMAGE DIGEST_DIR BRANCH GIT_SHA RUN_NUMBER RELEASE_BRANCH INCLUDE_LATEST [CHANNELS]" >&2
  exit 2
fi

image=$1
digest_dir=$2
branch=$3
git_sha=$4
run_number=$5
release_branch=$6
include_latest=$7
# CHANNELS: comma-separated release-channel tags this branch's builds own.
# Channel ownership is per-branch: v4 owns stable+candidate, the v5 line owns
# edge — without the split, every v4 merge silently re-pointed edge back onto
# v4 minutes after any deliberate promotion of edge to v5.
channels=${8:-stable,candidate,edge}
run_label=io.kubestellar.hive.github-actions-run-number

if [[ ! $run_number =~ ^[0-9]+$ ]]; then
  echo "invalid workflow run number: $run_number" >&2
  exit 2
fi
if [[ $include_latest != true && $include_latest != false ]]; then
  echo "INCLUDE_LATEST must be true or false, got: $include_latest" >&2
  exit 2
fi

shopt -s nullglob
digest_files=("$digest_dir"/*)
if (( ${#digest_files[@]} == 0 )); then
  echo "No digests to merge (per-arch builds published none) — skipping."
  exit 0
fi

branch_tag=${branch//\//-}
git_short=${git_sha:0:7}
moving_refs=("$image:${branch_tag}-latest")
if [[ $include_latest == true ]]; then
  moving_refs+=("$image:latest")
fi
if [[ $branch == "$release_branch" && -n $channels ]]; then
  IFS=, read -ra channel_tags <<<"$channels"
  for ch in "${channel_tags[@]}"; do
    [[ -n $ch ]] && moving_refs+=("$image:$ch")
  done
fi

inspect_status=
inspect_generation=
read_generation() {
  local ref=$1 inspect value
  if inspect=$(docker buildx imagetools inspect \
      --format '{{json (index .Image "linux/amd64")}}' "$ref" 2>&1); then
    value=$(jq -r --arg label "$run_label" '.config.Labels[$label] // "0"' <<<"$inspect")
    if [[ ! $value =~ ^[0-9]+$ ]]; then
      echo "::error::tag $ref has invalid $run_label label: $value" >&2
      exit 1
    fi
    inspect_status=found
    inspect_generation=$value
  elif grep -Eqi 'manifest unknown|manifest.*not found|not found.*manifest|no such manifest|(^|[[:space:]:])not found$' <<<"$inspect"; then
    inspect_status=missing
    inspect_generation=0
  else
    echo "::error::could not inspect tag $ref; refusing a potentially regressive publish" >&2
    echo "$inspect" >&2
    exit 1
  fi
}

# Read the amd64 config label from every moving tag this invocation would
# update. All platforms are built with the same run-number label. Evaluate
# each tag independently so a newer global :latest from another branch cannot
# prevent this branch's own -latest tag from advancing after out-of-order runs.
# A registry transport/auth failure is red, not a green skip.
tag_args=()
sha_ref="$image:$git_short"
read_generation "$sha_ref"
if [[ $inspect_status == missing ]]; then
  tag_args+=(-t "$sha_ref")
  echo "Publishing immutable build tag $sha_ref at run $run_number."
else
  echo "Immutable build tag $sha_ref already exists (generation: $inspect_generation); leaving it unchanged."
fi
for ref in "${moving_refs[@]}"; do
  read_generation "$ref"
  value=$inspect_generation
  if [[ $inspect_status == missing ]]; then
    echo "Moving tag $ref does not exist yet; treating it as generation 0."
  fi
  if (( run_number > value )); then
    tag_args+=(-t "$ref")
    echo "Advancing $ref at run $run_number (published generation: $value)."
  elif (( run_number == value )); then
    echo "Moving tag $ref is already at run $run_number; leaving it unchanged."
  else
    echo "::warning::run $run_number is older than $ref generation $value; leaving that moving tag unchanged"
  fi
done

if (( ${#tag_args[@]} == 0 )); then
  echo "No tags need publishing."
  exit 0
fi

source_args=()
for file in "${digest_files[@]}"; do
  source_args+=("$image@sha256:${file##*/}")
done

docker buildx imagetools create "${tag_args[@]}" "${source_args[@]}"
