package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestFormatETA(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-3 * time.Minute, "due now"},
		{0, "due now"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "2m"},
		{12 * time.Minute, "12m"},
		{2 * time.Hour, "2h"},
		{65 * time.Minute, "1h 5m"},
	}
	for _, tc := range tests {
		if got := formatETA(tc.d); got != tc.want {
			t.Errorf("formatETA(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestComputeNextKickETAEmptyWhenNoKickScheduled(t *testing.T) {
	now := time.Now()
	for _, cad := range []config.Cadence{"", "pause", "off"} {
		if got := computeNextKickETA(&now, cad); got != "" {
			t.Errorf("computeNextKickETA(cadence=%q) = %q, want empty", cad, got)
		}
		if stamp := computeNextKickFromCadence(&now, cad); stamp != "" {
			t.Errorf("computeNextKickFromCadence(cadence=%q) = %q, want empty", cad, stamp)
		}
	}
}

func TestComputeNextKickETAIntervalCountsFromLastKick(t *testing.T) {
	last := time.Now().Add(-25 * time.Minute)
	if got := computeNextKickETA(&last, config.Cadence("30m")); got != "5m" {
		t.Errorf("computeNextKickETA = %q, want %q", got, "5m")
	}
	overdue := time.Now().Add(-90 * time.Minute)
	if got := computeNextKickETA(&overdue, config.Cadence("30m")); got != "due now" {
		t.Errorf("overdue ETA = %q, want %q", got, "due now")
	}
}
