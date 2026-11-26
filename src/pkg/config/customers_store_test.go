package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// The store mode defaults to json, an unknown value fails closed to json,
// and the env wins over the saved value only when it names a known mode.
func TestCustomersStoreMode(t *testing.T) {
	t.Setenv(EnvCustomersStore, "")
	for _, cfg := range []*Config{nil, {}} {
		if got := cfg.CustomersStoreMode(); got != CustomersStoreJSON {
			t.Fatalf("default mode = %q, want json", got)
		}
	}
	cfg := &Config{Customers: CustomersConfig{Store: "Shadow"}}
	if got := cfg.CustomersStoreMode(); got != CustomersStoreShadow {
		t.Fatalf("saved mode = %q, want shadow", got)
	}
	cfg.Customers.Store = "colony-only"
	if got := cfg.CustomersStoreMode(); got != CustomersStoreJSON {
		t.Fatalf("unknown saved mode = %q, want json", got)
	}
	cfg.Customers.Store = CustomersStoreShadow
	t.Setenv(EnvCustomersStore, "colony")
	if got := cfg.CustomersStoreMode(); got != CustomersStoreColony {
		t.Fatalf("env mode = %q, want colony", got)
	}
	t.Setenv(EnvCustomersStore, "bogus")
	if got := cfg.CustomersStoreMode(); got != CustomersStoreShadow {
		t.Fatalf("unknown env must fall back to the saved mode, got %q", got)
	}
	t.Setenv(EnvCustomersStore, "json")
	if got := cfg.CustomersStoreMode(); got != CustomersStoreJSON {
		t.Fatalf("env rollback = %q, want json", got)
	}
}

func TestCustomersStoreYAML(t *testing.T) {
	t.Setenv(EnvCustomersStore, "")
	var cfg Config
	if err := yaml.Unmarshal([]byte("customers:\n  store: colony\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.CustomersStoreMode(); got != CustomersStoreColony {
		t.Fatalf("mode = %q, want colony", got)
	}
}

// Auto-accept is off by default, off whenever gitdm is off, and only the
// two known policies are recognised.
func TestCustomersEnrichmentAutoAccept(t *testing.T) {
	clearEnrichmentEnv(t)
	t.Setenv(EnvCustomersEnrichmentAutoAccept, "")
	for _, cfg := range []*Config{nil, {}} {
		if got := cfg.CustomersEnrichmentAutoAccept(); got != CustomersAutoAcceptOff {
			t.Fatalf("default = %q, want off", got)
		}
	}
	on := true
	cfg := &Config{Customers: CustomersConfig{Enrichment: CustomersEnrichmentConfig{
		AutoAccept: "gitdm_high",
	}}}
	if got := cfg.CustomersEnrichmentAutoAccept(); got != CustomersAutoAcceptOff {
		t.Fatalf("with gitdm off the policy must be off, got %q", got)
	}
	cfg.Customers.Enrichment.Enabled = &on
	cfg.Customers.Enrichment.Gitdm.Enabled = &on
	if got := cfg.CustomersEnrichmentAutoAccept(); got != CustomersAutoAcceptGitdmHigh {
		t.Fatalf("policy = %q, want gitdm_high", got)
	}
	cfg.Customers.Enrichment.AutoAccept = "everything"
	if got := cfg.CustomersEnrichmentAutoAccept(); got != CustomersAutoAcceptOff {
		t.Fatalf("unknown policy = %q, want off", got)
	}
	t.Setenv(EnvCustomersEnrichmentAutoAccept, "gitdm_high")
	if got := cfg.CustomersEnrichmentAutoAccept(); got != CustomersAutoAcceptGitdmHigh {
		t.Fatalf("env policy = %q, want gitdm_high", got)
	}
	t.Setenv(EnvCustomersEnrichmentAutoAccept, "off")
	cfg.Customers.Enrichment.AutoAccept = "gitdm_high"
	if got := cfg.CustomersEnrichmentAutoAccept(); got != CustomersAutoAcceptOff {
		t.Fatalf("env off must win, got %q", got)
	}
}
