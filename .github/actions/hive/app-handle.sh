#!/usr/bin/env bash
# Helpers for the hive composite action's comment-relay transport (#9707).
#
# The relay comment must @-mention the hive's GitHub App by its real handle:
# the mention parser (findMention in src/pkg/mention/grammar.go) matches
# "@<app-login>" or the login without its "[bot]" suffix, so an App named
# e.g. "hivecommons-hive" never sees a comment that says "@hive". Nothing here
# may assume the App's bot login is literally "hive" plus the bot suffix.
#
# Sourced by action.yml and by .github/scripts/test-hive-action-app-handle.sh.

# HIVE_DEFAULT_APP_HANDLE is the backward-compatible fallback used when no
# handle is configured: it matches an App whose handle is "hive".
HIVE_DEFAULT_APP_HANDLE='hive'

# hive_normalize_app_handle prints the bare handle for one candidate value.
# It accepts the handle with or without a leading "@" and with or without the
# "[bot]" suffix ("@x", "x[bot]", "@x[bot]" all print "x"). Blank input prints
# nothing and succeeds. A value that is not a GitHub login after normalization
# fails, so arbitrary text can never be spliced into the relay comment.
hive_normalize_app_handle() {
  local handle="$1"
  # Trim surrounding whitespace.
  handle="${handle#"${handle%%[![:space:]]*}"}"
  handle="${handle%"${handle##*[![:space:]]}"}"
  handle="${handle#@}"
  handle="${handle%\[bot\]}"
  if [[ -z "${handle}" ]]; then
    return 0
  fi
  # GitHub logins: alphanumerics and single hyphens, not starting with "-".
  if [[ ! "${handle}" =~ ^[A-Za-z0-9][A-Za-z0-9-]*$ ]]; then
    echo "hive action app_handle is not a valid GitHub App handle: '$1'" >&2
    return 1
  fi
  printf '%s\n' "${handle}"
}

# hive_resolve_app_handle prints the handle to mention, in resolution order:
# explicit action input ($1), then the fallback ($2, callers pass
# vars.HIVE_APP_HANDLE or the HIVE_APP_HANDLE environment variable), then
# HIVE_DEFAULT_APP_HANDLE. The first non-blank value wins; an invalid
# non-blank value fails rather than silently falling through.
hive_resolve_app_handle() {
  local candidate normalized
  for candidate in "$1" "${2:-}"; do
    normalized="$(hive_normalize_app_handle "${candidate}")" || return 1
    if [[ -n "${normalized}" ]]; then
      printf '%s\n' "${normalized}"
      return 0
    fi
  done
  printf '%s\n' "${HIVE_DEFAULT_APP_HANDLE}"
}

# hive_relay_comment_body prints the relay comment: "@<handle> <command>
# [prompt]", a blank line, then the hidden action marker.
# Arguments: handle command prompt marker.
hive_relay_comment_body() {
  local handle="$1" command="$2" prompt="$3" marker="$4"
  local body="@${handle} ${command}"
  if [[ -n "${prompt}" ]]; then
    body="${body} ${prompt}"
  fi
  printf '%s\n\n%s' "${body}" "${marker}"
}
