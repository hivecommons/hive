package commands

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl"
)

func TestPresetOnboardRequiresConfirmationBeforeWrites(t *testing.T) {
	for _, input := range []string{"n\n", ""} {
		t.Run(input, func(t *testing.T) {
			h := newHivesHarness(t)
			d := newPresetDiscovery(t, presetRepoA)
			path := writePreset(t, "version: 1\nname: first\nhives:\n  - url: "+d.server.URL+"\n    repositories: all-current\n")
			if err := h.run(t, input, "hives", "preset", "adopt", path, "--onboard"); err != nil {
				t.Fatal(err)
			}
			if len(h.reg.calls) != 0 || h.signalCalls != 0 {
				t.Fatal("cancelled onboarding registered or reloaded")
			}
			for _, file := range []string{h.store.Path(), h.store.EnvPath()} {
				if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cancelled onboarding created %s: %v", file, err)
				}
			}
			if !strings.Contains(h.out.String(), d.server.URL) || !strings.Contains(h.out.String(), presetRepoA) {
				t.Fatal("destination or scope missing from confirmation")
			}
		})
	}
}

func TestPresetOnboardRejectsYesAndPreservesLegacyOnCancel(t *testing.T) {
	h := newHivesHarness(t)
	h.writeEnv(t, "HIVE_HUB=wss://legacy.example/contribute\nHIVE_REGISTRATION_TOKEN=old-secret\nCONTRIBUTOR_ID=old-id\n")
	before := h.env(t)
	path := writePreset(t, "version: 1\nname: legacy\nhives:\n  - url: https://legacy.example\n    repositories: []\n")
	if err := h.run(t, "", "hives", "preset", "adopt", path, "--onboard", "--yes"); err == nil {
		t.Fatal("unattended first enrollment accepted")
	}
	if err := h.run(t, "n\n", "hives", "preset", "adopt", path, "--onboard"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.store.Path()); !errors.Is(err, os.ErrNotExist) || h.env(t) != before {
		t.Fatal("cancelled onboarding migrated or changed legacy configuration")
	}
	if len(h.reg.calls) != 0 || h.signalCalls != 0 {
		t.Fatal("cancelled onboarding registered or signaled")
	}
}

func TestPresetOnboardEnrollsAndReusesCredentials(t *testing.T) {
	h := newHivesHarness(t)
	d := newPresetDiscovery(t, presetRepoA)
	path := writePreset(t, "version: 1\nname: first\nhives:\n  - url: "+d.server.URL+"\n    repositories: all-current\n")
	args := []string{"hives", "preset", "adopt", path, "--onboard", "--github-user", "reviewed-user"}
	if err := h.run(t, "y\n", args...); err != nil {
		t.Fatal(err)
	}
	set := h.profiles(t)
	if len(set.Profiles) != 1 || len(set.Presets) != 1 || len(h.reg.calls) != 1 || h.reg.calls[0] != d.server.URL+" reviewed-user" {
		t.Fatalf("profiles=%d provenance=%d registrations=%v", len(set.Profiles), len(set.Presets), h.reg.calls)
	}
	p := set.Profiles[0]
	if p.Hub != strings.Replace(d.server.URL, "http://", "ws://", 1)+"/contribute" || p.RegistrationToken != "new-token" || p.ContributorID != "contrib_new" {
		t.Fatalf("incorrect registered profile: %s %s", p.Name, p.Hub)
	}
	if !reflect.DeepEqual(effective(t, h, p.Name), []string{presetRepoA}) || p.Selection.FollowNewRepositories {
		t.Fatal("incorrect scope or implicit follow policy")
	}
	if h.signalCalls != 1 || strings.Contains(h.out.String(), "new-token") {
		t.Fatal("incorrect reload count or token leaked")
	}
	before := readProfilesFile(t, h)
	if err := h.run(t, "y\n", args...); err != nil {
		t.Fatal(err)
	}
	if len(h.reg.calls) != 1 || h.signalCalls != 1 || readProfilesFile(t, h) != before {
		t.Fatal("retry registered or rewrote an already ready Hive")
	}
}

func TestPresetOnboardHubSnapshotDeduplicatesAndPreservesOverrides(t *testing.T) {
	h := newHivesHarness(t)
	d := newPresetDiscovery(t, presetRepoA, presetRepoB)
	other := newPresetDiscovery(t, presetRepoC)
	h.seed(t, presetHives(d.server.URL, &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected, Exclude: []string{presetRepoB}}))
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hives" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("hub discovery crossed credential boundary")
		}
		_, _ = w.Write([]byte(`{"hives":[{"hub_url":"` + d.server.URL + `"},{"hub_url":"` + other.server.URL + `"},{"hub_url":"` + other.server.URL + `/contribute"}]}`))
	}))
	defer hub.Close()
	path := writePreset(t, "version: 1\nname: hubs\nhives:\n  - url: "+d.server.URL+"\n    repositories:\n      - repo: org/b\nhubs:\n  - url: "+hub.URL+"\n    hives: all-current\n")
	if err := h.run(t, "y\n", "hives", "preset", "adopt", path, "--onboard"); err != nil {
		t.Fatal(err)
	}
	set := h.profiles(t)
	if len(set.Profiles) != 3 || len(h.reg.calls) != 1 || h.reg.calls[0] != other.server.URL+" octocat" {
		t.Fatalf("duplicate or wrong enrollment: profiles=%d calls=%v", len(set.Profiles), h.reg.calls)
	}
	if len(effective(t, h, "acme")) != 0 || set.Profiles[0].RegistrationToken != "tok-acme" || !reflect.DeepEqual(set.Profiles[0].Selection.Exclude, []string{presetRepoB}) {
		t.Fatal("hub recommendation overrode direct scope, exclusions or credentials")
	}
	if !reflect.DeepEqual(effective(t, h, set.Profiles[2].Name), []string{presetRepoC}) {
		t.Fatal("hub scope not snapshotted")
	}
}

func TestPresetOnboardPartialFailureKeepsReadyCredentials(t *testing.T) {
	h := newHivesHarness(t)
	d := newPresetDiscovery(t, presetRepoA)
	h.seed(t, presetHives(d.server.URL, nil))
	pending := newPresetDiscovery(t, presetRepoB)
	path := writePreset(t, "version: 1\nname: partial\nhives:\n  - url: "+d.server.URL+"\n    repositories: all-current\n  - url: "+pending.server.URL+"\n    repositories: all-current\n")
	h.reg.err = errors.New("registration rejected")
	args := []string{"hives", "preset", "adopt", path, "--onboard"}
	err := h.run(t, "y\n", args...)
	if err == nil || !strings.Contains(err.Error(), "enrollment pending") {
		t.Fatalf("partial enrollment not reported: %v", err)
	}
	if len(h.profiles(t).Profiles) != 2 || !reflect.DeepEqual(effective(t, h, "acme"), []string{presetRepoA}) || h.signalCalls != 1 {
		t.Fatal("failed target lost successful adoption or saved a placeholder")
	}
	h.reg.err = nil
	if err := h.run(t, "y\n", args...); err != nil {
		t.Fatal(err)
	}
	if len(h.profiles(t).Profiles) != 3 || len(h.reg.calls) != 2 || h.profiles(t).Profiles[0].RegistrationToken != "tok-acme" {
		t.Fatal("retry did not reuse the ready registration")
	}
}

func TestPresetOnboardUnavailableScopeNeverRegisters(t *testing.T) {
	for _, repositories := range []string{"all-current", "[{repo: org/a}]"} {
		t.Run(repositories, func(t *testing.T) {
			h := newHivesHarness(t)
			d := newPresetDiscovery(t)
			d.status = http.StatusNotFound
			path := writePreset(t, "version: 1\nname: unsupported\nhives:\n  - url: "+d.server.URL+"\n    repositories: "+repositories+"\n")
			if err := h.run(t, "y\n", "hives", "preset", "adopt", path, "--onboard"); err == nil {
				t.Fatal("unsupported scope reported success")
			}
			if len(h.reg.calls) != 0 || h.signalCalls != 0 {
				t.Fatal("unsupported target registered or reloaded")
			}
			if _, err := os.Stat(h.store.Path()); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("unsupported target persisted")
			}
		})
	}
}

func TestPresetOnboardHubFailuresDoNotBroadenScope(t *testing.T) {
	for _, body := range []string{`{}`, `not json`, `{"hives":[{"hub_url":"http://insecure.example"}]}`, `{"hives":[]} trailing`} {
		t.Run(body, func(t *testing.T) {
			h := newHivesHarness(t)
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer hub.Close()
			path := writePreset(t, "version: 1\nname: bad hub\nhives: []\nhubs:\n  - url: "+hub.URL+"\n    hives: all-current\n")
			if err := h.run(t, "y\n", "hives", "preset", "adopt", path, "--onboard"); err == nil {
				t.Fatal("bad hub reported ready")
			}
			if len(h.reg.calls) != 0 || h.signalCalls != 0 || !strings.Contains(h.out.String(), "hub discovery unavailable") {
				t.Fatal("bad hub enrolled or concealed outage")
			}
		})
	}
}

// An empty registry (including a JSON null, per hivectl.DiscoverHubHives) is a
// hub that advertises nothing: not an outage, and nothing is enrolled.
func TestPresetOnboardEmptyHubRegistryEnrollsNothing(t *testing.T) {
	for _, body := range []string{`{"hives":[]}`, `{"hives":null}`} {
		t.Run(body, func(t *testing.T) {
			h := newHivesHarness(t)
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer hub.Close()
			path := writePreset(t, "version: 1\nname: empty hub\nhives: []\nhubs:\n  - url: "+hub.URL+"\n    hives: all-current\n")
			_ = h.run(t, "y\n", "hives", "preset", "adopt", path, "--onboard")
			if len(h.reg.calls) != 0 || h.signalCalls != 0 || strings.Contains(h.out.String(), "hub discovery unavailable") {
				t.Fatalf("empty hub enrolled something or was reported as an outage:\n%s", h.out.String())
			}
		})
	}
}

// A registrar that succeeds for one destination and fails for another proves
// successful newly issued credentials survive a later failure in the batch.
type partialPresetRegistrar struct {
	fail  string
	calls []string
}

func (r *partialPresetRegistrar) Register(_ context.Context, base, _ string) (hubRegistration, error) {
	r.calls = append(r.calls, base)
	if base == r.fail {
		return hubRegistration{}, errors.New("registration rejected")
	}
	return hubRegistration{RegistrationToken: "per-hive-token", ContributorID: "ready-id"}, nil
}

func TestPresetOnboardNewSuccessSurvivesLaterFailure(t *testing.T) {
	h := newHivesHarness(t)
	ready := newPresetDiscovery(t, presetRepoA)
	pending := newPresetDiscovery(t, presetRepoB)
	registrar := &partialPresetRegistrar{fail: pending.server.URL}
	factory := hivesDepsFor
	hivesDepsFor = func(e *commandEnv) (*hivesDeps, error) {
		deps, err := factory(e)
		if deps != nil {
			deps.registrar = registrar
		}
		return deps, err
	}
	path := writePreset(t, "version: 1\nname: batch\nhives:\n  - url: "+ready.server.URL+"\n    repositories: all-current\n  - url: "+pending.server.URL+"\n    repositories: all-current\n")
	args := []string{"hives", "preset", "adopt", path, "--onboard"}
	if err := h.run(t, "y\n", args...); err == nil {
		t.Fatal("failure not reported")
	}
	set := h.profiles(t)
	if len(set.Profiles) != 1 || set.Profiles[0].RegistrationToken != "per-hive-token" || h.signalCalls != 1 {
		t.Fatal("successful new credential was lost on partial failure")
	}
	registrar.fail = ""
	if err := h.run(t, "y\n", args...); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(registrar.calls, []string{ready.server.URL, pending.server.URL, pending.server.URL}) {
		t.Fatalf("ready credential registered again: %v", registrar.calls)
	}
}

func TestPresetRefreshHubSnapshotDoesNotEnrollFutureHives(t *testing.T) {
	h := newHivesHarness(t)
	current := newPresetDiscovery(t, presetRepoA)
	future := newPresetDiscovery(t, presetRepoC)
	members := current.server.URL
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"hives":[{"hub_url":"` + members + `"}]}`))
	}))
	defer hub.Close()
	path := writePreset(t, "version: 1\nname: snapshot\nhubs:\n  - url: "+hub.URL+"\n    hives: all-current\n")
	if err := h.run(t, "y\n", "hives", "preset", "adopt", path, "--onboard", "--follow-new-repos"); err != nil {
		t.Fatal(err)
	}
	name := h.profiles(t).Profiles[0].Name
	current.repos = []string{presetRepoA, presetRepoB}
	if err := h.run(t, "", "hives", "preset", "refresh", "--yes"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(effective(t, h, name), []string{presetRepoA, presetRepoB}) {
		t.Fatal("adopted hub Hive did not refresh")
	}
	members = future.server.URL
	if err := h.run(t, "", "hives", "preset", "refresh", "--yes"); err != nil {
		t.Fatal(err)
	}
	if len(h.reg.calls) != 1 || len(h.profiles(t).Profiles) != 1 || !reflect.DeepEqual(effective(t, h, name), []string{presetRepoA, presetRepoB}) {
		t.Fatal("changed hub membership enrolled a Hive or erased accepted scope")
	}
}

func TestPresetOnboardHubRedirectNotFollowed(t *testing.T) {
	h := newHivesHarness(t)
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer hub.Close()
	path := writePreset(t, "version: 1\nname: redirect\nhives: []\nhubs:\n  - url: "+hub.URL+"\n    hives: all-current\n")
	if err := h.run(t, "y\n", "hives", "preset", "adopt", path, "--onboard"); err == nil || redirected || len(h.reg.calls) != 0 {
		t.Fatal("hub redirect followed or not reported")
	}
}
