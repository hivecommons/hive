package hivectl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDiscoverHiveRepositoriesReadsPublicList(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"repositories":[{"id":"GitHub/GitHub.com/Org/Repo"},{"id":"` + treeNewRepo + `"}]}`))
	}))
	defer server.Close()
	repos, err := DiscoverHiveRepositories(context.Background(), server.Client(), server.URL+"/contribute")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/contribute/repositories" || gotAuth != "" {
		t.Fatalf("request path %q auth %q", gotPath, gotAuth)
	}
	if len(repos) != 2 || repos[0] != treeRepo || repos[1] != treeNewRepo {
		t.Fatalf("repositories = %v", repos)
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"repositories":[]}`))
	}))
	defer empty.Close()
	if repos, err := DiscoverHiveRepositories(context.Background(), nil, empty.URL); err != nil || len(repos) != 0 {
		t.Fatalf("empty list = %v, %v", repos, err)
	}
}

func TestDiscoverHiveRepositoriesFailuresAreErrors(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"repositories":[{"id":"` + treeRepo + `"}]}`))
	}))
	defer target.Close()
	cases := map[string]struct {
		handler http.HandlerFunc
		want    string
	}{
		"redirect": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/api/contribute/repositories", http.StatusFound)
		}, "HTTP 302"},
		"status":    {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }, "HTTP 503"},
		"malformed": {func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`<html>`)) }, "invalid repository discovery"},
		"missing":   {func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }, "omitted repositories"},
		"null":      {func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"repositories":null}`)) }, "omitted repositories"},
		"trailing": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"repositories":[]} {}`))
		}, "trailing"},
		"bad id": {func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"repositories":[{"id":"not an id"}]}`))
		}, ""},
	}
	for name, tc := range cases {
		server := httptest.NewServer(tc.handler)
		repos, err := DiscoverHiveRepositories(context.Background(), server.Client(), server.URL)
		server.Close()
		if err == nil || repos != nil {
			t.Errorf("%s: repos %v err %v", name, repos, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
	if _, err := DiscoverHiveRepositories(context.Background(), nil, "ftp://bad"); err == nil {
		t.Error("invalid hub URL accepted")
	}
	if _, err := DiscoverHiveRepositories(context.Background(), &http.Client{Timeout: 50 * time.Millisecond}, "http://127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("connection failure err = %v", err)
	}
}

func TestSavedTreeHubsReflectReviewedOrigins(t *testing.T) {
	set := &ProfileSet{Hubs: []HubEntry{
		{Name: "commons", URL: "https://hub.example", Reviewed: []string{"https://acme.example"}},
		{Name: "quiet", URL: "https://quiet.example", Follow: HubFollowCurrent},
	}}
	hubs := SavedTreeHubs(set)
	if len(hubs) != 2 || hubs[0].Discovery != TreeUnknown || hubs[0].Follow != HubFollowOff || len(hubs[0].HiveOrigins) != 1 || hubs[0].HiveOrigins[0] != "https://acme.example" {
		t.Fatalf("hubs[0] = %+v", hubs[0])
	}
	if hubs[1].Follow != HubFollowCurrent || hubs[1].HiveOrigins != nil {
		t.Fatalf("hubs[1] = %+v", hubs[1])
	}
	hubs[0].HiveOrigins[0] = "mutated"
	if set.Hubs[0].Reviewed[0] != "https://acme.example" {
		t.Fatal("snapshot aliased the saved reviewed list")
	}
	if SavedTreeHubs(&ProfileSet{}) != nil {
		t.Fatal("no hubs should yield nil")
	}
}

func TestDiscoverContributionTreeKeepsSavedStateThroughOutages(t *testing.T) {
	hive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/contribute/repositories" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"repositories":[{"id":"` + treeRepo + `"}]}`))
	}))
	defer hive.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hives" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"hives":[{"id":"h1","project_name":"Acme","org":"acme","hub_url":"` + hive.URL + `"}]}`))
	}))
	defer hub.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()

	set := &ProfileSet{
		Profiles: []Profile{{Name: "acme", Hub: hive.URL}, {Name: "offline", Hub: down.URL}},
		Hubs: []HubEntry{
			{Name: "live", URL: hub.URL, Reviewed: []string{"https://stale.example"}},
			{Name: "dark", URL: down.URL, Reviewed: []string{"https://remembered.example"}},
		},
	}
	hubs, repos := DiscoverContributionTree(context.Background(), set, time.Second)
	if len(hubs) != 2 {
		t.Fatalf("hubs = %+v", hubs)
	}
	if hubs[0].Discovery != TreeComplete || len(hubs[0].HiveOrigins) != 1 || hubs[0].HiveOrigins[0] != hive.URL {
		t.Fatalf("live hub = %+v", hubs[0])
	}
	if hubs[1].Discovery != TreeUnavailable || len(hubs[1].HiveOrigins) != 1 || hubs[1].HiveOrigins[0] != "https://remembered.example" {
		t.Fatalf("dark hub = %+v", hubs[1])
	}
	if got := repos["acme"]; got.Discovery != TreeComplete || len(got.Repositories) != 1 || got.Repositories[0] != treeRepo {
		t.Fatalf("acme repositories = %+v", got)
	}
	if got := repos["offline"]; got.Discovery != TreeUnavailable || got.Repositories != nil {
		t.Fatalf("offline repositories = %+v", got)
	}
	if set.Hubs[0].Reviewed[0] != "https://stale.example" {
		t.Fatal("discovery wrote back to the saved hub entry")
	}
}
