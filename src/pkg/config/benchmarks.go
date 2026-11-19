package config

import (
	"strings"

	cruxconfig "github.com/hivecommons/hive/crux/config"
)

// BenchmarksConfig (top-level `benchmarks:`) governs what this hive
// contributes to the fleet leaderboard at https://hivecommons.dev/benchmarks/
// (RFC hivecommons/hive#17694, docs/benchmarks.md).
//
// Default ON, anonymized: every hive publishes per-PR records keyed by a
// salted hash of its hive id — model, harness, sandbox runtime, outcome,
// time-to-merge and ACMM level; never a login, prompt, finding or code.
// Repository and org names ride along only for PUBLIC repositories, whose
// activity is already public. Both knobs are opt-outs.
type BenchmarksConfig struct {
	// Publish false stops the spoke exporting records to the hub (and empties
	// GET /api/benchmarks/export). nil means on.
	Publish *bool `yaml:"publish,omitempty" json:"publish,omitempty"`
	// RepoConsent false anonymizes even public repositories: task.repo and
	// source.org are dropped from every record. nil means public repos are
	// named.
	RepoConsent *bool `yaml:"repo_consent,omitempty" json:"repo_consent,omitempty"`
}

// PublishEnabled reports whether records leave this hive.
func (b BenchmarksConfig) PublishEnabled() bool {
	return b.Publish == nil || *b.Publish
}

// RepoConsentEnabled reports whether public repositories may be named.
func (b BenchmarksConfig) RepoConsentEnabled() bool {
	return b.RepoConsent == nil || *b.RepoConsent
}

// BenchmarkSandboxRuntimeNone is the runtime published for in-process agents.
const BenchmarkSandboxRuntimeNone = "none"

// BenchmarkSandboxRuntime is the sandbox runtime an agent's kicks run under
// as the benchmark schema names it: the crux runtime when sandboxing is on
// for that agent, else "none".
func (c *Config) BenchmarkSandboxRuntime(agent string) string {
	if c == nil {
		return BenchmarkSandboxRuntimeNone
	}
	var ac *cruxconfig.AgentConfig
	if a, ok := c.Agents[agent]; ok {
		ac = &a
	}
	enabled := c.AgentSandbox.Enabled
	if ac != nil && ac.Sandbox != nil && ac.Sandbox.Enabled != nil {
		enabled = *ac.Sandbox.Enabled
	}
	if !enabled {
		return BenchmarkSandboxRuntimeNone
	}
	return strings.TrimSpace(ac.SandboxRuntime(c.AgentSandbox))
}
