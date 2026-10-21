package hivectl

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const treeRepo = "github/github.com/org/repo"
const treeNewRepo = "github/github.com/org/new"

func TestContributionTreeSelection(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		selection                      *RepositorySelection
		discovery                      TreeDiscovery
		repos                          []string
		state                          string
		selected, excluded, unreviewed int
		incomplete, legacy             bool
	}{
		{name: "empty", selection: &RepositorySelection{Mode: SelectionSelected}, discovery: TreeComplete, state: "off"},
		{name: "all", selection: &RepositorySelection{Mode: SelectionSelected, Repositories: []string{treeRepo}}, discovery: TreeComplete, repos: []string{treeRepo}, state: "all-current", selected: 1},
		{name: "partial", selection: &RepositorySelection{Mode: SelectionSelected, Repositories: []string{treeRepo}}, discovery: TreeComplete, repos: []string{treeRepo, treeNewRepo}, state: "partial", selected: 1, unreviewed: 1},
		{name: "excluded", selection: &RepositorySelection{Mode: SelectionSelected, Repositories: []string{treeRepo}, Exclude: []string{treeRepo}}, discovery: TreeComplete, repos: []string{treeRepo}, state: "off", excluded: 1},
		{name: "outage", selection: &RepositorySelection{Mode: SelectionSelected, Repositories: []string{treeRepo}}, discovery: TreeUnavailable, state: "partial", selected: 1, incomplete: true},
		{name: "stale", selection: &RepositorySelection{Mode: SelectionSelected, Repositories: []string{treeRepo}}, discovery: TreeStale, repos: []string{treeRepo}, state: "partial", selected: 1, incomplete: true},
		{name: "legacy", discovery: TreeComplete, repos: []string{treeRepo}, state: "off", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set := &ProfileSet{Profiles: []Profile{{Name: "one", Hub: "https://hive.example", Selection: tc.selection}}}
			before, _ := json.Marshal(tc.selection)
			tree, err := BuildContributionTree(set, nil, map[string]TreeRepositorySnapshot{"one": {Discovery: tc.discovery, Repositories: tc.repos}})
			if err != nil {
				t.Fatal(err)
			}
			got := tree.Selection
			if got.State != tc.state || got.Selected != tc.selected || got.Excluded != tc.excluded || got.Unreviewed != tc.unreviewed || got.MembershipIncomplete != tc.incomplete || got.Legacy != tc.legacy {
				t.Fatalf("selection = %+v", got)
			}
			after, _ := json.Marshal(tc.selection)
			if string(before) != string(after) {
				t.Fatal("mutated selection")
			}
		})
	}
}

func TestContributionTreeSharedAndSeparateIdentities(t *testing.T) {
	set := &ProfileSet{Profiles: []Profile{
		{Name: "one", Hub: "wss://HIVE.example:443/contribute", RegistrationToken: "SECRET", Selection: &RepositorySelection{Mode: SelectionSelected, Repositories: []string{treeRepo}}},
		{Name: "two", Hub: "https://hive.example", RegistrationToken: "OTHER_SECRET", Selection: &RepositorySelection{Mode: SelectionSelected, Repositories: []string{treeRepo}}},
		{Name: "direct", Hub: "https://other.example", Selection: &RepositorySelection{Mode: SelectionSelected, Repositories: []string{treeRepo}}},
	}}
	hubs := []TreeHubSnapshot{
		{Name: "b", URL: "https://b.example", Discovery: TreeComplete, HiveOrigins: []string{"https://hive.example/path", "https://hive.example"}},
		{Name: "a", URL: "https://a.example", Discovery: TreeComplete, HiveOrigins: []string{"wss://hive.example/contribute"}},
	}
	snapshots := map[string]TreeRepositorySnapshot{}
	for _, p := range set.Profiles {
		snapshots[p.Name] = TreeRepositorySnapshot{Discovery: TreeComplete, Repositories: []string{treeRepo}}
	}
	tree, err := BuildContributionTree(set, hubs, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Hubs) != 3 || tree.Hubs[0].Name != "a" || !tree.Hubs[2].Direct || tree.Selection.Selected != 3 {
		t.Fatalf("bad unique rollup: %+v", tree)
	}
	for _, h := range tree.Hubs[:2] {
		if !reflect.DeepEqual(h.Subscriptions, []string{"one", "two"}) || h.Selection.Selected != 2 {
			t.Fatalf("bad shared branch: %+v", h)
		}
	}
	for _, s := range tree.Subscriptions {
		if s.ID != "direct" && s.SharedAcrossHubs != 2 {
			t.Fatalf("lost shared marker: %+v", s)
		}
	}
	encoded, _ := json.Marshal(tree)
	if strings.Contains(string(encoded), "SECRET") || strings.Contains(string(encoded), "registration_token") {
		t.Fatal("leaked credentials")
	}
	// The returned view must not alias mutable selection slices.
	tree.Subscriptions[1].Repositories[0].ID = "changed"
	if set.Profiles[0].Selection.Repositories[0] != treeRepo {
		t.Fatal("view aliases consent")
	}
}

func TestContributionTreeExclusionUnionAndLegacy(t *testing.T) {
	set := &ProfileSet{Profiles: []Profile{
		{Name: "veto", Hub: "https://one.example", Disabled: true, Selection: &RepositorySelection{Mode: SelectionSelected, Exclude: []string{treeRepo}}},
		{Name: "scoped", Hub: "https://two.example", Selection: &RepositorySelection{Mode: SelectionSelected, Repositories: []string{treeRepo}, FollowNewRepositories: true}},
		{Name: "legacy", Hub: "https://three.example"},
	}}
	tree, err := BuildContributionTree(set, nil, map[string]TreeRepositorySnapshot{"legacy": {Discovery: TreeComplete, Repositories: []string{treeRepo}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range tree.Subscriptions {
		switch s.ID {
		case "scoped":
			if s.Repositories[0].Selected || !reflect.DeepEqual(s.Repositories[0].ExcludedBy, []string{"veto"}) || !s.Follow {
				t.Fatalf("exclusion/follow lost: %+v", s)
			}
		case "legacy":
			if !s.Selection.Legacy || len(s.Repositories[0].ExcludedBy) > 0 {
				t.Fatal("legacy narrowed by union")
			}
		case "veto":
			if s.Readiness != "disabled" {
				t.Fatal("disabled readiness missing")
			}
		}
	}
}

func TestContributionTreeRemovedAndUnknown(t *testing.T) {
	set := &ProfileSet{Profiles: []Profile{{Name: "one", Hub: "https://one.example", Selection: &RepositorySelection{Mode: SelectionAllCurrent, MembershipSnapshot: []string{treeRepo}}}}}
	for _, state := range []TreeDiscovery{TreeUnknown, TreeUnavailable, TreeComplete} {
		tree, err := BuildContributionTree(set, nil, map[string]TreeRepositorySnapshot{"one": {Discovery: state}})
		if err != nil {
			t.Fatal(err)
		}
		s := tree.Subscriptions[0]
		r := s.Repositories[0]
		if !r.Selected || s.Readiness != "unknown" || s.Activity != "unavailable" {
			t.Fatal("invented runtime state or dropped consent")
		}
		want := "unknown"
		if state == TreeComplete {
			want = "no"
		}
		if r.Offered != want {
			t.Fatalf("offered = %q", r.Offered)
		}
	}
}

func TestContributionTreeInvalidInputs(t *testing.T) {
	set := &ProfileSet{Profiles: []Profile{{Name: "one", Hub: "https://one.example"}}}
	for _, hubs := range [][]TreeHubSnapshot{
		{{URL: "bad"}},
		{{URL: "https://a.example"}, {URL: "https://A.example:443/path"}},
		{{URL: "https://a.example", Discovery: "bogus"}},
		{{URL: "https://a.example", HiveOrigins: []string{"bad"}}},
	} {
		if _, err := BuildContributionTree(set, hubs, nil); err == nil {
			t.Fatalf("accepted %+v", hubs)
		}
	}
	if _, err := BuildContributionTree(set, nil, map[string]TreeRepositorySnapshot{"one": {Repositories: []string{"bad"}}}); err == nil {
		t.Fatal("accepted invalid repository")
	}
	if _, err := BuildContributionTree(nil, nil, nil); err == nil {
		t.Fatal("accepted nil profiles")
	}
}
