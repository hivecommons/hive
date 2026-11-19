package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func clearAskEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		EnvAskEnabled, EnvAskMode, EnvAskKapaProjectID, EnvAskKapaIntegrationID, EnvAskKapaAPIKeyFile,
		EnvAskSendSectionLabel, EnvAskLimitPersonBurst, EnvAskLimitPersonPerHour, EnvAskLimitHivePerHour,
		EnvAskLimitGlobalPerMinute, EnvAskLimitInFlight, EnvAskLimitDaily, EnvAskLimitQuestionChars,
		EnvAskLimitAnswerChars, EnvAskLimitTimeoutSeconds,
	} {
		t.Setenv(name, "")
	}
}

func askTestConfig(t *testing.T, keyFile string) *Config {
	t.Helper()
	var cfg Config
	doc := "ask:\n  enabled: true\n  kapa:\n    project_id: proj-secret\n    integration_id: integ-secret\n    api_key_file: " + keyFile + "\n"
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatal(err)
	}
	return &cfg
}

func TestAskIsOffByDefault(t *testing.T) {
	clearAskEnv(t)
	for _, cfg := range []*Config{nil, {}} {
		s := ResolveAskSettings(cfg, true)
		if s.Active || s.Requested {
			t.Fatalf("default must be off and not requested: %+v", s)
		}
	}
}

func TestAskActiveOnHubDefaultsToDirect(t *testing.T) {
	clearAskEnv(t)
	root := keyFileTestRoot(t)
	s := ResolveAskSettings(askTestConfig(t, filepath.Join(root, "kapa")), true)
	if !s.Active || s.Mode != AskModeDirect || s.ProjectID != "proj-secret" || s.IntegrationID != "integ-secret" || !s.SendSectionLabel {
		t.Fatalf("settings = %+v", s)
	}
	if s.Limits.PersonBurst != DefaultAskPersonBurst || s.Limits.Daily != DefaultAskDaily || s.Limits.TimeoutSeconds != DefaultAskTimeoutSeconds {
		t.Fatalf("limits = %+v", s.Limits)
	}
}

func TestAskStaysOffWhenAValueIsMissingOrUnusable(t *testing.T) {
	clearAskEnv(t)
	root := keyFileTestRoot(t)
	key := filepath.Join(root, "kapa")

	cases := map[string]func(*Config){
		"ask.kapa.project_id is missing":     func(c *Config) { c.Ask.Kapa.ProjectID = "" },
		"ask.kapa.integration_id is missing": func(c *Config) { c.Ask.Kapa.IntegrationID = "" },
		"ask.kapa.api_key_file is missing":   func(c *Config) { c.Ask.Kapa.APIKeyFile = "" },
		"outside the allowed secret":         func(c *Config) { c.Ask.Kapa.APIKeyFile = "/etc/passwd" },
		"ask.mode is off":                    func(c *Config) { c.Ask.Mode = "off" },
		"not one of":                         func(c *Config) { c.Ask.Mode = "bogus" },
		"always uses direct mode":            func(c *Config) { c.Ask.Mode = "hub" },
	}
	for want, mutate := range cases {
		cfg := askTestConfig(t, key)
		mutate(cfg)
		s := ResolveAskSettings(cfg, true)
		if s.Active || !s.Requested || !strings.Contains(s.Reason, want) {
			t.Fatalf("%q: %+v", want, s)
		}
		// Whatever the reason, an inactive setting carries no vendor value, and
		// the reason itself names none.
		if s.ProjectID != "" || s.IntegrationID != "" || s.APIKeyFile != "" {
			t.Fatalf("%q: inactive settings still hold values: %+v", want, s)
		}
		for _, v := range []string{"proj-secret", "integ-secret", key} {
			if strings.Contains(s.Reason, v) {
				t.Fatalf("reason %q carries %q", s.Reason, v)
			}
		}
	}
}

func TestAskHiveModeWaitsForALaterPhase(t *testing.T) {
	clearAskEnv(t)
	cfg := askTestConfig(t, filepath.Join(keyFileTestRoot(t), "kapa"))
	// A non-hub process with no mode set stays off; hub mode is not built yet.
	if s := ResolveAskSettings(cfg, false); s.Active || !strings.Contains(s.Reason, "ask.mode is off") {
		t.Fatalf("hive with no mode: %+v", s)
	}
	cfg.Ask.Mode = "hub"
	if s := ResolveAskSettings(cfg, false); s.Active {
		t.Fatalf("hub relay mode must stay off until it exists: %+v", s)
	}
	cfg.Ask.Mode = "direct"
	if s := ResolveAskSettings(cfg, false); !s.Active {
		t.Fatalf("direct mode on a hive: %+v", s)
	}
}

func TestAskEnvOverridesConfig(t *testing.T) {
	clearAskEnv(t)
	root := keyFileTestRoot(t)
	cfg := askTestConfig(t, filepath.Join(root, "kapa"))

	t.Setenv(EnvAskEnabled, "false")
	if s := ResolveAskSettings(cfg, true); s.Active {
		t.Fatalf("env false must win over config true: %+v", s)
	}
	t.Setenv(EnvAskEnabled, "")
	t.Setenv(EnvAskKapaProjectID, "env-proj")
	t.Setenv(EnvAskKapaIntegrationID, "env-integ")
	t.Setenv(EnvAskKapaAPIKeyFile, filepath.Join(root, "other"))
	t.Setenv(EnvAskMode, "DIRECT")
	t.Setenv(EnvAskSendSectionLabel, "false")
	t.Setenv(EnvAskLimitPersonBurst, "7")
	t.Setenv(EnvAskLimitDaily, "42")
	t.Setenv(EnvAskLimitInFlight, "not-a-number")
	cfg.Ask.Limits.InFlight = 9
	s := ResolveAskSettings(cfg, true)
	if !s.Active || s.ProjectID != "env-proj" || s.IntegrationID != "env-integ" ||
		s.APIKeyFile != filepath.Join(root, "other") || s.SendSectionLabel {
		t.Fatalf("env values not applied: %+v", s)
	}
	if s.Limits.PersonBurst != 7 || s.Limits.Daily != 42 || s.Limits.InFlight != 9 {
		t.Fatalf("limits: %+v", s.Limits)
	}
	// With the switch only in env and no config file at all.
	t.Setenv(EnvAskEnabled, "true")
	if s := ResolveAskSettings(nil, true); !s.Active {
		t.Fatalf("env alone should be enough: %+v", s)
	}
}

func TestAskVendorValuesNeverAppearInJSON(t *testing.T) {
	clearAskEnv(t)
	cfg := askTestConfig(t, "/secrets/kapa")
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"proj-secret", "integ-secret", "/secrets/kapa"} {
		if strings.Contains(string(b), v) {
			t.Fatalf("JSON form carries %q: %s", v, b)
		}
	}
	// The YAML form is what persists, so it must keep them.
	y, err := yaml.Marshal(cfg.Ask)
	if err != nil || !strings.Contains(string(y), "proj-secret") {
		t.Fatalf("YAML form lost the project id: %s %v", y, err)
	}
}
