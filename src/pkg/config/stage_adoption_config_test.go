package config

import "testing"

// StageAdoptionConfig's tri-state booleans default on when unset (nil) and
// otherwise report the configured value.
func TestStageAdoptionConfigDefaults(t *testing.T) {
	f, tr := false, true
	cases := []struct {
		name              string
		cfg               StageAdoptionConfig
		enabled, inPlace  bool
		strictAttribution bool
	}{
		{name: "unset defaults on", cfg: StageAdoptionConfig{}, enabled: true, inPlace: true, strictAttribution: true},
		{name: "explicit true", cfg: StageAdoptionConfig{Enabled: &tr, InPlace: &tr, StrictAttribution: &tr}, enabled: true, inPlace: true, strictAttribution: true},
		{name: "explicit false", cfg: StageAdoptionConfig{Enabled: &f, InPlace: &f, StrictAttribution: &f}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.IsEnabled(); got != tc.enabled {
				t.Errorf("IsEnabled() = %v, want %v", got, tc.enabled)
			}
			if got := tc.cfg.InPlaceEnabled(); got != tc.inPlace {
				t.Errorf("InPlaceEnabled() = %v, want %v", got, tc.inPlace)
			}
			if got := tc.cfg.StrictAttributionEnabled(); got != tc.strictAttribution {
				t.Errorf("StrictAttributionEnabled() = %v, want %v", got, tc.strictAttribution)
			}
		})
	}
}
