package hivectltest

import (
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl"
)

func TestContributionStageFixtureShape(t *testing.T) {
	set, hubs, repos := ContributionStageFixture()

	if set == nil {
		t.Fatal("profile set is nil")
	}
	if set.Version != hivectl.ProfilesVersion {
		t.Errorf("version = %d, want %d", set.Version, hivectl.ProfilesVersion)
	}
	if set.Active != "alpha" {
		t.Errorf("active = %q, want alpha", set.Active)
	}
	if got := len(set.Profiles); got != 2 {
		t.Fatalf("profiles = %d, want 2", got)
	}
	for i, want := range []string{"alpha", "beta"} {
		p := set.Profiles[i]
		if p.Name != want {
			t.Errorf("profile[%d].Name = %q, want %q", i, p.Name, want)
		}
		if p.Selection == nil || p.Selection.Mode != hivectl.SelectionSelected {
			t.Errorf("profile %q selection = %+v, want mode %q with nothing selected", want, p.Selection, hivectl.SelectionSelected)
		} else if len(p.Selection.Repositories) != 0 {
			t.Errorf("profile %q has %d selected repositories, want 0", want, len(p.Selection.Repositories))
		}
	}

	if got := len(set.Hubs); got != 2 {
		t.Fatalf("hub entries = %d, want 2", got)
	}
	if set.Hubs[0].URL != ContributionFixtureHubOne || set.Hubs[1].URL != ContributionFixtureHubTwo {
		t.Errorf("hub entry URLs = %q, %q; want %q, %q", set.Hubs[0].URL, set.Hubs[1].URL, ContributionFixtureHubOne, ContributionFixtureHubTwo)
	}

	if got := len(hubs); got != 2 {
		t.Fatalf("hub snapshots = %d, want 2", got)
	}
	for i, h := range hubs {
		if h.URL != set.Hubs[i].URL || h.Name != set.Hubs[i].Name {
			t.Errorf("hub snapshot[%d] = %s %s, does not match hub entry %s %s", i, h.Name, h.URL, set.Hubs[i].Name, set.Hubs[i].URL)
		}
		if h.Discovery != hivectl.TreeComplete {
			t.Errorf("hub %q discovery = %q, want complete", h.Name, h.Discovery)
		}
	}
	// alpha is shared across both hubs; beta lives on hub one only.
	wantOrigins := [][]string{{"https://alpha.example", "https://beta.example"}, {"https://alpha.example"}}
	for i, want := range wantOrigins {
		if !reflect.DeepEqual(hubs[i].HiveOrigins, want) {
			t.Errorf("hub %q origins = %v, want %v", hubs[i].Name, hubs[i].HiveOrigins, want)
		}
	}

	if got := len(repos); got != 2 {
		t.Fatalf("repository snapshots = %d, want 2", got)
	}
	wantRepos := []string{ContributionFixtureRepoA, ContributionFixtureRepoB}
	for _, name := range []string{"alpha", "beta"} {
		snap, ok := repos[name]
		if !ok {
			t.Errorf("missing repository snapshot for %q", name)
			continue
		}
		if snap.Discovery != hivectl.TreeComplete {
			t.Errorf("repos[%q].Discovery = %q, want complete", name, snap.Discovery)
		}
		if !reflect.DeepEqual(snap.Repositories, wantRepos) {
			t.Errorf("repos[%q].Repositories = %v, want %v", name, snap.Repositories, wantRepos)
		}
	}
}

func TestContributionStageFixtureReturnsFreshValues(t *testing.T) {
	set1, hubs1, repos1 := ContributionStageFixture()
	set2, hubs2, repos2 := ContributionStageFixture()

	if set1 == set2 {
		t.Error("successive calls returned the same *ProfileSet")
	}
	if &set1.Profiles[0] == &set2.Profiles[0] {
		t.Error("successive calls share the Profiles backing array")
	}
	if set1.Profiles[0].Selection == set2.Profiles[0].Selection {
		t.Error("successive calls share a *RepositorySelection")
	}
	if &hubs1[0] == &hubs2[0] {
		t.Error("successive calls share the hub snapshot backing array")
	}
	if !reflect.DeepEqual(set1, set2) || !reflect.DeepEqual(hubs1, hubs2) || !reflect.DeepEqual(repos1, repos2) {
		t.Fatal("successive calls returned different content")
	}

	// Mutating one copy must not leak into the next.
	set1.Active = "beta"
	set1.Profiles[0].Selection.Mode = "mutated"
	hubs1[0].HiveOrigins = append(hubs1[0].HiveOrigins, "https://extra.example")
	delete(repos1, "beta")

	set3, hubs3, repos3 := ContributionStageFixture()
	if set3.Active != "alpha" || set3.Profiles[0].Selection.Mode != hivectl.SelectionSelected {
		t.Errorf("mutation leaked into a later profile set: %+v", set3)
	}
	if len(hubs3[0].HiveOrigins) != 2 {
		t.Errorf("mutation leaked into later hub snapshots: %v", hubs3[0].HiveOrigins)
	}
	if _, ok := repos3["beta"]; !ok {
		t.Error("mutation leaked into later repository snapshots")
	}
}
