#!/usr/bin/env bash
# Check recent protected-branch commits for a DCO trailer whose email matches
# the commit author. Maintainer override identities can be supplied with
# DCO_ALLOWLIST_EMAILS, but the default is intentionally empty so normal runs
# require the squash author's own sign-off.
set -u -o pipefail

limit="${1:-${DCO_COMMIT_LIMIT:-50}}"
ref="${2:-${DCO_REF:-HEAD}}"
allowlist_raw="${DCO_ALLOWLIST_EMAILS:-}"
skip_bots="${DCO_SKIP_BOT_AUTHORS:-true}"

usage() {
  cat >&2 <<'USAGE'
Usage: check-dco-trailers.sh [commit-limit] [ref]

Environment:
  DCO_COMMIT_LIMIT       Number of recent commits to inspect (default: 50)
  DCO_REF                Ref to inspect when no ref argument is supplied (default: HEAD)
  DCO_ALLOWLIST_EMAILS   Optional comma/space/newline-separated sign-off emails accepted
                         in addition to the author email. Empty by default.
  DCO_SKIP_BOT_AUTHORS   true/false; skip bot-authored commits by default.
USAGE
}

case "$limit" in
  ''|*[!0-9]*) echo "commit limit must be a positive integer: $limit" >&2; usage; exit 2 ;;
esac
if [ "$limit" -eq 0 ]; then
  echo "commit limit must be greater than zero" >&2
  usage
  exit 2
fi

if ! git rev-parse --verify --quiet "${ref}^{commit}" >/dev/null; then
  echo "ref does not resolve to a commit: $ref" >&2
  exit 2
fi

lower() { tr '[:upper:]' '[:lower:]'; }

email_allowed() {
  needle=$(printf '%s' "$1" | lower)
  [ -n "$needle" ] || return 1
  while IFS= read -r allowed; do
    allowed_lc=$(printf '%s' "$allowed" | lower)
    if [ -n "$allowed_lc" ] && [ "$needle" = "$allowed_lc" ]; then
      return 0
    fi
  done <<EOF_ALLOWLIST
$(printf '%s\n' "$allowlist_raw" | tr ',;[:space:]' '\n')
EOF_ALLOWLIST
  return 1
}

is_bot_author() {
  ident_lc=$(printf '%s <%s>' "$1" "$2" | lower)
  case "$ident_lc" in
    *'[bot]'*|*'hive-release-bot'*|*'github-actions'*) return 0 ;;
    *) return 1 ;;
  esac
}

extract_signoff_emails() {
  git log -1 --format=%B "$1" |
    git interpret-trailers --parse |
    awk '
      tolower($0) ~ /^signed-off-by:[[:space:]]*/ {
        line=$0
        sub(/^[^:]+:[[:space:]]*/, "", line)
        if (match(line, /<[^<>[:space:]]+@[^<>[:space:]]+>/)) {
          email=substr(line, RSTART + 1, RLENGTH - 2)
          print tolower(email)
        }
      }'
}

failures=""
checked=0
skipped_merges=0
skipped_bots=0

while IFS= read -r sha; do
  parent_count=$(git rev-list --parents -n 1 "$sha" | awk '{print NF-1}')
  if [ "$parent_count" -gt 1 ]; then
    skipped_merges=$((skipped_merges + 1))
    continue
  fi

  author_name=$(git log -1 --format=%an "$sha")
  author_email=$(git log -1 --format=%ae "$sha")
  if [ "$skip_bots" = "true" ] && is_bot_author "$author_name" "$author_email"; then
    skipped_bots=$((skipped_bots + 1))
    continue
  fi

  checked=$((checked + 1))
  author_lc=$(printf '%s' "$author_email" | lower)
  signoff_emails=$(extract_signoff_emails "$sha")

  if [ -z "$signoff_emails" ]; then
    failures="${failures}FAIL ${sha} missing-signoff author=${author_name} <${author_email}>\n"
    continue
  fi

  matched=0
  while IFS= read -r signoff_email; do
    if [ "$signoff_email" = "$author_lc" ] || email_allowed "$signoff_email"; then
      matched=1
      break
    fi
  done <<EOF_SIGNOFFS
$signoff_emails
EOF_SIGNOFFS

  if [ "$matched" -ne 1 ]; then
    rendered=$(printf '%s' "$signoff_emails" | paste -sd, -)
    failures="${failures}FAIL ${sha} mismatched-signoff author=${author_name} <${author_email}> signed-off-by=${rendered}\n"
  fi
done < <(git rev-list -n "$limit" "$ref")

printf 'DCO trailer check inspected %s recent commits on %s (%s non-merge, non-bot checked; %s merge skipped; %s bot skipped).\n' \
  "$limit" "$ref" "$checked" "$skipped_merges" "$skipped_bots"

if [ -n "$failures" ]; then
  printf '%b' "$failures"
  exit 1
fi

echo "DCO trailer check passed."
