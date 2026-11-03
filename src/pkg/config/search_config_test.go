package config

import (
	"testing"
	"time"

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

// search.health resolves to the documented defaults (stale_after 60d,
// security_doc_max_age 365d, weights summing to 100) and accepts day or Go
// durations; bad ages and negative weights fail validation
// (hivecommons/hive#14638).
func TestSearchHealthConfig(t *testing.T) {
	cases := []struct {
		src     string
		stale   time.Duration
		docAge  time.Duration
		weights map[string]float64
		wantErr bool
	}{
		{src: "project:\n  org: acme\n", stale: 60 * 24 * time.Hour, docAge: 365 * 24 * time.Hour},
		{src: "search:\n  health:\n    stale_after: 30d\n    security_doc_max_age: 720h\n    weights:\n      ci_pass_rate: 50\n      security_doc_rot: 0\n",
			stale: 30 * 24 * time.Hour, docAge: 30 * 24 * time.Hour, weights: map[string]float64{"ci_pass_rate": 50, "security_doc_rot": 0}},
		{src: "search:\n  health:\n    stale_after: soon\n", stale: 60 * 24 * time.Hour, docAge: 365 * 24 * time.Hour, wantErr: true},
		{src: "search:\n  health:\n    stale_after: xd\n", stale: 60 * 24 * time.Hour, docAge: 365 * 24 * time.Hour, wantErr: true},
		{src: "search:\n  health:\n    security_doc_max_age: 0d\n", stale: 60 * 24 * time.Hour, docAge: 365 * 24 * time.Hour, wantErr: true},
		{src: "search:\n  health:\n    weights:\n      signoff_rate: -1\n", stale: 60 * 24 * time.Hour, docAge: 365 * 24 * time.Hour,
			weights: map[string]float64{"signoff_rate": -1}, wantErr: true},
	}
	for _, tc := range cases {
		var cfg Config
		if err := yaml.Unmarshal([]byte(tc.src), &cfg); err != nil {
			t.Fatalf("unmarshal %q: %v", tc.src, err)
		}
		h := cfg.Search.Health
		if got := h.StaleAfterOrDefault(); got != tc.stale {
			t.Errorf("%q: StaleAfterOrDefault = %v, want %v", tc.src, got, tc.stale)
		}
		if got := h.SecurityDocMaxAgeOrDefault(); got != tc.docAge {
			t.Errorf("%q: SecurityDocMaxAgeOrDefault = %v, want %v", tc.src, got, tc.docAge)
		}
		got := h.Weights.Resolved()
		sum := 0.0
		for k, def := range DefaultSearchHealthWeights {
			want, ok := tc.weights[k]
			if !ok {
				want = def
			}
			if got[k] != want {
				t.Errorf("%q: weight %s = %v, want %v", tc.src, k, got[k], want)
			}
			sum += def
		}
		if sum != 100 || len(got) != len(DefaultSearchHealthWeights) {
			t.Errorf("default weights sum %v, resolved %v", sum, got)
		}
		if err := cfg.Search.Validate(); (err != nil) != tc.wantErr {
			t.Errorf("%q: Validate = %v, wantErr %v", tc.src, err, tc.wantErr)
		}
	}
}
