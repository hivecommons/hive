package hivectl

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestHubEntriesValidateAndRoundTrip(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	set := &ProfileSet{Version: ProfilesVersion, Profiles: []Profile{{Name: "acme", Hub: "wss://acme.example/contribute", RegistrationToken: "tok"}}}
	if err := set.AddHub(HubEntry{Name: "commons", URL: "https://Hub.Example/"}); err != nil {
		t.Fatal(err)
	}
	if set.Hubs[0].Follow != HubFollowOff {
		t.Fatalf("default follow = %q, want off", set.Hubs[0].Follow)
	}
	if err := set.AddHub(HubEntry{Name: "COMMONS", URL: "https://other.example"}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if err := set.AddHub(HubEntry{Name: "again", URL: "wss://hub.example:443/contribute"}); err == nil || !strings.Contains(err.Error(), "already tracked") {
		t.Fatalf("duplicate normalized URL error = %v", err)
	}
	for _, bad := range []HubEntry{
		{Name: "bad name", URL: "https://x.example"},
		{Name: "x", URL: "ftp://x.example"},
		{Name: "y", URL: "https://y.example", Follow: "always"},
	} {
		if err := set.AddHub(bad); err == nil {
			t.Errorf("AddHub(%+v) accepted", bad)
		}
	}
	if err := store.Commit(set); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Hubs) != 1 || loaded.Hubs[0].Name != "commons" || loaded.Hubs[0].EffectiveFollow() != HubFollowOff {
		t.Fatalf("loaded hubs = %+v", loaded.Hubs)
	}
	data, _ := os.ReadFile(store.Path())
	if !strings.Contains(string(data), "hubs:") || strings.Count(string(data), "registration_token") != 1 {
		t.Fatalf("hub entry carried credentials:\n%s", data)
	}
	if _, err := loaded.RemoveHub("missing"); !errors.Is(err, ErrHubNotFound) {
		t.Fatalf("RemoveHub(missing) = %v", err)
	}
	removed, err := loaded.RemoveHub("Commons")
	if err != nil || removed.Name != "commons" || loaded.Hubs != nil {
		t.Fatalf("RemoveHub = %+v, %v; hubs %+v", removed, err, loaded.Hubs)
	}
	if err := store.Commit(loaded); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(store.Path())
	if strings.Contains(string(data), "hubs:") {
		t.Fatalf("empty hubs serialized:\n%s", data)
	}
}

func TestHubEntryValidationRejectsDuplicatesInFile(t *testing.T) {
	for name, hubs := range map[string][]HubEntry{
		"name":   {{Name: "a", URL: "https://a.example"}, {Name: "A", URL: "https://b.example"}},
		"url":    {{Name: "a", URL: "https://a.example"}, {Name: "b", URL: "wss://A.example/contribute"}},
		"follow": {{Name: "a", URL: "https://a.example", Follow: "sometimes"}},
	} {
		set := &ProfileSet{Version: ProfilesVersion, Hubs: hubs}
		if err := set.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, hubs)
		}
	}
	for _, follow := range []string{"", HubFollowOff, HubFollowCurrent, HubFollowCurrentAndFutureRepos} {
		if err := ValidateHubFollow(follow); err != nil {
			t.Errorf("ValidateHubFollow(%q) = %v", follow, err)
		}
	}
}

func TestProfilesWithoutHubsStayByteStable(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	set := &ProfileSet{Version: ProfilesVersion, Active: "acme", Profiles: []Profile{{Name: "acme", Hub: "wss://acme.example/contribute", RegistrationToken: "tok"}}}
	if err := store.Save(set); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Hubs != nil {
		t.Fatalf("hubs = %+v, want nil", loaded.Hubs)
	}
	if err := store.Save(loaded); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(store.Path())
	if !bytes.Equal(before, after) || bytes.Contains(after, []byte("hubs")) {
		t.Fatalf("profiles.yml changed:\n%s\n---\n%s", before, after)
	}
}

func TestDiscoverHubHivesParsesRegistry(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"hives":[
			{"id":"h1","project_name":"Acme","org":"acme","hub_url":"wss://Acme.example/contribute","dashboard_url":"https://acme.example"},
			{"id":"h2","project_name":"Dup","org":"acme","hub_url":"https://acme.example"},
			{"id":"h3","project_name":"Bad","hub_url":"not a url"},
			{"id":"h4","project_name":"Beta","org":"beta","hub_url":"https://beta.example:8443"}
		]}`))
	}))
	defer server.Close()
	hives, err := DiscoverHubHives(context.Background(), server.Client(), server.URL+"/contribute")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/hives" || gotAuth != "" {
		t.Fatalf("request path %q auth %q", gotPath, gotAuth)
	}
	if len(hives) != 2 || hives[0].ID != "https://acme.example" || hives[0].RegistryID != "h1" || hives[0].DashboardURL != "https://acme.example" || hives[1].ID != "https://beta.example:8443" {
		t.Fatalf("hives = %+v", hives)
	}
}

func TestDiscoverHubHivesEmptyRegistryIsNotAnError(t *testing.T) {
	for _, body := range []string{`{"hives":[]}`, `{"hives":null}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		hives, err := DiscoverHubHives(context.Background(), nil, server.URL)
		server.Close()
		if err != nil || len(hives) != 0 {
			t.Errorf("%s: hives %+v err %v", body, hives, err)
		}
	}
}

func TestDiscoverHubHivesFailuresAreUnavailable(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"hives":[{"hub_url":"https://redirected.example"}]}`))
	}))
	defer target.Close()
	cases := map[string]http.HandlerFunc{
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/api/hives", http.StatusFound)
		},
		"status": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
		"malformed": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<html>`))
		},
		"missing hives": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) },
		"wrong type":    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"hives":{}}`)) },
		"trailing":      func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"hives":[]} {}`)) },
		"oversized": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"hives":[{"project_name":"` + strings.Repeat("x", maxHubRegistryBytes) + `"}]}`))
		},
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			hives, err := DiscoverHubHives(context.Background(), server.Client(), server.URL)
			var unavailable *HubUnavailableError
			if hives != nil || !errors.Is(err, ErrHubUnavailable) || !errors.As(err, &unavailable) || unavailable.Hub != server.URL {
				t.Fatalf("hives %+v err %v", hives, err)
			}
		})
	}
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	if _, err := DiscoverHubHives(context.Background(), nil, server.URL); !errors.Is(err, ErrHubUnavailable) || errors.Unwrap(err) == nil {
		t.Fatalf("closed server err = %v", err)
	}
	if _, err := DiscoverHubHives(context.Background(), nil, "ftp://hub.example"); !errors.Is(err, ErrHubUnavailable) || !strings.Contains(err.Error(), "invalid hub URL") {
		t.Fatalf("invalid URL err = %v", err)
	}
}
