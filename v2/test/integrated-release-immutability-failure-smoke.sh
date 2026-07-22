#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../integrated-release-immutability.sh
source "$root/integrated-release-immutability.sh"

work="$(mktemp -d "${TMPDIR:-/tmp}/hive-release-immutability.XXXXXX")"
trap 'rm -rf -- "$work"' EXIT INT TERM
export RUNNER_TEMP="$work"
export GH_TOKEN=test-token
export GITHUB_API_URL=https://api.github.test
export GITHUB_REPOSITORY=owner/repo
export RELEASE_TAG=v1.0.0-integrated.1

sleep() { :; }

# Transient queries are retried under `set -e` and eventually succeed.
view_counter="$work/views"
printf '0\n' > "$view_counter"
gh() {
  if [[ "$1 $2" == "release view" ]]; then
    local views
    views="$(( $(<"$view_counter") + 1 ))"
    printf '%s\n' "$views" > "$view_counter"
    if [[ "$views" -lt 3 ]]; then
      return 1
    fi
    printf 'true\n'
    return 0
  fi
  return 1
}
verify_published_release_immutable
[[ "$(<"$view_counter")" == "3" ]]

# A failed delete is acceptable only when the authenticated API proves the
# exact release is already absent.
delete_counter="$work/deletes"
printf '0\n' > "$delete_counter"
gh() {
  if [[ "$1 $2" == "release delete" ]]; then
    local deletes
    deletes="$(( $(<"$delete_counter") + 1 ))"
    printf '%s\n' "$deletes" > "$delete_counter"
    return 1
  fi
  if [[ "$1 $2" == "api graphql" ]]; then
    return 0
  fi
  return 1
}
curl() { printf '404'; }
cleanup_unverified_release
[[ "$(<"$delete_counter")" == "1" ]]

# The EXIT trap preserves the original failure only after it has proved the
# unverified publication absent.
printf '0\n' > "$delete_counter"
set +e
(
  release_verified=false
  trap cleanup_on_exit EXIT
  exit 7
)
trap_status=$?
set -e
[[ "$trap_status" == "7" ]]
[[ "$(<"$delete_counter")" == "1" ]]

# REST does not expose drafts. An exact-tag GraphQL result therefore blocks a
# false cleanup success even when REST reports 404.
printf '0\n' > "$delete_counter"
gh() {
  if [[ "$1 $2" == "release delete" ]]; then
    local deletes
    deletes="$(( $(<"$delete_counter") + 1 ))"
    printf '%s\n' "$deletes" > "$delete_counter"
    return 1
  fi
  if [[ "$1 $2" == "api graphql" ]]; then
    printf '{"id":"draft-id","tagName":"%s","isDraft":true}\n' "$RELEASE_TAG"
    return 0
  fi
  return 1
}
curl() { printf '404'; }
if cleanup_unverified_release; then
  echo "cleanup unexpectedly ignored an exact-tag draft release" >&2
  exit 1
fi
[[ "$(<"$delete_counter")" == "5" ]]

# A release that remains present after every deletion attempt is a hard error.
printf '0\n' > "$delete_counter"
curl() { printf '200'; }
if cleanup_unverified_release; then
  echo "cleanup unexpectedly accepted a still-present mutable release" >&2
  exit 1
fi
[[ "$(<"$delete_counter")" == "5" ]]

# Repeated query failures never bypass the immutability requirement.
printf '0\n' > "$view_counter"
gh() {
  if [[ "$1 $2" == "release view" ]]; then
    local views
    views="$(( $(<"$view_counter") + 1 ))"
    printf '%s\n' "$views" > "$view_counter"
  fi
  return 1
}
if verify_published_release_immutable; then
  echo "query failures unexpectedly verified release immutability" >&2
  exit 1
fi
[[ "$(<"$view_counter")" == "5" ]]
