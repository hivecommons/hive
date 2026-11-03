package commands

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl"
)

func TestHivesHubAddListRemoveRoundTrip(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, &hivectl.ProfileSet{Version: hivectl.ProfilesVersion, Profiles: []hivectl.Profile{{Name: "acme", Hub: "wss://acme.example/contribute", RegistrationToken: "tok"}}})

	if err := h.run(t, "", "hives", "hub", "add", "commons", "--url", "https://hub.example"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), `Tracking hub "commons"`) {
		t.Fatalf("add output = %q", h.out.String())
	}
	set := h.profiles(t)
	if len(set.Hubs) != 1 || set.Hubs[0].URL != "https://hub.example" || set.Hubs[0].Follow != hivectl.HubFollowOff {
		t.Fatalf("hubs = %+v", set.Hubs)
	}
	if len(set.Profiles) != 1 || set.Profiles[0].RegistrationToken != "tok" {
		t.Fatalf("profiles changed: %+v", set.Profiles)
	}

	if err := h.run(t, "", "hives", "hub", "add", "Commons", "--url", "https://other.example"); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if err := h.run(t, "", "hives", "hub", "add", "mirror", "--url", "wss://HUB.example:443/contribute"); err == nil {
		t.Fatal("duplicate normalized URL accepted")
	}
	if err := h.run(t, "", "hives", "hub", "add", "nourl"); err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("missing --url err = %v", err)
	}

	if err := h.run(t, "", "hives", "hub", "list"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "commons\thttps://hub.example\tfollow=off") {
		t.Fatalf("list output = %q", h.out.String())
	}
	if err := h.run(t, "", "hives", "hub", "list", "-o", "json"); err != nil {
		t.Fatal(err)
	}
	var rows []hubListRow
	if err := json.Unmarshal(h.out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].Name != "commons" || rows[0].Follow != "off" {
		t.Fatalf("json list = %q (%v)", h.out.String(), err)
	}
	if err := h.run(t, "", "hives", "hub", "list", "-o", "yaml"); err != nil || !strings.Contains(h.out.String(), "name: commons") {
		t.Fatalf("yaml list = %q (%v)", h.out.String(), err)
	}

	if err := h.run(t, "", "hives", "hub", "remove", "nope"); !errors.Is(err, hivectl.ErrHubNotFound) {
		t.Fatalf("remove unknown = %v", err)
	}
	if err := h.run(t, "", "hives", "hub", "remove", "commons"); err != nil {
		t.Fatal(err)
	}
	set = h.profiles(t)
	if len(set.Hubs) != 0 || len(set.Profiles) != 1 {
		t.Fatalf("after remove: %+v", set)
	}
	if err := h.run(t, "", "hives", "hub", "list"); err != nil || !strings.Contains(h.out.String(), "No hubs tracked") {
		t.Fatalf("empty list = %q (%v)", h.out.String(), err)
	}
	if len(h.reg.calls) != 0 || len(h.reissuer.calls) != 0 || h.signalCalls != 0 {
		t.Fatalf("hub commands registered or signaled: reg=%v reissue=%v signals=%d", h.reg.calls, h.reissuer.calls, h.signalCalls)
	}
}

func TestHivesHubAddWorksWithoutProfiles(t *testing.T) {
	h := newHivesHarness(t)
	if err := h.run(t, "", "hives", "hub", "add", "commons", "--url", "https://hub.example"); err != nil {
		t.Fatal(err)
	}
	if set := h.profiles(t); len(set.Hubs) != 1 || len(set.Profiles) != 0 {
		t.Fatalf("set = %+v", set)
	}
	if len(h.reg.calls) != 0 {
		t.Fatalf("hub add registered: %v", h.reg.calls)
	}
}

func TestHivesHubShowMarksOnboardedHives(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"hives":[
			{"id":"a","project_name":"Acme","org":"acme","hub_url":"wss://ACME.example:443/contribute"},
			{"id":"b","project_name":"Beta","org":"beta","hub_url":"https://beta.example"}
		]}`))
	}))
	defer server.Close()
	h := newHivesHarness(t)
	h.seed(t, &hivectl.ProfileSet{
		Version:  hivectl.ProfilesVersion,
		Profiles: []hivectl.Profile{{Name: "acme", Hub: "https://acme.example/contribute", RegistrationToken: "tok"}},
		Hubs:     []hivectl.HubEntry{{Name: "commons", URL: server.URL, Follow: hivectl.HubFollowOff}},
	})

	if err := h.run(t, "", "hives", "hub", "show", "commons"); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{"2 Hive(s) advertised", "https://acme.example\tacme/Acme\tonboarded as acme", "https://beta.example\tbeta/Beta\tnot onboarded"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output missing %q:\n%s", want, out)
		}
	}
	if err := h.run(t, "", "hives", "hub", "show", "commons", "-o", "json"); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ID        string `json:"id"`
		Onboarded bool   `json:"onboarded"`
		Profile   string `json:"profile"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &rows); err != nil || len(rows) != 2 || !rows[0].Onboarded || rows[0].Profile != "acme" || rows[1].Onboarded {
		t.Fatalf("json show = %q (%v)", h.out.String(), err)
	}
	for _, p := range paths {
		if p != "GET /api/hives" {
			t.Fatalf("unexpected hub request %q", p)
		}
	}
	if len(h.reg.calls) != 0 {
		t.Fatalf("show registered: %v", h.reg.calls)
	}
}

func TestHivesHubShowUnavailableHubFails(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
		},
		"status":    func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
		"malformed": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`not json`)) },
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			h := newHivesHarness(t)
			h.seed(t, &hivectl.ProfileSet{Version: hivectl.ProfilesVersion, Hubs: []hivectl.HubEntry{{Name: "commons", URL: server.URL}}})
			err := h.run(t, "", "hives", "hub", "show", "commons")
			if !errors.Is(err, hivectl.ErrHubUnavailable) || ExitCode(err) == ExitSuccess {
				t.Fatalf("err = %v (exit %d)", err, ExitCode(err))
			}
			if strings.Contains(h.out.String(), "Hive(s) advertised") {
				t.Fatalf("unavailable hub printed a list: %q", h.out.String())
			}
		})
	}
}

func TestHivesHubShowUnknownHub(t *testing.T) {
	h := newHivesHarness(t)
	if err := h.run(t, "", "hives", "hub", "show", "nope"); !errors.Is(err, hivectl.ErrHubNotFound) || !strings.Contains(err.Error(), "no hubs are tracked") {
		t.Fatalf("err = %v", err)
	}
	h.seed(t, &hivectl.ProfileSet{Version: hivectl.ProfilesVersion, Hubs: []hivectl.HubEntry{{Name: "commons", URL: "https://hub.example"}}})
	if err := h.run(t, "", "hives", "hub", "show", "nope"); !errors.Is(err, hivectl.ErrHubNotFound) || !strings.Contains(err.Error(), "tracked: commons") {
		t.Fatalf("err = %v", err)
	}
	if err := h.run(t, "", "hives", "hub", "list", "-o", "bogus"); err == nil {
		t.Fatal("bogus output accepted")
	}
	if err := h.run(t, "", "hives", "hub", "show", "commons", "-o", "bogus"); err == nil {
		t.Fatal("bogus output accepted")
	}
}
