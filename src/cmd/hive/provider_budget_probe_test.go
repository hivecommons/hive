package main

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/hub"
)

// TestProviderBudgetSuppressionProbesRatherThanDeadlocks is the regression test
// for the recovery deadlock in the first cut of #4294.
//
// The trap: the spend latch is cleared ONLY by a successful inference call, and
// inference calls happen ONLY when an agent is kicked. Suppressing every kick
// while latched therefore removed the only path back — on a hive whose sole
// kick source is the governor cadence, the provider's window reset (on
// whatever schedule the provider keeps) was
// unobservable and the hive stayed muted until an operator kicked it by hand or
// restarted the process, while its alert claimed it would resume by itself.
//
// The fix is that evidence expires. These cases pin that a fresh rebuff still
// suppresses (the saving is real) and a stale one does not (recovery is real).
func TestProviderBudgetSuppressionProbesRatherThanDeadlocks(t *testing.T) {
	now := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)
	const probe = 30 * time.Minute

	cases := []struct {
		name       string
		latched    bool
		lastRebuff time.Time
		want       bool
		why        string
	}{
		{
			name:       "serving provider never suppresses",
			latched:    false,
			lastRebuff: time.Time{},
			want:       false,
			why:        "no latch, no reason to withhold anything",
		},
		{
			name:       "rebuff seconds ago suppresses",
			latched:    true,
			lastRebuff: now.Add(-5 * time.Second),
			want:       true,
			why:        "the gateway just refused; every kick now is a run that cannot buy a token",
		},
		{
			name:       "rebuff just under the probe interval still suppresses",
			latched:    true,
			lastRebuff: now.Add(-probe + time.Second),
			want:       true,
			why:        "the evidence has not expired yet",
		},
		{
			name:       "rebuff exactly at the probe interval probes",
			latched:    true,
			lastRebuff: now.Add(-probe),
			want:       false,
			why:        "the boundary belongs to the probe: recovery must not need an extra cycle",
		},
		{
			name:       "hours-old rebuff probes",
			latched:    true,
			lastRebuff: now.Add(-9 * time.Hour),
			want:       false,
			why:        "THE DEADLOCK CASE — a day-long clip whose window has since reset must let a kick through to find out",
		},
		{
			name:       "latched with no stamp probes",
			latched:    true,
			lastRebuff: time.Time{},
			want:       false,
			why:        "an unstamped latch must not mute the hive forever; spending one run is the recoverable error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerBudgetSuppresses(tc.latched, tc.lastRebuff, now, probe); got != tc.want {
				t.Errorf("providerBudgetSuppresses = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestProviderBudgetProbeReSuppressesAfterAFailedProbe walks the full recovery
// loop, because the useful property is not any single decision but that the
// sequence terminates in both directions: still clipped costs one run per
// interval, reset resumes without an operator.
func TestProviderBudgetProbeReSuppressesAfterAFailedProbe(t *testing.T) {
	const probe = 30 * time.Minute
	t0 := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)

	// The clip lands. Kicks are withheld.
	if !providerBudgetSuppresses(true, t0, t0.Add(time.Minute), probe) {
		t.Fatal("a fresh clip must withhold kicks")
	}
	// Half an hour later the stamp is stale, so one cycle probes.
	probeAt := t0.Add(probe + time.Minute)
	if providerBudgetSuppresses(true, t0, probeAt, probe) {
		t.Fatal("a stale clip must probe — this is the deadlock")
	}
	// The probe is rebuffed: the stamp moves forward and suppression resumes
	// for another interval, so the burn is ~one run per interval, not all day.
	if !providerBudgetSuppresses(true, probeAt, probeAt.Add(time.Minute), probe) {
		t.Fatal("a failed probe must re-freshen the stamp and resume suppression")
	}
	// After the provider's window resets, the probe succeeds instead: the proxy
	// clears the latch, and with no latch there is nothing to suppress.
	if providerBudgetSuppresses(false, time.Time{}, probeAt.Add(2*probe), probe) {
		t.Fatal("a cleared latch must not suppress")
	}
}

// TestProviderBudgetNotifiesOncePerLatch pins that the high-priority
// notification fires on the CROSSING, not on the condition. runEvalCycle sees a
// latched signal on every cycle for as long as the provider stays clipped;
// paging on each of them would be a full day of identical high-priority
// notifications, which is what the dashboard banner is for.
func TestProviderBudgetNotifiesOncePerLatch(t *testing.T) {
	var st providerBudgetNotifyState
	latch := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)

	if !st.shouldSend(latch) {
		t.Fatal("the first cycle of a new clip must notify")
	}
	for i := 0; i < 200; i++ {
		if st.shouldSend(latch) {
			t.Fatalf("cycle %d notified again for the same clip", i+2)
		}
	}

	// Recovery, then a genuinely new clip the next day: the operator must be
	// told about that one too.
	st.reset()
	nextDay := latch.Add(24 * time.Hour)
	if !st.shouldSend(nextDay) {
		t.Error("a new clip after a recovery must notify again")
	}
	if st.shouldSend(nextDay) {
		t.Error("the new clip notified twice")
	}
}

// TestProviderBudgetProbeIntervalDefault pins the default an operator gets
// without configuring anything — the deadlock fix must not depend on config
// that no existing hive has.
func TestProviderBudgetProbeIntervalDefault(t *testing.T) {
	var unset config.ProviderBudgetConfig
	if got := unset.EffectiveProbeInterval(); got != 30*time.Minute {
		t.Errorf("default probe interval = %v, want 30m", got)
	}
	set := config.ProviderBudgetConfig{ProbeIntervalS: 300}
	if got := set.EffectiveProbeInterval(); got != 5*time.Minute {
		t.Errorf("configured probe interval = %v, want 5m", got)
	}
	// Zero and negative mean "unset", never "never probe": never probing is
	// exactly the deadlock, so it must not be reachable from config.
	neg := config.ProviderBudgetConfig{ProbeIntervalS: -1}
	if got := neg.EffectiveProbeInterval(); got != 30*time.Minute {
		t.Errorf("negative probe interval = %v, want the 30m default", got)
	}
}

// TestProviderBudgetProbeReArmsOnRelease pins the gap between "the stamp went
// stale" and "the probe's run produced evidence". An agent run takes minutes to
// reach its first inference call; at a five-minute eval cadence, a gate that
// keeps releasing kicks until the probe's rebuff finally lands would leak a
// kick per cycle — a slice of the very burn this feature exists to stop.
// Releasing a probe must therefore re-arm suppression immediately, with
// freshness measured from the release itself.
func TestProviderBudgetProbeReArmsOnRelease(t *testing.T) {
	var probe providerBudgetProbeState
	t.Cleanup(probe.reset)
	const interval = 30 * time.Minute
	rebuff := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)

	// Stale stamp: the gate opens for a probe.
	probeAt := rebuff.Add(interval + time.Minute)
	if providerBudgetSuppresses(true, probe.freshest(rebuff), probeAt, interval) {
		t.Fatal("a stale rebuff must open the gate for a probe")
	}
	probe.markReleased(probeAt)

	// The very next cycles: the probe's run is still in flight and has not
	// rebuffed yet, so lastRebuff is unchanged and stale — but the gate must
	// hold, because a probe already flew.
	for i := 1; i <= 5; i++ {
		now := probeAt.Add(time.Duration(i) * 5 * time.Minute)
		if !providerBudgetSuppresses(true, probe.freshest(rebuff), now, interval) {
			t.Fatalf("cycle %d after a released probe leaked more kicks before the probe resolved", i)
		}
	}

	// A full interval after the release with no new rebuff (the probe's run
	// died without ever calling inference, say): the next probe may fly.
	if providerBudgetSuppresses(true, probe.freshest(rebuff), probeAt.Add(interval), interval) {
		t.Fatal("an interval after the released probe the gate must open again")
	}

	// Recovery clears the stamp so a NEW latch starts its clock from its own
	// rebuffs rather than inheriting last week's probe time.
	probe.reset()
	if got := probe.freshest(rebuff); !got.Equal(rebuff) {
		t.Fatalf("after reset freshest = %v, want the rebuff %v", got, rebuff)
	}
}

// TestProviderBudgetNotifyResetReportsRecoveryOnce pins the recovery half of
// transition-only notification: the operator who was paged when the clip began
// is paged exactly once when it lifts — not every healthy cycle, and not at all
// on a hive that was never clipped.
func TestProviderBudgetNotifyResetReportsRecoveryOnce(t *testing.T) {
	var st providerBudgetNotifyState

	// Never latched: healthy cycles must not fabricate a recovery.
	if st.reset() {
		t.Fatal("a never-notified gate must not report a recovery")
	}

	latch := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	if !st.shouldSend(latch) {
		t.Fatal("a new clip must notify")
	}

	// The clip lifts: exactly one recovery crossing...
	if !st.reset() {
		t.Fatal("the first healthy cycle after a notified clip must report the recovery")
	}
	// ...and silence afterwards.
	for i := 0; i < 200; i++ {
		if st.reset() {
			t.Fatalf("healthy cycle %d reported the recovery again", i+2)
		}
	}
}

func TestProviderLimitHeartbeatFieldsFallsBackToAgentQuota(t *testing.T) {
	dashboard.SetInferenceBudgetProvider(nil)
	t.Cleanup(func() { dashboard.SetInferenceBudgetProvider(nil) })

	reason, rebuffs, hiveWide, names := providerLimitHeartbeatFields([]hub.AgentSummary{
		{Name: "guide", State: "running", QuotaExhausted: true},
		{Name: "scanner", State: "running", QuotaExhausted: true},
		{Name: "paused", State: "paused", Paused: true, QuotaExhausted: true},
		{Name: "supervisor"},
	})
	if rebuffs != 0 {
		t.Fatalf("rebuffs = %d, want 0 for pane-derived quota", rebuffs)
	}
	if hiveWide {
		t.Fatal("pane-derived quota must not be marked hive-wide")
	}
	if got := strings.Join(names, ","); got != "guide,scanner" {
		t.Fatalf("names = %q, want guide,scanner", got)
	}
	if reason != "2 agent(s) out of provider quota" {
		t.Fatalf("reason = %q, want provider quota count", reason)
	}
}
