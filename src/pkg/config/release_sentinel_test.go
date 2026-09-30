package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestReleaseSentinelDefaultOff(t *testing.T) {
	t.Setenv(ReleaseSentinelEnabledEnvVar, "")
	var nilCfg *Config
	if nilCfg.ReleaseSentinelEnabled() {
		t.Fatal("nil config enabled the sentinel")
	}
	if (&Config{}).ReleaseSentinelEnabled() {
		t.Fatal("zero config enabled the sentinel; it must ship default OFF")
	}
	if !(&Config{ReleaseSentinel: ReleaseSentinelConfig{Enabled: true}}).ReleaseSentinelEnabled() {
		t.Fatal("explicit opt-in ignored")
	}
}

func TestReleaseSentinelEnvOverride(t *testing.T) {
	on := &Config{ReleaseSentinel: ReleaseSentinelConfig{Enabled: true}}
	off := &Config{}
	t.Setenv(ReleaseSentinelEnabledEnvVar, "off")
	if on.ReleaseSentinelEnabled() {
		t.Fatal("env=off did not override config=true")
	}
	t.Setenv(ReleaseSentinelEnabledEnvVar, "true")
	if !off.ReleaseSentinelEnabled() {
		t.Fatal("env=true did not override config=false")
	}
	var nilCfg *Config
	if !nilCfg.ReleaseSentinelEnabled() {
		t.Fatal("env=true did not apply to a nil config")
	}
	t.Setenv(ReleaseSentinelEnabledEnvVar, "maybe")
	if off.ReleaseSentinelEnabled() || !on.ReleaseSentinelEnabled() {
		t.Fatal("an unparseable env value must leave the config in charge")
	}
}

func TestReleaseSentinelRepo(t *testing.T) {
	var nilCfg *Config
	if nilCfg.ReleaseSentinelRepo() != "" {
		t.Fatal("nil config produced a repo")
	}
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"explicit wins", Config{ReleaseSentinel: ReleaseSentinelConfig{Repo: " acme/rel "}, Project: ProjectConfig{Org: "o", PrimaryRepo: "o/p"}}, "acme/rel"},
		{"qualified primary", Config{Project: ProjectConfig{Org: "o", PrimaryRepo: "acme/widgets"}}, "acme/widgets"},
		{"bare primary + org", Config{Project: ProjectConfig{Org: "acme", PrimaryRepo: "widgets"}}, "acme/widgets"},
		{"bare primary no org", Config{Project: ProjectConfig{PrimaryRepo: "widgets"}}, ""},
		{"nothing", Config{}, ""},
	}
	for _, tc := range cases {
		if got := tc.cfg.ReleaseSentinelRepo(); got != tc.want {
			t.Errorf("%s: ReleaseSentinelRepo = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReleaseSentinelDurations(t *testing.T) {
	r := ReleaseSentinelConfig{RoundTimeout: " 90m ", PollInterval: "10m"}
	if r.RoundTimeoutDuration() != 90*time.Minute || r.PollIntervalDuration() != 10*time.Minute {
		t.Fatalf("parsed %s / %s", r.RoundTimeoutDuration(), r.PollIntervalDuration())
	}
	for _, bad := range []string{"", "soon", "-5m", "0s"} {
		r := ReleaseSentinelConfig{RoundTimeout: bad, PollInterval: bad}
		if r.RoundTimeoutDuration() != 0 || r.PollIntervalDuration() != 0 {
			t.Errorf("%q did not fall back to 0 (package default)", bad)
		}
	}
}

func TestReleaseSentinelYAML(t *testing.T) {
	var cfg Config
	src := `
release_sentinel:
  enabled: true
  repo: acme/widgets
  agent: release-fixer
  max_rounds: 3
  round_timeout: 1h
  poll_interval: 2m
  ignore_workflows: [Greetings]
`
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	r := cfg.ReleaseSentinel
	if !r.Enabled || r.Repo != "acme/widgets" || r.Agent != "release-fixer" || r.MaxRounds != 3 ||
		r.RoundTimeoutDuration() != time.Hour || r.PollIntervalDuration() != 2*time.Minute ||
		len(r.IgnoreWorkflows) != 1 || r.IgnoreWorkflows[0] != "Greetings" {
		t.Fatalf("parsed = %+v", r)
	}
}
