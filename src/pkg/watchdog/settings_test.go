package watchdog

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestSettingsFromZeroValueIsRFCDefaults(t *testing.T) {
	s, errs := SettingsFrom(config.WatchdogConfig{})
	if len(errs) != 0 {
		t.Fatalf("zero config must resolve without errors, got %v", errs)
	}
	want := DefaultSettings()
	if s.Mode != DefaultMode || !s.AuthProbe {
		t.Fatalf("zero config must default to mode %q with auth probe on, got mode %q authProbe=%v", DefaultMode, s.Mode, s.AuthProbe)
	}
	if s.MayAct() {
		t.Fatal("the default mode must NOT be allowed to act — observe is the default so an operator sees what it would do first")
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
	if s.Enabled() || s.Mode != ModeOff || s.AuthProbe {
		t.Fatal("explicit enabled:false must map to mode off, and auth_probe:false must win")
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
	if !s.Enabled() {
		t.Fatal("config errors must never disable the watchdog silently")
	}
}

// TestModeResolution covers the authority ladder: an explicit mode wins, the
// legacy enabled flag maps forward without ever granting heal, and an
// unrecognized value falls back to the default rather than up.
func TestModeResolution(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name    string
		cfg     config.WatchdogConfig
		want    Mode
		wantErr bool
	}{
		{"absent defaults to observe", config.WatchdogConfig{}, ModeObserve, false},
		{"explicit heal", config.WatchdogConfig{Mode: "heal"}, ModeHeal, false},
		{"explicit off", config.WatchdogConfig{Mode: "off"}, ModeOff, false},
		{"case and space insensitive", config.WatchdogConfig{Mode: "  HEAL "}, ModeHeal, false},
		{"legacy enabled:true maps to observe, never heal", config.WatchdogConfig{Enabled: &on}, ModeObserve, false},
		{"legacy enabled:false maps to off", config.WatchdogConfig{Enabled: &off}, ModeOff, false},
		{"explicit mode beats legacy enabled", config.WatchdogConfig{Mode: "heal", Enabled: &off}, ModeHeal, false},
		{"typo falls back to default, not up", config.WatchdogConfig{Mode: "healll"}, ModeObserve, true},
		{"a typo must not silently grant authority", config.WatchdogConfig{Mode: "HEAL!"}, ModeObserve, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, errs := SettingsFrom(tc.cfg)
			if s.Mode != tc.want {
				t.Fatalf("mode = %q, want %q", s.Mode, tc.want)
			}
			if gotErr := len(errs) > 0; gotErr != tc.wantErr {
				t.Fatalf("errs = %v, wantErr = %v", errs, tc.wantErr)
			}
		})
	}
}

// TestKillSwitchDowngradesHealToObserve asserts the fleet-wide switch can only
// ever REDUCE authority: it downgrades heal, and it never promotes off.
func TestKillSwitchDowngradesHealToObserve(t *testing.T) {
	t.Setenv(WatchdogPauseEnv, "true")

	s, errs := SettingsFrom(config.WatchdogConfig{Mode: "heal"})
	if s.Mode != ModeObserve {
		t.Fatalf("kill switch must downgrade heal to observe, got %q", s.Mode)
	}
	if s.MayAct() {
		t.Fatal("a paused watchdog must not be allowed to act")
	}
	if len(errs) == 0 {
		t.Fatal("the downgrade must be reported, not silent")
	}

	// It never promotes: off stays off.
	if s2, _ := SettingsFrom(config.WatchdogConfig{Mode: "off"}); s2.Mode != ModeOff {
		t.Fatalf("kill switch must never turn a watchdog on, got %q", s2.Mode)
	}

	// Positive control: without the switch, heal is heal.
	t.Setenv(WatchdogPauseEnv, "")
	if s3, _ := SettingsFrom(config.WatchdogConfig{Mode: "heal"}); s3.Mode != ModeHeal {
		t.Fatalf("without the kill switch heal must survive, got %q", s3.Mode)
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
