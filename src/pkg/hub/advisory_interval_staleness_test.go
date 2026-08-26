package hub

import (
	"strings"
	"testing"
	"time"
)

// #4820 invariant 2: a hive whose operator deliberately slowed the advisory
// cadence (governor.advisory.update_interval_s, reported over the heartbeat)
// must not false-alarm as wedged. The staleness baseline becomes
// max(advisoryStaleThreshold, 2× the reported interval); hives reporting no
// interval keep the default threshold exactly as before.

// TestAdvisoryStale_LongIntervalHealthyNotStale: a 2-hour cadence posting on
// schedule ages past the 90-minute default between posts — the widened
// baseline (4h) must keep it quiet.
func TestAdvisoryStale_LongIntervalHealthyNotStale(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryUpdateIntervalS = 7200 // 2h configured cadence
	e.AdvisoryLastPostedAt = rfc3339Ago(100 * time.Minute)

	if stale, reason := advisoryStale(e, advNow); stale {
		t.Fatalf("a healthy 2h cadence 100min after its last post must not be stale, got %q", reason)
	}
}

// TestAdvisoryStale_LongIntervalGenuinelyWedgedStillFlags: the widened
// baseline is 2× the interval, not infinity — a 2h-cadence hive silent for 5
// hours HAS missed windows and must still alarm.
func TestAdvisoryStale_LongIntervalGenuinelyWedgedStillFlags(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryUpdateIntervalS = 7200
	e.AdvisoryLastPostedAt = rfc3339Ago(5 * time.Hour)

	stale, reason := advisoryStale(e, advNow)
	if !stale {
		t.Fatal("a 2h-cadence hive silent for 5h must be flagged stale")
	}
	if !strings.Contains(reason, "has not updated since") {
		t.Errorf("reason = %q, want the aged-digest cause", reason)
	}
}

// TestAdvisoryStale_NoIntervalKeepsDefaultThreshold pins that nothing changes
// for existing fleets: an entry reporting no interval (0 — unset knob, or an
// old spoke) uses exactly advisoryStaleThreshold.
func TestAdvisoryStale_NoIntervalKeepsDefaultThreshold(t *testing.T) {
	if got := advisoryStaleThresholdFor(advisoryModeEntry()); got != advisoryStaleThreshold {
		t.Errorf("no interval: threshold = %v, want default %v", got, advisoryStaleThreshold)
	}

	e := advisoryModeEntry()
	e.AdvisoryLastPostedAt = rfc3339Ago(advisoryStaleThreshold + time.Minute)
	if stale, _ := advisoryStale(e, advNow); !stale {
		t.Error("default-cadence hive past the default threshold must still alarm — pre-#4820 behavior")
	}
}

// TestAdvisoryStaleThresholdFor_ShortIntervalNeverShrinks: an interval shorter
// than the default (e.g. the 30s floor) must not LOWER the baseline — the
// widening is one-directional.
func TestAdvisoryStaleThresholdFor_ShortIntervalNeverShrinks(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryUpdateIntervalS = 30
	if got := advisoryStaleThresholdFor(e); got != advisoryStaleThreshold {
		t.Errorf("30s interval: threshold = %v, want default %v (never below it)", got, advisoryStaleThreshold)
	}
}

// TestAdvisoryDiagnostics_LongIntervalAgedUsesSameBaseline keeps the
// diagnostics view consistent with the alarm: the same widened baseline
// decides "aged", so the fleet report cannot disagree with the pill.
func TestAdvisoryDiagnostics_LongIntervalAgedUsesSameBaseline(t *testing.T) {
	e := advisoryModeEntry()
	e.AdvisoryUpdateIntervalS = 7200
	e.AdvisoryLastPostedAt = rfc3339Ago(100 * time.Minute)

	d := diagnoseAdvisory(e, advNow)
	if d.Class != advisoryClassFresh {
		t.Errorf("healthy 2h-cadence hive diagnosed as %q, want %q", d.Class, advisoryClassFresh)
	}
	if d.HiddenStale {
		t.Error("a within-baseline digest must not count as hidden-stale")
	}
}
