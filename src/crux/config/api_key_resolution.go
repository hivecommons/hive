package config

import (
	"os"
	"strings"
)

// API key resolution for gateways and the legacy LiteLLM block, split out of
// agent_runtime.go. Every reader goes through SecretFilePathAllowed (audit N8)
// and reports a log-safe source alongside the key (hive#18532, #18533).

// ResolveAPIKey returns this gateway's key value, preferring the env var when
// set and falling back to the file. Returns "" when neither yields a value
// (a keyless endpoint).
func (gw GatewayConfig) ResolveAPIKey() string {
	key, _ := gw.ResolveAPIKeyWithSource()
	return key
}

// ResolveAPIKeyWithSource is ResolveAPIKey plus WHERE the key came from, as a
// log-safe "env:<NAME>" or "file:<path>" ("" when no key resolved). When the
// configured api_key_file is outside the allowed secret dirs it is skipped
// (audit N8) and a WARN names the skipped path and the source actually used,
// once per process (hive#18533).
func (gw GatewayConfig) ResolveAPIKeyWithSource() (string, string) {
	key, source := gw.resolveAPIKeyFromSources()
	if keyFileSkipped(gw.APIKeyFile) {
		warnSkippedKeyFileOnce(GatewayAPIKeyFileField(gw.Name), gw.APIKeyFile, source)
	}
	return key, source
}

// APIKeyFileProblem returns the operator-facing rejection for an api_key_file
// outside the allowed secret dirs, or "" when it is empty or allowed.
func (gw GatewayConfig) APIKeyFileProblem() string {
	return SecretFilePathRejection(GatewayAPIKeyFileField(gw.Name), gw.APIKeyFile)
}

func (gw GatewayConfig) resolveAPIKeyFromSources() (string, string) {
	if gw.APIKeyEnv != "" {
		if v := os.Getenv(gw.APIKeyEnv); v != "" {
			return v, KeySourceEnvPrefix + gw.APIKeyEnv
		}
	}
	if gw.APIKeyFile != "" {
		// SECURITY (audit N8): confine the read to the managed secrets dirs. This
		// gate lives HERE, not only in the HTTP handler, so every caller is
		// covered — including a path that reached hive.yaml some other way (a
		// hand-edited config, an older build, a restored backup).
		if !SecretFilePathAllowed(gw.APIKeyFile) {
			return "", ""
		}
		if b, err := readSecretFile(gw.APIKeyFile); err == nil {
			if key := strings.TrimSpace(string(b)); key != "" {
				return key, KeySourceFilePrefix + gw.APIKeyFile
			}
		}
	}
	return "", ""
}

// ResolveAPIKey returns the LiteLLM API key. Key FILES are consulted in
// priority order — the configured api_key_file, then the k8s Secret mount
// (DefaultLiteLLMAPIKeyFile), then the dashboard-written PVC file
// (WritableLiteLLMAPIKeyFile) — followed by the env var named by
// api_key_env and finally DefaultLiteLLMAPIKeyEnv. Returns "" when no key
// is configured. The key value itself is never stored in hive.yaml.
//
// Consulting all three file locations means a key saved via the dashboard
// keeps working even if hive.yaml is reset (e.g. re-seeded from a
// ConfigMap) and the api_key_file pointer is lost, and an admin-managed
// Secret key keeps working if the PVC copy is wiped.
func (c *LiteLLMConfig) ResolveAPIKey() string {
	key, _ := c.resolveAPIKeyWithSource()
	return key
}

// ResolveAPIKeySource reports where ResolveAPIKey found the key without
// exposing the value: "file:<path>", "env:<NAME>", or "" when no key is
// configured. Safe to return from APIs (the dashboard shows it as the
// "Key detected" store).
func (c *LiteLLMConfig) ResolveAPIKeySource() string {
	_, source := c.resolveAPIKeyWithSource()
	return source
}

// ResolveAPIKeyWithSource returns the key and its log-safe source in one
// resolution, so a caller needing both reads the key file once.
func (c *LiteLLMConfig) ResolveAPIKeyWithSource() (string, string) {
	return c.resolveAPIKeyWithSource()
}

func (c *LiteLLMConfig) resolveAPIKeyWithSource() (string, string) {
	key, source := c.resolveAPIKeyFromSources()
	// hive#18533: a configured api_key_file outside the allowed dirs is skipped
	// (audit N8) and resolution falls through to the defaults or the env var.
	// Say so, once, naming the path skipped and the source used instead, so an
	// operator who mounted a new Secret somewhere else can see why the old key
	// is still in use.
	if c.ConfiguredKeyFileSkipped() {
		warnSkippedKeyFileOnce(LiteLLMAPIKeyFileField, c.APIKeyFile, source)
	}
	return key, source
}

// ConfiguredKeyFileSkipped reports whether api_key_file is set but outside the
// allowed secret dirs, so ResolveAPIKey ignores it.
func (c *LiteLLMConfig) ConfiguredKeyFileSkipped() bool {
	return keyFileSkipped(c.APIKeyFile)
}

// APIKeyFileProblem returns the operator-facing rejection for an api_key_file
// outside the allowed secret dirs, or "" when it is empty or allowed.
func (c *LiteLLMConfig) APIKeyFileProblem() string {
	return SecretFilePathRejection(LiteLLMAPIKeyFileField, c.APIKeyFile)
}

// KeySourceDescription is the effective key source in display form, noting a
// skipped configured path. Never contains key material.
func (c *LiteLLMConfig) KeySourceDescription() string {
	return DescribeKeySource(c.ResolveAPIKeySource(), c.APIKeyFile)
}

func (c *LiteLLMConfig) resolveAPIKeyFromSources() (string, string) {
	files := []string{c.APIKeyFile, DefaultLiteLLMAPIKeyFile, WritableLiteLLMAPIKeyFile}
	seen := map[string]bool{"": true}
	for _, f := range files {
		if seen[f] {
			continue
		}
		seen[f] = true
		// SECURITY (audit N8): only the managed secrets dirs. The two defaults
		// appended above already satisfy this; the gate is for c.APIKeyFile,
		// which is operator/API-supplied.
		if !SecretFilePathAllowed(f) {
			continue
		}
		if data, err := readSecretFile(f); err == nil {
			if key := strings.TrimSpace(string(data)); key != "" {
				return key, KeySourceFilePrefix + f
			}
		}
	}
	if c.APIKeyEnv != "" {
		if key := os.Getenv(c.APIKeyEnv); key != "" {
			return key, KeySourceEnvPrefix + c.APIKeyEnv
		}
	}
	if key := os.Getenv(DefaultLiteLLMAPIKeyEnv); key != "" {
		return key, KeySourceEnvPrefix + DefaultLiteLLMAPIKeyEnv
	}
	return "", ""
}
