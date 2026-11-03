package config

import (
	"fmt"
	"strings"
)

// DashboardMatrixConfig is the experimental, default-off dashboard.matrix block
// (#15862). Repos is the single explicit public-repo allowlist gating every
// Matrix output; empty means nothing is captured. ExcludePaths adds globs to
// the built-in deny-list. There are deliberately no authors or private_repos
// keys.
type DashboardMatrixConfig struct {
	Enabled      bool     `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Repos        []string `yaml:"repos,omitempty" json:"repos,omitempty"`
	ExcludePaths []string `yaml:"exclude_paths,omitempty" json:"exclude_paths,omitempty"`
}

// Active reports whether Matrix may capture anything: enabled with at least
// one allowlisted repo.
func (m DashboardMatrixConfig) Active() bool {
	return m.Enabled && len(m.Repos) > 0
}

// RepoAllowed does an exact, case-insensitive owner/name match.
func (m DashboardMatrixConfig) RepoAllowed(ownerName string) bool {
	for _, r := range m.Repos {
		if strings.EqualFold(r, ownerName) {
			return true
		}
	}
	return false
}

func validMatrixRepo(s string) bool {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return false
	}
	for _, part := range []string{owner, name} {
		for _, r := range part {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			default:
				return false
			}
		}
	}
	return true
}

// Validate rejects anything that is not an exact owner/name repo entry and
// empty exclude_paths entries.
func (m DashboardMatrixConfig) Validate() error {
	for i, r := range m.Repos {
		if !validMatrixRepo(r) {
			return fmt.Errorf("dashboard.matrix.repos[%d]: %q must be an exact owner/name (no wildcards, URLs or empty values)", i, r)
		}
	}
	for i, p := range m.ExcludePaths {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("dashboard.matrix.exclude_paths[%d]: entry must not be empty", i)
		}
	}
	return nil
}
