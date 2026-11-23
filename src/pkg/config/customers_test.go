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

func TestCustomersInferenceDefaultsOffAndNeedsCustomers(t *testing.T) {
	t.Setenv(EnvCustomersEnabled, "")
	t.Setenv(EnvCustomersInferenceEnabled, "")
	var cfg Config
	if cfg.CustomersInferenceEnabled() {
		t.Fatal("inference must default off")
	}
	on, off := true, false
	cfg.Customers.Inference.Enabled = &on
	if !cfg.CustomersInferenceEnabled() {
		t.Fatal("explicit true")
	}
	cfg.Customers.Enabled = &off
	if cfg.CustomersInferenceEnabled() {
		t.Fatal("inference must not run with the Customers section off")
	}
	cfg.Customers.Enabled = nil
	t.Setenv(EnvCustomersInferenceEnabled, "false")
	if cfg.CustomersInferenceEnabled() {
		t.Fatal("env false must win over config true")
	}
	t.Setenv(EnvCustomersInferenceEnabled, "true")
	if !(&Config{}).CustomersInferenceEnabled() {
		t.Fatal("env true must win over the default")
	}
	var none *Config
	t.Setenv(EnvCustomersInferenceEnabled, "")
	if none.CustomersInferenceEnabled() {
		t.Fatal("a nil config has inference off")
	}
}

func TestCustomersAvatarsEnabled(t *testing.T) {
	t.Setenv(EnvCustomersAvatarsEnabled, "")
	cfg := &Config{}
	if !cfg.CustomersAvatarsEnabled() {
		t.Fatal("avatars must default on")
	}
	off, on := false, true
	cfg.Customers.Avatars.Enabled = &off
	if cfg.CustomersAvatarsEnabled() {
		t.Fatal("explicit false")
	}
	t.Setenv(EnvCustomersAvatarsEnabled, "true")
	if !cfg.CustomersAvatarsEnabled() {
		t.Fatal("env true must win over config false")
	}
	cfg.Customers.Avatars.Enabled = &on
	t.Setenv(EnvCustomersAvatarsEnabled, "false")
	if cfg.CustomersAvatarsEnabled() {
		t.Fatal("env false must win over config true")
	}
	var none *Config
	t.Setenv(EnvCustomersAvatarsEnabled, "")
	if !none.CustomersAvatarsEnabled() {
		t.Fatal("a nil config has avatars on")
	}
}

func TestCustomersMentionSearchDefaultsOffAndNeedsCustomers(t *testing.T) {
	t.Setenv(EnvCustomersEnabled, "")
	t.Setenv(EnvCustomersMentionSearchEnabled, "")
	var cfg Config
	if cfg.CustomersMentionSearchEnabled() {
		t.Fatal("the accept-time search must default off")
	}
	on := true
	cfg.Customers.MentionSearch.Enabled = &on
	if !cfg.CustomersMentionSearchEnabled() {
		t.Fatal("config true must turn it on")
	}
	t.Setenv(EnvCustomersEnabled, "false")
	if cfg.CustomersMentionSearchEnabled() {
		t.Fatal("it must be off whenever Customers is off")
	}
	t.Setenv(EnvCustomersEnabled, "")
	t.Setenv(EnvCustomersMentionSearchEnabled, "false")
	if cfg.CustomersMentionSearchEnabled() {
		t.Fatal("env false must win over config true")
	}
	t.Setenv(EnvCustomersMentionSearchEnabled, "true")
	if !(&Config{}).CustomersMentionSearchEnabled() {
		t.Fatal("env true must win over the default")
	}
	var none *Config
	t.Setenv(EnvCustomersMentionSearchEnabled, "")
	if none.CustomersMentionSearchEnabled() {
		t.Fatal("a nil config is the default, off")
	}
}

func TestCustomersMentionSearchRequestsPerHour(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want int
	}{
		{"absent", "customers: {}\n", DefaultCustomersMentionSearchMaxPerHour},
		{"zero", "customers:\n  mention_search:\n    max_requests_per_hour: 0\n", DefaultCustomersMentionSearchMaxPerHour},
		{"negative", "customers:\n  mention_search:\n    max_requests_per_hour: -3\n", DefaultCustomersMentionSearchMaxPerHour},
		{"set", "customers:\n  mention_search:\n    max_requests_per_hour: 4\n", 4},
		{"capped", "customers:\n  mention_search:\n    max_requests_per_hour: 100000\n", MaxCustomersMentionSearchMaxPerHour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := cfg.Customers.MentionSearch.RequestsPerHour(); got != tc.want {
				t.Fatalf("RequestsPerHour = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCustomersHubImportEnabled(t *testing.T) {
	t.Setenv(EnvCustomersEnabled, "")
	t.Setenv(EnvCustomersHubImportEnabled, "")
	cfg := &Config{}
	if cfg.CustomersHubImportEnabled() {
		t.Fatal("hub import must default off")
	}
	off, on := false, true
	cfg.Customers.HubImport.Enabled = &on
	if !cfg.CustomersHubImportEnabled() {
		t.Fatal("explicit true")
	}
	t.Setenv(EnvCustomersHubImportEnabled, "false")
	if cfg.CustomersHubImportEnabled() {
		t.Fatal("env false must win over config true")
	}
	cfg.Customers.HubImport.Enabled = &off
	t.Setenv(EnvCustomersHubImportEnabled, "true")
	if !cfg.CustomersHubImportEnabled() {
		t.Fatal("env true must win over config false")
	}
	cfg.Customers.Enabled = &off
	if cfg.CustomersHubImportEnabled() {
		t.Fatal("hub import needs Customers on")
	}
	var none *Config
	t.Setenv(EnvCustomersHubImportEnabled, "")
	if none.CustomersHubImportEnabled() {
		t.Fatal("a nil config has hub import off")
	}
}

func TestCustomersProfileFillEnabled(t *testing.T) {
	t.Setenv(EnvCustomersEnabled, "")
	t.Setenv(EnvCustomersProfileFillEnabled, "")
	t.Setenv(EnvCustomersMentionSearchEnabled, "")
	cfg := &Config{}
	if cfg.CustomersProfileFillEnabled() {
		t.Fatal("profile fill must default off")
	}
	off, on := false, true
	// Independent of the accept-time mention search.
	cfg.Customers.MentionSearch.Enabled = &on
	if cfg.CustomersProfileFillEnabled() {
		t.Fatal("mention_search must not turn the profile fill on")
	}
	cfg.Customers.MentionSearch.Enabled = &off
	cfg.Customers.ProfileFill.Enabled = &on
	if !cfg.CustomersProfileFillEnabled() || cfg.CustomersMentionSearchEnabled() {
		t.Fatal("profile_fill must turn on alone")
	}
	t.Setenv(EnvCustomersProfileFillEnabled, "false")
	if cfg.CustomersProfileFillEnabled() {
		t.Fatal("env false must win over config true")
	}
	cfg.Customers.ProfileFill.Enabled = &off
	t.Setenv(EnvCustomersProfileFillEnabled, "true")
	if !cfg.CustomersProfileFillEnabled() {
		t.Fatal("env true must win over config false")
	}
	cfg.Customers.Enabled = &off
	if cfg.CustomersProfileFillEnabled() {
		t.Fatal("profile fill needs Customers on")
	}
	var none *Config
	t.Setenv(EnvCustomersProfileFillEnabled, "")
	if none.CustomersProfileFillEnabled() {
		t.Fatal("a nil config has the profile fill off")
	}
}

func TestCustomersProfileFillRequestsPerHour(t *testing.T) {
	for in, want := range map[int]int{
		0:                                     DefaultCustomersProfileFillMaxPerHour,
		-3:                                    DefaultCustomersProfileFillMaxPerHour,
		25:                                    25,
		MaxCustomersProfileFillMaxPerHour + 1: MaxCustomersProfileFillMaxPerHour,
	} {
		if got := (CustomersProfileFillConfig{MaxRequestsPerHour: in}).RequestsPerHour(); got != want {
			t.Errorf("RequestsPerHour(%d) = %d, want %d", in, got, want)
		}
	}
}
