package dashboard

import (
	"sort"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/taskmcp"
	"github.com/hivecommons/hive/pkg/config"
)

func ciHealthCheckNames(t *testing.T, s *Server, repo string) ([]string, []taskmcp.CheckHealth) {
	t.Helper()
	input := dashboardTaskMCPProvider{server: s}.ciHealthSnapshot(taskmcp.Scope{Repo: repo})
	data, _ := taskmcp.BuildCIHealth(input, time.Now(), taskmcp.PageRequest{})
	names := make([]string, 0, len(data.Checks))
	for _, check := range data.Checks {
		names = append(names, check.Name)
	}
	sort.Strings(names)
	return names, data.Checks
}

// ci_health listed the global required checks for every repository, although
// a repository can replace them (auto_merge.repos.<repo>.required_checks). A
// worker in such a repository was told to watch checks its repository does
// not require, and not told about the ones it does.
func TestTaskMCPCIHealthUsesTheRepositorysRequiredChecks(t *testing.T) {
	s := newTestServer()
	cfg := &config.Config{}
	cfg.AutoMerge.RequiredChecks = []string{"build-gate"}
	cfg.AutoMerge.Repos = map[string]config.AutoMergeRepoConfig{
		"owner/docs":  {RequiredChecks: []string{"docs-lint", "links"}},
		"owner/empty": {RequiredChecks: []string{}},
	}
	s.deps = &Dependencies{Config: cfg}

	for repo, want := range map[string][]string{
		"owner/docs":  {"docs-lint", "links"},
		"owner/other": {"build-gate"},
		"owner/empty": {},
	} {
		got, _ := ciHealthCheckNames(t, s, repo)
		if len(got) != len(want) {
			t.Errorf("%s: ci_health lists %v, want %v", repo, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: ci_health lists %v, want %v", repo, got, want)
				break
			}
		}
	}
}

// ci_health filled default_branch with the primary repository's name. The
// hive has no default branch on record, so the field is left out.
func TestTaskMCPCIHealthDoesNotReportTheRepoNameAsDefaultBranch(t *testing.T) {
	s := newTestServer()
	cfg := &config.Config{}
	cfg.Project.PrimaryRepo = "widgets"
	cfg.AutoMerge.RequiredChecks = []string{"build"}
	s.deps = &Dependencies{Config: cfg}
	_, checks := ciHealthCheckNames(t, s, "owner/widgets")
	if len(checks) != 1 {
		t.Fatalf("checks = %#v", checks)
	}
	if checks[0].DefaultBranch != "" {
		t.Fatalf("default_branch = %q, want it left out", checks[0].DefaultBranch)
	}
}
