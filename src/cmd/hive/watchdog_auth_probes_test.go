package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// watchdogAuthProbes adapts the rotation package's provider probers (#4608)
// for the watchdog. Every provider the rotation subsystem can probe must be
// represented exactly once, keyed by the prober's own Provider() name — a
// missing key silently exempts that provider's credentials from watchdog
// verification, and a wrong key orphans the probe.
func TestWatchdogAuthProbesCoversAllProviders(t *testing.T) {
	probes := watchdogAuthProbes(&config.Config{})

	wantProviders := []string{"anthropic", "openai", "google", "deepseek"}
	if len(probes) != len(wantProviders) {
		t.Fatalf("got %d probes, want %d (%v)", len(probes), len(wantProviders), wantProviders)
	}
	for _, provider := range wantProviders {
		if _, ok := probes[provider]; !ok {
			t.Errorf("provider %q has no watchdog auth probe — its credentials would go unverified", provider)
		}
	}
}
