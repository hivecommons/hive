package config

import "testing"

func TestReplanDefaultOn(t *testing.T) {
	// nil Enabled (key omitted) → enabled
	var rc ReplanConfig
	if !rc.IsEnabled() {
		t.Error("omitted replan.enabled must default to on")
	}
	// explicit false → disabled
	f := false
	rc.Enabled = &f
	if rc.IsEnabled() {
		t.Error("explicit replan.enabled=false must disable")
	}
	// explicit true → enabled
	tr := true
	rc.Enabled = &tr
	if !rc.IsEnabled() {
		t.Error("explicit replan.enabled=true must enable")
	}
}
