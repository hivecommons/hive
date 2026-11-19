package advisory

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestPrimaryRepo_MultiRepoConfigNamesOnlyThePrimary pins #18568: a hive that
// manages several repos still has exactly one advisory report repo. The
// configured primary wins over every entry in project.repos, wherever it is
// listed, and with no primary configured the first listed repo is the single
// fallback. No configuration yields more than one repo.
func TestPrimaryRepo_MultiRepoConfigNamesOnlyThePrimary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		primary string
		repos   []string
		want    string
	}{
		{name: "primary listed first", primary: "hive", repos: []string{"hive", "crux", "dibs", "docs"}, want: "hive"},
		{name: "primary listed last", primary: "hive", repos: []string{"crux", "dibs", "hive"}, want: "hive"},
		{name: "primary not listed", primary: "hive", repos: []string{"crux", "dibs"}, want: "hive"},
		{name: "org-qualified primary", primary: "hivecommons/hive", repos: []string{"crux", "hive"}, want: "hivecommons/hive"},
		{name: "no primary falls back to first repo only", repos: []string{"spyre-inference", "torch-spyre"}, want: "spyre-inference"},
		{name: "nothing configured", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Project.Org = "hivecommons"
			cfg.Project.PrimaryRepo = tc.primary
			cfg.Project.Repos = tc.repos
			if got := PrimaryRepo(cfg); got != tc.want {
				t.Fatalf("PrimaryRepo = %q, want %q", got, tc.want)
			}
		})
	}
	if got := PrimaryRepo(nil); got != "" {
		t.Fatalf("PrimaryRepo(nil) = %q, want empty", got)
	}
}
