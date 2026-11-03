#!/usr/bin/env bash
# twitch-channel-cta.sh — set the title, category and tags of the Hive Twitch
# channels so every stream page carries the "Request a Hive" call to action
# (#16236). Companion to the on-screen badge in src/pkg/webassets/kioskcta.
#
# Twitch has no API for About-tab panels; those are pasted by hand (see
# src/docs/streaming-the-activity-monitor.md, "Channel title, category, tags").
#
# Auth: Helix channel updates need a USER access token per broadcaster with
# the channel:manage:broadcast scope; an app token is not enough. Obtain one
# once per channel with the device-code grant (documented in the same doc)
# and export it as TWITCH_TOKEN_<LOGIN_UPPERCASED>, e.g.
# TWITCH_TOKEN_HIVECOMMONSFLEET. TWITCH_CLIENT_ID is required (Helix sends it
# as Client-Id). TWITCH_CLIENT_SECRET is accepted for symmetry with the
# device-code flow but is not used by this script, which never mints tokens.
#
# Tokens are only ever placed in request headers; nothing in this script
# prints them, and `set -x` is never enabled.
set -euo pipefail

HELIX="https://api.twitch.tv/helix"
# "Software and Game Development". Verified on each run against
# /search/categories unless --no-verify-category is given.
CATEGORY_ID="${TWITCH_CATEGORY_ID:-1469308723}"
CATEGORY_NAME="Software and Game Development"
# Twitch allows at most 10 tags of at most 25 characters each, letters and
# digits only (no spaces or punctuation).
TAGS=(OpenSource DevOps AI Kubernetes GitHub Automation Golang LiveCoding)
CTA_URL="${TWITCH_CTA_URL:-hive.hivecommons.dev/get-started}"

# login|title (title must stay <= 140 characters after the URL is appended).
CHANNELS=(
  "hivecommons|Hive: AI agents maintaining open source, live · Want one? ${CTA_URL}"
  "hivecommonsfleet|Hive fleet: every hosted hive's activity, live · Request yours → ${CTA_URL}"
  "hivecommonsmatrix|Hive matrix: code as it's pushed by the hive · Get a hive → ${CTA_URL}"
)

dry_run=0
verify_category=1
only=""

usage() {
  cat <<'USAGE'
Usage: src/scripts/twitch-channel-cta.sh [options]

Sets title, category and tags on the three Hive Twitch channels. Idempotent:
a channel already carrying the wanted values is left untouched.

Environment:
  TWITCH_CLIENT_ID                 required
  TWITCH_TOKEN_<LOGIN_UPPERCASED>  user access token per channel with the
                                   channel:manage:broadcast scope, e.g.
                                   TWITCH_TOKEN_HIVECOMMONS
  TWITCH_CATEGORY_ID               override the category id (default 1469308723)
  TWITCH_CTA_URL                   override the URL appended to each title

Options:
  --only <login>          Update one channel only
  --dry-run               Show what would change; send no PATCH
  --no-verify-category    Skip the /search/categories check of the id
  -h, --help              Show this help
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) dry_run=1 ;;
    --no-verify-category) verify_category=0 ;;
    --only) only="${2:-}"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

for tool in curl jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required" >&2; exit 2; }
done
: "${TWITCH_CLIENT_ID:?TWITCH_CLIENT_ID is required}"

# helix <token> <method> <path> [json-body]. Prints the response body; a
# non-2xx status is reported WITHOUT echoing the request headers.
helix() {
  local token="$1" method="$2" path="$3" body="${4:-}"
  local out status
  out=$(curl -sS -o - -w '\n%{http_code}' -X "$method" "${HELIX}${path}" \
    -H "Authorization: Bearer ${token}" \
    -H "Client-Id: ${TWITCH_CLIENT_ID}" \
    -H "Content-Type: application/json" \
    ${body:+--data "$body"})
  status="${out##*$'\n'}"
  out="${out%$'\n'*}"
  if [ "${status:0:1}" != "2" ]; then
    echo "helix ${method} ${path}: HTTP ${status}: $(printf '%s' "$out" | jq -r '.message // .' 2>/dev/null | head -c 200)" >&2
    return 1
  fi
  printf '%s' "$out"
}

# Tags: length, count and character class are checked locally so a typo
# fails here rather than as an opaque 400.
if [ "${#TAGS[@]}" -gt 10 ]; then echo "more than 10 tags" >&2; exit 2; fi
for t in "${TAGS[@]}"; do
  if [ "${#t}" -gt 25 ] || ! [[ "$t" =~ ^[A-Za-z0-9]+$ ]]; then
    echo "tag '$t' must be <= 25 letters/digits with no spaces or punctuation" >&2; exit 2
  fi
done
tags_json=$(printf '%s\n' "${TAGS[@]}" | jq -R . | jq -sc .)

category_checked=0
check_category() {
  local token="$1" found
  [ "$verify_category" -eq 1 ] && [ "$category_checked" -eq 0 ] || return 0
  found=$(helix "$token" GET "/search/categories?query=software&first=50" \
    | jq -r --arg id "$CATEGORY_ID" '.data[] | select(.id == $id) | .name')
  if [ "$found" != "$CATEGORY_NAME" ]; then
    echo "category id ${CATEGORY_ID} is not '${CATEGORY_NAME}' (got '${found:-nothing}'); set TWITCH_CATEGORY_ID or pass --no-verify-category" >&2
    return 1
  fi
  category_checked=1
}

update_channel() {
  local login="$1" title="$2" var token info broadcaster_id current patch
  var="TWITCH_TOKEN_$(printf '%s' "$login" | tr '[:lower:]' '[:upper:]')"
  token=$(printenv "$var" || true)
  if [ -z "$token" ]; then
    echo "${login}: ${var} is not set; skipping (see the device-code flow in the streaming doc)" >&2
    return 1
  fi
  if [ "${#title}" -gt 140 ]; then
    echo "${login}: title is ${#title} characters, Twitch allows 140" >&2
    return 1
  fi
  check_category "$token"
  info=$(helix "$token" GET "/users?login=${login}")
  broadcaster_id=$(printf '%s' "$info" | jq -r '.data[0].id // empty')
  if [ -z "$broadcaster_id" ]; then
    echo "${login}: no such Twitch user" >&2
    return 1
  fi
  current=$(helix "$token" GET "/channels?broadcaster_id=${broadcaster_id}" | jq -c '.data[0]')
  patch=$(jq -nc --arg title "$title" --arg game_id "$CATEGORY_ID" --argjson tags "$tags_json" \
    '{title: $title, game_id: $game_id, tags: $tags}')
  if [ "$(printf '%s' "$current" | jq -c '{title, game_id, tags: (.tags // [])}')" = "$(printf '%s' "$patch" | jq -c .)" ]; then
    echo "${login}: already up to date"
    return 0
  fi
  echo "${login}: title  $(printf '%s' "$current" | jq -r .title) -> ${title}"
  echo "${login}: game   $(printf '%s' "$current" | jq -r '.game_name + " (" + .game_id + ")"') -> ${CATEGORY_NAME} (${CATEGORY_ID})"
  echo "${login}: tags   $(printf '%s' "$current" | jq -r '(.tags // []) | join(",")') -> $(IFS=,; echo "${TAGS[*]}")"
  if [ "$dry_run" -eq 1 ]; then
    echo "${login}: dry run, not patching"
    return 0
  fi
  helix "$token" PATCH "/channels?broadcaster_id=${broadcaster_id}" "$patch" >/dev/null
  echo "${login}: updated"
}

rc=0
for entry in "${CHANNELS[@]}"; do
  login="${entry%%|*}"
  title="${entry#*|}"
  if [ -n "$only" ] && [ "$only" != "$login" ]; then continue; fi
  update_channel "$login" "$title" || rc=1
done
exit "$rc"
