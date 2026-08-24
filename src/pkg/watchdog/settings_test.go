package watchdog

import (
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

func TestSettingsFromZeroValueIsRFCDefaults(t *testing.T) {
	s, errs := SettingsFrom(config.WatchdogConfig{})
	if len(errs) != 0 {
		t.Fatalf("zero config must resolve without errors, got %v", errs)
	}
	want := DefaultSettings()
	if !s.Enabled || !s.AuthProbe {
		t.Fatal("watchdog and auth probe default on per the RFC")
	}
	if s.ProbeInterval != want.ProbeInterval ||
		s.StuckOverlayAfter != want.StuckOverlayAfter ||
		s.ShellPromptAfter != want.ShellPromptAfter ||
		s.CrashLoopAfter != want.CrashLoopAfter ||
		s.HealthyReset != want.HealthyReset ||
		s.NoProductionFor != want.NoProductionFor ||
		s.RestartTimeout != want.RestartTimeout {
		t.Fatalf("zero config resolved to %+v, want RFC defaults %+v", s, want)
	}
	if len(s.Backoff) != 5 || s.Backoff[0] != time.Minute || s.Backoff[4] != 16*time.Minute {
		t.Fatalf("default backoff ladder wrong: %v", s.Backoff)
	}
}

func TestSettingsFromExplicitValues(t *testing.T) {
	off := false
	cfg := config.WatchdogConfig{
		Enabled:        &off,
		ProbeIntervalS: 60,
		Liveness: config.WatchdogLivenessConfig{
			StuckOverlayAfter: "3m",
			ShellPromptAfter:  "90s",
		},
		Restart: config.WatchdogRestartConfig{
			Backoff:        []string{"30s", "1m"},
			CrashLoopAfter: 2,
			HealthyReset:   "10m",
		},
		Readiness: config.WatchdogReadinessConfig{NoProductionFor: "2h"},
		AuthProbe: &off,
	}
	s, errs := SettingsFrom(cfg)
	if len(errs) != 0 {
		t.Fatalf("valid config must not error: %v", errs)
	}
	if s.Enabled || s.AuthProbe {
		t.Fatal("explicit false must win")
	}
	if s.ProbeInterval != time.Minute || s.StuckOverlayAfter != 3*time.Minute ||
		s.ShellPromptAfter != 90*time.Second || s.CrashLoopAfter != 2 ||
		s.HealthyReset != 10*time.Minute || s.NoProductionFor != 2*time.Hour {
		t.Fatalf("explicit values not honored: %+v", s)
	}
	if len(s.Backoff) != 2 || s.Backoff[0] != 30*time.Second || s.Backoff[1] != time.Minute {
		t.Fatalf("explicit backoff not honored: %v", s.Backoff)
	}
}

func TestSettingsFromInvalidValuesFallBackLoudly(t *testing.T) {
	cfg := config.WatchdogConfig{
		ProbeIntervalS: -1,
		Liveness: config.WatchdogLivenessConfig{
			StuckOverlayAfter: "not-a-duration",
			ShellPromptAfter:  "-5m",
		},
		Restart: config.WatchdogRestartConfig{
			Backoff:        []string{"1m", "banana"},
			CrashLoopAfter: -3,
			HealthyReset:   "0s",
		},
		Readiness: config.WatchdogReadinessConfig{NoProductionFor: "6 parsecs"},
	}
	s, errs := SettingsFrom(cfg)
	// One error per invalid field: probe interval, two liveness durations,
	// backoff entry, crash-loop count, healthy reset, no-production-for.
	const wantErrs = 7
	if len(errs) != wantErrs {
		t.Fatalf("want %d config errors, got %d: %v", wantErrs, len(errs), errs)
	}
	want := DefaultSettings()
	if s.ProbeInterval != want.ProbeInterval || s.StuckOverlayAfter != want.StuckOverlayAfter ||
		s.ShellPromptAfter != want.ShellPromptAfter || s.CrashLoopAfter != want.CrashLoopAfter ||
		s.HealthyReset != want.HealthyReset || s.NoProductionFor != want.NoProductionFor {
		t.Fatalf("invalid fields must fall back to defaults: %+v", s)
	}
	if len(s.Backoff) != len(want.Backoff) {
		t.Fatalf("one bad backoff entry must restore the whole default ladder: %v", s.Backoff)
	}
	if !s.Enabled {
		t.Fatal("config errors must never disable the watchdog silently")
	}
}

func TestBackoffForClampsToLadderCap(t *testing.T) {
	s := DefaultSettings()
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, time.Minute}, // defensive floor
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{5, 16 * time.Minute},
		{6, 16 * time.Minute},
		{100, 16 * time.Minute},
	}
	for _, tc := range cases {
		if got := s.backoffFor(tc.failures); got != tc.want {
			t.Errorf("backoffFor(%d) = %v, want %v", tc.failures, got, tc.want)
		}
	}
	empty := Settings{}
	if got := empty.backoffFor(3); got != 0 {
		t.Errorf("empty ladder backoff = %v, want 0", got)
	}
}
