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
