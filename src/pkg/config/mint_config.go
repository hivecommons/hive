package config

// MintConfig configures the OIDC token mint service (pkg/mint). It is additive
// and DISABLED by default: an absent `mint:` block, or Enabled=false, leaves
// existing behavior byte-identical. When enabled, the mint issues short-lived
// scoped JWTs (a Workload Identity Federation broker) that downstream cloud/
// registry WIF providers can trust via Issuer + JWKS.
type MintConfig struct {
	// Enabled turns the mint service on. Default false (deny).
	Enabled bool `yaml:"enabled,omitempty"`
	// KeyPath is the PEM path of the signing key. If the file is absent the
	// mint generates one and persists it with 0600 perms. Required when enabled.
	KeyPath string `yaml:"key_path,omitempty"`
	// Issuer is the `iss` claim and the identity WIF providers are configured to
	// trust (typically the mint's public URL). Required when enabled.
	Issuer string `yaml:"issuer,omitempty"`
	// MaxTTLSeconds bounds a minted token's lifetime. 0 uses the package default
	// (15m). The value is clamped to the package hard cap (1h) regardless.
	MaxTTLSeconds int `yaml:"max_ttl_seconds,omitempty"`
}
