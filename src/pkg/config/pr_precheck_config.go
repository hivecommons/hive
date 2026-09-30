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
	// checks. Default ON; set false to opt out.
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
	return p.GoTests == nil || *p.GoTests
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
