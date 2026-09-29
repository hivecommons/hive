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

// TestValidNPSRelayURL pins the relay URL rule (issue #9619): https only, or
// plain http to loopback, and never a URL that could smuggle the bearer token
// into a query string or userinfo.
func TestValidNPSRelayURL(t *testing.T) {
	cases := map[string]string{
		"":                                     "",
		"   ":                                  "",
		"https://docs.hivecommons.dev/api/nps": "https://docs.hivecommons.dev/api/nps",
		"https://relay.example/api/nps/":       "https://relay.example/api/nps",
		" https://relay.example ":              "https://relay.example",
		"http://127.0.0.1:8080/api/nps":        "http://127.0.0.1:8080/api/nps",
		"http://localhost:9000":                "http://localhost:9000",
		"http://[::1]:9000":                    "http://[::1]:9000",
		"http://relay.example/api/nps":         "",
		"http://10.0.0.5/api/nps":              "",
		"ftp://relay.example":                  "",
		"https://user:pw@relay.example":        "",
		"https://relay.example/api/nps?t=1":    "",
		"https://relay.example/api/nps#frag":   "",
		"relay.example/api/nps":                "",
		"https://":                             "",
		"://bad":                               "",
	}
	for in, want := range cases {
		if got := ValidNPSRelayURL(in); got != want {
			t.Errorf("ValidNPSRelayURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNPSRelaySettingsPrecedence proves each relay setting defaults to empty
// (relay disabled), reads the config field, and is overridden by its env var.
func TestNPSRelaySettingsPrecedence(t *testing.T) {
	t.Setenv(NPSRelayURLEnvVar, "")
	t.Setenv(NPSRelayTokenEnvVar, "")
	t.Setenv(NPSRelayPullSecretEnvVar, "")

	var empty HubConfig
	if empty.EffectiveNPSRelayURL() != "" || empty.EffectiveNPSRelayToken() != "" || empty.EffectiveNPSRelayPullSecret() != "" {
		t.Fatal("relay settings must default to empty")
	}
	if empty.NPSRelayConfigured() {
		t.Fatal("an empty config must not report the relay as configured")
	}

	cfg := HubConfig{
		NPSRelayURL:        "https://cfg.example/api/nps",
		NPSRelayToken:      " cfg-token ",
		NPSRelayPullSecret: "cfg-pull",
	}
	if got := cfg.EffectiveNPSRelayURL(); got != "https://cfg.example/api/nps" {
		t.Errorf("config URL = %q", got)
	}
	if got := cfg.EffectiveNPSRelayToken(); got != "cfg-token" {
		t.Errorf("config token = %q, want trimmed cfg-token", got)
	}
	if got := cfg.EffectiveNPSRelayPullSecret(); got != "cfg-pull" {
		t.Errorf("config pull secret = %q", got)
	}
	if !cfg.NPSRelayConfigured() {
		t.Error("URL + token must report the relay as configured")
	}

	t.Setenv(NPSRelayURLEnvVar, "https://env.example/api/nps")
	t.Setenv(NPSRelayTokenEnvVar, "env-token")
	t.Setenv(NPSRelayPullSecretEnvVar, "env-pull")
	if got := cfg.EffectiveNPSRelayURL(); got != "https://env.example/api/nps" {
		t.Errorf("env URL = %q", got)
	}
	if got := cfg.EffectiveNPSRelayToken(); got != "env-token" {
		t.Errorf("env token = %q", got)
	}
	if got := cfg.EffectiveNPSRelayPullSecret(); got != "env-pull" {
		t.Errorf("env pull secret = %q", got)
	}

	// An invalid env URL disables the relay rather than falling back.
	t.Setenv(NPSRelayURLEnvVar, "http://relay.example")
	if cfg.EffectiveNPSRelayURL() != "" || cfg.NPSRelayConfigured() {
		t.Error("a plain-http non-loopback relay URL must disable the relay")
	}

	// A URL without a token is not configured.
	t.Setenv(NPSRelayURLEnvVar, "")
	t.Setenv(NPSRelayTokenEnvVar, "")
	if (HubConfig{NPSRelayURL: "https://cfg.example"}).NPSRelayConfigured() {
		t.Error("a relay URL without an install token must not report configured")
	}
}
