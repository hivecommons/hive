package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// The search indexer is off unless `search.enabled: true`; unset limits and
// retention resolve to the documented defaults (hivecommons/hive#14635).
func TestSearchConfigDefaults(t *testing.T) {
	cases := []struct {
		src       string
		enabled   bool
		maxIndex  int64
		maxMemory int64
		months    int
		wantErr   bool
	}{
		{src: "project:\n  org: acme\n", maxIndex: DefaultSearchMaxIndexBytesPerRepo, maxMemory: DefaultSearchMaxMemoryBytes, months: 24},
		{src: "search:\n  enabled: false\n", maxIndex: DefaultSearchMaxIndexBytesPerRepo, maxMemory: DefaultSearchMaxMemoryBytes, months: 24},
		{src: "search:\n  enabled: true\n  max_index_bytes_per_repo: 1024\n  max_memory_bytes: 4096\n  retention: 6\n", enabled: true, maxIndex: 1024, maxMemory: 4096, months: 6},
		{src: "search:\n  retention: -1\n", maxIndex: DefaultSearchMaxIndexBytesPerRepo, maxMemory: DefaultSearchMaxMemoryBytes, months: 24, wantErr: true},
		{src: "search:\n  max_memory_bytes: -1\n", maxIndex: DefaultSearchMaxIndexBytesPerRepo, maxMemory: DefaultSearchMaxMemoryBytes, months: 24, wantErr: true},
		{src: "search:\n  max_index_bytes_per_repo: -1\n", maxIndex: DefaultSearchMaxIndexBytesPerRepo, maxMemory: DefaultSearchMaxMemoryBytes, months: 24, wantErr: true},
	}
	for _, tc := range cases {
		var cfg Config
		if err := yaml.Unmarshal([]byte(tc.src), &cfg); err != nil {
			t.Fatalf("unmarshal %q: %v", tc.src, err)
		}
		s := cfg.Search
		if got := s.IsEnabled(); got != tc.enabled {
			t.Errorf("%q: IsEnabled = %v, want %v", tc.src, got, tc.enabled)
		}
		if got := s.MaxIndexBytesPerRepoOrDefault(); got != tc.maxIndex {
			t.Errorf("%q: MaxIndexBytesPerRepoOrDefault = %d, want %d", tc.src, got, tc.maxIndex)
		}
		if got := s.MaxMemoryBytesOrDefault(); got != tc.maxMemory {
			t.Errorf("%q: MaxMemoryBytesOrDefault = %d, want %d", tc.src, got, tc.maxMemory)
		}
		if got := s.RetentionMonthsOrDefault(); got != tc.months {
			t.Errorf("%q: RetentionMonthsOrDefault = %d, want %d", tc.src, got, tc.months)
		}
		if err := s.Validate(); (err != nil) != tc.wantErr {
			t.Errorf("%q: Validate = %v, wantErr %v", tc.src, err, tc.wantErr)
		}
	}
}
