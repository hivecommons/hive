package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Jev provider names. Defaults and endpoints mirror the classifier-side Jev
// client on the v6 line so a hive that later syncs both keeps one set of
// operator-facing values.
const (
	JevProviderOpenRouter = "openrouter"
	JevProviderTypeSafe   = "typesafe"

	// JevDefaultAPIKeyEnv is the environment variable consulted for the Jev
	// key before falling back to the connected OpenRouter gateway's key.
	JevDefaultAPIKeyEnv = "JEV_API_KEY"

	jevDefaultTimeout = 5 * time.Second
)

// JevConfig is the hive-wide Jev client configuration (hivecommons/hive#8939).
// Per-agent enablement is AgentConfig.JevMode; this block only says where the
// hive sends decisions and which key it uses. Agents never see the key: every
// call goes through the hive's local decision endpoint.
type JevConfig struct {
	// Provider selects the hosted endpoint: openrouter (default) or typesafe.
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	// Model is the Jev model id; defaults per provider.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// Endpoint overrides the provider's systemone URL (http(s) only).
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	// APIKeyEnv names the env var holding the Jev key. Defaults to JEV_API_KEY.
	// When that variable is empty the connected OpenRouter gateway key is used.
	APIKeyEnv string `yaml:"api_key_env,omitempty" json:"api_key_env,omitempty"`
	// Timeout bounds one decision round-trip. Defaults to 5s.
	Timeout time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// EffectiveProvider returns the normalized provider name.
func (j JevConfig) EffectiveProvider() string {
	p := strings.ToLower(strings.TrimSpace(j.Provider))
	if p == "" {
		return JevProviderOpenRouter
	}
	return p
}

// EffectiveModel returns the configured model or the provider default.
func (j JevConfig) EffectiveModel() string {
	if m := strings.TrimSpace(j.Model); m != "" {
		return m
	}
	if j.EffectiveProvider() == JevProviderTypeSafe {
		return "jev-latest"
	}
	return "typesafe/jev-1.13"
}

// EffectiveEndpoint returns the systemone endpoint the hive posts decisions to.
func (j JevConfig) EffectiveEndpoint() string {
	if e := strings.TrimSpace(j.Endpoint); e != "" {
		return strings.TrimRight(e, "/")
	}
	if j.EffectiveProvider() == JevProviderTypeSafe {
		return "https://api.typesafe.ai/v1/systemone"
	}
	return "https://openrouter.ai/api/v1/systemone"
}

// EffectiveAPIKeyEnv returns the env var name consulted for the Jev key.
func (j JevConfig) EffectiveAPIKeyEnv() string {
	if v := strings.TrimSpace(j.APIKeyEnv); v != "" {
		return v
	}
	return JevDefaultAPIKeyEnv
}

// EffectiveTimeout returns the per-decision timeout.
func (j JevConfig) EffectiveTimeout() time.Duration {
	if j.Timeout > 0 {
		return j.Timeout
	}
	return jevDefaultTimeout
}

// Validate rejects a Jev block the client could not act on. The zero value is
// always valid.
func (j JevConfig) Validate() error {
	switch j.EffectiveProvider() {
	case JevProviderOpenRouter, JevProviderTypeSafe:
	default:
		return fmt.Errorf("jev: invalid provider %q (must be openrouter or typesafe)", j.Provider)
	}
	if e := strings.TrimSpace(j.Endpoint); e != "" {
		u, err := url.Parse(e)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("jev: invalid endpoint %q (must be an http(s) URL)", j.Endpoint)
		}
	}
	if j.Timeout < 0 {
		return fmt.Errorf("jev: timeout must not be negative")
	}
	return nil
}

// ResolveJevAPIKey returns the key the hive uses for Jev calls: the Jev key env
// var first, then the connected OpenRouter gateway's key. Empty means the tool
// is not ready — the dashboard shows the per-agent toggle disabled and the
// decision endpoint answers 503 without touching the network.
func (c *Config) ResolveJevAPIKey() string {
	if c == nil {
		return ""
	}
	if v := strings.TrimSpace(os.Getenv(c.Jev.EffectiveAPIKeyEnv())); v != "" {
		return v
	}
	if gw := c.Governor.ResolveGateway(GatewayKindOpenRouter); gw != nil {
		return strings.TrimSpace(gw.ResolveAPIKey())
	}
	return ""
}

// JevReady reports whether a Jev key is resolvable right now.
func (c *Config) JevReady() bool { return c.ResolveJevAPIKey() != "" }
