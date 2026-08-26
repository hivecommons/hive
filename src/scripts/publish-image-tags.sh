#!/usr/bin/env bash
# Publish one multi-arch image without starving moving tags during merge bursts.
#
# Every successful build receives its immutable short-SHA tag. Moving tags are
# guarded by the workflow run number stored in each platform image's config:
# an older queued run may fill a gap, but can never move a tag backwards over a
# newer run that finished first.
set -euo pipefail

if [[ $# -ne 7 ]]; then
  echo "usage: $0 IMAGE DIGEST_DIR BRANCH GIT_SHA RUN_NUMBER RELEASE_BRANCH INCLUDE_LATEST" >&2
  exit 2
fi

image=$1
digest_dir=$2
branch=$3
git_sha=$4
run_number=$5
release_branch=$6
include_latest=$7
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
if [[ $branch == "$release_branch" ]]; then
  moving_refs+=("$image:stable" "$image:candidate" "$image:edge")
fi

# Read the amd64 config label from every moving tag this invocation would
# update. All platforms are built with the same run-number label. Evaluate
# each tag independently so a newer global :latest from another branch cannot
# prevent this branch's own -latest tag from advancing after out-of-order runs.
# A registry transport/auth failure is red, not a green skip.
tag_args=(-t "$image:$git_short")
for ref in "${moving_refs[@]}"; do
  if inspect=$(docker buildx imagetools inspect \
      --format '{{json (index .Image "linux/amd64")}}' "$ref" 2>&1); then
    value=$(jq -r --arg label "$run_label" '.config.Labels[$label] // "0"' <<<"$inspect")
    if [[ ! $value =~ ^[0-9]+$ ]]; then
      echo "::error::moving tag $ref has invalid $run_label label: $value" >&2
      exit 1
    fi
  elif grep -Eqi 'manifest unknown|manifest.*not found|not found.*manifest|no such manifest' <<<"$inspect"; then
    echo "Moving tag $ref does not exist yet; treating it as generation 0."
    value=0
  else
    echo "::error::could not inspect moving tag $ref; refusing a potentially regressive publish" >&2
    echo "$inspect" >&2
    exit 1
  fi
  if (( run_number >= value )); then
    tag_args+=(-t "$ref")
    echo "Advancing $ref at run $run_number (published generation: $value)."
  else
    echo "::warning::run $run_number is older than $ref generation $value; leaving that moving tag unchanged"
  fi
done

source_args=()
for file in "${digest_files[@]}"; do
  source_args+=("$image@sha256:${file##*/}")
done

docker buildx imagetools create "${tag_args[@]}" "${source_args[@]}"
