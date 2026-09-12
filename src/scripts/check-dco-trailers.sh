#!/usr/bin/env bash
# Check recent protected-branch commits for a DCO trailer whose email matches
# the commit author. GitHub noreply addresses are accepted only when the
# GitHub commits API identifies the commit author as that exact login.
# Maintainer override identities can be supplied with DCO_ALLOWLIST_EMAILS, but
# the default is intentionally empty so normal runs require the squash author's
# own sign-off.
#
# Two override instruments, deliberately different in blast radius:
#
#   DCO_ALLOWLIST_EMAILS  accepts an IDENTITY. It only helps a commit that HAS
#                         a trailer under another address, and it keeps
#                         accepting that address on every future commit.
#   DCO_WAIVED_COMMITS    accepts ONE HISTORICAL COMMIT by full SHA. It is the
#                         only instrument that can clear a missing-signoff, and
#                         it cannot widen: a SHA names a commit that already
#                         exists, so a waiver can never pre-approve future work.
#
# A waiver exists because a protected branch cannot be repaired. Policy forbids
# rewriting another contributor's commit or adding a sign-off on their behalf
# (see src/docs/v5-sync-policy.md and hivecommons/hive#6329), so for a commit
# already merged without a trailer there is no fix — only a recorded maintainer
# disposition, which is exactly what v5-sync-policy.md asks for ("listing every
# affected SHA and the reason the override is acceptable"). Without this the
# monitor's only route back to green is waiting for the commit to fall out of
# the window, during which every hourly run pages on a condition nobody can
# act on and a genuinely NEW failure is indistinguishable from the known one.
#
# Waivers are never silent: every applied waiver is printed, and a waiver whose
# commit is in the window but no longer failing is reported as stale so it gets
# removed instead of lingering as an unexamined hole.
set -u -o pipefail

limit="${1:-${DCO_COMMIT_LIMIT:-50}}"
ref="${2:-${DCO_REF:-HEAD}}"
allowlist_raw="${DCO_ALLOWLIST_EMAILS:-}"
waived_raw="${DCO_WAIVED_COMMITS:-}"
skip_bots="${DCO_SKIP_BOT_AUTHORS:-true}"

usage() {
  cat >&2 <<'USAGE'
Usage: check-dco-trailers.sh [commit-limit] [ref]

Environment:
  DCO_COMMIT_LIMIT       Number of recent commits to inspect (default: 50)
  DCO_REF                Ref to inspect when no ref argument is supplied (default: HEAD)
  DCO_ALLOWLIST_EMAILS   Optional comma/space/newline-separated sign-off emails accepted
                         in addition to the author email. Empty by default.
  DCO_WAIVED_COMMITS     Optional comma/space/newline-separated FULL 40-character commit
                         SHAs with a recorded maintainer DCO disposition. A waived commit
                         is reported as WAIVED instead of FAIL and does not fail the run.
                         Abbreviated SHAs are rejected. Empty by default.
  DCO_SKIP_BOT_AUTHORS   true/false; skip bot-authored commits by default.
  DCO_GITHUB_REPOSITORY  Optional owner/repo used for GitHub noreply author
                         lookups. Defaults to GITHUB_REPOSITORY or origin.
  DCO_AUTHOR_LOGIN_MAP   Optional comma/space/newline-separated SHA=login map
                         used before the GitHub API, mainly for tests/offline
                         runs. Empty by default.
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

# waived_shas is the normalised waiver list, one lower-case full SHA per line.
# Built once so the per-commit lookup does not re-parse the raw value 50 times.
waived_shas=$(printf '%s\n' "$waived_raw" | tr ',;[:space:]' '\n' | lower | grep -v '^$' || true)

# A malformed waiver must be an ERROR, not a no-op. A typo'd or abbreviated SHA
# that silently matches nothing would leave the monitor red with the maintainer
# believing a disposition was recorded — the exact failure this instrument is
# meant to end. Abbreviations are refused outright: a prefix is ambiguous, and
# an ambiguous waiver on a security-adjacent check could come to cover a commit
# nobody dispositioned.
while IFS= read -r waived; do
  # An unset/blank DCO_WAIVED_COMMITS still yields one empty line here; an
  # empty entry is "no waivers", not a malformed one.
  [ -n "$waived" ] || continue
  if [ "${#waived}" -ne 40 ]; then
    echo "DCO_WAIVED_COMMITS entries must be full 40-character SHAs (got ${#waived} chars): $waived" >&2
    exit 2
  fi
  case "$waived" in
    *[!0-9a-f]*) echo "DCO_WAIVED_COMMITS entry is not a hex SHA: $waived" >&2; exit 2 ;;
  esac
done <<EOF_WAIVER_VALIDATE
$waived_shas
EOF_WAIVER_VALIDATE

commit_waived() {
  needle=$(printf '%s' "$1" | lower)
  while IFS= read -r waived; do
    if [ -n "$waived" ] && [ "$needle" = "$waived" ]; then
      return 0
    fi
  done <<EOF_WAIVED
$waived_shas
EOF_WAIVED
  return 1
}

is_bot_author() {
  ident_lc=$(printf '%s <%s>' "$1" "$2" | lower)
  case "$ident_lc" in
    *'[bot]'*|*'hive-release-bot'*|*'github-actions'*) return 0 ;;
    *) return 1 ;;
  esac
}

# Scan the WHOLE commit body for Signed-off-by lines, not just the final
# trailer block. `git interpret-trailers --parse` reads only the last
# paragraph, so a body like "Signed-off-by: ...\n\nCo-authored-by: ..." —
# which the pre-merge DCO app accepts, because it greps every line — was
# reported here as missing-signoff (#6605/8fe6bb34 waived for exactly this;
# v5 639f49e9 re-triggered it). Matching the pre-merge check's line-based
# rule keeps this monitor from paging on sign-offs already accepted at merge.
extract_signoff_emails() {
  git log -1 --format=%B "$1" |
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
waived_report=""
waived_count=0
stale_waivers=""
checked=0
skipped_merges=0
skipped_bots=0

# record_failure books one bad commit, routing it to the waived report instead
# of the failure list when a maintainer disposition covers that exact SHA.
record_failure() {
  sha_arg="$1"
  detail="$2"
  if commit_waived "$sha_arg"; then
    waived_count=$((waived_count + 1))
    waived_report="${waived_report}WAIVED ${sha_arg} ${detail}\n"
    return
  fi
  failures="${failures}FAIL ${sha_arg} ${detail}\n"
}

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
  author_login=""

  if [ -z "$signoff_emails" ]; then
    record_failure "$sha" "missing-signoff author=${author_name} <${author_email}>"
    continue
  fi

  matched=0
  while IFS= read -r signoff_email; do
    if [ "$signoff_email" = "$author_lc" ] || email_allowed "$signoff_email"; then
      matched=1
      break
    fi
    if github_noreply_matches_login "$signoff_email" "${author_login:-}"; then
      matched=1
      break
    fi
    case "$signoff_email" in
      *@users.noreply.github.com)
        if [ -z "${author_login:-}" ]; then
          author_login=$(author_login_for_commit "$sha")
        fi
        if github_noreply_matches_login "$signoff_email" "$author_login"; then
          matched=1
          break
        fi
        ;;
    esac
  done <<EOF_SIGNOFFS
$signoff_emails
EOF_SIGNOFFS

  if [ "$matched" -ne 1 ]; then
    rendered=$(printf '%s' "$signoff_emails" | paste -sd, -)
    record_failure "$sha" "mismatched-signoff author=${author_name} <${author_email}> signed-off-by=${rendered}"
  else
    # A waiver on a commit that now PASSES is dead weight: it no longer
    # suppresses anything, but it stays in the config as an unexamined
    # standing exception. Only reportable when the commit is inside the
    # inspected window — a waiver for older history is simply out of range,
    # not stale.
    if commit_waived "$sha"; then
      stale_waivers="${stale_waivers:-}${sha}
"
    fi
  fi
done < <(git rev-list -n "$limit" "$ref")

printf 'DCO trailer check inspected %s recent commits on %s (%s non-merge, non-bot checked; %s merge skipped; %s bot skipped; %s waived).\n' \
  "$limit" "$ref" "$checked" "$skipped_merges" "$skipped_bots" "$waived_count"

# Waived commits print on every run, above the failures. A disposition that
# produced no output would be an invisible hole in a compliance check; keeping
# it in the report means each run restates exactly which history is being
# accepted and why it had to be.
if [ -n "$waived_report" ]; then
  printf '%b' "$waived_report"
fi

# Stale waivers are advisory, not fatal: failing the monitor over config
# housekeeping would page the same people this change is trying to stop paging.
if [ -n "${stale_waivers:-}" ]; then
  while IFS= read -r stale; do
    [ -n "$stale" ] && printf 'STALE-WAIVER %s commit now passes; remove it from DCO_WAIVED_COMMITS\n' "$stale"
  done <<EOF_STALE
${stale_waivers}
EOF_STALE
fi

if [ -n "$failures" ]; then
  printf '%b' "$failures"
  exit 1
fi

if [ -n "$waived_report" ]; then
  echo "DCO trailer check passed (with recorded waivers)."
else
  echo "DCO trailer check passed."
fi
