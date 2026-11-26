package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// clearEnrichmentEnv blanks every enrichment override so a test starts from
// the saved config alone.
func clearEnrichmentEnv(t *testing.T) {
	t.Helper()
	for _, env := range []string{
		EnvCustomersEnabled,
		EnvCustomersEnrichmentEnabled,
		EnvCustomersEnrichmentGitdmEnabled,
		EnvCustomersEnrichmentGitdmRefreshInterval,
		EnvCustomersEnrichmentDevstatsEnabled,
		EnvCustomersEnrichmentDevstatsHourlyLimit,
		EnvCustomersEnrichmentCrowdDevEnabled,
		EnvCustomersEnrichmentCrowdDevToken,
		EnvCustomersEnrichmentCrowdDevTokenFile,
	} {
		t.Setenv(env, "")
	}
}

// With nothing configured enrichment is inactive and the knobs hold their
// defaults: enrichment changes nothing until an owner turns it on.
func TestCustomersEnrichmentDefaultsOff(t *testing.T) {
	clearEnrichmentEnv(t)
	for _, cfg := range []*Config{nil, {}} {
		if cfg.CustomersEnrichmentEnabled() || cfg.CustomersEnrichmentGitdmEnabled() ||
			cfg.CustomersEnrichmentDevstatsEnabled() || cfg.CustomersEnrichmentCrowdDevEnabled() {
			t.Fatalf("enrichment must default off (cfg=%v)", cfg)
		}
		if got := cfg.CustomersEnrichmentGitdmRefresh(); got != DefaultCustomersEnrichmentGitdmRefresh {
			t.Fatalf("refresh = %v, want default", got)
		}
		if got := cfg.CustomersEnrichmentDevstatsHourlyLimit(); got != DefaultCustomersEnrichmentDevstatsHourlyLimit {
			t.Fatalf("hourly limit = %d, want default", got)
		}
	}
}

func TestCustomersEnrichmentGitdmDefault(t *testing.T) {
	clearEnrichmentEnv(t)
	on, off := true, false
	cfg := &Config{Customers: CustomersConfig{Enrichment: CustomersEnrichmentConfig{Enabled: &on}}}
	if !cfg.CustomersEnrichmentGitdmEnabled() || cfg.CustomersEnrichmentDevstatsEnabled() {
		t.Fatal("enrichment must enable gitdm, but not devstats, by default")
	}
	cfg.Customers.Enrichment.Gitdm.Enabled = &off
	if cfg.CustomersEnrichmentGitdmEnabled() {
		t.Fatal("explicit gitdm false must win")
	}
	t.Setenv(EnvCustomersEnrichmentGitdmEnabled, "true")
	if !cfg.CustomersEnrichmentGitdmEnabled() {
		t.Fatal("environment must override explicit false")
	}
	t.Setenv(EnvCustomersEnrichmentGitdmEnabled, "false")
	cfg.Customers.Enrichment.Gitdm.Enabled = &on
	if cfg.CustomersEnrichmentGitdmEnabled() {
		t.Fatal("environment false must override explicit true")
	}
	t.Setenv(EnvCustomersEnrichmentGitdmEnabled, "")
	t.Setenv(EnvCustomersEnrichmentEnabled, "true")
	var nilConfig *Config
	if !nilConfig.CustomersEnrichmentGitdmEnabled() {
		t.Fatal("environment-enabled enrichment must use the gitdm default with nil config")
	}
}

func TestCustomersEnrichmentYAMLRoundTrip(t *testing.T) {
	clearEnrichmentEnv(t)
	const doc = `customers:
  enrichment:
    enabled: true
    gitdm:
      enabled: true
      refresh_interval: 12h
    devstats:
      enabled: true
      hourly_limit: 40
    crowddev:
      enabled: true
      token_file: /data/secrets/crowd
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.CustomersEnrichmentEnabled() || !cfg.CustomersEnrichmentGitdmEnabled() ||
		!cfg.CustomersEnrichmentDevstatsEnabled() || !cfg.CustomersEnrichmentCrowdDevEnabled() {
		t.Fatalf("switches not decoded: %+v", cfg.Customers.Enrichment)
	}
	if got := cfg.CustomersEnrichmentGitdmRefresh(); got != 12*time.Hour {
		t.Fatalf("refresh = %v", got)
	}
	if got := cfg.CustomersEnrichmentDevstatsHourlyLimit(); got != 40 {
		t.Fatalf("hourly limit = %d", got)
	}
	if got := cfg.Customers.Enrichment.CrowdDev.TokenFileOrDefault(); got != "/data/secrets/crowd" {
		t.Fatalf("token file = %q", got)
	}
	out, err := yaml.Marshal(cfg.Customers)
	if err != nil {
		t.Fatal(err)
	}
	var back CustomersConfig
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Enrichment.IsEnabled() || back.Enrichment.Gitdm.RefreshInterval != "12h" || back.Enrichment.Devstats.HourlyLimit != 40 {
		t.Fatalf("round trip lost values: %+v", back.Enrichment)
	}
}

// A provider switch needs the enrichment switch, which needs Customers.
func TestCustomersEnrichmentProviderNeedsParents(t *testing.T) {
	clearEnrichmentEnv(t)
	on, off := true, false
	cfg := &Config{Customers: CustomersConfig{Enrichment: CustomersEnrichmentConfig{
		Enabled:  &off,
		Gitdm:    CustomersEnrichmentGitdmConfig{Enabled: &on},
		Devstats: CustomersEnrichmentDevstatsConfig{Enabled: &on},
		CrowdDev: CustomersEnrichmentCrowdDevConfig{Enabled: &on},
	}}}
	if cfg.CustomersEnrichmentGitdmEnabled() || cfg.CustomersEnrichmentDevstatsEnabled() || cfg.CustomersEnrichmentCrowdDevEnabled() {
		t.Fatal("a provider must stay off while enrichment is off")
	}
	cfg.Customers.Enrichment.Enabled = &on
	if !cfg.CustomersEnrichmentGitdmEnabled() {
		t.Fatal("gitdm on once enrichment is on")
	}
	cfg.Customers.Enabled = &off
	if cfg.CustomersEnrichmentEnabled() || cfg.CustomersEnrichmentGitdmEnabled() {
		t.Fatal("enrichment must stay off while Customers is off")
	}
}

func TestCustomersEnrichmentEnvOverrides(t *testing.T) {
	clearEnrichmentEnv(t)
	on, off := true, false
	cfg := &Config{Customers: CustomersConfig{Enrichment: CustomersEnrichmentConfig{
		Enabled: &on,
		Gitdm:   CustomersEnrichmentGitdmConfig{Enabled: &off, RefreshInterval: "6h"},
		Devstats: CustomersEnrichmentDevstatsConfig{
			Enabled: &on, HourlyLimit: 10,
		},
	}}}
	t.Setenv(EnvCustomersEnrichmentGitdmEnabled, "true")
	if !cfg.CustomersEnrichmentGitdmEnabled() {
		t.Fatal("env true must win over gitdm false")
	}
	t.Setenv(EnvCustomersEnrichmentDevstatsEnabled, "false")
	if cfg.CustomersEnrichmentDevstatsEnabled() {
		t.Fatal("env false must win over devstats true")
	}
	t.Setenv(EnvCustomersEnrichmentEnabled, "false")
	if cfg.CustomersEnrichmentEnabled() || cfg.CustomersEnrichmentGitdmEnabled() {
		t.Fatal("env false on the block must turn every provider off")
	}
	t.Setenv(EnvCustomersEnrichmentEnabled, "nonsense")
	if !cfg.CustomersEnrichmentEnabled() {
		t.Fatal("an unrecognised env value leaves the saved value in charge")
	}
	t.Setenv(EnvCustomersEnrichmentGitdmRefreshInterval, "48h")
	if got := cfg.CustomersEnrichmentGitdmRefresh(); got != 48*time.Hour {
		t.Fatalf("env refresh = %v", got)
	}
	t.Setenv(EnvCustomersEnrichmentGitdmRefreshInterval, "1m")
	if got := cfg.CustomersEnrichmentGitdmRefresh(); got != MinCustomersEnrichmentGitdmRefresh {
		t.Fatalf("refresh below the floor = %v, want %v", got, MinCustomersEnrichmentGitdmRefresh)
	}
	t.Setenv(EnvCustomersEnrichmentGitdmRefreshInterval, "garbage")
	if got := cfg.CustomersEnrichmentGitdmRefresh(); got != DefaultCustomersEnrichmentGitdmRefresh {
		t.Fatalf("unparsable refresh = %v, want default", got)
	}
	t.Setenv(EnvCustomersEnrichmentDevstatsHourlyLimit, "50")
	if got := cfg.CustomersEnrichmentDevstatsHourlyLimit(); got != 50 {
		t.Fatalf("env hourly limit = %d", got)
	}
	t.Setenv(EnvCustomersEnrichmentDevstatsHourlyLimit, "100000")
	if got := cfg.CustomersEnrichmentDevstatsHourlyLimit(); got != MaxCustomersEnrichmentDevstatsHourlyLimit {
		t.Fatalf("hourly limit above the cap = %d", got)
	}
	t.Setenv(EnvCustomersEnrichmentCrowdDevTokenFile, "/tmp/elsewhere")
	if got := cfg.Customers.Enrichment.CrowdDev.TokenFileOrDefault(); got != "/tmp/elsewhere" {
		t.Fatalf("env token file = %q", got)
	}
}

func TestCustomersEnrichmentCrowdDevToken(t *testing.T) {
	clearEnrichmentEnv(t)
	path := filepath.Join(t.TempDir(), "secrets", "crowd")
	c := CustomersEnrichmentCrowdDevConfig{TokenFile: path}
	ctx := context.Background()
	if v, err := c.Token(ctx); err != nil || v != "" {
		t.Fatalf("missing file = %q, %v; want empty, nil", v, err)
	}
	if err := WriteCustomersEnrichmentSecret(ctx, path, "  tok-value \n"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v, %v", fi, err)
	}
	if v, err := c.Token(ctx); err != nil || v != "tok-value" {
		t.Fatalf("stored token = %q, %v", v, err)
	}
	t.Setenv(EnvCustomersEnrichmentCrowdDevToken, "from-env")
	if v, _ := c.Token(ctx); v != "from-env" {
		t.Fatalf("env token must win, got %q", v)
	}
	t.Setenv(EnvCustomersEnrichmentCrowdDevToken, "")
	if err := WriteCustomersEnrichmentSecret(ctx, path, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("an empty value must remove the file: %v", err)
	}
	if err := WriteCustomersEnrichmentSecret(ctx, path, ""); err != nil {
		t.Fatalf("clearing an absent file is not an error: %v", err)
	}
}
