package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestPRQueueRepoAllowedEmptyOrg(t *testing.T) {
	tests := []struct {
		name  string
		org   string
		repos []string
		repo  string
		want  bool
	}{
		{"org joins bare name", "acme", []string{"web"}, "acme/web", true},
		{"no org does not produce /name", "", []string{"web"}, "/web", false},
		{"no org bare name matches bare", "", []string{"web"}, "web", true},
		{"qualified entry untouched", "", []string{"other/web"}, "other/web", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer()
			s.deps = &Dependencies{Config: &config.Config{Project: config.ProjectConfig{Org: tc.org, Repos: tc.repos}}}
			if got := s.prQueueRepoAllowed(tc.repo); got != tc.want {
				t.Fatalf("prQueueRepoAllowed(%q) = %v, want %v", tc.repo, got, tc.want)
			}
		})
	}
}
