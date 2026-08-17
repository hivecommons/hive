#!/bin/bash
# gh-app-token.sh — Generate a GitHub App installation token for the hive.
# Refreshes automatically when called; tokens last 1 hour.
# Caches the token so repeated calls within the hour don't re-generate.
#
# Usage:
#   /usr/local/bin/gh-app-token.sh          # refreshes the cache; does NOT print the token
#   eval "$(/usr/local/bin/gh-app-token.sh --export)"  # exports GH_TOKEN
#   /usr/local/bin/gh-app-token.sh --scoped <tier> [repos]  # prints {"token":...} JSON

set -euo pipefail

APP_ID="${GH_APP_ID:?GH_APP_ID must be set (GitHub App → General → App ID)}"
INSTALLATION_ID="${GH_APP_INSTALLATION_ID:?GH_APP_INSTALLATION_ID must be set (org settings → Installed GitHub Apps → URL tail)}"
PRIVATE_KEY_FILE="${GH_APP_KEY_FILE:-/etc/hive/gh-app-key.pem}"
CACHE_FILE="/var/run/hive-metrics/gh-app-token.cache"
CACHE_MAX_AGE_SECONDS=3300  # refresh 5 min before expiry (tokens last 3600s)

# Mint a short-lived RS256 App JWT (9 minutes; GitHub's max is 10). Used both
# by the installation-token exchange below and by --scoped minting.
mint_app_jwt() {
  local now iat exp header payload signature
  now=$(date +%s)
  iat=$((now - 60))
  exp=$((now + 540))
  header=$(printf '%s' '{"alg":"RS256","typ":"JWT"}' | openssl base64 -e -A | tr '+/' '-_' | tr -d '=')
  payload=$(printf '%s' "{\"iat\":${iat},\"exp\":${exp},\"iss\":\"${APP_ID}\"}" | openssl base64 -e -A | tr '+/' '-_' | tr -d '=')
  signature=$(printf '%s' "${header}.${payload}" | openssl dgst -sha256 -sign "$PRIVATE_KEY_FILE" | openssl base64 -e -A | tr '+/' '-_' | tr -d '=')
  printf '%s.%s.%s' "$header" "$payload" "$signature"
}

# --scoped is handled BEFORE the shared full-token cache is consulted or
# written, and never touches $CACHE_FILE. It mints its own App JWT, so it has
# no need of the full installation token.
#
# Ordering is load-bearing: when this ran after the cache check, a warm cache
# short-circuited at "Check if cached token is still valid" and handed the
# caller the FULL unscoped installation token instead of the tier-scoped one
# it asked for — silently defeating the per-agent scoped-token scheme. And
# when the cache was cold, it minted and cached a full-privilege token as a
# side effect of a request that only ever wanted a scoped one.
if [ "${1:-}" = "--scoped" ]; then
  TIER="${2:?Usage: gh-app-token.sh --scoped <tier> [repos]}"
  REPOS="${3:-}"

  case "$TIER" in
    newcomer)
      PERMISSIONS='{"issues":"write","metadata":"read"}'
      ;;
    contributor)
      PERMISSIONS='{"issues":"write","contents":"write","pull_requests":"write","metadata":"read"}'
      ;;
    trusted)
      PERMISSIONS='{"issues":"write","contents":"write","pull_requests":"write","metadata":"read","checks":"read"}'
      ;;
    advisor)
      PERMISSIONS='{"issues":"read","metadata":"read"}'
      ;;
    *)
      echo "ERROR: unknown tier: $TIER (valid: newcomer, contributor, trusted, advisor)" >&2
      exit 1
      ;;
  esac

  SCOPED_BODY="{\"permissions\":${PERMISSIONS}}"
  if [ -n "$REPOS" ]; then
    REPO_ARRAY=$(echo "$REPOS" | tr ',' '\n' | sed 's/.*/"&"/' | paste -sd ',' -)
    SCOPED_BODY="{\"permissions\":${PERMISSIONS},\"repositories\":[${REPO_ARRAY}]}"
  fi

  SCOPED_JWT=$(mint_app_jwt)

  SCOPED_RESPONSE=$(curl -s -X POST \
    -H "Authorization: Bearer ${SCOPED_JWT}" \
    -H "Accept: application/vnd.github+json" \
    -d "$SCOPED_BODY" \
    "https://api.github.com/app/installations/${INSTALLATION_ID}/access_tokens")

  SCOPED_TOKEN=$(echo "$SCOPED_RESPONSE" | jq -r '.token // empty')
  SCOPED_EXPIRES=$(echo "$SCOPED_RESPONSE" | jq -r '.expires_at // empty')

  if [ -z "$SCOPED_TOKEN" ]; then
    echo "ERROR: Failed to mint scoped token for tier=$TIER" >&2
    echo "$SCOPED_RESPONSE" >&2
    exit 1
  fi

  echo "{\"token\":\"${SCOPED_TOKEN}\",\"expires_at\":\"${SCOPED_EXPIRES}\"}"
  exit 0
fi

# Check if cached token is still valid
if [ -f "$CACHE_FILE" ]; then
  cache_age=$(( $(date +%s) - $(stat -c %Y "$CACHE_FILE" 2>/dev/null || echo 0) ))
  if [ "$cache_age" -lt "$CACHE_MAX_AGE_SECONDS" ]; then
    if [ "${1:-}" = "--export" ]; then
      echo "export GH_TOKEN=$(cat "$CACHE_FILE")"
    else
      # Same contract as the freshly-minted path below: the token stays in the
      # cache file, it does not go to stdout.
      echo "token cached at ${CACHE_FILE}"
    fi
    exit 0
  fi
fi

JWT=$(mint_app_jwt)

# Exchange JWT for installation access token
RESPONSE=$(curl -s -X POST \
  -H "Authorization: Bearer ${JWT}" \
  -H "Accept: application/vnd.github+json" \
  "https://api.github.com/app/installations/${INSTALLATION_ID}/access_tokens")

TOKEN=$(echo "$RESPONSE" | jq -r '.token // empty')

if [ -z "$TOKEN" ]; then
  echo "ERROR: Failed to get installation token" >&2
  echo "$RESPONSE" >&2
  exit 1
fi

# Cache the token. Create it 0600 from the start (umask) rather than writing at
# the process umask and chmod-ing after: the old order left a window in which
# the full-privilege token was readable by every agent UID on the box. The
# chmod stays for caches created by earlier versions — ">" truncates in place
# and preserves the existing mode.
mkdir -p "$(dirname "$CACHE_FILE")"
(umask 077; printf '%s' "$TOKEN" > "$CACHE_FILE")
chmod 600 "$CACHE_FILE"

if [ "${1:-}" = "--export" ]; then
  echo "export GH_TOKEN=$TOKEN"
  exit 0
fi

# Do not echo the token to stdout — callers should read the cache file
# directly to avoid leaking it into logs or captured output.
echo "token cached at ${CACHE_FILE}"
