package config

// SecurityConfig is the hive-wide `security:` block.
//
// Owner rule (hivecommons/hive#15640): a contributor's PR runs CI and the
// review swarm hands-free; the only thing that may park it for a person
// during CI/Review is a concrete security / trust finding
// (review.IsSecuritySensitiveFinding). The finding predicate treats a
// review finding on a sensitive path as such a finding; the path set is
// sentinel.DefaultSensitivePaths plus the globs here.
type SecurityConfig struct {
	// SensitivePaths are extra glob patterns (same syntax as
	// sentinel.sensitive_paths: `deploy/**`, `internal/auth/**`,
	// `**/Dockerfile`) that an untrusted author touching, with a review
	// finding on them, parks the PR for a person during CI/Review.
	SensitivePaths []string `yaml:"sensitive_paths,omitempty" json:"sensitive_paths,omitempty"`
}
