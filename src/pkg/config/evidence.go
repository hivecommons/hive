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
}

// IsEnabled reports whether bundles are written (default on).
func (e EvidenceConfig) IsEnabled() bool {
	return e.Enabled == nil || *e.Enabled
}

// SigningKeyPath is the trimmed signing key path, empty when unset.
func (e EvidenceConfig) SigningKeyPath() string {
	return strings.TrimSpace(e.SigningKeyFile)
}
