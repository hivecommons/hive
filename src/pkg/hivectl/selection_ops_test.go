package hivectl

import (
	"reflect"
	"testing"
)

func TestSelectionEditsPreserveExclusionsAndCredentials(t *testing.T) {
	const repo = "github/github.com/org/repo"
	set := &ProfileSet{Version: ProfilesVersion, Active: "a", Profiles: []Profile{{Name: "a", Hub: "https://a.example", RegistrationToken: "secret", ContributorID: "person"}, {Name: "b", Hub: "https://b.example"}}}
	if err := set.ExcludeRepo("a", repo); err == nil {
		t.Fatal("cannot subtract from an unreviewed wildcard")
	}
	if err := set.SetSelectionMode("a", SelectionAllCurrent, []string{"GitHub/GitHub.com/Org/Repo", repo}); err != nil {
		t.Fatal(err)
	}
	if err := set.ExcludeRepo("a", repo); err != nil {
		t.Fatal(err)
	}
	if err := set.IncludeRepo("a", repo); err != nil {
		t.Fatal(err)
	}
	if set.SelectionSummary("a") != "none" {
		t.Fatal("include erased exclusion")
	}
	if _, err := set.Rename("a", "renamed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := set.Move("renamed", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := set.Use("b"); err != nil {
		t.Fatal(err)
	}
	p, _ := set.Find("renamed")
	if !reflect.DeepEqual(p.Selection.Exclude, []string{repo}) || p.RegistrationToken != "secret" || p.ContributorID != "person" {
		t.Fatal("unrelated mutation erased consent/credentials")
	}
	if err := set.SetSelectionMode("renamed", "none", nil); err != nil {
		t.Fatal(err)
	}
	if set.SelectionSummary("renamed") != "none" || len(p.Selection.Exclude) != 1 {
		t.Fatal("none must idle and retain exclusions")
	}
	if err := set.SetSelectionMode("renamed", SelectionAllCurrent, []string{repo}); err != nil {
		t.Fatal(err)
	}
	if set.SelectionSummary("renamed") != "none" {
		t.Fatal("snapshot overwrote personal exclusion")
	}
	if err := set.ClearRepoOverride("renamed", repo); err != nil {
		t.Fatal(err)
	}
	if set.SelectionSummary("renamed") != "1 repos" {
		t.Fatal("clear did not restore reviewed membership")
	}
	if err := set.ClearSelection("renamed"); err != nil {
		t.Fatal(err)
	}
	if set.SelectionSummary("renamed") != "all" {
		t.Fatal("reset did not restore legacy")
	}
}

func TestSelectionToggleRetainsPersonalInclusion(t *testing.T) {
	const repo = "github/github.com/org/repo"
	set := &ProfileSet{Profiles: []Profile{{Name: "a", Hub: "https://a.example"}}}
	if err := set.IncludeRepo("a", repo); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"none", "1 repos"} {
		if err := set.ToggleRepoExclusion("a", repo); err != nil {
			t.Fatal(err)
		}
		if got := set.SelectionSummary("a"); got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	}
	if err := set.IncludeRepo("a", repo); err != nil {
		t.Fatal(err)
	}
	if len(set.Profiles[0].Selection.Include) != 1 {
		t.Fatal("duplicate inclusion")
	}
	for _, operation := range []func(string, string) error{set.IncludeRepo, set.ExcludeRepo, set.ClearRepoOverride, set.ToggleRepoExclusion} {
		if err := operation("missing", repo); err == nil {
			t.Fatal("unknown hive accepted")
		}
		if err := operation("a", "bare"); err == nil {
			t.Fatal("bare name accepted")
		}
	}
	before := *set.Profiles[0].Selection
	if err := set.SetSelectionMode("a", SelectionAllCurrent, []string{"bad"}); err == nil {
		t.Fatal("malformed snapshot accepted")
	}
	if !reflect.DeepEqual(before, *set.Profiles[0].Selection) {
		t.Fatal("failed mutation changed selection")
	}
	if err := set.SetSelectionMode("a", "wildcard", nil); err == nil {
		t.Fatal("bad mode accepted")
	}
}

func TestRepositoryInput(t *testing.T) {
	for _, tc := range []struct{ input, forge, host, want string }{
		{"Org/Repo", "github", "GitHub.com", "github/github.com/org/repo"},
		{"github/repo", "github", "github.com", "github/github.com/github/repo"},
		{"GitLab/GitLab.example/Team/Sub/Repo", "github", "github.com", "gitlab/gitlab.example/team/sub/repo"},
		{"Team/Sub/Repo", "gitlab", "gitlab.example", "gitlab/gitlab.example/team/sub/repo"},
	} {
		got, err := RepositoryInput(tc.input, tc.forge, tc.host)
		if err != nil || got != tc.want {
			t.Fatalf("%q => %q, %v", tc.input, got, err)
		}
	}
	for _, input := range []string{"repo", "org/", "https://github.com/org/repo", "org/../repo"} {
		if _, err := RepositoryInput(input, "github", "github.com"); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}
