package commands

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
)

const (
	presetRepoA = "github/github.com/org/a"
	presetRepoB = "github/github.com/org/b"
	presetRepoC = "github/github.com/org/c"
)

// presetDiscovery is a fake Hive answering GET /api/contribute/repositories.
type presetDiscovery struct {
	server *httptest.Server
	repos  []string
	status int
}

func newPresetDiscovery(t *testing.T, repos ...string) *presetDiscovery {
	t.Helper()
	d := &presetDiscovery{repos: repos, status: http.StatusOK}
	d.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/contribute/repositories" || r.Header.Get("Authorization") != "" {
			t.Error("discovery path or credential boundary violated")
		}
		w.WriteHeader(d.status)
		if d.status != http.StatusOK {
			return
		}
		ids := make([]string, 0, len(d.repos))
		for _, id := range d.repos {
			ids = append(ids, `{"id":"`+id+`"}`)
		}
		_, _ = w.Write([]byte(`{"repositories":[` + strings.Join(ids, ",") + `]}`))
	}))
	t.Cleanup(d.server.Close)
	return d
}

func writePreset(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "preset.yml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func presetHives(hub string, selection *hivectl.RepositorySelection) *hivectl.ProfileSet {
	set := twoHives()
	set.Profiles[0].Hub = hub
	set.Profiles[0].Selection = selection
	return set
}

func effective(t *testing.T, h *hivesHarness, name string) []string {
	t.Helper()
	p, _ := h.profiles(t).Find(name)
	if p == nil || p.Selection == nil {
		t.Fatalf("%s has no explicit selection", name)
	}
	repos, err := p.Selection.EffectiveRepositories()
	if err != nil {
		t.Fatal(err)
	}
	return repos
}

func readProfilesFile(t *testing.T, h *hivesHarness) string {
	t.Helper()
	data, err := os.ReadFile(h.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestHivesPresetAdoptReviewsThenRecords(t *testing.T) {
	h := newHivesHarness(t)
	d := newPresetDiscovery(t, presetRepoA, presetRepoB)
	h.seed(t, presetHives(d.server.URL, &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected, Exclude: []string{presetRepoB}}))
	path := writePreset(t, `version: 1
name: Bluefin ecosystem
revision: "r1"
hives:
  - url: `+d.server.URL+`
    repositories: all-current
  - url: https://other.example
    repositories:
      - repo: Org/C
  - url: https://missing.example
    repositories:
      - repo: org/d
`)
	before := readProfilesFile(t, h)
	if err := h.run(t, "n\n", "hives", "preset", "adopt", path); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{"https://missing.example: not onboarded, skipped", "+ " + presetRepoA, "x (personally excluded) " + presetRepoB, "+ " + presetRepoC} {
		if !strings.Contains(out, want) {
			t.Fatalf("review missing %q:\n%s", want, out)
		}
	}
	if readProfilesFile(t, h) != before || h.signalCalls != 0 {
		t.Fatal("declined review wrote or signaled")
	}

	if err := h.run(t, "", "hives", "preset", "adopt", path, "--yes"); err != nil {
		t.Fatal(err)
	}
	set := h.profiles(t)
	if len(set.Profiles) != 2 || len(h.reg.calls) != 0 {
		t.Fatal("adopt onboarded a hive or created credentials")
	}
	acme, other := set.Profiles[0], set.Profiles[1]
	if !reflect.DeepEqual(acme.Selection.Repositories, []string{presetRepoA}) || !reflect.DeepEqual(acme.Selection.Exclude, []string{presetRepoB}) || !reflect.DeepEqual(acme.Selection.Sources, []string{"bluefin-ecosystem"}) {
		t.Fatalf("acme selection = %+v", acme.Selection)
	}
	if !reflect.DeepEqual(other.Selection.Repositories, []string{presetRepoC}) || other.Selection.FollowNewRepositories {
		t.Fatalf("other selection = %+v", other.Selection)
	}
	if acme.RegistrationToken != "tok-acme" || other.RegistrationToken != "tok-other" {
		t.Fatal("adopt changed credentials")
	}
	if len(set.Presets) != 1 || set.Presets[0].Source != path || !strings.HasPrefix(set.Presets[0].ReviewedRevision, "r1@sha256:") || !set.Presets[0].AcceptedAt.Equal(time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("provenance = %+v", set.Presets)
	}
	if h.signalCalls != 1 {
		t.Fatal("adopt did not reload exactly once")
	}

	version := set.RepoScopeVersion
	if err := h.run(t, "", "hives", "preset", "adopt", path, "--yes"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "already adopted") || h.profiles(t).RepoScopeVersion != version || h.signalCalls != 1 {
		t.Fatal("re-adopting the same revision was not a no-op")
	}
}

func TestHivesPresetAdoptEmptySelectionStaysEmpty(t *testing.T) {
	for _, tc := range []struct{ name, repositories string }{
		{"explicit empty list", "[]"},
		{"empty all-current snapshot", "all-current"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHivesHarness(t)
			d := newPresetDiscovery(t)
			h.seed(t, presetHives(d.server.URL, nil))
			path := writePreset(t, "version: 1\nname: empty\nhives:\n  - url: "+d.server.URL+"\n    repositories: "+tc.repositories+"\n")
			if err := h.run(t, "", "hives", "preset", "adopt", path, "--yes"); err != nil {
				t.Fatal(err)
			}
			set := h.profiles(t)
			if set.Profiles[0].Selection == nil || set.SelectionSummary("acme") != "none" {
				t.Fatal("empty recommendation became unrestricted")
			}
			if !strings.Contains(h.env(t), "HIVE_REPO_SCOPE=',*'") {
				t.Fatalf("projection is not an explicit empty scope:\n%s", h.env(t))
			}
		})
	}
}

func TestHivesPresetAdoptUnknownHiveAndNoProfiles(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	path := writePreset(t, "version: 1\nname: x\nhives:\n  - url: https://missing.example\n    repositories: all-current\n")
	if err := h.run(t, "", "hives", "preset", "adopt", path, "--hive", "nope", "--yes"); err == nil {
		t.Fatal("unknown --hive accepted")
	}
	before := readProfilesFile(t, h)
	if err := h.run(t, "", "hives", "preset", "adopt", path, "--yes"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "not onboarded, skipped") || readProfilesFile(t, h) != before || h.signalCalls != 0 {
		t.Fatal("a preset with no onboarded hive wrote state")
	}
	if err := h.run(t, "", "hives", "preset", "refresh"); err == nil {
		t.Fatal("refresh without an adopted preset succeeded")
	}
}

func TestHivesPresetRefreshAdditionsAndRemovals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		follow  bool
		exclude []string
		stdin   string
		args    []string
		want    []string
		changed bool
	}{
		{name: "additions need review by default", stdin: "n\nn\n", want: []string{presetRepoA}},
		{name: "follow policy includes additions, removal kept", follow: true, stdin: "n\n", want: []string{presetRepoA, presetRepoB}, changed: true},
		{name: "exclusion wins over follow policy", follow: true, exclude: []string{presetRepoB}, stdin: "n\n", want: []string{presetRepoA}},
		{name: "--yes accepts additions but never removals", args: []string{"--yes"}, want: []string{presetRepoA, presetRepoB}, changed: true},
		{name: "--accept-removals confirms removal", args: []string{"--yes", "--accept-removals"}, want: []string{presetRepoB}, changed: true},
		{name: "interactive confirmation", stdin: "y\ny\n", want: []string{presetRepoB}, changed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHivesHarness(t)
			d := newPresetDiscovery(t, presetRepoA)
			var selection *hivectl.RepositorySelection
			if tc.exclude != nil {
				selection = &hivectl.RepositorySelection{Mode: hivectl.SelectionAllCurrent, Exclude: tc.exclude}
			}
			h.seed(t, presetHives(d.server.URL, selection))
			path := writePreset(t, "version: 1\nname: p\nhives:\n  - url: "+d.server.URL+"\n    repositories: all-current\n")
			adopt := []string{"hives", "preset", "adopt", path, "--yes"}
			if tc.follow {
				adopt = append(adopt, "--follow-new-repos")
			}
			if err := h.run(t, "", adopt...); err != nil {
				t.Fatal(err)
			}
			d.repos = []string{presetRepoB}
			version, signals := h.profiles(t).RepoScopeVersion, h.signalCalls
			if err := h.run(t, tc.stdin, append([]string{"hives", "preset", "refresh"}, tc.args...)...); err != nil {
				t.Fatal(err)
			}
			if got := effective(t, h, "acme"); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("effective = %v, want %v\n%s", got, tc.want, h.out.String())
			}
			if !strings.Contains(h.out.String(), presetRepoA) {
				t.Fatalf("removal was not shown for review:\n%s", h.out.String())
			}
			changed := h.profiles(t).RepoScopeVersion != version
			if changed != tc.changed || (h.signalCalls != signals) != tc.changed {
				t.Fatalf("changed = %v, want %v", changed, tc.changed)
			}
			if !reflect.DeepEqual(h.profiles(t).Profiles[0].Selection.Exclude, tc.exclude) {
				t.Fatal("refresh touched personal exclusions")
			}
		})
	}
}

func TestHivesPresetRefreshUnavailablePreservesState(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(d *presetDiscovery, path string)
	}{
		{"preset fetch failure", func(_ *presetDiscovery, path string) { _ = os.Remove(path) }},
		{"discovery outage", func(d *presetDiscovery, _ string) { d.status = http.StatusServiceUnavailable }},
		{"discovery returns nothing usable", func(d *presetDiscovery, _ string) { d.status = http.StatusNotFound }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHivesHarness(t)
			d := newPresetDiscovery(t, presetRepoA)
			h.seed(t, presetHives(d.server.URL, nil))
			path := writePreset(t, "version: 1\nname: p\nhives:\n  - url: "+d.server.URL+"\n    repositories: all-current\n")
			if err := h.run(t, "", "hives", "preset", "adopt", path, "--yes", "--follow-new-repos"); err != nil {
				t.Fatal(err)
			}
			before, signals := readProfilesFile(t, h), h.signalCalls
			tc.fail(d, path)
			err := h.run(t, "", "hives", "preset", "refresh", "--yes", "--accept-removals")
			if err == nil || !strings.Contains(err.Error(), "discovery unavailable") {
				t.Fatalf("outage not reported: %v", err)
			}
			if readProfilesFile(t, h) != before || h.signalCalls != signals {
				t.Fatal("outage changed saved consent")
			}
			if got := effective(t, h, "acme"); !reflect.DeepEqual(got, []string{presetRepoA}) {
				t.Fatalf("outage changed scope: %v", got)
			}
		})
	}
}
