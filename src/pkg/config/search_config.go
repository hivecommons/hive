package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Search defaults (hivecommons/hive#14491). The byte limits match
// search.DefaultMaxIndexBytesPerRepo / search.DefaultMaxMemoryBytes.
const (
	DefaultSearchMaxIndexBytesPerRepo int64 = 32 << 20
	DefaultSearchMaxMemoryBytes       int64 = 256 << 20
	DefaultSearchRetentionMonths            = 24

	// Health score defaults (hivecommons/hive#14638); they match
	// search.DefaultHealthStaleAfter, search.DefaultHealthSecurityDocMaxAge
	// and search.DefaultHealthWeights.
	DefaultSearchHealthStaleAfter        = 60 * 24 * time.Hour
	DefaultSearchHealthSecurityDocMaxAge = 365 * 24 * time.Hour
)

// DefaultSearchHealthWeights are the default search.health.weights, keyed by
// component (sum 100).
var DefaultSearchHealthWeights = map[string]float64{
	"ci_pass_rate":         20,
	"time_to_merge":        15,
	"revert_followup_rate": 20,
	"review_verdict_mix":   10,
	"stale_issue_ratio":    10,
	"signoff_rate":         10,
	"dependency_freshness": 10,
	"security_doc_rot":     5,
}

// SearchConfig (top-level `search:`) controls the portfolio search index
// (pkg/search). Default off: an unset block starts no indexer goroutine and
// creates no /data/search directory.
type SearchConfig struct {
	// Enabled turns the background indexer on when explicitly true.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// MaxIndexBytesPerRepo caps one repo's serialised index; 0 means
	// DefaultSearchMaxIndexBytesPerRepo. Over budget, the previous snapshot
	// stays published and the repo is reported as a coverage gap.
	MaxIndexBytesPerRepo int64 `yaml:"max_index_bytes_per_repo,omitempty" json:"max_index_bytes_per_repo,omitempty"`
	// MaxMemoryBytes caps loaded index snapshots; 0 means
	// DefaultSearchMaxMemoryBytes.
	MaxMemoryBytes int64 `yaml:"max_memory_bytes,omitempty" json:"max_memory_bytes,omitempty"`
	// Retention is how many months of documents are kept, by document time;
	// 0 means DefaultSearchRetentionMonths.
	Retention int `yaml:"retention,omitempty" json:"retention,omitempty"`
	// Health tunes the report-only per-repo health score recomputed on each
	// index rebuild and served at GET /api/repos/{owner}/{repo}/health.
	Health SearchHealthConfig `yaml:"health,omitempty" json:"health,omitzero"`
}

// SearchHealthConfig is search.health. The score is report-only: never an
// ACMM input and never read by merge decisions or Governor priority.
type SearchHealthConfig struct {
	// StaleAfter is how long an open issue may sit idle before it counts as
	// stale ("60d" or a Go duration); empty means 60d.
	StaleAfter string `yaml:"stale_after,omitempty" json:"stale_after,omitempty"`
	// SecurityDocMaxAge is how old the last SECURITY.md / threat-model
	// change may be before the security-doc component decays ("365d" or a
	// Go duration); empty means 365d.
	SecurityDocMaxAge string `yaml:"security_doc_max_age,omitempty" json:"security_doc_max_age,omitempty"`
	// Weights overrides per-component weights; unset keeps the default, 0
	// reports the component without scoring it.
	Weights SearchHealthWeights `yaml:"weights,omitempty" json:"weights,omitzero"`
}

// SearchHealthWeights is search.health.weights.
type SearchHealthWeights struct {
	CIPassRate          *float64 `yaml:"ci_pass_rate,omitempty" json:"ci_pass_rate,omitempty"`
	TimeToMerge         *float64 `yaml:"time_to_merge,omitempty" json:"time_to_merge,omitempty"`
	RevertFollowupRate  *float64 `yaml:"revert_followup_rate,omitempty" json:"revert_followup_rate,omitempty"`
	ReviewVerdictMix    *float64 `yaml:"review_verdict_mix,omitempty" json:"review_verdict_mix,omitempty"`
	StaleIssueRatio     *float64 `yaml:"stale_issue_ratio,omitempty" json:"stale_issue_ratio,omitempty"`
	SignoffRate         *float64 `yaml:"signoff_rate,omitempty" json:"signoff_rate,omitempty"`
	DependencyFreshness *float64 `yaml:"dependency_freshness,omitempty" json:"dependency_freshness,omitempty"`
	SecurityDocRot      *float64 `yaml:"security_doc_rot,omitempty" json:"security_doc_rot,omitempty"`
}

func (w SearchHealthWeights) byKey() map[string]*float64 {
	return map[string]*float64{
		"ci_pass_rate":         w.CIPassRate,
		"time_to_merge":        w.TimeToMerge,
		"revert_followup_rate": w.RevertFollowupRate,
		"review_verdict_mix":   w.ReviewVerdictMix,
		"stale_issue_ratio":    w.StaleIssueRatio,
		"signoff_rate":         w.SignoffRate,
		"dependency_freshness": w.DependencyFreshness,
		"security_doc_rot":     w.SecurityDocRot,
	}
}

// Resolved returns every component's weight, defaults filled in.
func (w SearchHealthWeights) Resolved() map[string]float64 {
	out := make(map[string]float64, len(DefaultSearchHealthWeights))
	for k, v := range DefaultSearchHealthWeights {
		out[k] = v
	}
	for k, v := range w.byKey() {
		if v != nil {
			out[k] = *v
		}
	}
	return out
}

// StaleAfterOrDefault resolves search.health.stale_after.
func (h SearchHealthConfig) StaleAfterOrDefault() time.Duration {
	if d, err := parseSearchAge(h.StaleAfter); err == nil && d > 0 {
		return d
	}
	return DefaultSearchHealthStaleAfter
}

// SecurityDocMaxAgeOrDefault resolves search.health.security_doc_max_age.
func (h SearchHealthConfig) SecurityDocMaxAgeOrDefault() time.Duration {
	if d, err := parseSearchAge(h.SecurityDocMaxAge); err == nil && d > 0 {
		return d
	}
	return DefaultSearchHealthSecurityDocMaxAge
}

// parseSearchAge parses "<N>d" or a Go duration; empty is 0.
func parseSearchAge(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid day count %q", raw)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(raw)
}

// Validate rejects unparsable or non-positive ages and negative weights.
func (h SearchHealthConfig) Validate() error {
	for key, raw := range map[string]string{"stale_after": h.StaleAfter, "security_doc_max_age": h.SecurityDocMaxAge} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if d, err := parseSearchAge(raw); err != nil || d <= 0 {
			return fmt.Errorf("search.health.%s must be a positive duration such as 60d or 1440h, got %q", key, raw)
		}
	}
	for key, v := range h.Weights.byKey() {
		if v != nil && !(*v >= 0) {
			return fmt.Errorf("search.health.weights.%s must not be negative", key)
		}
	}
	return nil
}

// IsEnabled reports whether the search indexer runs (default off).
func (s SearchConfig) IsEnabled() bool {
	return s.Enabled != nil && *s.Enabled
}

// MaxIndexBytesPerRepoOrDefault resolves search.max_index_bytes_per_repo.
func (s SearchConfig) MaxIndexBytesPerRepoOrDefault() int64 {
	if s.MaxIndexBytesPerRepo <= 0 {
		return DefaultSearchMaxIndexBytesPerRepo
	}
	return s.MaxIndexBytesPerRepo
}

// MaxMemoryBytesOrDefault resolves search.max_memory_bytes.
func (s SearchConfig) MaxMemoryBytesOrDefault() int64 {
	if s.MaxMemoryBytes <= 0 {
		return DefaultSearchMaxMemoryBytes
	}
	return s.MaxMemoryBytes
}

// RetentionMonthsOrDefault resolves search.retention in months.
func (s SearchConfig) RetentionMonthsOrDefault() int {
	if s.Retention <= 0 {
		return DefaultSearchRetentionMonths
	}
	return s.Retention
}

// Validate rejects negative limits and retention, which would otherwise be
// read silently as "use the default".
func (s SearchConfig) Validate() error {
	switch {
	case s.MaxIndexBytesPerRepo < 0:
		return fmt.Errorf("search.max_index_bytes_per_repo must not be negative")
	case s.MaxMemoryBytes < 0:
		return fmt.Errorf("search.max_memory_bytes must not be negative")
	case s.Retention < 0:
		return fmt.Errorf("search.retention must not be negative (months)")
	}
	return s.Health.Validate()
}
