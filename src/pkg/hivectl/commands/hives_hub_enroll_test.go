package commands

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl"
)

func TestHivesHubAdoptReusesExistingAndRegistersMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hives" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"hives":[
			{"id":"a","project_name":"Acme","org":"acme","hub_url":"wss://ACME.example:443/contribute"},
			{"id":"b","project_name":"Beta","org":"beta","hub_url":"wss://beta.example/contribute"}
		]}`))
	}))
	defer server.Close()

	h := newHivesHarness(t)
	h.seed(t, &hivectl.ProfileSet{
		Version: hivectl.ProfilesVersion,
		Profiles: []hivectl.Profile{{
			Name:              "acme",
			Hub:               "wss://acme.example/contribute",
			ContributorID:     "old-contributor",
			RegistrationToken: "old-token",
		}},
		Hubs: []hivectl.HubEntry{{Name: "commons", URL: server.URL}},
	})

	if err := h.run(t, "", "hives", "hub", "adopt", "commons", "--yes"); err != nil {
		t.Fatal(err)
	}
	if len(h.reg.calls) != 1 || !strings.HasPrefix(h.reg.calls[0], "https://beta.example ") {
		t.Fatalf("registration calls = %v, want only beta", h.reg.calls)
	}
	set := h.profiles(t)
	if len(set.Profiles) != 2 {
		t.Fatalf("profiles = %+v, want existing plus beta", set.Profiles)
	}
	acme, _ := set.Find("acme")
	if acme == nil || acme.RegistrationToken != "old-token" {
		t.Fatalf("existing profile was changed: %+v", acme)
	}
	beta, _ := set.Find("beta")
	if beta == nil || beta.RegistrationToken != "new-token" {
		t.Fatalf("new profile missing: %+v", set.Profiles)
	}
	if got := set.Hubs[0].Reviewed; !reflect.DeepEqual(got, []string{"https://acme.example", "https://beta.example"}) {
		t.Fatalf("reviewed = %v", got)
	}
	if !strings.Contains(h.out.String(), "ready") || !strings.Contains(h.out.String(), "beta") {
		t.Fatalf("adoption output = %q", h.out.String())
	}

	beforeCalls := len(h.reg.calls)
	if err := h.run(t, "", "hives", "hub", "adopt", "commons", "--yes"); err != nil {
		t.Fatal(err)
	}
	if len(h.reg.calls) != beforeCalls {
		t.Fatalf("rerun registered an already onboarded Hive: %v", h.reg.calls)
	}
}

func TestHivesHubAdoptSavesSuccessesAndRetriesFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hives" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"hives":[
			{"project_name":"Beta","hub_url":"wss://beta.example/contribute"},
			{"project_name":"Gamma","hub_url":"wss://gamma.example/contribute"}
		]}`))
	}))
	defer server.Close()

	h := newHivesHarness(t)
	h.reg.errorsByBase = map[string]error{"https://gamma.example": errors.New("gamma is offline")}
	h.seed(t, &hivectl.ProfileSet{
		Version: hivectl.ProfilesVersion,
		Hubs:    []hivectl.HubEntry{{Name: "commons", URL: server.URL}},
	})
	if err := h.run(t, "", "hives", "hub", "adopt", "commons", "--yes"); err == nil || !strings.Contains(err.Error(), "gamma is offline") {
		t.Fatalf("partial adoption error = %v", err)
	}
	set := h.profiles(t)
	if profile, _ := set.Find("beta"); profile == nil {
		t.Fatalf("successful beta enrollment was not saved: %+v", set.Profiles)
	}
	if profile, _ := set.Find("gamma"); profile != nil {
		t.Fatalf("failed gamma enrollment was saved: %+v", profile)
	}

	h.reg.errorsByBase = nil
	beforeCalls := len(h.reg.calls)
	if err := h.run(t, "", "hives", "hub", "adopt", "commons", "--yes"); err != nil {
		t.Fatal(err)
	}
	if got := len(h.reg.calls) - beforeCalls; got != 1 || !strings.HasPrefix(h.reg.calls[len(h.reg.calls)-1], "https://gamma.example ") {
		t.Fatalf("retry calls = %v, delta %d; want gamma only", h.reg.calls, got)
	}
	if profile, _ := h.profiles(t).Find("gamma"); profile == nil {
		t.Fatal("gamma was not enrolled on retry")
	}
}

func TestHivesHubAdoptWithoutTTYDoesNotRegisterOrWrite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hives":[{"hub_url":"wss://new.example/contribute"}]}`))
	}))
	defer server.Close()

	h := newHivesHarness(t)
	h.seed(t, &hivectl.ProfileSet{
		Version: hivectl.ProfilesVersion,
		Hubs:    []hivectl.HubEntry{{Name: "commons", URL: server.URL}},
	})
	before, err := os.ReadFile(h.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "", "hives", "hub", "adopt", "commons"); err == nil || ExitCode(err) != ExitUsage || !strings.Contains(err.Error(), "pass --yes") {
		t.Fatalf("non-TTY adoption error = %v (exit %d)", err, ExitCode(err))
	}
	after, err := os.ReadFile(h.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || len(h.reg.calls) != 0 {
		t.Fatalf("non-TTY adoption changed state or registered: changed=%t calls=%v", string(after) != string(before), h.reg.calls)
	}
}

func TestHivesHubFollowChangesOnlyPolicy(t *testing.T) {
	h := newHivesHarness(t)
	selection := &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected, Repositories: []string{"github/github.com/acme/repo"}}
	h.seed(t, &hivectl.ProfileSet{
		Version:  hivectl.ProfilesVersion,
		Profiles: []hivectl.Profile{{Name: "acme", Hub: "wss://acme.example/contribute", RegistrationToken: "tok", Selection: selection}},
		Hubs:     []hivectl.HubEntry{{Name: "commons", URL: "https://commons.example", Follow: hivectl.HubFollowOff}},
	})
	before := *selection
	if err := h.run(t, "", "hives", "hub", "follow", "commons", "--mode", hivectl.HubFollowCurrentAndFutureRepos); err != nil {
		t.Fatal(err)
	}
	set := h.profiles(t)
	if set.Hubs[0].Follow != hivectl.HubFollowCurrentAndFutureRepos {
		t.Fatalf("follow = %q", set.Hubs[0].Follow)
	}
	if !reflect.DeepEqual(*set.Profiles[0].Selection, before) {
		t.Fatalf("current membership changed: before=%+v after=%+v", before, *set.Profiles[0].Selection)
	}
	if len(h.reg.calls) != 0 || h.signalCalls != 0 {
		t.Fatalf("follow called registration/reload: calls=%v signals=%d", h.reg.calls, h.signalCalls)
	}
}

func TestHivesHubRefreshKeepsRemovedAndRequiresConfirmationWhenOff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hives":[{"hub_url":"wss://new.example/contribute"}]}`))
	}))
	defer server.Close()

	h := newHivesHarness(t)
	h.seed(t, &hivectl.ProfileSet{
		Version:  hivectl.ProfilesVersion,
		Profiles: []hivectl.Profile{{Name: "old", Hub: "wss://old.example/contribute", RegistrationToken: "old-token"}},
		Hubs:     []hivectl.HubEntry{{Name: "commons", URL: server.URL, Reviewed: []string{"https://old.example"}}},
	})
	before, err := os.ReadFile(h.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "", "hives", "hub", "refresh", "commons"); err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("refresh without confirmation error = %v (exit %d)", err, ExitCode(err))
	}
	if len(h.reg.calls) != 0 {
		t.Fatalf("refresh without confirmation registered: %v", h.reg.calls)
	}
	after, err := os.ReadFile(h.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("refresh without confirmation changed profiles.yml")
	}

	if err := h.run(t, "", "hives", "hub", "refresh", "commons", "--yes"); err != nil {
		t.Fatal(err)
	}
	set := h.profiles(t)
	if profile, _ := set.Find("old"); profile == nil || profile.RegistrationToken != "old-token" {
		t.Fatalf("removed Hive profile was not retained: %+v", set.Profiles)
	}
	if profile, _ := set.Find("new"); profile == nil {
		t.Fatalf("new Hive was not enrolled: %+v", set.Profiles)
	}
	if !strings.Contains(h.out.String(), "no longer advertised") {
		t.Fatalf("refresh output did not flag removal: %q", h.out.String())
	}
}

func TestHivesHubAdoptCurrentAndFutureRepositories(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/hives":
			_, _ = w.Write([]byte(`{"hives":[{"project_name":"Acme","hub_url":"` + server.URL + `/contribute"}]}`))
		case "/api/contribute/repositories":
			_, _ = w.Write([]byte(`{"repositories":[{"id":"github/github.com/acme/repo"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	h := newHivesHarness(t)
	h.seed(t, &hivectl.ProfileSet{
		Version: hivectl.ProfilesVersion,
		Hubs:    []hivectl.HubEntry{{Name: "commons", URL: server.URL}},
	})
	if err := h.run(t, "", "hives", "hub", "adopt", "commons", "--follow", hivectl.HubFollowCurrentAndFutureRepos, "--yes"); err != nil {
		t.Fatal(err)
	}
	set := h.profiles(t)
	if len(set.Profiles) != 1 || set.Profiles[0].Selection == nil {
		t.Fatalf("profile selection = %+v", set.Profiles)
	}
	selection := set.Profiles[0].Selection
	if selection.Mode != hivectl.SelectionAllCurrent || !selection.FollowNewRepositories || len(selection.MembershipSnapshot) != 1 {
		t.Fatalf("selection = %+v", selection)
	}
	if set.Hubs[0].Follow != hivectl.HubFollowCurrentAndFutureRepos {
		t.Fatalf("hub follow = %q", set.Hubs[0].Follow)
	}
}
