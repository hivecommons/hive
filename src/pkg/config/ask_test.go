package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	askTestProject     = "proj-1111-secret"
	askTestIntegration = "integ-2222-secret"
	askTestKey         = "kapa-key-3333-secret"
)

func askEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// askKeyFile writes a key under a temp dir registered as a secret root.
func askKeyFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(SetSecretFileRootsForTest(dir))
	path := filepath.Join(dir, "kapa-api-key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveAskIsOffByDefault(t *testing.T) {
	s := ResolveAsk(AskConfig{}, nil, false)
	if s.Requested || s.Enabled || s.Mode != AskModeOff || s.Problem != "" {
		t.Fatalf("zero config must be off and quiet: %+v", s)
	}
	if !s.SendSectionLabel || s.RelayHives != AskRelayHosted {
		t.Fatalf("defaults: %+v", s)
	}
	want := AskLimits{DefaultAskPersonPerMinute, DefaultAskPersonPerHour, DefaultAskHivePerHour, DefaultAskGlobalPerMinute, DefaultAskInFlight, DefaultAskDaily}
	if s.Limits != want {
		t.Fatalf("limits %+v, want %+v", s.Limits, want)
	}
	// Credentials alone never switch it on.
	key := askKeyFile(t, askTestKey)
	s = ResolveAsk(AskConfig{Mode: AskModeDirect, Kapa: AskKapaConfig{ProjectID: askTestProject, IntegrationID: askTestIntegration, APIKeyFile: key}}, nil, false)
	if s.Enabled {
		t.Fatal("configured but not switched on must stay off")
	}
}

func TestResolveAskBudgetIsASmallFractionOfTheVendorLimit(t *testing.T) {
	if DefaultAskGlobalPerMinute*askGlobalShareDivisor > KapaTeamChatLimitPerMinute || DefaultAskGlobalPerMinute <= 0 {
		t.Fatalf("default hub-wide budget %d is not a small fraction of %d", DefaultAskGlobalPerMinute, KapaTeamChatLimitPerMinute)
	}
	s := ResolveAsk(AskConfig{Limits: AskLimitsConfig{GlobalPerMinute: KapaTeamChatLimitPerMinute}}, askEnv(map[string]string{EnvAskLimitGlobalPerMinute: "1000"}), true)
	if s.Limits.GlobalPerMinute != MaxAskGlobalPerMinute || MaxAskGlobalPerMinute >= KapaTeamChatLimitPerMinute {
		t.Fatalf("override not clamped below the vendor limit: %d", s.Limits.GlobalPerMinute)
	}
	s = ResolveAsk(AskConfig{Limits: AskLimitsConfig{Daily: 9}}, askEnv(map[string]string{EnvAskLimitDaily: "-4", EnvAskLimitPersonPerHour: "7"}), true)
	if s.Limits.Daily != 9 || s.Limits.PersonPerHour != 7 {
		t.Fatalf("limit precedence: %+v", s.Limits)
	}
}

func TestResolveAskEnvWinsAndHubIsDirect(t *testing.T) {
	key := askKeyFile(t, askTestKey+"\n")
	env := askEnv(map[string]string{
		EnvAskEnabled: "true", EnvAskMode: "hub",
		EnvAskKapaProjectID: askTestProject, EnvAskKapaIntegrationID: askTestIntegration, EnvAskKapaAPIKeyFile: key,
		EnvAskSendSectionLabel: "false", EnvAskRelayDisabledHives: " hive-a , ,hive-b",
	})
	off := false
	hub := ResolveAsk(AskConfig{Enabled: &off}, env, true)
	if !hub.Enabled || hub.Mode != AskModeDirect || hub.SendSectionLabel || !hub.DisabledHives["hive-a"] || !hub.DisabledHives["hive-b"] || len(hub.DisabledHives) != 2 {
		t.Fatalf("hub settings: %+v", hub)
	}
	hive := ResolveAsk(AskConfig{}, env, false)
	if !hive.Enabled || hive.Mode != AskModeHub {
		t.Fatalf("a hive in hub mode needs no vendor credentials: %+v", hive)
	}
	got, err := LoadAskAPIKey(key)
	if err != nil || got != askTestKey {
		t.Fatalf("key %q %v", got, err)
	}
}

func TestResolveAskProblemsNameNoValues(t *testing.T) {
	key := askKeyFile(t, askTestKey)
	empty := filepath.Join(filepath.Dir(key), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	on := true
	base := AskConfig{Enabled: &on, Mode: AskModeDirect, Kapa: AskKapaConfig{ProjectID: askTestProject, IntegrationID: askTestIntegration, APIKeyFile: key}}
	if s := ResolveAsk(base, nil, false); !s.Enabled || s.Problem != "" {
		t.Fatalf("complete config: %+v", s)
	}
	cases := map[string]func(c *AskConfig){
		"mode off":          func(c *AskConfig) { c.Mode = "" },
		"no project":        func(c *AskConfig) { c.Kapa.ProjectID = "" },
		"bad project":       func(c *AskConfig) { c.Kapa.ProjectID = askTestProject + "/../x" },
		"no integration":    func(c *AskConfig) { c.Kapa.IntegrationID = "" },
		"bad integration":   func(c *AskConfig) { c.Kapa.IntegrationID = askTestIntegration + "?q" },
		"no key file":       func(c *AskConfig) { c.Kapa.APIKeyFile = "" },
		"key outside roots": func(c *AskConfig) { c.Kapa.APIKeyFile = "/etc/" + askTestKey },
		"key file missing":  func(c *AskConfig) { c.Kapa.APIKeyFile = key + "-missing" },
		"key file empty":    func(c *AskConfig) { c.Kapa.APIKeyFile = empty },
	}
	for name, mutate := range cases {
		c := base
		mutate(&c)
		s := ResolveAsk(c, nil, false)
		if s.Enabled || s.Problem == "" {
			t.Errorf("%s: must stay off with a reason: %+v", name, s)
		}
		for _, v := range []string{askTestProject, askTestIntegration, askTestKey, key} {
			if strings.Contains(s.Problem, v) {
				t.Errorf("%s: problem %q names a value", name, s.Problem)
			}
		}
	}
	if _, err := LoadAskAPIKey("/etc/passwd"); err == nil {
		t.Fatal("a key file outside the secret dirs must never be read")
	}
}

// askClearEnv blanks every Ask env var so Config.AskSettings sees only the
// config under test.
func askClearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvAskEnabled, EnvAskMode, EnvAskKapaProjectID, EnvAskKapaIntegrationID,
		EnvAskKapaAPIKeyFile, EnvAskSendSectionLabel, EnvAskRelayHives, EnvAskRelayDisabledHives} {
		t.Setenv(k, "")
	}
}

func askHostedConfig(ask AskConfig) *Config {
	return &Config{HiveID: "hosted-hive", Hub: HubConfig{HiveType: HiveTypeHosted, URL: "https://hub.example"}, Ask: ask}
}

// TestAskDefaultsOnForHostedHives: a hosted hive with a hub link and a hive
// id and no ask config resolves as requested and on, in hub mode.
func TestAskDefaultsOnForHostedHives(t *testing.T) {
	askClearEnv(t)
	cfg := askHostedConfig(AskConfig{})
	if !cfg.AskHostedDefault() {
		t.Fatal("a hosted hive with a hub link and a hive id must take the hosted default")
	}
	s := cfg.AskSettings()
	if !s.Requested || !s.Enabled || !s.Defaulted || s.Mode != AskModeHub || s.Problem != "" {
		t.Fatalf("hosted hive with no ask config must be on in hub mode: %+v", s)
	}
	// An explicit hub mode with nothing else set is the same default.
	if s := askHostedConfig(AskConfig{Mode: AskModeHub}).AskSettings(); !s.Enabled || !s.Defaulted || s.Mode != AskModeHub {
		t.Fatalf("hosted hive with mode hub only: %+v", s)
	}
	// ask.enabled: true alone means hub mode on a hosted hive.
	on := true
	if s := askHostedConfig(AskConfig{Enabled: &on}).AskSettings(); !s.Enabled || s.Defaulted || s.Mode != AskModeHub {
		t.Fatalf("hosted hive with ask.enabled true only: %+v", s)
	}
	// Case and spacing in hub.hive_type do not matter.
	cfg.Hub.HiveType = " Hosted "
	if !cfg.AskHostedDefault() || !cfg.AskSettings().Enabled {
		t.Fatal("hive_type matching must ignore case and spacing")
	}
}

// TestAskExplicitOffWinsOnHostedHives: ask.enabled false, HIVE_ASK_ENABLED
// false and ask.mode off each keep a hosted hive off, and quietly.
func TestAskExplicitOffWinsOnHostedHives(t *testing.T) {
	off := false
	cases := map[string]struct {
		ask AskConfig
		env map[string]string
	}{
		"yaml enabled false":               {ask: AskConfig{Enabled: &off}},
		"env enabled false":                {env: map[string]string{EnvAskEnabled: "false"}},
		"env enabled 0":                    {env: map[string]string{EnvAskEnabled: "0"}},
		"yaml mode off":                    {ask: AskConfig{Mode: AskModeOff}},
		"env mode off":                     {env: map[string]string{EnvAskMode: AskModeOff}},
		"yaml enabled false with mode hub": {ask: AskConfig{Enabled: &off, Mode: AskModeHub}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			askClearEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			s := askHostedConfig(tc.ask).AskSettings()
			if s.Requested || s.Enabled || s.Defaulted || s.Problem != "" {
				t.Fatalf("an explicit off must win over the hosted default: %+v", s)
			}
		})
	}
	// An explicit direct mode is the operator asking for direct: the hosted
	// default never switches it on without ask.enabled.
	askClearEnv(t)
	if s := askHostedConfig(AskConfig{Mode: AskModeDirect}).AskSettings(); s.Requested || s.Enabled {
		t.Fatalf("direct mode without ask.enabled must stay off: %+v", s)
	}
}

// TestAskStaysOffByDefaultOffHostedHives: self-hosted, standalone and
// half-linked hosted hives keep the off default (the relay does not serve
// registered self-hosted hives yet, #19177).
func TestAskStaysOffByDefaultOffHostedHives(t *testing.T) {
	askClearEnv(t)
	cases := map[string]*Config{
		"nil config":             nil,
		"standalone":             {HiveID: "solo"},
		"self-hosted with a hub": {HiveID: "self", Hub: HubConfig{Enabled: true, URL: "https://hub.example"}},
		"self-hosted typed":      {HiveID: "self", Hub: HubConfig{HiveType: "self-hosted", URL: "https://hub.example"}},
		"hosted without hub url": {HiveID: "hosted-hive", Hub: HubConfig{HiveType: HiveTypeHosted}},
		"hosted without hive id": {Hub: HubConfig{HiveType: HiveTypeHosted, URL: "https://hub.example"}},
	}
	for name, cfg := range cases {
		if cfg.AskHostedDefault() {
			t.Errorf("%s: must not take the hosted default", name)
		}
		if s := cfg.AskSettings(); s.Requested || s.Enabled || s.Defaulted || s.Mode != AskModeOff || s.Problem != "" {
			t.Errorf("%s: must be off and quiet: %+v", name, s)
		}
	}
	// The hub itself never takes the hosted default.
	if s := ResolveAsk(AskConfig{}, nil, true); s.Requested || s.Enabled {
		t.Fatalf("the hub must stay off without HIVE_ASK_ENABLED: %+v", s)
	}
}
