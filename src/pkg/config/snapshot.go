package config

import "gopkg.in/yaml.v3"

// Snapshot copies persisted configuration for background readers. Callers must
// serialize this with writers that do not use Save's lock and treat the result
// as immutable.
// YAML (rather than JSON) includes internal notification credentials that are
// deliberately omitted from API responses. Never log the serialized bytes.
func (c *Config) Snapshot() (*Config, error) {
	saveMu.Lock()
	defer saveMu.Unlock()
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	var out Config
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	out.SourcePath = c.SourcePath
	out.ReviewerAutoscaleActive = c.ReviewerAutoscaleActive
	out.LaunchCmdDegradations = append([]LaunchCmdDegradation(nil), c.LaunchCmdDegradations...)
	// Environment overrides are runtime policy, not persisted YAML.
	if p := c.GitHub.selfAuthorizationHoldEnvOverride; p != nil {
		v := *p
		out.GitHub.selfAuthorizationHoldEnvOverride = &v
	}
	if p := c.GitHub.reporterTrustHoldEnvOverride; p != nil {
		v := *p
		out.GitHub.reporterTrustHoldEnvOverride = &v
	}
	return &out, nil
}

// ReplaceFromReload installs a loaded config without racing config saves or
// readers of the live write-back policy.
func (c *Config) ReplaceFromReload(next *Config) {
	saveMu.Lock()
	defer saveMu.Unlock()
	*c = *next
}
