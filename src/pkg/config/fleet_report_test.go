package config

import "testing"

func TestFleetReportDryRunDefault(t *testing.T) {
	if !((FleetReportConfig{}).DryRun()) {
		t.Fatal("fleet reporting must dry-run by default")
	}
	if (FleetReportConfig{FileUpstream: true}).DryRun() {
		t.Fatal("file_upstream=true should explicitly opt in to writes")
	}
}

func TestFleetReportTriStateResolution(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name       string
		cfg        FleetReportConfig
		hubManaged bool
		want       bool
	}{
		{"unset self-hosted", FleetReportConfig{}, false, false},
		{"unset hosted", FleetReportConfig{}, true, true},
		{"legacy file_upstream self-hosted", FleetReportConfig{FileUpstream: true}, false, true},
		{"explicit false hosted", FleetReportConfig{Enabled: &off}, true, false},
		{"explicit false beats legacy", FleetReportConfig{Enabled: &off, FileUpstream: true}, true, false},
		{"explicit true self-hosted", FleetReportConfig{Enabled: &on}, false, true},
	}
	for _, tc := range cases {
		if got := tc.cfg.FilesUpstream(tc.hubManaged); got != tc.want {
			t.Errorf("%s: FilesUpstream(%v)=%v want %v", tc.name, tc.hubManaged, got, tc.want)
		}
	}
}

func TestFleetReportDefaults(t *testing.T) {
	var f FleetReportConfig
	if f.EffectiveRepo() != DefaultFleetReportRepo || f.EffectiveMaxPerDay() != defaultFleetReportMaxPerDay {
		t.Fatalf("defaults: repo=%q max=%d", f.EffectiveRepo(), f.EffectiveMaxPerDay())
	}
	f = FleetReportConfig{Repo: " acme/fork ", MaxPerDay: 2}
	if f.EffectiveRepo() != "acme/fork" || f.EffectiveMaxPerDay() != 2 {
		t.Fatalf("explicit: repo=%q max=%d", f.EffectiveRepo(), f.EffectiveMaxPerDay())
	}
}
