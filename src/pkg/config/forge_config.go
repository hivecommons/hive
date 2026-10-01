package config

import (
	"path/filepath"
	"time"
)

const (
	DefaultPRPrecheckTimeout       = 15 * time.Minute
	DefaultPRPrecheckMaxConcurrent = 1
)

// PRPrecheckConfig controls deterministic PR-open prechecks run by the hive
// before an agent-authored PR is created.
type PRPrecheckConfig struct {
	// Docs gates the docs/link/citation guards. Default ON; set false to opt out.
	Docs *bool `yaml:"docs,omitempty" json:"docs,omitempty"`
	// GoTests gates touched-package Go tests and cross-cutting ratchets/parity
	// checks. Default OFF: the target repo's CI owns test verdicts, and running
	// `go test` inside the live hive pod lets tests read and mutate production
	// state under /data (config overlays, the audit log, tmux sessions), which
	// produced false rejections and clobbered live config (#9824, #9833).
	// Set true only on a hive whose precheck checkout is isolated from /data.
	GoTests *bool `yaml:"go_tests,omitempty" json:"go_tests,omitempty"`
	// Timeout bounds each PR precheck run. Default 15m.
	Timeout time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// CacheDir stores warm Go build/module caches for PR prechecks. Empty
	// defaults under the hive data dir.
	CacheDir string `yaml:"cache_dir,omitempty" json:"cache_dir,omitempty"`
	// MaxConcurrent bounds concurrent Tier C Go prechecks. Default 1.
	MaxConcurrent int `yaml:"max_concurrent,omitempty" json:"max_concurrent,omitempty"`
}

func (p PRPrecheckConfig) DocsEnabled() bool {
	return p.Docs == nil || *p.Docs
}

func (p PRPrecheckConfig) GoTestsEnabled() bool {
	return p.GoTests != nil && *p.GoTests
}

func (p PRPrecheckConfig) EffectiveTimeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return DefaultPRPrecheckTimeout
}

func (p PRPrecheckConfig) EffectiveMaxConcurrent() int {
	if p.MaxConcurrent > 0 {
		return p.MaxConcurrent
	}
	return DefaultPRPrecheckMaxConcurrent
}

func (p PRPrecheckConfig) EffectiveCacheDir(dataRoot string) string {
	if p.CacheDir != "" {
		return p.CacheDir
	}
	if dataRoot == "" {
		dataRoot = "/data"
	}
	return filepath.Join(dataRoot, "pr-precheck", "gocache")
}

// AppSignedCommitsEnabled reports whether the PR-request watcher re-authors
// agent branches through createCommitOnBranch so their commits are GitHub-
// signed. Opt-in: nil and false both mean off. See AppSignedCommits.
func (g GitHubConfig) AppSignedCommitsEnabled() bool {
	return g.AppSignedCommits != nil && *g.AppSignedCommits
}

// SelfAuthorizationHoldEnabled reports whether the #5117 self-authorization
// hold is active for this hive without considering ACMM level. Default ON
// preserves the existing policy for callers that have not been wired to the
// live level-aware resolver.
func (g GitHubConfig) SelfAuthorizationHoldEnabled() bool {
	return g.SelfAuthorizationHoldEnabledAtLevel(0)
}

// SelfAuthorizationHoldEnabledAtLevel reports whether the #5117
// self-authorization hold is active at the provided live ACMM level. Explicit
// env/config values win; otherwise L6 fully autonomous defaults the policy off.
func (g GitHubConfig) SelfAuthorizationHoldEnabledAtLevel(acmmLevel int) bool {
	if g.selfAuthorizationHoldEnvOverride != nil {
		return *g.selfAuthorizationHoldEnvOverride
	}
	if g.SelfAuthorizationHold == nil {
		return acmmLevel < MaxACMMLevel
	}
	return *g.SelfAuthorizationHold
}

// SelfAuthorizationHoldEnvOverrideSet reports whether
// HIVE_SELF_AUTHORIZATION_HOLD is currently forcing the effective value.
func (g GitHubConfig) SelfAuthorizationHoldEnvOverrideSet() bool {
	return g.selfAuthorizationHoldEnvOverride != nil
}

// ReporterTrustHoldEnabled reports whether the #9665 reporter-trust hold is
// active hive-wide. Env wins, then an explicit config value, then the gate's
// own enabled flag: switching reporter_trust on covers both admission and
// merge unless the operator says otherwise. It never depends on ACMM level —
// holding a stranger's request for a human is the point at L6.
func (g GitHubConfig) ReporterTrustHoldEnabled(gateEnabled bool) bool {
	if g.reporterTrustHoldEnvOverride != nil {
		return *g.reporterTrustHoldEnvOverride
	}
	if g.ReporterTrustHold == nil {
		return gateEnabled
	}
	return *g.ReporterTrustHold
}

// ReporterTrustHoldEnvOverrideSet reports whether HIVE_REPORTER_TRUST_HOLD is
// currently forcing the effective value.
func (g GitHubConfig) ReporterTrustHoldEnvOverrideSet() bool {
	return g.reporterTrustHoldEnvOverride != nil
}
