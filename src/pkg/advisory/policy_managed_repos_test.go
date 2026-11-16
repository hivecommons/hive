package advisory

import (
	"fmt"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestManagedRepos pins #18448: every project repo is managed, primary first,
// and one repository spelled two ways is listed once under its first spelling.
func TestManagedRepos(t *testing.T) {
	if got := ManagedRepos(nil); got != nil {
		t.Fatalf("nil config = %v, want nil", got)
	}
	cfg := &config.Config{}
	cfg.Project.Org = "nate"
	cfg.Project.Repos = []string{"pr-pal", "talks", " NATE/talks ", "", "other/talks"}
	if got, want := fmt.Sprint(ManagedRepos(cfg)), "[pr-pal talks other/talks]"; got != want {
		t.Fatalf("ManagedRepos = %s, want %s", got, want)
	}
	cfg.Project.PrimaryRepo = "nate/talks"
	if got, want := fmt.Sprint(ManagedRepos(cfg)), "[nate/talks pr-pal other/talks]"; got != want {
		t.Fatalf("ManagedRepos with primary = %s, want %s", got, want)
	}
}
