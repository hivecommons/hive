package config

// ProvidersConfig groups provider-level settings (ADR-0024). Hive is not a
// credential refresh authority for any provider, so there is deliberately no
// refresh or warming knob here — only the read-only status contract.
type ProvidersConfig struct {
	Status ProviderStatusConfig `yaml:"status,omitempty" json:"status,omitempty"`
}

// ProviderStatusConfig toggles the per-provider auth/availability roll-up the
// spoke serves on /api/status and sends in its hub heartbeat.
type ProviderStatusConfig struct {
	// Enabled defaults to true; set false to omit the providers block.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// StatusEnabled reports whether the provider status roll-up is emitted.
// Default ON.
func (p ProvidersConfig) StatusEnabled() bool {
	return p.Status.Enabled == nil || *p.Status.Enabled
}
