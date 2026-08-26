package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWatchdogConfigTriStateAccessors(t *testing.T) {
	var zero WatchdogConfig
	if !zero.WatchdogEnabled() || !zero.AuthProbeEnabled() {
		t.Fatal("absent enabled/auth_probe must default ON per RFC #4665")
	}
	off := false
	on := true
	if (WatchdogConfig{Enabled: &off}).WatchdogEnabled() {
		t.Fatal("explicit enabled:false must win")
	}
	if !(WatchdogConfig{Enabled: &on}).WatchdogEnabled() {
		t.Fatal("explicit enabled:true must win")
	}
	if (WatchdogConfig{AuthProbe: &off}).AuthProbeEnabled() {
		t.Fatal("explicit auth_probe:false must win")
	}
}

func TestWatchdogConfigYAMLRoundtrip(t *testing.T) {
	src := `
watchdog:
  enabled: true
  probe_interval_s: 120
  liveness:
    stuck_overlay_after: 10m
    shell_prompt_after: 5m
  restart:
    backoff: [1m, 2m, 4m]
    crash_loop_after: 4
    healthy_reset: 30m
  readiness:
    no_production_for: 6h
  auth_probe: false
`
	var gov GovernorConfig
	if err := yaml.Unmarshal([]byte(src), &gov); err != nil {
		t.Fatal(err)
	}
	w := gov.Watchdog
	if !w.WatchdogEnabled() || w.AuthProbeEnabled() {
		t.Fatalf("tri-states not parsed: %+v", w)
	}
	if w.ProbeIntervalS != 120 ||
		w.Liveness.StuckOverlayAfter != "10m" ||
		w.Liveness.ShellPromptAfter != "5m" ||
		w.Restart.CrashLoopAfter != 4 ||
		w.Restart.HealthyReset != "30m" ||
		w.Readiness.NoProductionFor != "6h" {
		t.Fatalf("watchdog block not parsed: %+v", w)
	}
	if len(w.Restart.Backoff) != 3 || w.Restart.Backoff[2] != "4m" {
		t.Fatalf("backoff ladder not parsed: %v", w.Restart.Backoff)
	}

	// An omitted block stays zero (defaults resolve at consumption time —
	// never materialized into a saved config).
	var empty GovernorConfig
	if err := yaml.Unmarshal([]byte("eval_interval_s: 300"), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Watchdog.Enabled != nil || empty.Watchdog.ProbeIntervalS != 0 {
		t.Fatalf("omitted watchdog block must stay zero: %+v", empty.Watchdog)
	}
}
