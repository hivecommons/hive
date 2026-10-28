package config

import "fmt"

// Search defaults (hivecommons/hive#14491). The byte limits match
// search.DefaultMaxIndexBytesPerRepo / search.DefaultMaxMemoryBytes.
const (
	DefaultSearchMaxIndexBytesPerRepo int64 = 32 << 20
	DefaultSearchMaxMemoryBytes       int64 = 256 << 20
	DefaultSearchRetentionMonths            = 24
)

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
	return nil
}
