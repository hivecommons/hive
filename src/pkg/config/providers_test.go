package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProvidersStatusEnabledDefaultsOn(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("project: {}\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Providers.StatusEnabled() {
		t.Fatal("provider status must default on")
	}
	if err := yaml.Unmarshal([]byte("providers:\n  status:\n    enabled: false\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Providers.StatusEnabled() {
		t.Fatal("providers.status.enabled: false must turn the roll-up off")
	}
	if !(ProvidersConfig{Status: ProviderStatusConfig{Enabled: boolPtr(true)}}).StatusEnabled() {
		t.Fatal("explicit true must stay on")
	}
}
