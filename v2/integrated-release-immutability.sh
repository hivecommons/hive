#!/usr/bin/env bash

# Sourced by the tagged release job. Callers provide GH_TOKEN,
# GITHUB_API_URL, GITHUB_REPOSITORY, RELEASE_TAG, and RUNNER_TEMP.

verify_release_absent() {
  local response_file="$RUNNER_TEMP/hive-release-cleanup-response.json"
  local status
  if ! status="$(curl --silent --show-error \
    --connect-timeout 10 --max-time 30 \
    --retry 3 --retry-all-errors \
    --header "Accept: application/vnd.github+json" \
    --header "Authorization: Bearer $GH_TOKEN" \
    --header "X-GitHub-Api-Version: 2026-03-10" \
    --output "$response_file" --write-out '%{http_code}' \
    "$GITHUB_API_URL/repos/$GITHUB_REPOSITORY/releases/tags/$RELEASE_TAG")"; then
    return 1
  fi
  [[ "$status" == "404" ]] || return 1

  # The REST tag endpoint does not enumerate drafts. `gh release create` can
  # leave a draft if an asset upload fails, so also require GraphQL's exact-tag
  # release lookup (the lookup used by gh for draft releases) to return null.
  local owner="${GITHUB_REPOSITORY%%/*}"
  local name="${GITHUB_REPOSITORY#*/}"
  local release_node
  [[ -n "$owner" && -n "$name" && "$name" != "$GITHUB_REPOSITORY" ]] || return 1
  if ! release_node="$(gh api graphql \
    -f 'query=query($owner:String!,$name:String!,$tag:String!){repository(owner:$owner,name:$name){release(tagName:$tag){id tagName isDraft}}}' \
    -F "owner=$owner" -F "name=$name" -F "tag=$RELEASE_TAG" \
    --jq '.data.repository.release // empty')"; then
    return 1
  fi
  [[ -z "$release_node" ]]
}

cleanup_unverified_release() {
  local attempt
  for attempt in 1 2 3 4 5; do
    # Deletion may report not-found after an earlier successful try; the
    # authenticated REST 404 below is the decisive condition.
    if ! gh release delete "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" --yes; then
      echo "Release cleanup attempt $attempt did not complete; verifying exact tag state." >&2
    fi
    if verify_release_absent; then
      return 0
    fi
    sleep 2
  done
  return 1
}

verify_published_release_immutable() {
  local attempt
  local release_immutable=false
  for attempt in 1 2 3 4 5; do
    if ! release_immutable="$(gh release view "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" --json isImmutable --jq .isImmutable)"; then
      release_immutable=false
      echo "Release immutability query attempt $attempt failed; retrying." >&2
    fi
    [[ "$release_immutable" == "true" ]] && return 0
    sleep 2
  done
  return 1
}

cleanup_on_exit() {
  local original_status=$?
  trap - EXIT
  if [[ "${release_verified:-false}" != "true" ]]; then
    if ! cleanup_unverified_release; then
      echo "CRITICAL: unable to prove removal of unverified release $RELEASE_TAG." >&2
      exit 1
    fi
  fi
  exit "$original_status"
}
