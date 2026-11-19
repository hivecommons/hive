package config

import (
	"testing"
	"time"
)

func TestMarketplaceConfigDefaults(t *testing.T) {
	var m MarketplaceConfig
	if !m.MarketplaceEnabled() {
		t.Fatal("marketplace should default to enabled")
	}
	if m.EffectiveRefreshInterval() != DefaultMarketplaceRefreshInterval {
		t.Fatalf("interval = %v", m.EffectiveRefreshInterval())
	}
	off := false
	m = MarketplaceConfig{Enabled: &off, RefreshInterval: time.Minute}
	if m.MarketplaceEnabled() || m.EffectiveRefreshInterval() != time.Minute {
		t.Fatalf("explicit values ignored: %+v", m)
	}

	var nilCfg *Config
	if nilCfg.MarketplaceURL() != "" {
		t.Fatal("nil config URL")
	}
	cfg := &Config{}
	cfg.Hub.URL = "https://hub.example/"
	if got := cfg.MarketplaceURL(); got != "https://hub.example" {
		t.Fatalf("falls back to hub.url, got %q", got)
	}
	cfg.Workflow.Marketplace.URL = " https://mkt.example// "
	if got := cfg.MarketplaceURL(); got != "https://mkt.example" {
		t.Fatalf("explicit url, got %q", got)
	}
}
