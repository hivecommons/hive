package watchdog

import (
	"fmt"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

// RFC #4665 defaults. The zero value of every governor.watchdog config field
// resolves to these, so a hive that never mentions the block gets exactly the
// RFC's proposed behavior.
const (
	// DefaultProbeInterval is the minimum gap between watchdog sweeps.
	DefaultProbeInterval = 300 * time.Second
	// DefaultStuckOverlayAfter is how long a modal may sit unanswered.
	DefaultStuckOverlayAfter = 10 * time.Minute
	// DefaultShellPromptAfter is the dead-pane (bare shell / silent) grace.
	DefaultShellPromptAfter = 5 * time.Minute
	// DefaultCrashLoopAfter is the consecutive-failure escalation threshold.
	DefaultCrashLoopAfter = 5
	// DefaultHealthyReset is the continuous-ready window that clears the
	// failure counter.
	DefaultHealthyReset = 30 * time.Minute
	// DefaultNoProductionFor is the zero-production window that flips
	// Producing to False.
	DefaultNoProductionFor = 6 * time.Hour
	// DefaultRestartTimeout hard-bounds one restart attempt so a wedged
	// restart/kick path (failure mode 4 of the RFC) can never block the
	// reconciler.
	DefaultRestartTimeout = 90 * time.Second
)

// defaultBackoff is the RFC restart ladder; the last entry is the cap.
func defaultBackoff() []time.Duration {
	return []time.Duration{
		1 * time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		16 * time.Minute,
	}
}

// Settings is the fully resolved, validated form of config.WatchdogConfig.
type Settings struct {
	Enabled           bool
	ProbeInterval     time.Duration
	StuckOverlayAfter time.Duration
	ShellPromptAfter  time.Duration
	Backoff           []time.Duration
	CrashLoopAfter    int
	HealthyReset      time.Duration
	NoProductionFor   time.Duration
	AuthProbe         bool
	RestartTimeout    time.Duration
}

// DefaultSettings returns the RFC #4665 defaults.
func DefaultSettings() Settings {
	return Settings{
		Enabled:           true,
		ProbeInterval:     DefaultProbeInterval,
		StuckOverlayAfter: DefaultStuckOverlayAfter,
		ShellPromptAfter:  DefaultShellPromptAfter,
		Backoff:           defaultBackoff(),
		CrashLoopAfter:    DefaultCrashLoopAfter,
		HealthyReset:      DefaultHealthyReset,
		NoProductionFor:   DefaultNoProductionFor,
		AuthProbe:         true,
		RestartTimeout:    DefaultRestartTimeout,
	}
}

// SettingsFrom resolves a governor.watchdog config block into Settings.
// Invalid values never disable the watchdog silently: each problem is
// reported in the returned slice and the corresponding RFC default applies,
// so a typo in one duration cannot turn self-healing off fleet-wide.
func SettingsFrom(cfg config.WatchdogConfig) (Settings, []error) {
	s := DefaultSettings()
	var errs []error

	s.Enabled = cfg.WatchdogEnabled()
	s.AuthProbe = cfg.AuthProbeEnabled()

	if cfg.ProbeIntervalS < 0 {
		errs = append(errs, fmt.Errorf("watchdog.probe_interval_s: negative value %d, using default %v", cfg.ProbeIntervalS, DefaultProbeInterval))
	} else if cfg.ProbeIntervalS > 0 {
		s.ProbeInterval = time.Duration(cfg.ProbeIntervalS) * time.Second
	}

	s.StuckOverlayAfter = resolveDuration("watchdog.liveness.stuck_overlay_after", cfg.Liveness.StuckOverlayAfter, DefaultStuckOverlayAfter, &errs)
	s.ShellPromptAfter = resolveDuration("watchdog.liveness.shell_prompt_after", cfg.Liveness.ShellPromptAfter, DefaultShellPromptAfter, &errs)
	s.HealthyReset = resolveDuration("watchdog.restart.healthy_reset", cfg.Restart.HealthyReset, DefaultHealthyReset, &errs)
	s.NoProductionFor = resolveDuration("watchdog.readiness.no_production_for", cfg.Readiness.NoProductionFor, DefaultNoProductionFor, &errs)

	if cfg.Restart.CrashLoopAfter < 0 {
		errs = append(errs, fmt.Errorf("watchdog.restart.crash_loop_after: negative value %d, using default %d", cfg.Restart.CrashLoopAfter, DefaultCrashLoopAfter))
	} else if cfg.Restart.CrashLoopAfter > 0 {
		s.CrashLoopAfter = cfg.Restart.CrashLoopAfter
	}

	if len(cfg.Restart.Backoff) > 0 {
		ladder := make([]time.Duration, 0, len(cfg.Restart.Backoff))
		for i, raw := range cfg.Restart.Backoff {
			d, err := time.ParseDuration(raw)
			if err != nil || d <= 0 {
				errs = append(errs, fmt.Errorf("watchdog.restart.backoff[%d]: invalid duration %q, using default ladder", i, raw))
				ladder = nil
				break
			}
			ladder = append(ladder, d)
		}
		if len(ladder) > 0 {
			s.Backoff = ladder
		}
	}

	return s, errs
}

// resolveDuration parses one optional duration field, falling back to def and
// recording the problem when the value is unparseable or non-positive.
func resolveDuration(field, raw string, def time.Duration, errs *[]error) time.Duration {
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		*errs = append(*errs, fmt.Errorf("%s: invalid duration %q, using default %v", field, raw, def))
		return def
	}
	return d
}

// backoffFor returns the restart delay for the Nth consecutive failure
// (1-based), clamped to the ladder's cap.
func (s Settings) backoffFor(failures int) time.Duration {
	if len(s.Backoff) == 0 {
		return 0
	}
	if failures < 1 {
		failures = 1
	}
	if failures > len(s.Backoff) {
		return s.Backoff[len(s.Backoff)-1]
	}
	return s.Backoff[failures-1]
}
