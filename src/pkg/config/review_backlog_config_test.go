package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReviewBacklogConfigRoundTrips(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte(`
review:
  out_of_scope_backlog_disabled: true
  max_out_of_scope_backlog_issues: 2
`), &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if !cfg.Review.OutOfScopeBacklogDisabled {
		t.Fatal("out_of_scope_backlog_disabled did not load")
	}
	if cfg.Review.MaxOutOfScopeBacklogIssues != 2 {
		t.Fatalf("max_out_of_scope_backlog_issues = %d, want 2", cfg.Review.MaxOutOfScopeBacklogIssues)
	}
}
