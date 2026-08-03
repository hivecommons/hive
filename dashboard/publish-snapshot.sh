#!/usr/bin/env bash
# Build dashboard snapshots (light + classic) and push to the docs repo.
# Creates a PR and merges with a normal squash (auto-merge fallback) — never --admin.
#
# Produces:
#   public/live/hive/index.html        — default (light mode)
#   public/live/hive/light/index.html   — light mode
#   public/live/hive/classic/index.html — classic mode
#
# Usage: ./publish-snapshot.sh
# Env vars:
#   HIVE_DASHBOARD_URL  — dashboard URL (default: http://localhost:3001)
#   DOCS_REPO_DIR       — local clone of kubestellar/docs (default: /tmp/kubestellar-docs-snapshot)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DASHBOARD_URL="${HIVE_DASHBOARD_URL:-http://localhost:3001}"
DOCS_REPO="${DOCS_REPO_DIR:-/tmp/kubestellar-docs-snapshot}"
DOCS_REPO_SLUG="kubestellar/docs"

GH_APP_TOKEN_FILE="/var/run/hive-metrics/gh-app-token.cache"
if [ -f "$GH_APP_TOKEN_FILE" ]; then
  GH_APP_TOKEN=$(cat "$GH_APP_TOKEN_FILE")
  DOCS_REMOTE="https://x-access-token:${GH_APP_TOKEN}@github.com/${DOCS_REPO_SLUG}.git"
  export GH_TOKEN="$GH_APP_TOKEN"
else
  echo "ERROR: no GitHub App token at $GH_APP_TOKEN_FILE"
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

# Build all three snapshots from the same API data
mkdir -p public/live/hive/light public/live/hive/classic public/live/hive/api-docs

node "${SCRIPT_DIR}/build-snapshot.mjs" --mode light "$DASHBOARD_URL" public/live/hive/light/index.html
node "${SCRIPT_DIR}/build-snapshot.mjs" --mode classic "$DASHBOARD_URL" public/live/hive/classic/index.html
cp public/live/hive/light/index.html public/live/hive/index.html

# Build static Redoc API docs page with inline spec
SPEC_JSON=$(curl -sf "${DASHBOARD_URL}/api/openapi.json" || cat "${SCRIPT_DIR}/openapi.json")
cat > public/live/hive/api-docs/index.html <<REDOC_EOF
<!DOCTYPE html>
<html><head>
  <title>Hive API Reference</title>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <link href="https://fonts.googleapis.com/css?family=Montserrat:300,400,700|Roboto:300,400,700" rel="stylesheet">
  <style>body { margin: 0; padding: 0; }</style>
</head><body>
  <div id="redoc-container"></div>
  <script src="https://cdn.redoc.ly/redoc/latest/bundles/redoc.standalone.js"></script>
  <script>
    var spec = ${SPEC_JSON};
    Redoc.init(spec, {}, document.getElementById('redoc-container'));
  </script>
</body></html>
REDOC_EOF
echo "Redoc API docs written to public/live/hive/api-docs/index.html"

# Check if anything changed (tracked or untracked)
git add public/live/hive/
if git diff --cached --quiet -- public/live/hive/; then
  echo "No changes to snapshot — skipping."
  git reset HEAD -- public/live/hive/ >/dev/null 2>&1
  exit 0
fi
git reset HEAD -- public/live/hive/ >/dev/null 2>&1

TIMESTAMP=$(date -u '+%Y-%m-%d %H:%M UTC')
SNAPSHOT_BRANCH="chore/hive-snapshot-$(date -u '+%Y%m%d-%H%M%S')"

git checkout -b "$SNAPSHOT_BRANCH"
git add public/live/hive/
git commit -s -m "chore: update hive dashboard snapshot $TIMESTAMP"
git push origin "$SNAPSHOT_BRANCH"

PR_URL=$(gh pr create \
  --repo "$DOCS_REPO_SLUG" \
  --title "chore: hive dashboard snapshot $TIMESTAMP" \
  --body "Automated snapshot update from hive server." \
  --head "$SNAPSHOT_BRANCH" \
  --base main 2>&1)

PR_NUM=$(echo "$PR_URL" | grep -o '[0-9]*$')
echo "Created PR #${PR_NUM}: ${PR_URL}"

# Wait for Netlify deploy-preview before merging
NETLIFY_CHECK="netlify/kubestellar-docs/deploy-preview"
NETLIFY_TIMEOUT_SECONDS=600
NETLIFY_POLL_INTERVAL=15
elapsed=0
netlify_status="pending"

echo "Waiting for Netlify deploy-preview (timeout: ${NETLIFY_TIMEOUT_SECONDS}s)..."
while [ "$elapsed" -lt "$NETLIFY_TIMEOUT_SECONDS" ]; do
  checks_output=$(gh pr checks "$PR_NUM" --repo "$DOCS_REPO_SLUG" 2>/dev/null || true)
  netlify_line=$(echo "$checks_output" | grep -i "$NETLIFY_CHECK" || true)

  if [ -n "$netlify_line" ]; then
    if echo "$netlify_line" | grep -qi "pass"; then
      netlify_status="pass"
      break
    elif echo "$netlify_line" | grep -qi "fail"; then
      netlify_status="fail"
      break
    fi
  fi

  sleep "$NETLIFY_POLL_INTERVAL"
  elapsed=$((elapsed + NETLIFY_POLL_INTERVAL))
  echo "  ...waiting (${elapsed}s/${NETLIFY_TIMEOUT_SECONDS}s)"
done

if [ "$netlify_status" = "pass" ]; then
  echo "Netlify deploy-preview passed."
  # Never --admin: branch protection is the gate, not an obstacle. A plain
  # squash lands when required checks are green; otherwise queue auto-merge.
  if gh pr merge "$PR_NUM" --repo "$DOCS_REPO_SLUG" --squash --delete-branch 2>/dev/null; then
    echo "Snapshot published via PR #${PR_NUM}."
  else
    echo "Checks still pending — enabling auto-merge on PR #${PR_NUM}."
    gh pr merge "$PR_NUM" --repo "$DOCS_REPO_SLUG" --squash --auto --delete-branch
    echo "Auto-merge enabled on PR #${PR_NUM} — will merge when checks pass."
  fi
elif [ "$netlify_status" = "fail" ]; then
  echo "ERROR: Netlify deploy-preview FAILED for PR #${PR_NUM}. NOT merging."
  echo "Check: https://github.com/${DOCS_REPO_SLUG}/pull/${PR_NUM}"
  # Send ntfy alert if available
  if [ -n "${NTFY_TOPIC:-}" ]; then
    curl -s -d "Netlify deploy-preview failed for snapshot PR #${PR_NUM} — not merged" \
      -H "Title: Hive snapshot blocked" -H "Priority: high" \
      "${NTFY_SERVER:-https://ntfy.sh}/${NTFY_TOPIC}" >/dev/null 2>&1 || true
  fi
  exit 1
else
  echo "WARNING: Netlify deploy-preview timed out after ${NETLIFY_TIMEOUT_SECONDS}s for PR #${PR_NUM}. NOT merging."
  echo "Check: https://github.com/${DOCS_REPO_SLUG}/pull/${PR_NUM}"
  if [ -n "${NTFY_TOPIC:-}" ]; then
    curl -s -d "Netlify deploy-preview timed out for snapshot PR #${PR_NUM} — not merged" \
      -H "Title: Hive snapshot timeout" -H "Priority: default" \
      "${NTFY_SERVER:-https://ntfy.sh}/${NTFY_TOPIC}" >/dev/null 2>&1 || true
  fi
  exit 1
fi

# Reset back to main for next run
git checkout main
git reset --hard origin/main
