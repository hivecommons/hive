package config

import (
	"fmt"
	"strings"
	"time"
)

// ClassifierConfig makes the tier-classification keyword lists (pkg/classify)
// config-driven and dashboard-visible, mirroring how per-agent lane_keywords
// drive lane routing. Both fields are optional: when a list is empty, the
// classifier keeps its built-in default for that tier, so an absent
// `classifier:` block is byte-for-byte the old hardcoded behavior. Wired via
// classify.SetTierKeywords from cmd/hive.
type ClassifierConfig struct {
	// Backend selects the decision backend: "keywords" (default) or "jev".
	Backend string `yaml:"backend,omitempty" json:"backend,omitempty"`
	// Mode is retained only to tolerate legacy "shadow" YAML. Jev is advisory
	// only; deterministic keyword/label rules always decide.
	Mode string              `yaml:"mode,omitempty" json:"mode,omitempty"`
	Jev  JevClassifierConfig `yaml:"jev,omitempty" json:"jev,omitempty"`
	// SimpleKeywords are title substrings that classify an issue as Tier
	// "Simple" (→ haiku). Empty keeps the built-in default set.
	SimpleKeywords []string `yaml:"simple_keywords,omitempty" json:"simple_keywords,omitempty"`
	// ComplexSignals are title substrings that classify an issue as Tier
	// "Complex" (→ opus). Empty keeps the built-in default set.
	ComplexSignals []string `yaml:"complex_signals,omitempty" json:"complex_signals,omitempty"`
}

type JevClassifierConfig struct {
	Provider      string        `yaml:"provider,omitempty" json:"provider,omitempty"`
	Model         string        `yaml:"model,omitempty" json:"model,omitempty"`
	Endpoint      string        `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	APIKeyEnv     string        `yaml:"api_key_env,omitempty" json:"api_key_env,omitempty"`
	MinConfidence float64       `yaml:"min_confidence,omitempty" json:"min_confidence,omitempty"`
	Timeout       time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Decisions     []string      `yaml:"decisions,omitempty" json:"decisions,omitempty"`
}

func (c ClassifierConfig) EffectiveBackend() string {
	if strings.TrimSpace(c.Backend) == "" {
		return "keywords"
	}
	return strings.ToLower(strings.TrimSpace(c.Backend))
}

func (c ClassifierConfig) EffectiveJev() JevClassifierConfig {
	j := c.Jev
	if strings.TrimSpace(j.Provider) == "" {
		j.Provider = "openrouter"
	}
	j.Provider = strings.ToLower(strings.TrimSpace(j.Provider))
	if strings.TrimSpace(j.Model) == "" {
		if j.Provider == "typesafe" {
			j.Model = "jev-latest"
		} else {
			j.Model = "typesafe/jev-1.13"
		}
	}
	if j.MinConfidence == 0 {
		j.MinConfidence = 0.8
	}
	if j.Timeout == 0 {
		j.Timeout = 2 * time.Second
	}
	if len(j.Decisions) == 0 {
		j.Decisions = []string{"lane", "tier", "triage"}
	}
	if strings.TrimSpace(j.Endpoint) == "" {
		if j.Provider == "typesafe" {
			j.Endpoint = "https://api.typesafe.ai/v1/systemone"
		} else {
			j.Endpoint = "https://openrouter.ai/api/v1/systemone"
		}
	}
	return j
}

func (j JevClassifierConfig) EffectiveProvider() string {
	return strings.ToLower(strings.TrimSpace(j.Provider))
}
func (j JevClassifierConfig) EffectiveModel() string {
	if strings.TrimSpace(j.Model) != "" {
		return strings.TrimSpace(j.Model)
	}
	if j.EffectiveProvider() == "typesafe" {
		return "jev-latest"
	}
	return "typesafe/jev-1.13"
}
func (j JevClassifierConfig) EffectiveEndpoint() string {
	if strings.TrimSpace(j.Endpoint) != "" {
		return strings.TrimSpace(j.Endpoint)
	}
	if j.EffectiveProvider() == "typesafe" {
		return "https://api.typesafe.ai/v1/systemone"
	}
	return "https://openrouter.ai/api/v1/systemone"
}
func (j JevClassifierConfig) EffectiveMinConfidence() float64 {
	if j.MinConfidence == 0 {
		return 0.8
	}
	return j.MinConfidence
}
func (j JevClassifierConfig) EffectiveTimeout() time.Duration {
	if j.Timeout == 0 {
		return 2 * time.Second
	}
	return j.Timeout
}
func (j JevClassifierConfig) EffectiveDecisions() []string {
	if len(j.Decisions) == 0 {
		return []string{"lane", "tier", "triage"}
	}
	out := make([]string, 0, len(j.Decisions))
	for _, d := range j.Decisions {
		if s := strings.ToLower(strings.TrimSpace(d)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (c ClassifierConfig) Validate() error {
	switch c.EffectiveBackend() {
	case "keywords", "jev":
	default:
		return fmt.Errorf("classifier.backend must be keywords or jev, got %q", c.Backend)
	}
	if mode := strings.ToLower(strings.TrimSpace(c.Mode)); mode != "" && mode != "shadow" {
		return fmt.Errorf("classifier.mode only supports shadow/advisory measurement; remove %q because Jev cannot enforce deterministic classifier decisions", c.Mode)
	}
	j := c.EffectiveJev()
	switch j.Provider {
	case "openrouter", "typesafe":
	default:
		return fmt.Errorf("classifier.jev.provider must be openrouter or typesafe, got %q", c.Jev.Provider)
	}
	if j.MinConfidence < 0 || j.MinConfidence > 1 {
		return fmt.Errorf("classifier.jev.min_confidence must be between 0 and 1")
	}
	if j.Timeout <= 0 {
		return fmt.Errorf("classifier.jev.timeout must be positive")
	}
	valid := map[string]bool{"lane": true, "tier": true, "triage": true}
	for _, d := range j.Decisions {
		if !valid[strings.ToLower(strings.TrimSpace(d))] {
			return fmt.Errorf("classifier.jev.decisions contains invalid decision %q (must be lane, tier, or triage)", d)
		}
	}
	return nil
}
