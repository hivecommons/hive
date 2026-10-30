package config

import "testing"

// TestGovernorHotspotCapsOrDefault pins the resolution of the merge-hotspot
// issue caps (governor.hotspot_max_open / governor.hotspot_max_per_pass):
// zero and negative values fall back to the defaults, positive values win.
func TestGovernorHotspotCapsOrDefault(t *testing.T) {
	var zero GovernorConfig
	if got := zero.HotspotMaxOpenOrDefault(); got != DefaultHotspotMaxOpen {
		t.Fatalf("HotspotMaxOpenOrDefault() zero = %d, want %d", got, DefaultHotspotMaxOpen)
	}
	if got := zero.HotspotMaxPerPassOrDefault(); got != DefaultHotspotMaxPerPass {
		t.Fatalf("HotspotMaxPerPassOrDefault() zero = %d, want %d", got, DefaultHotspotMaxPerPass)
	}

	neg := GovernorConfig{HotspotMaxOpen: -1, HotspotMaxPerPass: -3}
	if got := neg.HotspotMaxOpenOrDefault(); got != DefaultHotspotMaxOpen {
		t.Fatalf("HotspotMaxOpenOrDefault() negative = %d, want %d", got, DefaultHotspotMaxOpen)
	}
	if got := neg.HotspotMaxPerPassOrDefault(); got != DefaultHotspotMaxPerPass {
		t.Fatalf("HotspotMaxPerPassOrDefault() negative = %d, want %d", got, DefaultHotspotMaxPerPass)
	}

	set := GovernorConfig{HotspotMaxOpen: 5, HotspotMaxPerPass: 3}
	if got := set.HotspotMaxOpenOrDefault(); got != 5 {
		t.Fatalf("HotspotMaxOpenOrDefault() set = %d, want 5", got)
	}
	if got := set.HotspotMaxPerPassOrDefault(); got != 3 {
		t.Fatalf("HotspotMaxPerPassOrDefault() set = %d, want 3", got)
	}
}
