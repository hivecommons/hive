package commands

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl"
)

func TestSelectHubHives(t *testing.T) {
	hives := []hivectl.HubHive{
		{ID: "https://a.example", HubURL: "wss://a.example/contribute"},
		{ID: "https://b.example", HubURL: "wss://b.example/contribute"},
	}
	tests := []struct {
		name      string
		requested []string
		wantIDs   []string
		wantErr   string
	}{
		{name: "no filter keeps all", wantIDs: []string{"https://a.example", "https://b.example"}},
		{name: "filter one", requested: []string{"wss://b.example/contribute"}, wantIDs: []string{"https://b.example"}},
		{name: "not advertised", requested: []string{"wss://c.example/contribute"}, wantErr: "not advertised"},
		{name: "invalid url", requested: []string{"://bad"}, wantErr: "--hive"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectHubHives(hives, tc.requested)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ids := hubHiveIDs(got); strings.Join(ids, ",") != strings.Join(tc.wantIDs, ",") {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}

func TestHubEnrollHelpers(t *testing.T) {
	if got := optionalHubName(nil); got != "" {
		t.Fatalf("optionalHubName(nil) = %q", got)
	}
	if got := optionalHubName([]string{"commons"}); got != "commons" {
		t.Fatalf("optionalHubName = %q", got)
	}
	if !sameStrings([]string{"a", "b"}, []string{"a", "b"}) || sameStrings([]string{"a"}, []string{"b"}) || sameStrings([]string{"a"}, nil) {
		t.Fatal("sameStrings mismatch")
	}
	if got := filterReviewedIDs([]string{"a", "b"}, nil); len(got) != 2 {
		t.Fatalf("filterReviewedIDs without failures = %v", got)
	}
	if got := filterReviewedIDs([]string{"a", "b"}, map[string]bool{"a": true}); len(got) != 1 || got[0] != "b" {
		t.Fatalf("filterReviewedIDs = %v", got)
	}
	if got := sortedReviewedIDs([]string{"b", "a"}); got[0] != "a" || got[1] != "b" {
		t.Fatalf("sortedReviewedIDs = %v", got)
	}
	reviewed := hubReviewedSet([]string{"wss://a.example/contribute", "://bad"})
	if !reviewed["https://a.example"] || len(reviewed) != 1 {
		t.Fatalf("hubReviewedSet = %v", reviewed)
	}
	if err := joinHubErrors("x", nil); err != nil {
		t.Fatalf("joinHubErrors(nil) = %v", err)
	}
	if err := joinHubErrors("x", []string{"a", "b"}); err == nil || err.Error() != "x: a; b" {
		t.Fatalf("joinHubErrors = %v", err)
	}
	taken := map[string]struct{}{"beta": {}}
	if got := uniqueHubProfileName("Beta", taken); got != "Beta-2" {
		t.Fatalf("uniqueHubProfileName = %q", got)
	}
	if got := uniqueHubProfileName("", map[string]struct{}{}); got != "hive" {
		t.Fatalf("uniqueHubProfileName empty = %q", got)
	}
	set := &hivectl.ProfileSet{Profiles: []hivectl.Profile{{Name: "Acme"}}}
	if _, ok := profileNameSet(set)["acme"]; !ok {
		t.Fatal("profileNameSet should lower-case names")
	}
	if got := nextHubProfileName(set, "wss://acme.example/contribute"); got == "" {
		t.Fatal("nextHubProfileName returned empty name")
	}
	if err := applyHubFollowSelection(set, "acme", hivectl.HubFollowOff, nil); err != nil {
		t.Fatalf("applyHubFollowSelection off = %v", err)
	}
	if err := applyHubFollowSelection(set, "missing", hivectl.HubFollowCurrent, nil); err == nil {
		t.Fatal("applyHubFollowSelection for unknown profile should fail")
	}
}

func TestHubsToRefresh(t *testing.T) {
	empty := &hivectl.ProfileSet{}
	if _, err := hubsToRefresh(empty, ""); err == nil || !strings.Contains(err.Error(), "no hubs tracked") {
		t.Fatalf("empty err = %v", err)
	}
	if _, err := hubsToRefresh(empty, "nope"); !errors.Is(err, hivectl.ErrHubNotFound) {
		t.Fatalf("unknown err = %v", err)
	}
	set := &hivectl.ProfileSet{Hubs: []hivectl.HubEntry{{Name: "a", URL: "https://a.example"}, {Name: "b", URL: "https://b.example"}}}
	all, err := hubsToRefresh(set, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("all = %v, %v", all, err)
	}
	one, err := hubsToRefresh(set, "b")
	if err != nil || len(one) != 1 || one[0].Name != "b" {
		t.Fatalf("one = %v, %v", one, err)
	}
}

func TestConfirmHubEnrollment(t *testing.T) {
	root := NewRootCommand(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	ok, err := confirmHubEnrollment(root, true, "q?")
	if err != nil || !ok {
		t.Fatalf("yes = %v, %v", ok, err)
	}
	ok, err = confirmHubEnrollment(root, false, "q?")
	if err == nil || ok || ExitCode(err) != ExitUsage {
		t.Fatalf("non-tty = %v, %v", ok, err)
	}
}

func TestPrintHubTables(t *testing.T) {
	var out bytes.Buffer
	printHubAdoptionTable(&out, "Heading", []hubAdoptionRow{
		{ID: "a", Endpoint: "https://a.example", Profile: "a", Status: "ready", Reused: true},
		{ID: "b", Endpoint: "https://b.example", Status: "to enroll"},
		{ID: "c", Status: "ready", Removed: true},
	})
	for _, want := range []string{"Heading", "ready (reused)", "to enroll", "no longer advertised"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("adoption table missing %q: %q", want, out.String())
		}
	}
	out.Reset()
	printHubRefreshTable(&out, "Refresh", []hubRefreshPlan{{
		hub:  &hivectl.HubEntry{Name: "commons", URL: "https://commons.example"},
		rows: []hubAdoptionRow{{ID: "a", Profile: "a", Status: "ready", Reused: true}, {ID: "b", Status: "unavailable: boom"}},
	}})
	for _, want := range []string{"Refresh", `Hub "commons"`, "ready (reused)", "unavailable: boom"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("refresh table missing %q: %q", want, out.String())
		}
	}
}

func TestHivesHubFollowValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing mode", args: []string{"hives", "hub", "follow", "commons"}, want: "--mode is required"},
		{name: "blank mode", args: []string{"hives", "hub", "follow", "commons", "--mode", " "}, want: "--mode is required"},
		{name: "invalid mode", args: []string{"hives", "hub", "follow", "commons", "--mode", "sometimes"}, want: "must be one of"},
		{name: "unknown hub", args: []string{"hives", "hub", "follow", "ghost", "--mode", "off"}, want: "ghost"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHivesHarness(t)
			h.seed(t, &hivectl.ProfileSet{
				Version: hivectl.ProfilesVersion,
				Hubs:    []hivectl.HubEntry{{Name: "commons", URL: "https://commons.example"}},
			})
			err := h.run(t, "", tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}

	h := newHivesHarness(t)
	h.seed(t, &hivectl.ProfileSet{
		Version: hivectl.ProfilesVersion,
		Hubs:    []hivectl.HubEntry{{Name: "commons", URL: "https://commons.example", Follow: hivectl.HubFollowOff}},
	})
	if err := h.run(t, "", "hives", "hub", "follow", "commons", "--mode", hivectl.HubFollowOff); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "already follows") {
		t.Fatalf("output = %q", h.out.String())
	}
}

func TestHivesHubAdoptRejectsBadInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hives":[{"hub_url":"wss://a.example/contribute"}]}`))
	}))
	defer server.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failing.Close()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "invalid output", args: []string{"-o", "xml", "hives", "hub", "adopt", "commons", "--yes"}, want: "invalid --output"},
		{name: "invalid follow", args: []string{"hives", "hub", "adopt", "commons", "--follow", "later", "--yes"}, want: "must be one of"},
		{name: "unknown hub", args: []string{"hives", "hub", "adopt", "ghost", "--yes"}, want: "ghost"},
		{name: "hive not advertised", args: []string{"hives", "hub", "adopt", "commons", "--hive", "wss://z.example/contribute", "--yes"}, want: "not advertised"},
		{name: "discovery failure", args: []string{"hives", "hub", "adopt", "broken", "--yes"}, want: "cannot adopt Hives"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHivesHarness(t)
			h.seed(t, &hivectl.ProfileSet{
				Version: hivectl.ProfilesVersion,
				Hubs: []hivectl.HubEntry{
					{Name: "commons", URL: server.URL},
					{Name: "broken", URL: failing.URL},
				},
			})
			err := h.run(t, "", tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(h.reg.calls) != 0 {
				t.Fatalf("registered despite error: %v", h.reg.calls)
			}
		})
	}
}

func TestHivesHubAdoptJSONOutputAndRepositoryDiscoveryFailure(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hives" {
			_, _ = w.Write([]byte(`{"hives":[{"project_name":"Acme","hub_url":"` + server.URL + `/contribute"}]}`))
			return
		}
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer server.Close()

	h := newHivesHarness(t)
	h.seed(t, &hivectl.ProfileSet{
		Version: hivectl.ProfilesVersion,
		Hubs:    []hivectl.HubEntry{{Name: "commons", URL: server.URL}},
	})
	err := h.run(t, "", "-o", "json", "hives", "hub", "adopt", "commons", "--follow", hivectl.HubFollowCurrent, "--yes")
	if err == nil || !strings.Contains(err.Error(), "repository discovery failed") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.out.String(), `"status"`) || len(h.reg.calls) != 0 {
		t.Fatalf("json output = %q, calls = %v", h.out.String(), h.reg.calls)
	}

	if err := h.run(t, "", "-o", "json", "hives", "hub", "adopt", "commons", "--follow", hivectl.HubFollowOff, "--yes"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), `"ready"`) {
		t.Fatalf("json output = %q", h.out.String())
	}
}

func TestHivesHubRefreshErrorsAndJSON(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, &hivectl.ProfileSet{Version: hivectl.ProfilesVersion})
	if err := h.run(t, "", "hives", "hub", "refresh"); err == nil || !strings.Contains(err.Error(), "no hubs tracked") {
		t.Fatalf("no hubs err = %v", err)
	}
	if err := h.run(t, "", "-o", "xml", "hives", "hub", "refresh"); err == nil || !strings.Contains(err.Error(), "invalid --output") {
		t.Fatalf("invalid output err = %v", err)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failing.Close()
	h.seed(t, &hivectl.ProfileSet{
		Version: hivectl.ProfilesVersion,
		Hubs:    []hivectl.HubEntry{{Name: "broken", URL: failing.URL}},
	})
	err := h.run(t, "", "-o", "json", "hives", "hub", "refresh")
	if err == nil || !strings.Contains(err.Error(), "completed with failures") {
		t.Fatalf("unreachable err = %v", err)
	}
	if !strings.Contains(h.out.String(), "unavailable") {
		t.Fatalf("output = %q", h.out.String())
	}
}
