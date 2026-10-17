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

func TestHivesReposPersistThenReload(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "", "hives", "repos", "acme", "-o", "json"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), `"legacy_unrestricted": true`) || h.signalCalls != 0 {
		t.Fatal("read mutated state or omitted legacy status")
	}
	for i, args := range [][]string{
		{"--select", "none"}, {"--include", "Org/Repo"}, {"--exclude", "org/repo"}, {"--toggle-exclude", "org/repo"}, {"--clear", "org/repo"}, {"--reset", "--yes"},
	} {
		before := h.profiles(t)
		if err := h.run(t, "", append([]string{"hives", "repos", "acme"}, args...)...); err != nil {
			t.Fatal(err)
		}
		after := h.profiles(t)
		if after.RepoScopeVersion <= before.RepoScopeVersion || h.signalCalls != i+1 {
			t.Fatal("mutation did not commit and signal exactly once")
		}
		if after.Profiles[0].RegistrationToken != "tok-acme" || after.Profiles[0].ContributorID != "c1" {
			t.Fatal("scope edit changed credentials")
		}
		if !strings.Contains(h.env(t), "HIVE_REPO_SCOPE=") {
			t.Fatal("missing projection")
		}
	}
	if len(h.reg.calls) != 0 || len(h.reissuer.calls) != 0 {
		t.Fatal("selection edit registered or rotated credentials")
	}
}

func TestHivesReposSignalSeesCommittedProjection(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	original := hivesDepsFor
	hivesDepsFor = func(env *commandEnv) (*hivesDeps, error) {
		deps, err := original(env)
		if err != nil {
			return nil, err
		}
		deps.signalRelay = func(context.Context, *hivectl.ProfileStore) (hivectl.RelaySwitchResult, error) {
			if h.profiles(t).SelectionSummary("acme") != "none" || !strings.Contains(h.env(t), "HIVE_REPO_SCOPE=',*'") {
				t.Fatal("relay signaled before saved profiles and aligned empty scope projection")
			}
			h.signalCalls++
			return hivectl.RelaySwitchResult{}, nil
		}
		return deps, nil
	}
	if err := h.run(t, "", "hives", "repos", "acme", "--select", "none"); err != nil {
		t.Fatal(err)
	}
	if h.signalCalls != 1 {
		t.Fatal("relay not signaled")
	}
}

func TestHivesReposProjectionFailureDoesNotSignal(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := os.Mkdir(h.store.EnvPath(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "", "hives", "repos", "acme", "--select", "none"); err == nil {
		t.Fatal("projection failure ignored")
	}
	if h.signalCalls != 0 {
		t.Fatal("failed projection signaled relay")
	}
}

func TestHivesReposRejectBeforeWriting(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	before, err := os.ReadFile(h.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--reset"}, {"--select", "all"}, {"--include", "bare"}, {"--exclude", "org/repo"}, {"--select", "none", "--include", "org/repo"}, {"--include", ""}, {"--include", "org/repo", "--forge", "unsupported"},
	} {
		if err := h.run(t, "", append([]string{"hives", "repos", "acme"}, args...)...); err == nil {
			t.Fatalf("accepted %v", args)
		}
		after, err := os.ReadFile(h.store.Path())
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) || h.signalCalls != 0 {
			t.Fatal("invalid edit wrote or signaled")
		}
	}
	h.signalErr = errors.New("test reload failed")
	if err := h.run(t, "", "hives", "repos", "acme", "--select", "none"); err == nil || !strings.Contains(err.Error(), "profiles saved") {
		t.Fatalf("reload failure not explicit: %v", err)
	}
	if h.profiles(t).Profiles[0].Selection == nil {
		t.Fatal("reload failure lost saved selection")
	}
}

func TestHivesReposDiscoveryPreservesConsentOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		success    bool
	}{
		{"snapshot", `{"repositories":[{"id":"GitHub/GitHub.com/Org/Repo"},{"id":"github/github.com/org/repo"}]}`, 200, true},
		{"empty", `{"repositories":[]}`, 200, true},
		{"unsupported", `{}`, 404, false},
		{"missing", `{}`, 200, false},
		{"null", `{"repositories":null}`, 200, false},
		{"invalid", `{"repositories":[{"id":"bare"}]}`, 200, false},
		{"trailing", `{"repositories":[]} {}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHivesHarness(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/contribute/repositories" || r.Header.Get("Authorization") != "" {
					t.Error("discovery path or credential boundary violated")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			set := twoHives()
			set.Profiles[0].Hub = server.URL
			set.Profiles[0].Selection = &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected, Exclude: []string{"github/github.com/org/repo"}, Include: []string{"github/github.com/org/personal"}}
			h.seed(t, set)
			before, err := os.ReadFile(h.store.Path())
			if err != nil {
				t.Fatal(err)
			}
			err = h.run(t, "", "hives", "repos", "acme", "--select", "all-current")
			if !tc.success {
				if err == nil {
					t.Fatal("discovery failure accepted")
				}
				after, readErr := os.ReadFile(h.store.Path())
				if readErr != nil {
					t.Fatal(readErr)
				}
				if string(before) != string(after) || h.signalCalls != 0 {
					t.Fatal("failed discovery changed consent")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			selection := h.profiles(t).Profiles[0].Selection
			if selection.Mode != hivectl.SelectionAllCurrent || !reflect.DeepEqual(selection.Exclude, set.Profiles[0].Selection.Exclude) || !reflect.DeepEqual(selection.Include, set.Profiles[0].Selection.Include) {
				t.Fatal("snapshot erased overrides")
			}
			if len(selection.MembershipSnapshot) > 1 {
				t.Fatal("snapshot was not deduplicated")
			}
			if h.signalCalls != 1 {
				t.Fatal("snapshot not reloaded")
			}
		})
	}
}

func TestHivesReposCLIProjectionMatchesSharedMutation(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	expected := twoHives()
	if err := expected.IncludeRepo("acme", "github/github.com/org/repo"); err != nil {
		t.Fatal(err)
	}
	store := hivectl.NewProfileStore(t.TempDir())
	if err := store.Commit(expected); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "", "hives", "repos", "acme", "--include", "Org/Repo"); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][2]string{{h.store.Path(), store.Path()}, {h.store.EnvPath(), store.EnvPath()}} {
		actual, err := os.ReadFile(paths[0])
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(paths[1])
		if err != nil {
			t.Fatal(err)
		}
		if string(actual) != string(want) {
			t.Fatalf("CLI and shared mutation differ:\n%s\n%s", actual, want)
		}
	}
}

func TestHivesReposExclusionSurvivesReissue(t *testing.T) {
	h := newHivesHarness(t)
	set := twoHives()
	const repo = "github/github.com/org/repo"
	if err := set.SetSelectionMode("acme", hivectl.SelectionAllCurrent, []string{repo}); err != nil {
		t.Fatal(err)
	}
	if err := set.ExcludeRepo("acme", repo); err != nil {
		t.Fatal(err)
	}
	h.seed(t, set)
	if err := h.run(t, "", "hives", "reissue", "acme"); err != nil {
		t.Fatal(err)
	}
	current := h.profiles(t)
	if current.SelectionSummary("acme") != "none" || !reflect.DeepEqual(current.Profiles[0].Selection.Exclude, []string{repo}) {
		t.Fatal("reissue erased exclusion")
	}
}

func TestHivesAddRemoveReload(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	if err := h.run(t, "token\n", "hives", "add", "third", "--hub", "https://third.example", "--token-stdin"); err != nil {
		t.Fatal(err)
	}
	if h.signalCalls != 1 {
		t.Fatal("add did not reload")
	}
	if err := h.run(t, "", "hives", "remove", "third", "--yes"); err != nil {
		t.Fatal(err)
	}
	if h.signalCalls != 2 {
		t.Fatal("remove did not reload")
	}
}
