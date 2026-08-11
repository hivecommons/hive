#!/usr/bin/env bash
# Multi-instance snapshot publisher for Hive v2.
#
# Publishes read-only dashboard snapshots to kubestellar.io/live/hive/<instance>/
# Each instance gets light + classic modes at:
#   public/live/hive/<instance>/index.html        (default = light)
#   public/live/hive/<instance>/light/index.html
#   public/live/hive/<instance>/classic/index.html
#
# Usage:
#   ./publish-snapshot-v2.sh <instance> <dashboard_url>
#
# Examples:
#   ./publish-snapshot-v2.sh bluefin http://192.168.4.85:3001
#   ./publish-snapshot-v2.sh console http://192.168.4.56:3001
#
# The default (no instance) path at /live/hive/ is a separate deployment
# managed by the original publish-snapshot.sh.
#
# Env vars:
#   DOCS_REPO_DIR  — local clone of kubestellar/docs (default: /tmp/kubestellar-docs-snapshot)

set -euo pipefail

export PATH="/usr/local/bin:$PATH"

# Hive's ACMM proxy intercepts TLS with its own CA. Append the proxy CA
# to the system bundle so git and node can verify HTTPS connections.
PROXY_CA="/data/proxy-ca.pem"
if [ -f "$PROXY_CA" ]; then
  COMBINED_CA="/tmp/ca-combined.pem"
  cat /etc/ssl/certs/ca-certificates.crt "$PROXY_CA" > "$COMBINED_CA" 2>/dev/null || true
  export GIT_SSL_CAINFO="$COMBINED_CA"
  export NODE_EXTRA_CA_CERTS="$COMBINED_CA"
fi
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Use the real gh binary, not the agent wrapper at /usr/local/bin/gh.
# /opt/hive/bin/gh-real is off-PATH so agents can't bypass ACMM enforcement.
REAL_GH="/opt/hive/bin/gh-real"
if [ ! -x "$REAL_GH" ]; then
  REAL_GH="/data/bin/gh"
fi
if [ ! -x "$REAL_GH" ]; then
  REAL_GH="$(command -v gh)"
fi

if [ $# -lt 2 ]; then
  echo "Usage: $0 <instance-name> <dashboard-url>"
  echo "  e.g.: $0 bluefin http://192.168.4.85:3001"
  exit 1
fi

INSTANCE="$1"
DASHBOARD_URL="$2"
DOCS_REPO="${DOCS_REPO_DIR:-/tmp/kubestellar-docs-snapshot}"
DOCS_REPO_SLUG="kubestellar/docs"
PUBLISH_PATH="public/live/hive/${INSTANCE}"
BASE_PATH="/live/hive/${INSTANCE}"

DOCS_TOKEN_FILE="/var/run/hive-metrics/gh-app-token-docs.cache"
FALLBACK_TOKEN_FILE="/var/run/hive-metrics/gh-app-token.cache"
if [ -f "$DOCS_TOKEN_FILE" ]; then
  GH_APP_TOKEN=$(cat "$DOCS_TOKEN_FILE")
elif [ -f "$FALLBACK_TOKEN_FILE" ]; then
  echo "WARN: docs token not found, falling back to default token"
  GH_APP_TOKEN=$(cat "$FALLBACK_TOKEN_FILE")
else
  echo "ERROR: no GitHub App token at $DOCS_TOKEN_FILE or $FALLBACK_TOKEN_FILE"
  exit 1
fi
DOCS_REMOTE="https://x-access-token:${GH_APP_TOKEN}@github.com/${DOCS_REPO_SLUG}.git"
export GH_TOKEN="$GH_APP_TOKEN"

echo "[${INSTANCE}] Snapshot from ${DASHBOARD_URL} → ${BASE_PATH}"

# Verify dashboard is reachable
if ! curl -sf --max-time 10 "${DASHBOARD_URL}/api/status" >/dev/null 2>&1; then
  echo "ERROR: dashboard at ${DASHBOARD_URL} is not reachable"
  exit 1
fi

# Ensure docs repo clone exists
if [ ! -d "$DOCS_REPO/.git" ]; then
  git clone --depth 1 --single-branch -b main "$DOCS_REMOTE" "$DOCS_REPO"
fi

cd "$DOCS_REPO"
git remote set-url origin "$DOCS_REMOTE"
git fetch origin main
git checkout main 2>/dev/null || git checkout -b main origin/main
git reset --hard origin/main

mkdir -p "${PUBLISH_PATH}/light" "${PUBLISH_PATH}/classic"

LIVE_HTML="${HIVE_DASHBOARD_HTML:-/opt/hive/proxy/public/index.html}"
if [ ! -f "$LIVE_HTML" ]; then
  LIVE_HTML="${SCRIPT_DIR}/index.html"
fi

node "${SCRIPT_DIR}/build-snapshot.mjs" \
  --mode light --base-path "$BASE_PATH" --html "$LIVE_HTML" \
  "$DASHBOARD_URL" "${PUBLISH_PATH}/light/index.html"

node "${SCRIPT_DIR}/build-snapshot.mjs" \
  --mode classic --base-path "$BASE_PATH" --html "$LIVE_HTML" \
  "$DASHBOARD_URL" "${PUBLISH_PATH}/classic/index.html"

cp "${PUBLISH_PATH}/light/index.html" "${PUBLISH_PATH}/index.html"

# Build static Redoc API docs page with inline spec
echo "[${INSTANCE}] Building Redoc API docs..."
mkdir -p "${PUBLISH_PATH}/api-docs"
SPEC_JSON=$(curl -sf --max-time 10 "${DASHBOARD_URL}/api/openapi.json" || cat "${SCRIPT_DIR}/openapi.json")
cat > "${PUBLISH_PATH}/api-docs/index.html" <<REDOC_EOF
<!DOCTYPE html>
<html><head>
  <title>Hive API Reference</title>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <link href="https://fonts.googleapis.com/css?family=Montserrat:300,400,700|Roboto:300,400,700" rel="stylesheet">
  <style>body { margin: 0; padding: 0; }</style>
</head><body>
  <div id="redoc-container"></div>
  <!-- Pinned to a specific Redoc version with a Subresource Integrity hash so a
       compromise of the CDN or a silent 'latest'-tag update cannot execute
       arbitrary script. crossorigin is required for SRI to be enforced on a
       cross-origin script. Pin matches v2/proxy/server.js. -->
  <script src="https://cdn.redoc.ly/redoc/v2.1.5/bundles/redoc.standalone.js"
          integrity="sha384-0GrsyTQc9Oqd8h+b2dbc4XdR2T/DYpy0tLNNstyx+LBMUyiBbcWPbEs9aRmUcaxD"
          crossorigin="anonymous"></script>
  <script>
    var spec = ${SPEC_JSON};
    Redoc.init(spec, {}, document.getElementById('redoc-container'));
  </script>
</body></html>
REDOC_EOF
echo "[${INSTANCE}] Redoc API docs written."

# Capture the leaderboard page as a static snapshot.
# The Go handler at /leaderboard serves a self-contained HTML page with
# baked-in data; we just fetch and save it.
echo "[${INSTANCE}] Capturing leaderboard..."
mkdir -p "${PUBLISH_PATH}/leaderboard"
LEADERBOARD_FETCH_TIMEOUT_S=10
if curl -sf --max-time "$LEADERBOARD_FETCH_TIMEOUT_S" "${DASHBOARD_URL}/leaderboard" -o "${PUBLISH_PATH}/leaderboard/index.html" 2>/dev/null; then
  # Strip interactive "Join the swarm" contribute link from snapshot
  python3 -c "
import re, sys
html = open(sys.argv[1]).read()
html = re.sub(r'<div[^>]*>\\s*<a[^>]*contribute-link[^>]*>.*?</a>\\s*</div>', '', html, flags=re.DOTALL)
open(sys.argv[1], 'w').write(html)
" "${PUBLISH_PATH}/leaderboard/index.html"
  echo "[${INSTANCE}] Leaderboard captured."
else
  echo "[${INSTANCE}] WARN: leaderboard fetch failed — skipping."
fi

# Stage first so both new and modified files are detected
git add "${PUBLISH_PATH}/"
if git diff --cached --quiet -- "${PUBLISH_PATH}/"; then
  echo "[${INSTANCE}] No changes — skipping."
  git reset HEAD -- "${PUBLISH_PATH}/" >/dev/null 2>&1
  exit 0
fi

TIMESTAMP=$(date -u '+%Y-%m-%d %H:%M UTC')
SNAPSHOT_BRANCH="chore/hive-snapshot-${INSTANCE}-$(date -u '+%Y%m%d-%H%M%S')"

git checkout -b "$SNAPSHOT_BRANCH"
git add "${PUBLISH_PATH}/"
git commit -s -m "chore: update hive snapshot (${INSTANCE}) ${TIMESTAMP}"
git push origin "$SNAPSHOT_BRANCH"

PR_URL=$($REAL_GH pr create \
  --repo "$DOCS_REPO_SLUG" \
  --title "chore: hive snapshot (${INSTANCE}) ${TIMESTAMP}" \
  --body "Automated snapshot update for **${INSTANCE}** from ${DASHBOARD_URL}." \
  --head "$SNAPSHOT_BRANCH" \
  --base main 2>&1)

PR_NUM=$(echo "$PR_URL" | grep -o '[0-9]*$')
echo "[${INSTANCE}] Created PR #${PR_NUM}: ${PR_URL}"

# Merge immediately — snapshot PRs are static HTML updates that don't need
# Netlify validation. Waiting for deploy-preview caused chronic timeouts
# and stale snapshot pileups.
echo "[${INSTANCE}] Merging snapshot PR #${PR_NUM}..."
# Never --admin: a plain squash lands when required checks are green;
# otherwise queue auto-merge and let branch protection decide.
if $REAL_GH pr merge "$PR_NUM" --repo "$DOCS_REPO_SLUG" --squash --delete-branch 2>/dev/null; then
  echo "[${INSTANCE}] Snapshot published via PR #${PR_NUM}."
else
  $REAL_GH pr merge "$PR_NUM" --repo "$DOCS_REPO_SLUG" --squash --auto --delete-branch
  echo "[${INSTANCE}] Auto-merge enabled on PR #${PR_NUM}."
fi

git checkout main
git reset --hard origin/main
