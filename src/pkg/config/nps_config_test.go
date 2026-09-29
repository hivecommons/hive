package config

import "testing"

// TestNPSFeedbackEnabledDefaults pins the opt-in rule from issue #9610: only a
// hosted spoke defaults ON; a self-hosted or standalone hive defaults OFF.
func TestNPSFeedbackEnabledDefaults(t *testing.T) {
	t.Setenv(NPSEnabledEnvVar, "")
	cases := []struct {
		name string
		hub  HubConfig
		want bool
	}{
		{"hosted spoke defaults on", HubConfig{HiveType: HiveTypeHosted}, true},
		{"hosted spoke, case/space tolerant", HubConfig{HiveType: " Hosted "}, true},
		{"self-hosted defaults off", HubConfig{HiveType: "self-hosted"}, false},
		{"standalone (no type) defaults off", HubConfig{}, false},
		{"lite defaults off", HubConfig{HiveType: "lite"}, false},
		{"self-hosted explicit opt-in", HubConfig{NPSEnabled: boolPtr(true)}, true},
		{"hosted explicit opt-out", HubConfig{HiveType: HiveTypeHosted, NPSEnabled: boolPtr(false)}, false},
	}
	for _, tc := range cases {
		if got := tc.hub.NPSFeedbackEnabled(); got != tc.want {
			t.Errorf("%s: NPSFeedbackEnabled() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestNPSFeedbackEnabledEnvOverride proves HIVE_NPS_ENABLED outranks both the
// config field and the hosted default, in both directions, and that an
// unparseable value is ignored rather than read as false.
func TestNPSFeedbackEnabledEnvOverride(t *testing.T) {
	t.Setenv(NPSEnabledEnvVar, "false")
	if (HubConfig{HiveType: HiveTypeHosted, NPSEnabled: boolPtr(true)}).NPSFeedbackEnabled() {
		t.Error("HIVE_NPS_ENABLED=false did not override an enabled hosted config")
	}
	t.Setenv(NPSEnabledEnvVar, "on")
	if !(HubConfig{NPSEnabled: boolPtr(false)}).NPSFeedbackEnabled() {
		t.Error("HIVE_NPS_ENABLED=on did not override a disabled config")
	}
	t.Setenv(NPSEnabledEnvVar, "maybe")
	if !(HubConfig{HiveType: HiveTypeHosted}).NPSFeedbackEnabled() {
		t.Error("an unparseable HIVE_NPS_ENABLED must fall through to the hosted default")
	}
	if (HubConfig{}).NPSFeedbackEnabled() {
		t.Error("an unparseable HIVE_NPS_ENABLED must fall through to the standalone default (off)")
	}
}

func TestNPSHubLinked(t *testing.T) {
	cases := []struct {
		hub  HubConfig
		want bool
	}{
		{HubConfig{Enabled: true, URL: "https://hub.example"}, true},
		{HubConfig{Enabled: true, URL: "  "}, false},
		{HubConfig{Enabled: false, URL: "https://hub.example"}, false},
		{HubConfig{}, false},
	}
	for _, tc := range cases {
		if got := tc.hub.NPSHubLinked(); got != tc.want {
			t.Errorf("NPSHubLinked(%+v) = %v, want %v", tc.hub, got, tc.want)
		}
	}
}
