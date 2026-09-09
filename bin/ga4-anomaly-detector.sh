#!/bin/bash
# ga4-anomaly-detector.sh — Pre-compute GA4 error anomalies for the ci-maintainer agent.
# Compares recent error counts against 7-day baseline.
# Writes /var/run/hive-metrics/ga4-anomalies.json.
#
# Requires: GA4 service account key (path from hive-project.yaml → outreach.ga4.service_account_key)
# Falls back to "no data" output if GA4 is unavailable.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUTPUT_FILE="/var/run/hive-metrics/ga4-anomalies.json"
TMP_FILE="${OUTPUT_FILE}.tmp"
LOG="/var/log/kick-agents.log"
REAL_GH="/usr/bin/gh"

# Thresholds for bin/ga4_anomaly_lib.py's spike/drop arms. Overridable via env;
# defaults live as named constants in that module (DEFAULT_*).
GA4_ANOMALY_THRESHOLD="${GA4_ANOMALY_THRESHOLD:-}"
GA4_DROP_THRESHOLD="${GA4_DROP_THRESHOLD:-}"
GA4_DROP_MIN_BASELINE_DAILY="${GA4_DROP_MIN_BASELINE_DAILY:-}"

PROJECT_YAML="${HIVE_PROJECT_YAML:-/etc/hive/hive-project.yaml}"
if [ ! -f "$PROJECT_YAML" ]; then
  PROJECT_YAML="$(find "$(dirname "$(dirname "$0")")/examples" -name 'hive-project.yaml' -type f 2>/dev/null | head -1)"
fi

log() { echo "[$(date -Is)] GA4-ANOMALY $*" >> "$LOG"; }

# Read GA4 config from project yaml
GA4_CONFIG=$(python3 -c "
import yaml, sys, json
with open(sys.argv[1]) as f:
    cfg = yaml.safe_load(f)
ga4 = cfg.get('outreach', {}).get('ga4', {})
print(json.dumps(ga4))
" "$PROJECT_YAML" 2>/dev/null || echo '{}')

PROPERTY_ID=$(echo "$GA4_CONFIG" | python3 -c "import json,sys; print(json.load(sys.stdin).get('property_id',''))" 2>/dev/null)
SA_KEY=$(echo "$GA4_CONFIG" | python3 -c "import json,sys; print(json.load(sys.stdin).get('service_account_key',''))" 2>/dev/null)

if [ -z "$PROPERTY_ID" ] || [ -z "$SA_KEY" ] || [ ! -f "$SA_KEY" ]; then
  log "SKIP — GA4 not configured or service account key missing"
  python3 -c "
import json
from datetime import datetime, timezone
result = {
    'generated_at': datetime.now(timezone.utc).isoformat(),
    'status': 'unavailable',
    'reason': 'GA4 not configured or service account key missing',
    'anomalies': [],
    'summary': 'GA4 data unavailable — skipping anomaly detection'
}
print(json.dumps(result, indent=2))
" > "$OUTPUT_FILE"
  exit 0
fi

log "START — checking GA4 property $PROPERTY_ID"

# Use google-auth + requests to query GA4 Data API
python3 -c "
import json, sys, os
from datetime import datetime, timezone, timedelta

property_id = sys.argv[1]
sa_key_path = sys.argv[2]
output_path = sys.argv[3]
script_dir = sys.argv[4]

now = datetime.now(timezone.utc)

try:
    from google.oauth2 import service_account
    from google.analytics.data_v1beta import BetaAnalyticsDataClient
    from google.analytics.data_v1beta.types import RunReportRequest, DateRange, Dimension, Metric

    credentials = service_account.Credentials.from_service_account_file(
        sa_key_path,
        scopes=['https://www.googleapis.com/auth/analytics.readonly']
    )
    client = BetaAnalyticsDataClient(credentials=credentials)

    # Recent window (last 30 minutes approximated as today's last-hour data)
    recent_request = RunReportRequest(
        property=f'properties/{property_id}',
        date_ranges=[DateRange(start_date='today', end_date='today')],
        dimensions=[Dimension(name='eventName')],
        metrics=[Metric(name='eventCount')],
        dimension_filter={
            'filter': {
                'field_name': 'eventName',
                'string_filter': {'match_type': 'CONTAINS', 'value': 'error'}
            }
        }
    )
    recent_response = client.run_report(recent_request)

    # Same recent window, UNFILTERED. The spike arm above deliberately only
    # sees error events; the drop arm (#6430 item 4) needs real pages/events
    # too; a domain migration's traffic collapse is not an error event and
    # would otherwise be invisible to this detector entirely.
    recent_all_request = RunReportRequest(
        property=f'properties/{property_id}',
        date_ranges=[DateRange(start_date='today', end_date='today')],
        dimensions=[Dimension(name='eventName')],
        metrics=[Metric(name='eventCount')],
    )
    recent_all_response = client.run_report(recent_all_request)

    # Baseline (last 7 days)
    baseline_request = RunReportRequest(
        property=f'properties/{property_id}',
        date_ranges=[DateRange(start_date='7daysAgo', end_date='yesterday')],
        dimensions=[Dimension(name='eventName')],
        metrics=[Metric(name='eventCount')],
    )
    baseline_response = client.run_report(baseline_request)

    recent_events = {}
    for row in recent_response.rows:
        event_name = row.dimension_values[0].value
        count = int(row.metric_values[0].value)
        recent_events[event_name] = count

    recent_all_events = {}
    for row in recent_all_response.rows:
        event_name = row.dimension_values[0].value
        count = int(row.metric_values[0].value)
        recent_all_events[event_name] = count

    baseline_events = {}
    for row in baseline_response.rows:
        event_name = row.dimension_values[0].value
        count = int(row.metric_values[0].value)
        baseline_events[event_name] = count / 7.0

    sys.path.insert(0, script_dir)
    from ga4_anomaly_lib import (
        compute_anomalies,
        DEFAULT_ANOMALY_THRESHOLD,
        DEFAULT_DROP_THRESHOLD,
        DEFAULT_DROP_MIN_BASELINE_DAILY,
    )

    anomaly_threshold = float(os.environ.get('GA4_ANOMALY_THRESHOLD') or DEFAULT_ANOMALY_THRESHOLD)
    drop_threshold = float(os.environ.get('GA4_DROP_THRESHOLD') or DEFAULT_DROP_THRESHOLD)
    drop_min_baseline_daily = float(os.environ.get('GA4_DROP_MIN_BASELINE_DAILY') or DEFAULT_DROP_MIN_BASELINE_DAILY)

    anomalies = compute_anomalies(
        recent_events,
        recent_all_events,
        baseline_events,
        anomaly_threshold=anomaly_threshold,
        drop_threshold=drop_threshold,
        drop_min_baseline_daily=drop_min_baseline_daily,
    )

    spike_count = sum(1 for a in anomalies if a['type'] == 'spike')
    drop_count = sum(1 for a in anomalies if a['type'] == 'drop')
    if anomalies:
        summary = f'{len(anomalies)} GA4 anomalies detected ({spike_count} spike, {drop_count} drop)'
    else:
        summary = 'GA4 nominal — no anomalies'

    result = {
        'generated_at': now.isoformat(),
        'status': 'ok',
        'anomaly_count': len(anomalies),
        'anomalies': anomalies,
        'summary': summary
    }

except ImportError:
    result = {
        'generated_at': now.isoformat(),
        'status': 'unavailable',
        'reason': 'google-analytics-data library not installed',
        'anomalies': [],
        'summary': 'GA4 library missing — install google-analytics-data'
    }
except Exception as e:
    result = {
        'generated_at': now.isoformat(),
        'status': 'error',
        'reason': str(e)[:200],
        'anomalies': [],
        'summary': f'GA4 error: {str(e)[:100]}'
    }

with open(output_path, 'w') as f:
    json.dump(result, f, indent=2)

print(result['summary'])
" "$PROPERTY_ID" "$SA_KEY" "$TMP_FILE" "$SCRIPT_DIR"

mv "$TMP_FILE" "$OUTPUT_FILE" 2>/dev/null || true
log "DONE"
