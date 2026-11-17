package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCustomersEnabledDefaultsOn(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want bool
	}{
		{"absent", "project:\n  org: o\n", true},
		{"empty block", "customers: {}\n", true},
		{"explicit true", "customers:\n  enabled: true\n", true},
		{"explicit false", "customers:\n  enabled: false\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := cfg.Customers.IsEnabled(); got != tc.want {
				t.Fatalf("customers.enabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCustomersEnabledEnvOverridesConfig(t *testing.T) {
	off := false
	cfg := &Config{Customers: CustomersConfig{Enabled: &off}}
	t.Setenv(EnvCustomersEnabled, "")
	if cfg.CustomersEnabled() {
		t.Fatal("config false must turn the section off")
	}
	t.Setenv(EnvCustomersEnabled, "true")
	if !cfg.CustomersEnabled() {
		t.Fatal("env true must win over config false")
	}
	t.Setenv(EnvCustomersEnabled, "false")
	if (&Config{}).CustomersEnabled() {
		t.Fatal("env false must win over the default")
	}
	t.Setenv(EnvCustomersEnabled, "not-a-bool")
	var none *Config
	if !none.CustomersEnabled() {
		t.Fatal("a nil config with no usable env is the default, on")
	}
}
