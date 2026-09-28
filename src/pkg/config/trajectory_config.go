package config

import (
	"os"
	"strings"
)

// TrajectoryConfig governs the trajectory-review lane: a periodic,
// second-model check that reads each running agent's recent transcript and
// scores whether the sequence of actions is still working toward the agent's
// assigned intent (its last kick), pausing the agent when it diverges. This
// is defense against trajectory-level goal drift — individually-innocuous
// steps that assemble toward an unauthorized outcome — which action-level
// gating cannot see. On by default; the lane no-ops when no reviewer endpoint
// resolves, so "on" is safe even without inference set up.
//
// The reviewer needs any OpenAI-compatible /v1/chat/completions endpoint — a
// LiteLLM gateway, a vLLM server, or an llm-d front. It does NOT drive an
// interactive CLI: it makes one stateless request per agent per cycle and
// reads back a strict-JSON verdict, which the CLIs (stateful tmux sessions)
// cannot provide.
type TrajectoryConfig struct {
	// Enabled turns the review lane on. Pointer so an omitted key defaults to
	// enabled (applyDefaults sets it), while an explicit "false" disables it.
	Enabled *bool `yaml:"enabled"`
	// Endpoint is the reviewer's OpenAI-compatible base URL (LiteLLM, vLLM, or
	// llm-d — anything serving /v1/chat/completions). Empty → fall back to the
	// governor LiteLLM endpoint. Storing it here lets the reviewer target a
	// cheap local model independent of the agents' inference config.
	Endpoint string `yaml:"endpoint"`
	// APIKeyEnv / APIKeyFile resolve the reviewer endpoint's key (env var NAME
	// or file PATH; never the value). Empty → fall back to the LiteLLM key.
	// A bare vLLM server needs no key; a gateway usually does.
	APIKeyEnv  string `yaml:"api_key_env"`
	APIKeyFile string `yaml:"api_key_file"`
	// IntervalS is how often (seconds) the lane evaluates running agents.
	// It runs off the governor tick, so the effective floor is the governor
	// eval interval; a value below that reviews every tick. 0 → default.
	IntervalS int `yaml:"interval_s"`
	// Model is the reviewer model id. Empty → the governor LiteLLM
	// default_model. A cheap-but-capable model is appropriate; a tiny model
	// will emit weaker verdicts (the lane fails open, so it degrades toward
	// "catches less," not "false-pauses").
	Model string `yaml:"model"`
	// TranscriptLines caps how many trailing transcript lines are sent to the
	// reviewer. 0 → default. Bounds token cost and keeps the review focused
	// on recent behavior.
	TranscriptLines int `yaml:"transcript_lines"`
	// OnDivergence is the action taken when a trajectory is judged divergent:
	// "pause" (stop the agent and alert — default) or "alert" (notify only,
	// leave the agent running). Any other value is treated as "alert".
	OnDivergence string `yaml:"on_divergence"`
	// ExemptAgents are never reviewed (e.g. advisory-only agents that open no
	// PRs and touch no infrastructure).
	ExemptAgents []string `yaml:"exempt_agents"`
}

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

// IsEnabled reports whether the trajectory-review lane is on. Default is ON:
// a nil Enabled (key omitted) counts as enabled, only an explicit false
// disables it. The lane itself no-ops when no reviewer endpoint resolves, so
// defaulting on is safe even for hives without inference configured.
func (t TrajectoryConfig) IsEnabled() bool {
	return t.Enabled == nil || *t.Enabled
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
