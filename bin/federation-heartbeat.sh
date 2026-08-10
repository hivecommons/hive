#!/bin/bash
# federation-heartbeat.sh — Periodically sends this hive's live stats
# to the federation registry so contributors can see which hives need help.
#
# Called by: cron or systemd timer (every 5 minutes)
#
# Environment:
#   HIVE_FEDERATION_REGISTRY — URL of the registry hub (default: https://hive.kubestellar.io)
#   HIVE_FEDERATION_ID       — this hive's ID in the registry
#
# Reads from:
#   /var/run/hive-metrics/contributors.json — active contributor count + names
#   /var/run/hive-metrics/actionable.json   — actionable items count

set -euo pipefail

REGISTRY_URL="${HIVE_FEDERATION_REGISTRY:-https://hive.kubestellar.io}"
HIVE_ID="${HIVE_FEDERATION_ID:-}"
METRICS_DIR="${HIVE_METRICS_DIR:-/var/run/hive-metrics}"

if [[ -z "$HIVE_ID" ]]; then
  echo "HIVE_FEDERATION_ID not set — skipping heartbeat" >&2
  exit 0
fi

ACTIVE_CONTRIBUTORS=0
ACTIVE_CONTRIBUTOR_NAMES='[]'
if [[ -f "$METRICS_DIR/contributors.json" ]]; then
  ACTIVE_CONTRIBUTORS=$(python3 -c "
import json
try:
    with open('$METRICS_DIR/contributors.json') as f:
        d = json.load(f)
    print(len(d.get('active', [])))
except: print(0)
" 2>/dev/null)
  # Names make an honest cross-hive 'Theaters of Operation' possible: a dossier
  # can only claim a hive that reported the contributor by name. Capped so the
  # heartbeat body stays small.
  ACTIVE_CONTRIBUTOR_NAMES=$(python3 -c "
import json
try:
    with open('$METRICS_DIR/contributors.json') as f:
        d = json.load(f)
    names = [c.get('github_username', '') for c in d.get('active', [])]
    print(json.dumps([n for n in names if n][:100]))
except: print('[]')
" 2>/dev/null)
fi

ACTIONABLE_ITEMS=0
if [[ -f "$METRICS_DIR/actionable.json" ]]; then
  ACTIONABLE_ITEMS=$(python3 -c "
import json
try:
    with open('$METRICS_DIR/actionable.json') as f:
        d = json.load(f)
    print(len(d.get('issues', {}).get('items', [])))
except: print(0)
" 2>/dev/null)
fi

# Count enabled agents from config
ACTIVE_AGENTS=0
if [[ -f /etc/hive/config.env ]]; then
  AGENTS_LINE=$(grep '^AGENTS_ENABLED=' /etc/hive/config.env 2>/dev/null || true)
  if [[ -n "$AGENTS_LINE" ]]; then
    ACTIVE_AGENTS=$(echo "${AGENTS_LINE#*=}" | wc -w)
  fi
fi

curl -sf -X POST "${REGISTRY_URL}/api/hives/${HIVE_ID}/heartbeat" \
  -H "Content-Type: application/json" \
  -d "{\"active_contributors\":${ACTIVE_CONTRIBUTORS},\"active_contributor_names\":${ACTIVE_CONTRIBUTOR_NAMES},\"active_agents\":${ACTIVE_AGENTS},\"actionable_items\":${ACTIONABLE_ITEMS}}" \
  >/dev/null 2>&1 || echo "Heartbeat failed — registry may be unreachable" >&2
