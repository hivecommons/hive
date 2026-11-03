package config

import (
	"os"
	"strings"
)

// ReplanConfig governs the Phase 3 stall-replan lane: a periodic check that
// finds approved plans whose sub-tasks have stopped progressing and re-kicks the
// architect to revise them, bounded by a per-plan replan cap. It runs off the
// governor tick on its own cadence (like the trajectory lane). On by default; it
// is a no-op when there are no approved plans, so "on" is always safe.
type ReplanConfig struct {
	// Enabled turns the stall-replan lane on. Pointer so an omitted key defaults
	// to enabled, while an explicit "false" disables it.
	Enabled *bool `yaml:"enabled"`
	// IntervalS is how often (seconds) the lane scans for stalled plans. It runs
	// off the governor tick, so the effective floor is the governor eval
	// interval. 0 → default (30m).
	IntervalS int `yaml:"interval_s"`
	// StallThresholdS is how long (seconds) a plan may go without any child
	// progressing before it is considered stalled. 0 → default (6h).
	StallThresholdS int `yaml:"stall_threshold_s"`
	// MaxReplans caps replans per plan before the lane stops and escalates to a
	// human. 0 → default (5).
	MaxReplans int `yaml:"max_replans"`
}

// IsEnabled reports whether the stall-replan lane is on. Default is ON: a nil
// Enabled (key omitted) counts as enabled, only an explicit false disables it.
func (r ReplanConfig) IsEnabled() bool {
	return r.Enabled == nil || *r.Enabled
}

// ResolveReviewer returns the reviewer endpoint, API key, and model, falling
// back to the governor LiteLLM block for any field the trajectory block leaves
// empty. This is what lets the reviewer target a LiteLLM gateway, a vLLM
// server, or an llm-d front interchangeably: it only needs an
// OpenAI-compatible /v1/chat/completions URL. The key value is resolved from
// a file or env var and is never stored in hive.yaml.
func (g *GovernorConfig) ResolveReviewer() (endpoint, apiKey, model string) {
	t := g.Trajectory
	endpoint = strings.TrimRight(strings.TrimSpace(t.Endpoint), "/")
	if endpoint == "" {
		endpoint = g.LiteLLM.ResolveEndpoint()
	}
	apiKey = resolveKeyFromFileThenEnv(t.APIKeyFile, t.APIKeyEnv)
	if apiKey == "" {
		apiKey = g.LiteLLM.ResolveAPIKey()
	}
	model = strings.TrimSpace(t.Model)
	if model == "" {
		model = g.LiteLLM.DefaultModel
	}
	return endpoint, apiKey, model
}

// ReviewerReady reports whether the reviewer has both an endpoint and a model
// resolved — i.e. whether enabling the lane will actually run reviews. The UI
// uses this to distinguish "on and running" from "on but not configured", so
// a safety control never silently no-ops while showing as enabled.
func (g *GovernorConfig) ReviewerReady() bool {
	endpoint, _, model := g.ResolveReviewer()
	return endpoint != "" && model != ""
}

// resolveKeyFromFileThenEnv reads a secret from a file path, then an env var
// NAME, returning "" if neither yields a value. Mirrors the LiteLLM/inference
// resolution order without pulling in defaults.
func resolveKeyFromFileThenEnv(file, env string) string {
	if file != "" {
		if data, err := os.ReadFile(file); err == nil {
			if k := strings.TrimSpace(string(data)); k != "" {
				return k
			}
		}
	}
	if env != "" {
		if k := os.Getenv(env); k != "" {
			return k
		}
	}
	return ""
}
