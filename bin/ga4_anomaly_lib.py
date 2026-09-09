"""Pure, network-free anomaly computation for bin/ga4-anomaly-detector.sh.

Split out so it can be unit-tested (bin/test_ga4_anomaly_detector.sh) without a
live GA4 property, a service account key, or the google-analytics-data
library: this module has no imports beyond the standard library.

Two arms, both compared against the same 7-day daily baseline:

  * spike (pre-existing): an error-event's recent count is more than
    ANOMALY_THRESHOLD times its baseline daily average. This is what the
    detector has always caught.

  * drop (#6430 item 4): a domain migration's signature is the opposite
    shape — real pages/events COLLAPSE, which no ceiling-only check can see.
    An event/page whose recent count falls below DROP_THRESHOLD of its
    baseline daily average is flagged, provided the baseline itself carried
    enough volume to distinguish a real drop from noise
    (DROP_MIN_BASELINE_DAILY).

Every threshold here is a named constant with an env-overridable default —
see ga4-anomaly-detector.sh for the GA4_* environment variable names.
"""

# Recent-window count must exceed this multiple of the baseline daily average
# to flag a spike.
DEFAULT_ANOMALY_THRESHOLD = 2.0

# A spike anomaly is 'high' severity once its ratio exceeds the threshold by
# this additional multiple; otherwise 'medium'.
DEFAULT_SPIKE_HIGH_SEVERITY_MULTIPLE = 2.5

# An event with zero baseline history still flags (as 'high') once the recent
# count exceeds this many occurrences.
DEFAULT_SPIKE_MIN_COUNT_NO_BASELINE = 5

# Recent-window count must fall below this fraction of the baseline daily
# average to flag a drop.
DEFAULT_DROP_THRESHOLD = 0.5

# Events/pages with less than this much baseline daily volume are too noisy to
# reliably drop-detect (a page that normally gets 2 hits/day going to 0 is not
# a signal); they are skipped by the drop arm entirely.
DEFAULT_DROP_MIN_BASELINE_DAILY = 10.0

# A drop anomaly is 'high' severity once its ratio falls below the threshold
# by this additional fraction; otherwise 'medium'.
DEFAULT_DROP_HIGH_SEVERITY_FRACTION = 0.5


def compute_anomalies(
    recent_filtered_events,
    recent_all_events,
    baseline_daily_events,
    anomaly_threshold=DEFAULT_ANOMALY_THRESHOLD,
    spike_high_severity_multiple=DEFAULT_SPIKE_HIGH_SEVERITY_MULTIPLE,
    spike_min_count_no_baseline=DEFAULT_SPIKE_MIN_COUNT_NO_BASELINE,
    drop_threshold=DEFAULT_DROP_THRESHOLD,
    drop_min_baseline_daily=DEFAULT_DROP_MIN_BASELINE_DAILY,
    drop_high_severity_fraction=DEFAULT_DROP_HIGH_SEVERITY_FRACTION,
):
    """Return a list of anomaly dicts.

    recent_filtered_events: {event_name: count} for the recent window,
        filtered the way the existing spike arm expects (error events only).
    recent_all_events: {event_name: count} for the SAME recent window, but
        unfiltered — needed by the drop arm, since a traffic collapse is not
        an "error" event.
    baseline_daily_events: {event_name: daily_average_count} over the last 7
        days, unfiltered.
    """
    spikes = []
    for event, recent_count in recent_filtered_events.items():
        baseline_daily = baseline_daily_events.get(event, 0)
        if baseline_daily > 0:
            ratio = recent_count / baseline_daily
            if ratio > anomaly_threshold:
                spikes.append({
                    "type": "spike",
                    "event": event,
                    "recent_count": recent_count,
                    "baseline_daily_avg": round(baseline_daily, 1),
                    "ratio": round(ratio, 1),
                    "severity": "high" if ratio > anomaly_threshold * spike_high_severity_multiple else "medium",
                })
        elif recent_count > spike_min_count_no_baseline:
            spikes.append({
                "type": "spike",
                "event": event,
                "recent_count": recent_count,
                "baseline_daily_avg": 0,
                "ratio": float("inf"),
                "severity": "high",
            })
    spikes.sort(key=lambda a: a.get("ratio", 0), reverse=True)

    drops = []
    for event, baseline_daily in baseline_daily_events.items():
        if baseline_daily < drop_min_baseline_daily:
            continue
        recent_count = recent_all_events.get(event, 0)
        ratio = recent_count / baseline_daily
        if ratio < drop_threshold:
            drops.append({
                "type": "drop",
                "event": event,
                "recent_count": recent_count,
                "baseline_daily_avg": round(baseline_daily, 1),
                "ratio": round(ratio, 2),
                "severity": "high" if ratio < drop_threshold * drop_high_severity_fraction else "medium",
            })
    drops.sort(key=lambda a: a.get("ratio", 0))

    return spikes + drops
