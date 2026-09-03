package watchdog

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
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
	// DefaultBootGrace is how long after an agent's launch every dead verdict
	// is suppressed. It deliberately matches the agent manager's
	// cliBootGraceSeconds (60s), which matches the production cliReadyTimeout:
	// a CLI that is still booting has no session, no pane and no ready marker,
	// and restarting it underneath itself spawns a second concurrent CLI.
	// Kept numerically equal to that constant rather than importing it,
	// because pkg/agent imports pkg/watchdog and the reverse would cycle.
	DefaultBootGrace = 60 * time.Second
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

// WatchdogPauseEnv is the fleet-wide watchdog kill switch. Set it to a truthy
// value (1/true/yes/on) on the deployment and every hive that reads it drops
// from heal to observe on its next config resolve — conditions and alerts keep
// flowing, restarts and pauses stop.
//
// This mirrors the spoke-upgrade kill switch (hub: spokeUpgradesPaused) in
// purpose: an operator watching something go wrong across the fleet must be
// able to stop the automated actor without editing 55 per-hive configs or
// waiting for a redeploy. It is deliberately an ENV var rather than hub state
// because the watchdog runs spoke-side, where the hub's persisted pause file
// and settings store do not exist; a hive that cannot reach the hub must still
// honor the switch.
//
// It can only ever REDUCE authority: it never turns a watchdog ON, and it
// never promotes observe to heal.
const WatchdogPauseEnv = "HIVE_WATCHDOG_PAUSE"

// WatchdogActionPaused reports whether the fleet-wide kill switch is engaged.
// Read at every config resolve rather than cached at boot, so engaging it
// takes effect without a restart.
func WatchdogActionPaused() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(WatchdogPauseEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Mode is how much authority the reconciler has over the fleet.
type Mode string

const (
	// ModeOff — the reconciler does not run at all.
	ModeOff Mode = "off"
	// ModeObserve — classify, publish conditions, emit alerts, and log what a
	// restart/pause WOULD have done, but take no action. The default.
	ModeObserve Mode = "observe"
	// ModeHeal — full reconciliation: restart with backoff, escalate to pause.
	ModeHeal Mode = "heal"
)

// DefaultMode is observe, deliberately.
//
// This reconciler restarts agents on a 55-hive production fleet. Shipping it
// straight into heal would turn fleet-wide healing on the moment the image
// rolls, with probe_interval_s as the only throttle — the shape of the
// image-pull storm that put a wave size on spoke upgrades. Observe mode gives
// the operator the one thing a test fleet cannot: evidence of what this
// watchdog would have done to THEIR agents, gathered on their own fleet,
// before it is allowed to do it. Promotion to heal is then a decision made on
// data rather than on trust — the same progression the ACMM applies to agents,
// applied to the tooling that supervises them.
const DefaultMode = ModeObserve

// ParseMode resolves a configured mode string. Unrecognized values fall back
// to the default and report a problem, never to a MORE powerful mode: a typo
// must not silently grant the watchdog authority to restart agents.
func ParseMode(raw string) (Mode, bool) {
	switch Mode(strings.ToLower(strings.TrimSpace(raw))) {
	case ModeOff:
		return ModeOff, true
	case ModeObserve:
		return ModeObserve, true
	case ModeHeal:
		return ModeHeal, true
	}
	return DefaultMode, false
}

// Settings is the fully resolved, validated form of config.WatchdogConfig.
type Settings struct {
	// Mode is the authority level. Enabled is derived from it: a reconciler in
	// ModeOff does not run.
	Mode              Mode
	ProbeInterval     time.Duration
	StuckOverlayAfter time.Duration
	ShellPromptAfter  time.Duration
	BootGrace         time.Duration
	Backoff           []time.Duration
	CrashLoopAfter    int
	HealthyReset      time.Duration
	NoProductionFor   time.Duration
	AuthProbe         bool
	RestartTimeout    time.Duration
}

// Enabled reports whether the reconciler runs at all. Observe still runs — it
// is the evidence-gathering mode, not an off switch.
func (s Settings) Enabled() bool { return s.Mode != ModeOff }

// MayAct reports whether the reconciler may take fleet-changing action
// (restart, pause). Observe classifies and alerts but never acts.
func (s Settings) MayAct() bool { return s.Mode == ModeHeal }

// DefaultSettings returns the RFC #4665 defaults.
func DefaultSettings() Settings {
	return Settings{
		Mode:              DefaultMode,
		ProbeInterval:     DefaultProbeInterval,
		StuckOverlayAfter: DefaultStuckOverlayAfter,
		ShellPromptAfter:  DefaultShellPromptAfter,
		BootGrace:         DefaultBootGrace,
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

	// Mode resolution, most explicit first:
	//   1. an explicit `mode:` wins outright;
	//   2. otherwise the legacy `enabled:` maps forward — false means off,
	//      true/absent means observe (NOT heal: an existing config that never
	//      mentioned a mode never consented to fleet-wide restarts);
	//   3. otherwise the default, observe.
	if raw := strings.TrimSpace(cfg.Mode); raw != "" {
		mode, ok := ParseMode(raw)
		if !ok {
			errs = append(errs, fmt.Errorf("watchdog.mode: unrecognized value %q, using default %q (valid: off, observe, heal)", raw, DefaultMode))
		}
		s.Mode = mode
	} else if !cfg.WatchdogEnabled() {
		s.Mode = ModeOff
	} else {
		s.Mode = DefaultMode
	}

	// The fleet-wide kill switch outranks every per-hive config, so an
	// operator can stop all watchdog action without editing 55 hive.yamls or
	// waiting for a redeploy. It can only ever REDUCE authority.
	if WatchdogActionPaused() && s.Mode == ModeHeal {
		errs = append(errs, fmt.Errorf("watchdog.mode: heal downgraded to observe by the %s kill switch", WatchdogPauseEnv))
		s.Mode = ModeObserve
	}

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
