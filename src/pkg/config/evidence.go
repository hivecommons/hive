package config

import "strings"

// EvidenceConfig (top-level `evidence:`) controls the per-PR review evidence
// bundles (pkg/evidence, docs/review-evidence.md) the review relay writes
// under the durable data dir (hivecommons/hive#11060).
//
// Default ON: an unset block writes unsigned bundles. Signing is opt-in
// because it needs a key the operator provisions.
type EvidenceConfig struct {
	// Enabled turns bundle writing off when explicitly false. nil means on.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// SigningKeyFile is the path to an Ed25519 private key (32-byte seed or
	// 64-byte key, hex or base64). Empty or absent means bundles are written
	// unsigned with "signed": false.
	SigningKeyFile string `yaml:"signing_key_file,omitempty" json:"signing_key_file,omitempty"`
	// Complexity controls the monthly, report-only complexity snapshot of
	// each managed repo's default branch (RFC #12062, hivecommons/hive#12363).
	Complexity ComplexityConfig `yaml:"complexity,omitempty" json:"complexity,omitempty"`
}

// ComplexityConfig (`evidence.complexity:`) gates the monthly complexity
// snapshot runner (pkg/evidence/complexity). Measuring is an AST pass over a
// tarball of the default branch, once a month per repo, so it is on by
// default. Static analysis (go vet, staticcheck) compiles the repo and
// downloads its modules, so it is opt-in. Nothing here gates a merge.
type ComplexityConfig struct {
	// Enabled turns the monthly snapshot off when explicitly false. nil
	// means on.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// StaticAnalysis also runs go vet and, when installed, staticcheck over
	// the checkout and records their finding counts. Off by default: it
	// compiles the repo and downloads modules on the spoke.
	StaticAnalysis bool `yaml:"static_analysis,omitempty" json:"static_analysis,omitempty"`
}

// IsEnabled reports whether the monthly complexity snapshot runs (default on).
func (c ComplexityConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// IsEnabled reports whether bundles are written (default on).
func (e EvidenceConfig) IsEnabled() bool {
	return e.Enabled == nil || *e.Enabled
}

// SigningKeyPath is the trimmed signing key path, empty when unset.
func (e EvidenceConfig) SigningKeyPath() string {
	return strings.TrimSpace(e.SigningKeyFile)
}
