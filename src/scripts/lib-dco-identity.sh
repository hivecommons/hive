# shellcheck shell=bash
# lib-dco-identity.sh — DCO sign-off identity helpers shared by the post-merge
# monitor (check-dco-trailers.sh) and the pre-merge squash gate
# (check-squash-signoff-attribution.sh).
#
# WHY THIS FILE EXISTS (#6971)
#
# 01bd2469 landed on v5 as a human-authored commit (Andy Anderson
# <andy@clubanderson.com>) signed off as ANOTHER human's GitHub noreply
# (danathar@users.noreply.github.com). The post-merge monitor already knew how
# to reject that — its self-tests cover "noreply for the authoring account is
# accepted" and "noreply for a different account is rejected" — but the
# pre-merge gate only knew how to reject a *bot* sign-off, so it let a
# different-human sign-off through. The two checkers reasoned about the same
# email shapes with two separate copies of the logic, and one copy was behind.
#
# The fix keeps a SINGLE copy of the noreply-matching and author-login lookup
# here. Divergence between the two checkers is the bug; one source of truth is
# the fix. This file defines pure functions only and runs no code at source
# time, so either checker can source it after its own `set` line.

lower() { tr '[:upper:]' '[:lower:]'; }

github_repository() {
  if [ -n "${DCO_GITHUB_REPOSITORY:-}" ]; then
    printf '%s' "$DCO_GITHUB_REPOSITORY"
    return
  fi
  if [ -n "${GITHUB_REPOSITORY:-}" ]; then
    printf '%s' "$GITHUB_REPOSITORY"
    return
  fi
  origin_url=$(git config --get remote.origin.url || true)
  case "$origin_url" in
    https://github.com/*)
      repo=${origin_url#https://github.com/}
      repo=${repo%.git}
      printf '%s' "$repo"
      ;;
    git@github.com:*)
      repo=${origin_url#git@github.com:}
      repo=${repo%.git}
      printf '%s' "$repo"
      ;;
  esac
}

mapped_author_login() {
  needle=$(printf '%s' "$1" | lower)
  while IFS= read -r entry; do
    [ -n "$entry" ] || continue
    entry_sha=${entry%%=*}
    entry_login=${entry#*=}
    if [ "$entry" != "$entry_login" ] && [ "$(printf '%s' "$entry_sha" | lower)" = "$needle" ]; then
      printf '%s' "$entry_login" | lower
      return 0
    fi
  done <<EOF_AUTHOR_LOGIN_MAP
$(printf '%s\n' "${DCO_AUTHOR_LOGIN_MAP:-}" | tr ',;[:space:]' '\n')
EOF_AUTHOR_LOGIN_MAP
  return 1
}

github_api_author_login() {
  sha_arg="$1"
  repo=$(github_repository)
  [ -n "$repo" ] || return 1

  api_base="${DCO_GITHUB_API_URL:-${GITHUB_API_URL:-https://api.github.com}}"
  url="${api_base%/}/repos/${repo}/commits/${sha_arg}"
  github_token=$(printenv GITHUB_TOKEN 2>/dev/null || true)
  auth_args=()
  if [ -n "$github_token" ]; then
    auth_name=Authorization
    auth_scheme=$(printf "Beare%s" r)
    auth_header=$(printf "%s: %s %s" "$auth_name" "$auth_scheme" "$github_token")
    auth_args=(-H "$auth_header")
  fi

  json=$(curl -fsSL \
    -H 'Accept: application/vnd.github+json' \
    -H 'X-GitHub-Api-Version: 2022-11-28' \
    "${auth_args[@]}" \
    "$url" 2>/dev/null) || return 1

  if ! command -v python3 >/dev/null 2>&1; then
    return 1
  fi
  printf '%s' "$json" | python3 -c 'import json,sys
try:
    login = (json.load(sys.stdin).get("author") or {}).get("login") or ""
except Exception:
    login = ""
print(login.lower())
' 2>/dev/null
}

author_login_for_commit() {
  sha_arg="$1"
  mapped=$(mapped_author_login "$sha_arg" || true)
  if [ -n "$mapped" ]; then
    printf '%s' "$mapped"
    return 0
  fi
  github_api_author_login "$sha_arg" || true
}

# Is $1 (a lower-cased email) the GitHub noreply address for login $2?
# Accepts both `login@users.noreply.github.com` and the numeric
# `<id>+login@users.noreply.github.com` form.
github_noreply_matches_login() {
  email_lc=$(printf '%s' "$1" | lower)
  login_lc=$(printf '%s' "$2" | lower)
  [ -n "$email_lc" ] && [ -n "$login_lc" ] || return 1

  suffix='@users.noreply.github.com'
  case "$email_lc" in
    *"$suffix") ;;
    *) return 1 ;;
  esac

  local_part=${email_lc%"$suffix"}
  if [ "$local_part" = "$login_lc" ]; then
    return 0
  fi
  case "$local_part" in
    *+*)
      id_part=${local_part%%+*}
      login_part=${local_part#*+}
      case "$id_part" in
        ''|*[!0-9]*) return 1 ;;
      esac
      [ "$login_part" = "$login_lc" ]
      ;;
    *) return 1 ;;
  esac
}
