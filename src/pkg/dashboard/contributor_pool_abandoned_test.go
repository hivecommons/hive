package dashboard

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestContributorAbandonedCounts(t *testing.T) {
	profiles := []ContributorProfile{
		{GitHubUsername: " Alice ", Abandoned: 2},
		{GitHubUsername: "ALICE", Abandoned: 1},
		{GitHubUsername: "bob", TrustTier: "revoked", Abandoned: 4},
		{GitHubUsername: "quiet"},
		{Abandoned: 1},
	}
	total, byLogin := contributorAbandonedCounts(profiles)
	want := map[string]int{"alice": 3, "bob": 4, "quiet": 0}
	if total != 8 || !reflect.DeepEqual(byLogin, want) {
		t.Fatalf("counts = %d, %v; want 8, %v", total, byLogin, want)
	}
}

func TestBuildContributorPoolStatusAbandoned(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	for _, p := range []ContributorProfile{
		{ContributorID: "a", GitHubUsername: "Alice", Abandoned: 2, AbandonedPRs: []ContributorAbandonRecord{
			{Repo: "org/repo", PR: 1}, {Repo: "org/repo", PR: 2},
		}},
		{ContributorID: "b", GitHubUsername: "bob", TrustTier: "revoked", Abandoned: 1, AbandonedPRs: []ContributorAbandonRecord{
			{Repo: "org/repo", PR: 3},
		}},
		{ContributorID: "c", GitHubUsername: "quiet"},
	} {
		if err := saveContributorProfile(&p); err != nil {
			t.Fatal(err)
		}
	}
	// No hub or sockets: offline profiles still contribute historical counts.
	got := (&Server{}).BuildContributorPoolStatus()
	if got.Abandoned != 3 || !reflect.DeepEqual(got.AbandonedByLogin, map[string]int{"alice": 2, "bob": 1, "quiet": 0}) {
		t.Fatalf("pool = %+v", got)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Abandoned int            `json:"abandoned"`
		ByLogin   map[string]int `json:"abandoned_by_login"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Abandoned != 3 || wire.ByLogin["alice"] != 2 {
		t.Fatalf("missing status JSON counts: %s", data)
	}
}

func TestContributorPoolAbandonedEmptyJSON(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	data, err := json.Marshal((&Server{}).BuildContributorPoolStatus())
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire["abandoned"]) != "0" || string(wire["abandoned_by_login"]) != "{}" {
		t.Fatalf("empty counts must be zero/object: %s", data)
	}
}
