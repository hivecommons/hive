package hub

import "testing"

func TestRepoRefHost(t *testing.T) {
	cases := map[string]string{
		"https://github.ibm.com/z-aiops-unite/ui": "github.ibm.com",
		"github.ibm.com/z-aiops-unite/ui":         "github.ibm.com",
		"z-aiops-unite/ui":                        "",
		"ui":                                      "",
		"":                                        "",
	}
	for in, want := range cases {
		if got := repoRefHost(in); got != want {
			t.Errorf("repoRefHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameGitHubHost(t *testing.T) {
	if !sameGitHubHost("", "github.com") {
		t.Error(`"" and "github.com" should be the same host`)
	}
	if !sameGitHubHost("GitHub.com", "") {
		t.Error("host comparison should be case-insensitive and treat blank as public")
	}
	if sameGitHubHost("github.ibm.com", "github.com") {
		t.Error("GHE and public must NOT be the same host")
	}
	if !sameGitHubHost("github.ibm.com", "github.ibm.com") {
		t.Error("a GHE host equals itself")
	}
}

func TestValidateSingleRepoHost(t *testing.T) {
	// All repos on the spoke host (public) — bare names and matching hosts pass.
	if err := validateSingleRepoHost("", "console", []string{"console", "docs", "github.com/kubestellar/kb"}); err != nil {
		t.Errorf("all-public should pass, got %v", err)
	}
	// A GHE hive with all-GHE repos passes.
	if err := validateSingleRepoHost("github.ibm.com", "github.ibm.com/org/a", []string{"github.ibm.com/org/a", "b"}); err != nil {
		t.Errorf("all-GHE should pass, got %v", err)
	}
	// A public hive with a GHE repo mixed in must be REJECTED.
	if err := validateSingleRepoHost("", "console", []string{"console", "github.ibm.com/org/secret"}); err == nil {
		t.Error("mixed public+GHE must be rejected")
	}
	// A GHE hive with a github.com repo mixed in must be REJECTED.
	if err := validateSingleRepoHost("github.ibm.com", "org/a", []string{"a", "github.com/other/pub"}); err == nil {
		t.Error("mixed GHE+public must be rejected")
	}
}
