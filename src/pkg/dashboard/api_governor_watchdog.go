package dashboard

import (
	"fmt"
	"net/http"
	"time"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/watchdog"
)

// This file is the config surface for the agent self-healing watchdog
// (RFC #4665). It follows the api_governor_features.go pattern: the payload
// reports the RESOLVED settings rather than the raw YAML, so a hive that never
// wrote a `governor.watchdog` block still shows the values actually in force
// instead of a screen full of blanks.

// watchdogSettingBounds keep an operator from typing a value that defeats the
// feature. Named rather than inline so the handler and its tests agree, and so
// the reason for each bound is written down once.
const (
	// watchdogMinProbeInterval floors the sweep interval. Each sweep captures
	// a pane per agent and may run provider probes; letting an operator set
	// this to a few seconds would turn the throttle into a load generator.
	watchdogMinProbeInterval = 30
	// watchdogMaxProbeInterval caps it at a day — beyond this the watchdog
	// would observe so rarely that a dead agent could sit unhealed for longer
	// than any operator would call "self-healing".
	watchdogMaxProbeInterval = 86400
	// watchdogMinCrashLoopAfter is 1: give-up after a single failed restart is
	// aggressive but coherent. Zero would mean "give up before trying".
	watchdogMinCrashLoopAfter = 1
	// watchdogMaxCrashLoopAfter bounds the restart budget. Past this the cap
	// stops being a give-up and becomes the unbounded restarter it replaced.
	watchdogMaxCrashLoopAfter = 50
	// watchdogMinHealthyReset floors the recovery window. Below a minute an
	// agent could launder its failure counter between two sweeps, which is the
	// flap-laundering the continuous-ready window exists to prevent.
	watchdogMinHealthyReset = time.Minute
	// watchdogMaxHealthyReset caps it at a week.
	watchdogMaxHealthyReset = 7 * 24 * time.Hour
)

// watchdogConfigPayload reports the watchdog's RESOLVED settings for the
// Health tab, plus the backoff ladder as display-only strings and whether the
// fleet-wide kill switch is currently forcing a downgrade.
func watchdogConfigPayload(cfg *config.Config) map[string]any {
	if cfg == nil {
		return nil
	}
	settings, _ := watchdog.SettingsFrom(cfg.Governor.Watchdog)

	backoff := make([]string, 0, len(settings.Backoff))
	for _, d := range settings.Backoff {
		backoff = append(backoff, d.String())
	}

	return map[string]any{
		"mode":           string(settings.Mode),
		"probeIntervalS": int(settings.ProbeInterval / time.Second),
		"crashLoopAfter": settings.CrashLoopAfter,
		"healthyReset":   settings.HealthyReset.String(),
		"authProbe":      settings.AuthProbe,
		// Display-only: the ladder is a derived progression, and letting an
		// operator type an arbitrary one invites a 1-second cap.
		"backoff": backoff,
		// paused reports the fleet-wide kill switch. When it is engaged, a
		// saved mode of "heal" is still stored but runs as observe, and the UI
		// must say so rather than showing an authority the hive does not have.
		"paused":    watchdog.WatchdogActionPaused(),
		"pausedEnv": watchdog.WatchdogPauseEnv,
	}
}

// handleGovernorWatchdog persists the operator-editable watchdog settings.
// Write-through to hive.yaml, like every other governor section, so nobody has
// to hand-edit config to change how the watchdog behaves.
func (s *Server) handleGovernorWatchdog(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		Mode           string `json:"mode"`
		ProbeIntervalS int    `json:"probeIntervalS"`
		CrashLoopAfter int    `json:"crashLoopAfter"`
		HealthyReset   string `json:"healthyReset"`
		AuthProbe      *bool  `json:"authProbe"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := validateGovernorWatchdog(body.Mode, body.ProbeIntervalS, body.CrashLoopAfter, body.HealthyReset); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	wd := &s.deps.Config.Governor.Watchdog
	if body.Mode != "" {
		mode, _ := watchdog.ParseMode(body.Mode)
		wd.Mode = string(mode)
		// The legacy Enabled flag would otherwise keep overriding a mode set
		// here on the next load. Clearing it makes Mode the single source of
		// truth once an operator has touched this screen.
		wd.Enabled = nil //nolint:staticcheck // SA1019: intentional write to the deprecated field — this IS the migration off it, not accidental use.
	}
	if body.ProbeIntervalS > 0 {
		wd.ProbeIntervalS = body.ProbeIntervalS
	}
	if body.CrashLoopAfter > 0 {
		wd.Restart.CrashLoopAfter = body.CrashLoopAfter
	}
	if body.HealthyReset != "" {
		wd.Restart.HealthyReset = body.HealthyReset
	}
	if body.AuthProbe != nil {
		wd.AuthProbe = body.AuthProbe
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after watchdog update", "error", err)
	}
	// The mode change is itself an auditable event: it is the operator
	// granting or withdrawing this component's authority to restart agents.
	s.auditFromRequest(r, "config_governor_watchdog",
		auditDetail("section", "watchdog", "mode", wd.Mode), "")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// validateGovernorWatchdog rejects values that would defeat the feature rather
// than tune it. An unrecognized mode is refused outright here (unlike config
// loading, which falls back to observe and logs): a typo typed into a form is
// a mistake the operator can see and correct immediately.
func validateGovernorWatchdog(mode string, probeIntervalS, crashLoopAfter int, healthyReset string) error {
	if mode != "" {
		if _, ok := watchdog.ParseMode(mode); !ok {
			return fmt.Errorf("mode must be one of: %s, %s, %s", watchdog.ModeOff, watchdog.ModeObserve, watchdog.ModeHeal)
		}
	}
	if probeIntervalS != 0 && (probeIntervalS < watchdogMinProbeInterval || probeIntervalS > watchdogMaxProbeInterval) {
		return fmt.Errorf("probe interval must be between %d and %d seconds", watchdogMinProbeInterval, watchdogMaxProbeInterval)
	}
	if crashLoopAfter != 0 && (crashLoopAfter < watchdogMinCrashLoopAfter || crashLoopAfter > watchdogMaxCrashLoopAfter) {
		return fmt.Errorf("crash-loop threshold must be between %d and %d", watchdogMinCrashLoopAfter, watchdogMaxCrashLoopAfter)
	}
	if healthyReset != "" {
		d, err := time.ParseDuration(healthyReset)
		if err != nil {
			return fmt.Errorf("healthy reset must be a duration like 30m: %v", err)
		}
		if d < watchdogMinHealthyReset || d > watchdogMaxHealthyReset {
			return fmt.Errorf("healthy reset must be between %v and %v", watchdogMinHealthyReset, watchdogMaxHealthyReset)
		}
	}
	return nil
}
