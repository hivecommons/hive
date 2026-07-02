#!/bin/bash
# gh-app-token.sh — Generate a GitHub App installation token for the hive.
# Refreshes automatically when called; tokens last 1 hour.
# Caches the token so repeated calls within the hour don't re-generate.
#
# Usage:
#   /usr/local/bin/gh-app-token.sh          # prints token to stdout
#   eval "$(/usr/local/bin/gh-app-token.sh --export)"  # exports GH_TOKEN

set -euo pipefail

APP_ID="${GH_APP_ID:?GH_APP_ID must be set (GitHub App → General → App ID)}"
INSTALLATION_ID="${GH_APP_INSTALLATION_ID:?GH_APP_INSTALLATION_ID must be set (org settings → Installed GitHub Apps → URL tail)}"
PRIVATE_KEY_FILE="${GH_APP_KEY_FILE:-/etc/hive/gh-app-key.pem}"
CACHE_FILE="/var/run/hive-metrics/gh-app-token.cache"
CACHE_MAX_AGE_SECONDS=3300  # refresh 5 min before expiry (tokens last 3600s)

# Check if cached token is still valid
if [ -f "$CACHE_FILE" ]; then
  cache_age=$(( $(date +%s) - $(stat -c %Y "$CACHE_FILE" 2>/dev/null || echo 0) ))
  if [ "$cache_age" -lt "$CACHE_MAX_AGE_SECONDS" ]; then
    TOKEN=$(cat "$CACHE_FILE")
    if [ "${1:-}" = "--export" ]; then
      echo "export GH_TOKEN=$TOKEN"
    else
      echo "$TOKEN"
    fi
    exit 0
  fi
fi

# Generate JWT
NOW=$(date +%s)
IAT=$((NOW - 60))
EXP=$((NOW + 540))  # 9 minutes (max 10)

HEADER=$(echo -n '{"alg":"RS256","typ":"JWT"}' | openssl base64 -e -A | tr '+/' '-_' | tr -d '=')
PAYLOAD=$(echo -n "{\"iat\":${IAT},\"exp\":${EXP},\"iss\":\"${APP_ID}\"}" | openssl base64 -e -A | tr '+/' '-_' | tr -d '=')
SIGNATURE=$(echo -n "${HEADER}.${PAYLOAD}" | openssl dgst -sha256 -sign "$PRIVATE_KEY_FILE" | openssl base64 -e -A | tr '+/' '-_' | tr -d '=')

JWT="${HEADER}.${PAYLOAD}.${SIGNATURE}"

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

# Cache the token
mkdir -p "$(dirname "$CACHE_FILE")"
echo -n "$TOKEN" > "$CACHE_FILE"
chmod 600 "$CACHE_FILE"

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

  SCOPED_NOW=$(date +%s)
  SCOPED_IAT=$((SCOPED_NOW - 60))
  SCOPED_EXP=$((SCOPED_NOW + 540))
  SCOPED_HEADER=$(echo -n '{"alg":"RS256","typ":"JWT"}' | openssl base64 -e -A | tr '+/' '-_' | tr -d '=')
  SCOPED_PAYLOAD=$(echo -n "{\"iat\":${SCOPED_IAT},\"exp\":${SCOPED_EXP},\"iss\":\"${APP_ID}\"}" | openssl base64 -e -A | tr '+/' '-_' | tr -d '=')
  SCOPED_SIG=$(echo -n "${SCOPED_HEADER}.${SCOPED_PAYLOAD}" | openssl dgst -sha256 -sign "$PRIVATE_KEY_FILE" | openssl base64 -e -A | tr '+/' '-_' | tr -d '=')
  SCOPED_JWT="${SCOPED_HEADER}.${SCOPED_PAYLOAD}.${SCOPED_SIG}"

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

# Token is already cached to $CACHE_FILE (line 59-60). Do not echo it to
# stdout — callers should read the cache file directly to avoid leaking
# the token into logs or captured output.
echo "token cached at ${CACHE_FILE}"
