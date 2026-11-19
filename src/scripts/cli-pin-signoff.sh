#!/usr/bin/env bash
# Prints the extra Signed-off-by trailers a cli-pin-bump commit needs so the
# probot DCO rule passes on the commit GitHub's squash writes (#18697).
#
# Usage: cli-pin-signoff.sh LOGIN ID [EMAIL...]
#
# The squash commit is authored as the PR opener (the TOPUP_PUSH_TOKEN owner),
# under whatever email GitHub attributes to them: their primary address, which
# is not the users.noreply.github.com one. The DCO app wants the AUTHOR email
# in a Signed-off-by, so emit one trailer per distinct email: the noreply
# identity first (always valid), then each real address supplied. Empty,
# "null" and malformed emails are skipped; duplicates are dropped.
set -euo pipefail

login="${1:?login required}"
id="${2:?id required}"
shift 2

declare -A seen=()
emit() {
  local email="${1,,}"
  [ -n "$email" ] && [ "$email" != "null" ] || return 0
  [[ "$email" =~ ^[^[:space:]@\<\>]+@[^[:space:]@\<\>]+$ ]] || return 0
  [ -z "${seen[$email]:-}" ] || return 0
  seen[$email]=1
  printf 'Signed-off-by: %s <%s>\n' "$login" "$1"
}

emit "${id}+${login}@users.noreply.github.com"
for e in "$@"; do emit "$e"; done
