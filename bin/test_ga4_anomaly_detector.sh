#!/usr/bin/env bash
# Contract test for bin/ga4_anomaly_lib.py's compute_anomalies() (#6430 item 4).
#
# bin/ga4-anomaly-detector.sh only ever fired on INCREASES: an error-event
# count more than ANOMALY_THRESHOLD times its 7-day baseline, or more than 5
# occurrences with no baseline history at all. A domain migration is the
# opposite shape — real pages/events COLLAPSE, and the collapse is not itself
# an "error" event, so the detector's error-only recent-window filter could
# not have seen it either way. Nothing in the old code path could have flagged
# the traffic loss #6430 describes, and nothing would flag a recurrence.
#
# This is hermetic: bin/ga4_anomaly_lib.py has no imports beyond the standard
# library (no google-analytics-data, no network, no service account key), so
# these are plain Python assertions against synthetic recent/baseline dicts.
#
# Run: bash bin/test_ga4_anomaly_detector.sh
# Exit codes: 0 every case passes, 1 at least one failed.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LIB="${ROOT}/bin"

PASS=0
FAIL=0

ok() { printf '  \xe2\x9c\x93 %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf '  \xe2\x9c\x97 %s\n' "$1"; FAIL=$((FAIL + 1)); }

run_case() {
  # $1: description, $2: python snippet that must print exactly "PASS"
  local desc="$1" snippet="$2" out
  if out="$(python3 -c "
import sys
sys.path.insert(0, '${LIB}')
from ga4_anomaly_lib import compute_anomalies, DEFAULT_ANOMALY_THRESHOLD, DEFAULT_DROP_THRESHOLD, DEFAULT_DROP_MIN_BASELINE_DAILY
${snippet}
" 2>&1)"; then
    if [ "$out" = "PASS" ]; then
      ok "$desc"
    else
      bad "$desc: $out"
    fi
  else
    bad "$desc: python raised: $out"
  fi
}

printf '=== ga4_anomaly_lib.compute_anomalies contract (#6430) ===\n\n'

# --- pre-existing spike arm must be unchanged in shape and trigger condition ---

run_case "a recent error count over threshold*baseline flags a spike" '
anomalies = compute_anomalies({"error_x": 20}, {"error_x": 20}, {"error_x": 5.0})
assert len(anomalies) == 1, anomalies
a = anomalies[0]
assert a["type"] == "spike", a
assert a["event"] == "error_x", a
assert a["ratio"] == 4.0, a
print("PASS")
'

run_case "a recent error count at or below threshold*baseline does not flag" '
anomalies = compute_anomalies({"error_x": 9}, {"error_x": 9}, {"error_x": 5.0})
assert anomalies == [], anomalies
print("PASS")
'

run_case "an error event with no baseline history flags once over the min count" '
anomalies = compute_anomalies({"error_new": 6}, {"error_new": 6}, {})
assert len(anomalies) == 1, anomalies
assert anomalies[0]["type"] == "spike", anomalies
assert anomalies[0]["ratio"] == float("inf"), anomalies
print("PASS")
'

run_case "an error event with no baseline history at or below the min count does not flag" '
anomalies = compute_anomalies({"error_new": 5}, {"error_new": 5}, {})
assert anomalies == [], anomalies
print("PASS")
'

# --- new drop arm: the class of failure #6430 says was invisible ---

run_case "a real page collapsing below drop_threshold*baseline flags a drop" '
anomalies = compute_anomalies({}, {"/learn": 2}, {"/learn": 40.0})
assert len(anomalies) == 1, anomalies
a = anomalies[0]
assert a["type"] == "drop", a
assert a["event"] == "/learn", a
assert a["ratio"] == 0.05, a
print("PASS")
'

run_case "a page at or above drop_threshold*baseline does not flag" '
anomalies = compute_anomalies({}, {"/learn": 20}, {"/learn": 40.0})
assert anomalies == [], anomalies
print("PASS")
'

run_case "a low-volume baseline is skipped by the drop arm to avoid noise" '
anomalies = compute_anomalies({}, {"/rare": 0}, {"/rare": 2.0})
assert anomalies == [], anomalies
print("PASS")
'

run_case "a page exactly at the noise floor baseline is still eligible" '
anomalies = compute_anomalies({}, {"/edge": 1}, {"/edge": DEFAULT_DROP_MIN_BASELINE_DAILY})
assert len(anomalies) == 1, anomalies
assert anomalies[0]["type"] == "drop", anomalies
print("PASS")
'

run_case "an error-event spike and a real-page drop are both reported together" '
anomalies = compute_anomalies(
    {"error_x": 20},
    {"error_x": 20, "/learn": 2},
    {"error_x": 5.0, "/learn": 40.0},
)
types = sorted(a["type"] for a in anomalies)
assert types == ["drop", "spike"], anomalies
print("PASS")
'

run_case "a deep drop is high severity, a shallow drop is medium" '
deep = compute_anomalies({}, {"/deep": 1}, {"/deep": 100.0})
shallow = compute_anomalies({}, {"/shallow": 18}, {"/shallow": 40.0})
assert deep[0]["severity"] == "high", deep
assert shallow[0]["severity"] == "medium", shallow
print("PASS")
'

run_case "thresholds are overridable, not hardcoded" '
# The same recent/baseline pair flags with the default threshold and does not
# flag once the caller widens the floor — proving the ratio is compared
# against a parameter, not a literal baked into the function.
narrow = compute_anomalies({}, {"/x": 15}, {"/x": 40.0})
widened = compute_anomalies({}, {"/x": 15}, {"/x": 40.0}, drop_threshold=0.1)
assert len(narrow) == 1, narrow
assert widened == [], widened
print("PASS")
'

run_case "the output shape keeps event/recent_count/baseline_daily_avg/ratio/severity" '
anomalies = compute_anomalies({"error_x": 20}, {"error_x": 20}, {"error_x": 5.0})
required = {"type", "event", "recent_count", "baseline_daily_avg", "ratio", "severity"}
assert required.issubset(anomalies[0].keys()), anomalies
print("PASS")
'

printf '\npass=%d fail=%d\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
