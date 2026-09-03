package config

import "strings"

const DefaultOTelServiceName = "hive"

// OTelConfig configures OpenTelemetry trace export. It is additive and OFF by
// default: a config with no `otel:` block (or enabled:false) yields a
// zero-overhead no-op tracer. When enabled, spans are exported over OTLP/HTTP
// to Endpoint (or the standard OTEL_EXPORTER_OTLP_ENDPOINT env var when
// Endpoint is empty).
type OTelConfig struct {
	// Enabled turns OTLP trace export on. Default false — the zero value keeps
	// every existing config a no-op with no exporter and no network activity.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Endpoint is the OTLP/HTTP collector endpoint (host:port or full URL).
	// When empty, the exporter falls back to OTEL_EXPORTER_OTLP_ENDPOINT.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	// Headers are optional static OTLP/HTTP headers, commonly used for collector
	// authentication. Values should come from env interpolation, not literals.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	// ServiceName is recorded as resource service.name. Empty defaults to "hive".
	ServiceName string `yaml:"service_name,omitempty" json:"service_name,omitempty"`
	// Insecure disables TLS for OTLP/HTTP. Leave false for https collectors.
	Insecure bool `yaml:"insecure,omitempty" json:"insecure,omitempty"`
	// SampleRatio is the head-based sampling ratio in [0.0, 1.0]. The zero
	// value is treated as "sample everything" (1.0) so an operator who only
	// sets enabled:true gets full traces; set explicitly to sample less.
	SampleRatio float64 `yaml:"sample_ratio,omitempty" json:"sample_ratio,omitempty"`
}

// ServiceNameOrDefault returns a valid resource service.name.
func (o OTelConfig) ServiceNameOrDefault() string {
	if name := strings.TrimSpace(o.ServiceName); name != "" {
		return name
	}
	return DefaultOTelServiceName
}

// IsZero reports whether the block contains no operator-provided settings.
func (o OTelConfig) IsZero() bool {
	return !o.Enabled && o.Endpoint == "" && len(o.Headers) == 0 && o.ServiceName == "" && !o.Insecure && o.SampleRatio == 0
}

// EffectiveOTel returns the preferred otel block, falling back to the legacy
// tracing block so existing configs continue to work unchanged.
func (c *Config) EffectiveOTel() OTelConfig {
	if c == nil {
		return OTelConfig{}
	}
	if !c.OTel.IsZero() {
		return c.OTel
	}
	return c.Tracing
}

// MintConfig configures the OIDC token mint service (pkg/mint). It is additive
// and DISABLED by default: an absent `mint:` block, or Enabled=false, leaves
// existing behavior byte-identical. When enabled, the mint issues short-lived
// scoped JWTs (a Workload Identity Federation broker) that downstream cloud/
