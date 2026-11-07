package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestReposSectionEmptyOrg(t *testing.T) {
	tests := []struct {
		name  string
		org   string
		repos []string
		want  string
		deny  string
	}{
		{"org joins bare name", "acme", []string{"web"}, "/acme/web\n", ""},
		{"no org keeps bare name", "", []string{"web"}, "/web\n", "//web"},
		{"qualified entry untouched", "", []string{"other/web"}, "/other/web\n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Project.Org = tc.org
			cfg.Project.Repos = tc.repos
			got := (&Scheduler{cfg: cfg}).reposSection()
			if !strings.Contains(got, tc.want) {
				t.Fatalf("missing %q in:\n%s", tc.want, got)
			}
			if tc.deny != "" && strings.Contains(got, tc.deny) {
				t.Fatalf("unexpected %q in:\n%s", tc.deny, got)
			}
		})
	}
}
