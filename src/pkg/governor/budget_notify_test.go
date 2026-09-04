package governor

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestProviderBudgetSuppressesProbesRatherThanDeadlocks(t *testing.T) {
	now := time.Date(2026, 8, 20, 3, 0, 0, 0, time.UTC)
	const probe = 30 * time.Minute

	cases := []struct {
		name       string
		latched    bool
		lastRebuff time.Time
		want       bool
	}{
		{"serving provider never suppresses", false, time.Time{}, false},
		{"rebuff seconds ago suppresses", true, now.Add(-5 * time.Second), true},
		{"rebuff just under the probe interval still suppresses", true, now.Add(-probe + time.Second), true},
		{"rebuff exactly at the probe interval probes", true, now.Add(-probe), false},
		{"hours-old rebuff probes", true, now.Add(-9 * time.Hour), false},
		{"latched with no stamp probes", true, time.Time{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProviderBudgetSuppresses(tc.latched, tc.lastRebuff, now, probe); got != tc.want {
				t.Errorf("ProviderBudgetSuppresses = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProviderBudgetNotifyOncePerLatchAndRecovery(t *testing.T) {
	var st ProviderBudgetNotifyState
	latch := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)

	if !st.ShouldSend(latch) {
		t.Fatal("the first cycle of a new clip must notify")
	}
	if st.ShouldSend(latch) {
		t.Fatal("the same clip notified twice")
	}
	if !st.Reset() {
		t.Fatal("the first healthy cycle after a notified clip must report recovery")
	}
	if st.Reset() {
		t.Fatal("healthy cycles after recovery must stay silent")
	}

	nextDay := latch.Add(24 * time.Hour)
	if !st.ShouldSend(nextDay) {
		t.Error("a new clip after a recovery must notify")
	}
	if st.ShouldSend(nextDay) {
		t.Error("the new clip notified twice")
	}
}

func TestProviderBudgetProbeReArmsOnRelease(t *testing.T) {
	var probe ProviderBudgetProbeState
	const interval = 30 * time.Minute
	rebuff := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)

	probeAt := rebuff.Add(interval + time.Minute)
	if ProviderBudgetSuppresses(true, probe.Freshest(rebuff), probeAt, interval) {
		t.Fatal("a stale rebuff must open the gate for a probe")
	}
	probe.MarkReleased(probeAt)

	for i := 1; i <= 5; i++ {
		now := probeAt.Add(time.Duration(i) * 5 * time.Minute)
		if !ProviderBudgetSuppresses(true, probe.Freshest(rebuff), now, interval) {
			t.Fatalf("cycle %d after a released probe leaked more kicks before the probe resolved", i)
		}
	}
	if ProviderBudgetSuppresses(true, probe.Freshest(rebuff), probeAt.Add(interval), interval) {
		t.Fatal("an interval after the released probe the gate must open again")
	}

	probe.Reset()
	if got := probe.Freshest(rebuff); !got.Equal(rebuff) {
		t.Fatalf("after reset Freshest = %v, want %v", got, rebuff)
	}
}

func TestProviderBudgetProbeIntervalDefault(t *testing.T) {
	var unset config.ProviderBudgetConfig
	if got := unset.EffectiveProbeInterval(); got != 30*time.Minute {
		t.Errorf("default probe interval = %v, want 30m", got)
	}
	set := config.ProviderBudgetConfig{ProbeIntervalS: 300}
	if got := set.EffectiveProbeInterval(); got != 5*time.Minute {
		t.Errorf("configured probe interval = %v, want 5m", got)
	}
	neg := config.ProviderBudgetConfig{ProbeIntervalS: -1}
	if got := neg.EffectiveProbeInterval(); got != 30*time.Minute {
		t.Errorf("negative probe interval = %v, want the 30m default", got)
	}
}
