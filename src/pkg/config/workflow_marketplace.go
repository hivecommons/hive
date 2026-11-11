package config

import (
	"strings"
	"time"
)

// MarketplaceConfig is the `workflow.marketplace:` block. The spoke proxies
// the hub catalogue index, imports bundles from it and publishes library
// entries to it (hivecommons/hive#17620 phase 7).
type MarketplaceConfig struct {
	// Enabled defaults to true; set false to hide the Marketplace view and
	// return 404 from the proxy routes.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// URL is the hub base URL serving /api/marketplace/*. Empty means the
	// configured hub.url (which itself defaults to the public hub).
	URL string `yaml:"url,omitempty" json:"url,omitempty"`
	// RefreshInterval bounds how long the spoke serves a cached index
	// before revalidating with the hub (If-None-Match). Zero means the
	// default of 15 minutes.
	RefreshInterval time.Duration `yaml:"refresh_interval,omitempty" json:"refresh_interval,omitempty"`
}

// DefaultMarketplaceRefreshInterval is the spoke-side index cache TTL.
const DefaultMarketplaceRefreshInterval = 15 * time.Minute

// MarketplaceEnabled reports the effective workflow.marketplace.enabled.
func (m MarketplaceConfig) MarketplaceEnabled() bool {
	return m.Enabled == nil || *m.Enabled
}

// EffectiveRefreshInterval returns refresh_interval or its default.
func (m MarketplaceConfig) EffectiveRefreshInterval() time.Duration {
	if m.RefreshInterval <= 0 {
		return DefaultMarketplaceRefreshInterval
	}
	return m.RefreshInterval
}

// MarketplaceURL resolves the hub base URL for marketplace calls: the
// explicit workflow.marketplace.url, else hub.url.
func (c *Config) MarketplaceURL() string {
	if c == nil {
		return ""
	}
	if u := strings.TrimSpace(c.Workflow.Marketplace.URL); u != "" {
		return strings.TrimRight(u, "/")
	}
	return strings.TrimRight(strings.TrimSpace(c.Hub.URL), "/")
}
