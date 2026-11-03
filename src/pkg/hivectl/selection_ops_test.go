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

const (
	autoRepoA = "github/github.com/org/a"
	autoRepoB = "github/github.com/org/b"
	autoRepoC = "github/github.com/org/c"
	autoRepoD = "github/github.com/org/d"
)

func autoSet(selection *RepositorySelection) *ProfileSet {
	return &ProfileSet{Version: ProfilesVersion, Profiles: []Profile{{Name: "a", Hub: "https://a.example", RegistrationToken: "t", Selection: selection}}}
}

// A stale snapshot beside mode "selected" is not membership, and a repository
// the user excluded or already chose is never re-added by following.
func TestAddAutoRecordsOnlyGenuinelyNewMembers(t *testing.T) {
	set := autoSet(&RepositorySelection{Mode: SelectionSelected, Repositories: []string{autoRepoA}, MembershipSnapshot: []string{autoRepoB}, Exclude: []string{autoRepoC}, FollowNewRepositories: true})
	n, err := set.AddAuto("a", []string{autoRepoA, autoRepoB, autoRepoC, autoRepoD})
	if err != nil || n != 2 {
		t.Fatalf("added %d, %v; want b and d", n, err)
	}
	sel := set.Profiles[0].Selection
	if want := []string{autoRepoA, autoRepoB, autoRepoD}; !reflect.DeepEqual(sel.Repositories, want) {
		t.Fatalf("Repositories = %v, want %v", sel.Repositories, want)
	}
	if want := []string{autoRepoB, autoRepoD}; !reflect.DeepEqual(sel.Auto, want) {
		t.Fatalf("Auto = %v, want %v", sel.Auto, want)
	}
	effective, err := sel.EffectiveRepositories()
	if want := []string{autoRepoA, autoRepoB, autoRepoD}; err != nil || !reflect.DeepEqual(effective, want) {
		t.Fatalf("effective = %v, %v; want %v (c stays excluded)", effective, err, want)
	}
	if !sel.IsAuto(autoRepoB) || sel.IsAuto(autoRepoA) {
		t.Fatal("IsAuto must be true for b only: a was chosen by hand")
	}
	if n, err := set.AddAuto("a", []string{autoRepoB, autoRepoD}); err != nil || n != 0 {
		t.Fatalf("second AddAuto added %d, %v; want a no-op", n, err)
	}
}

func TestAddAutoUsesTheSnapshotInAllCurrentMode(t *testing.T) {
	set := autoSet(&RepositorySelection{Mode: SelectionAllCurrent, MembershipSnapshot: []string{autoRepoA}})
	if n, err := set.AddAuto("a", []string{autoRepoA, autoRepoB}); err != nil || n != 1 {
		t.Fatalf("added %d, %v", n, err)
	}
	sel := set.Profiles[0].Selection
	if want := []string{autoRepoA, autoRepoB}; !reflect.DeepEqual(sel.MembershipSnapshot, want) || sel.Repositories != nil {
		t.Fatalf("snapshot = %v, repositories = %v", sel.MembershipSnapshot, sel.Repositories)
	}
	if _, err := autoSet(nil).AddAuto("a", []string{autoRepoA}); err == nil {
		t.Fatal("following must not turn an unrestricted profile into a reviewed one")
	}
}

// Auto is provenance for "added by following". Touching a member by hand ends it.
func TestAutoIsDroppedWhenTouchedByHand(t *testing.T) {
	mk := func() *ProfileSet {
		return autoSet(&RepositorySelection{Mode: SelectionSelected, Repositories: []string{autoRepoA, autoRepoB}, Auto: []string{autoRepoB}, FollowNewRepositories: true})
	}
	for _, tc := range []struct {
		name string
		do   func(*ProfileSet) error
	}{
		{"exclude", func(s *ProfileSet) error { return s.ExcludeRepo("a", autoRepoB) }},
		{"include", func(s *ProfileSet) error { return s.IncludeRepo("a", autoRepoB) }},
		{"deselect current", func(s *ProfileSet) error { return s.DeselectCurrent("a") }},
		{"select current", func(s *ProfileSet) error {
			return s.SetSelectionMode("a", SelectionSelected, []string{autoRepoA, autoRepoB})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := mk()
			if err := tc.do(set); err != nil {
				t.Fatal(err)
			}
			if got := set.Profiles[0].Selection.Auto; got != nil {
				t.Fatalf("Auto = %v, want nil", got)
			}
		})
	}
	set := mk()
	if err := set.ExcludeRepo("a", autoRepoA); err != nil {
		t.Fatal(err)
	}
	if got := set.Profiles[0].Selection.Auto; !reflect.DeepEqual(got, []string{autoRepoB}) {
		t.Fatalf("editing a different repository changed Auto: %v", got)
	}
}

func TestPresetRemovalForgetsAuto(t *testing.T) {
	set := autoSet(&RepositorySelection{Mode: SelectionSelected, Repositories: []string{autoRepoA, autoRepoB}, Auto: []string{autoRepoB}, FollowNewRepositories: true})
	if _, err := set.ApplyPreset("a", "curated", false, nil, []string{autoRepoB}); err != nil {
		t.Fatal(err)
	}
	sel := set.Profiles[0].Selection
	if sel.Auto != nil || !reflect.DeepEqual(sel.Repositories, []string{autoRepoA}) {
		t.Fatalf("selection = %+v", sel)
	}
}

// A personal inclusion was chosen by hand, so `preset refresh` must not mark it.
func TestMarkAutoSkipsPersonalInclusionsAndNonMembers(t *testing.T) {
	set := autoSet(&RepositorySelection{Mode: SelectionSelected, Repositories: []string{autoRepoA, autoRepoB}, Include: []string{autoRepoB}})
	if err := set.MarkAuto("a", []string{autoRepoA, autoRepoB, autoRepoC}); err != nil {
		t.Fatal(err)
	}
	if got := set.Profiles[0].Selection.Auto; !reflect.DeepEqual(got, []string{autoRepoA}) {
		t.Fatalf("Auto = %v, want only a", got)
	}
}

func TestAutoRejectsMalformedIdentity(t *testing.T) {
	selection := &RepositorySelection{Mode: SelectionSelected, Auto: []string{"not-a-repository"}}
	if err := selection.Validate(); err == nil {
		t.Fatal("Validate accepted a malformed Auto entry")
	}
}
