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

func TestDeselectCurrentPreservesFollowAndOverrides(t *testing.T) {
	const repo = "github/github.com/org/repo"
	for _, mode := range []string{SelectionSelected, SelectionAllCurrent} {
		for _, follow := range []bool{false, true} {
			set := &ProfileSet{Profiles: []Profile{{Name: "a", Hub: "https://a.example", RegistrationToken: "secret", Selection: &RepositorySelection{
				Mode: mode, Repositories: []string{repo}, MembershipSnapshot: []string{repo},
				Include: []string{"github/github.com/org/personal"}, Exclude: []string{repo},
				FollowNewRepositories: follow, Sources: []string{"invitation"},
			}}}}
			if err := set.DeselectCurrent("a"); err != nil {
				t.Fatal(err)
			}
			want := &RepositorySelection{Mode: SelectionSelected, Exclude: []string{repo}, FollowNewRepositories: follow, Sources: []string{"invitation"}}
			if !reflect.DeepEqual(set.Profiles[0].Selection, want) || set.SelectionSummary("a") != "none" || set.Profiles[0].RegistrationToken != "secret" {
				t.Fatalf("deselect changed retained state: %+v", set.Profiles[0])
			}
			if err := set.DeselectCurrent("a"); err != nil || !reflect.DeepEqual(set.Profiles[0].Selection, want) {
				t.Fatal("deselect is not idempotent")
			}
			if err := set.SetSelectionMode("a", "none", nil); err != nil {
				t.Fatal(err)
			}
			if set.Profiles[0].Selection.FollowNewRepositories || len(set.Profiles[0].Selection.Exclude) != 1 {
				t.Fatal("select none must stop following without clearing exclusions")
			}
		}
	}
}

func TestDeselectCurrentRejectsLegacyAndUnknown(t *testing.T) {
	set := &ProfileSet{Profiles: []Profile{{Name: "legacy", Hub: "https://a.example"}}}
	for _, name := range []string{"legacy", "missing"} {
		if err := set.DeselectCurrent(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
		if set.Profiles[0].Selection != nil || set.SelectionSummary("legacy") != "all" {
			t.Fatal("failed deselect migrated legacy scope")
		}
	}
}

func TestSetFollowNewRepositoriesPreservesMembership(t *testing.T) {
	const repo = "github/github.com/org/repo"
	for _, mode := range []string{SelectionSelected, SelectionAllCurrent} {
		set := &ProfileSet{Profiles: []Profile{{Name: "a", Hub: "https://a.example", Selection: &RepositorySelection{
			Mode: mode, Repositories: []string{repo}, MembershipSnapshot: []string{repo},
			Include: []string{repo}, Exclude: []string{repo}, Sources: []string{"invitation"},
		}}}}
		want := *set.Profiles[0].Selection
		for _, follow := range []bool{true, false, false} {
			if _, err := set.SetFollowNewRepositories("a", follow); err != nil {
				t.Fatal(err)
			}
			want.FollowNewRepositories = follow
			if !reflect.DeepEqual(*set.Profiles[0].Selection, want) {
				t.Fatal("follow changed membership, overrides or provenance")
			}
		}
	}
	set := &ProfileSet{Profiles: []Profile{{Name: "legacy", Hub: "https://a.example"}}}
	for _, name := range []string{"legacy", "missing"} {
		for _, follow := range []bool{false, true} {
			if _, err := set.SetFollowNewRepositories(name, follow); err == nil || set.Profiles[0].Selection != nil {
				t.Fatal("follow accepted unknown or migrated legacy profile")
			}
		}
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
