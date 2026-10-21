package commands

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/spf13/cobra"
)

func TestPresetHubMembersRejectsRegistryLimits(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{name: "oversized", body: strings.Repeat(" ", (4<<20)+1), want: "malformed registry"},
		{name: "redirect", body: `{"hives":[]}`, status: http.StatusFound, want: "HTTP 302"},
		{name: "unsafe origin", body: `{"hives":[{"hub_url":"http://hive.example"}]}`, want: "must use https://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.status != 0 {
					w.Header().Set("Location", "/elsewhere")
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer hub.Close()
			e := &commandEnv{options: &rootOptions{timeout: time.Second}}
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			members, err := e.presetHubMembers(cmd, hub.URL)
			if err == nil || !strings.Contains(err.Error(), tc.want) || members != nil {
				t.Fatalf("members=%v err=%v; want rejection containing %q", members, err, tc.want)
			}
		})
	}
}

func TestPresetHubMembersSkipsUnusableOriginsAndDeduplicates(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"hives":[{"hub_url":"not a URL"},{"hub_url":"https://hive.example"},{"hub_url":"wss://hive.example/contribute"}]}`))
	}))
	defer hub.Close()
	e := &commandEnv{options: &rootOptions{timeout: time.Second}}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	members, err := e.presetHubMembers(cmd, hub.URL)
	if err != nil || len(members) != 1 || members[0] != "https://hive.example" {
		t.Fatalf("members=%v err=%v; want the single usable origin", members, err)
	}
}

func TestPresetOnboardHubExplicitMembersAndExpansionLimit(t *testing.T) {
	for _, limit := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit members", true: "expansion limit"}[limit], func(t *testing.T) {
			h := newHivesHarness(t)
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"hives":[{"hub_url":"https://selected.example"},{"hub_url":"https://ignored.example"}]}`))
			}))
			defer hub.Close()
			preset := &hivectl.Preset{Name: "members", Hubs: []hivectl.PresetHub{{URL: hub.URL}}}
			preset.Hubs[0].Hives.URLs = []string{"https://selected.example", "https://missing.example"}
			if limit {
				preset.Hubs[0].Hives.AllCurrent = true
				preset.Hubs[0].Hives.URLs = nil
				preset.Hives = make([]hivectl.PresetHive, hivectl.MaxPresetEntries)
				for i := range preset.Hives {
					preset.Hives[i].URL = "https://existing.example"
				}
			}
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetOut(h.out)
			e := &commandEnv{options: &rootOptions{timeout: time.Second}}
			expanded, reports := e.expandPresetOnboard(cmd, preset, "members")
			if len(reports) != 1 {
				t.Fatalf("reports=%+v", reports)
			}
			if limit {
				if len(expanded.Hives) != hivectl.MaxPresetEntries || !strings.Contains(reports[0].Status, "exceeds Hive limit") {
					t.Fatalf("limit not enforced: %+v", reports)
				}
			} else if len(expanded.Hives) != 1 || expanded.Hives[0].URL != "https://selected.example" || !strings.Contains(reports[0].Status, "not advertised by hub") {
				t.Fatalf("explicit membership not respected: %+v %+v", expanded, reports)
			}
			if expanded.Hubs != nil || len(preset.Hubs) != 1 {
				t.Fatal("expansion retained hubs or mutated publisher input")
			}
		})
	}
}

func TestPresetProfileNameAvoidsCollisionsAndTruncates(t *testing.T) {
	origin := "https://" + strings.Repeat("a", 60) + ".example"
	set := &hivectl.ProfileSet{}
	first := presetProfileName(set, origin)
	if len(first) != 40 {
		t.Fatalf("name length=%d", len(first))
	}
	set.Profiles = []hivectl.Profile{{Name: first}, {Name: first + "-2"}}
	if got := presetProfileName(set, origin); got != first+"-3" {
		t.Fatalf("collision name=%q", got)
	}
}

func TestPresetOnboardRegistrationWithoutTokenSavesNothing(t *testing.T) {
	h := newHivesHarness(t)
	d := newPresetDiscovery(t, presetRepoA)
	h.reg.result = hubRegistration{Message: "already registered"}
	path := writePreset(t, "version: 1\nname: no token\nhives:\n  - url: "+d.server.URL+"\n    repositories: all-current\n")
	err := h.run(t, "y\n", "hives", "preset", "adopt", path, "--onboard")
	if err == nil || !strings.Contains(err.Error(), "enrollment pending") || len(h.reg.calls) != 1 || h.signalCalls != 0 {
		t.Fatalf("err=%v calls=%v reloads=%d", err, h.reg.calls, h.signalCalls)
	}
	if _, err := os.Stat(h.store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed registration persisted: %v", err)
	}
}

func TestPresetOnboardPreflightErrorsDoNotWrite(t *testing.T) {
	for _, scenario := range []string{"dependencies", "store", "missing preset", "unknown profile", "github user"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHivesHarness(t)
			d := newPresetDiscovery(t, presetRepoA)
			path := writePreset(t, "version: 1\nname: preflight\nhives:\n  - url: "+d.server.URL+"\n    repositories: all-current\n")
			args := []string{"hives", "preset", "adopt", path, "--onboard"}
			factory := hivesDepsFor
			switch scenario {
			case "dependencies":
				hivesDepsFor = func(*commandEnv) (*hivesDeps, error) { return nil, errors.New("dependencies unavailable") }
			case "store":
				if err := os.Mkdir(h.store.Path(), 0o700); err != nil {
					t.Fatal(err)
				}
			case "missing preset":
				args[3] = path + "-missing"
			case "unknown profile":
				args = append(args, "--hive", "unknown")
			case "github user":
				hivesDepsFor = func(e *commandEnv) (*hivesDeps, error) {
					deps, err := factory(e)
					deps.githubUser = func(context.Context) (string, error) { return "", errors.New("GitHub identity unavailable") }
					return deps, err
				}
			}
			if err := h.run(t, "y\n", args...); err == nil {
				t.Fatal("preflight failure reported success")
			}
			if len(h.reg.calls) != 0 || h.signalCalls != 0 {
				t.Fatal("preflight failure registered or reloaded")
			}
		})
	}
}
