package commands

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl"
)

func TestHivesTreeReadOnly(t *testing.T) {
	for _, format := range []string{"table", "json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			h := newHivesHarness(t)
			set := twoHives()
			set.Profiles[0].Selection = &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected, Repositories: []string{"github/github.com/org/repo"}, FollowNewRepositories: true}
			h.seed(t, set)
			before, err := os.ReadFile(h.store.Path())
			if err != nil {
				t.Fatal(err)
			}
			// A pre-existing generated projection must remain byte-identical too.
			if err := os.WriteFile(h.store.EnvPath(), []byte("existing projection\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := h.run(t, "", "hives", "tree", "-o", format); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(h.store.Path())
			if err != nil {
				t.Fatal(err)
			}
			env, err := os.ReadFile(h.store.EnvPath())
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) || string(env) != "existing projection\n" || h.signalCalls != 0 || len(h.reg.calls) != 0 || len(h.reissuer.calls) != 0 {
				t.Fatal("tree changed state")
			}
			output := h.out.String()
			for _, secret := range []string{"tok-acme", "tok-beta", "registration_token", "contributor_id"} {
				if strings.Contains(output, secret) {
					t.Fatalf("tree leaks %s", secret)
				}
			}
			for _, want := range []string{"Direct Hives", "github/github.com/org/repo", "unavailable", "relay has not published a status snapshot", hivectl.DurableActivityNotShipped} {
				if !strings.Contains(output, want) {
					t.Fatalf("missing %q: %s", want, output)
				}
			}
			if strings.Contains(output, "idle") || strings.Contains(output, "0 completed") {
				t.Fatalf("unavailable runtime rendered as a claim: %s", output)
			}
		})
	}
}

func TestHivesTreeRendersRuntimeOverlay(t *testing.T) {
	h := newHivesHarness(t)
	set := twoHives()
	set.RepoScopeVersion = 3
	set.Profiles[0].Selection = &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected, Repositories: []string{"github/github.com/org/repo"}}
	h.seed(t, set)
	hiveID, err := hivectl.NormalizeHiveID(set.Profiles[0].Hub)
	if err != nil {
		t.Fatal(err)
	}
	var sawSaved uint64
	h.treeLive = func(saved *hivectl.ProfileSet) hivectl.TreeLive {
		sawSaved = saved.RepoScopeVersion
		return hivectl.TreeLive{Available: true, PID: 77, AdoptedScopeVersion: 3, SavedScopeVersion: 3, Acknowledged: "acknowledged",
			CurrentTask:   &hivectl.RelayCurrentTask{Hive: hiveID, Repository: "github/github.com/org/repo"},
			Subscriptions: map[string]hivectl.TreeLiveSubscription{hiveID: {Readiness: "connected", Acknowledged: "acknowledged"}}}
	}
	h.treeDurable = func(*hivectl.ProfileStore) (hivectl.TreeDurable, error) {
		return hivectl.TreeDurable{Available: true, Retention: "30d", Attempts: 1, Completed: 1,
			Repositories: map[string]hivectl.TreeDurableRepository{"github/github.com/org/repo": {Attempts: 1, Completed: 1}}}, nil
	}
	if err := h.run(t, "", "hives", "tree"); err != nil {
		t.Fatal(err)
	}
	if sawSaved != 3 {
		t.Fatalf("live overlay saw saved scope version %d", sawSaved)
	}
	output := h.out.String()
	for _, want := range []string{"readiness=connected", "activity=working on github/github.com/org/repo", "history=1 attempt(s) · 1 completed", "Live: relay pid 77 · scope v3 acknowledged · working on github/github.com/org/repo", "Durable: 1 attempt(s) · 1 completed · retention 30d"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q: %s", want, output)
		}
	}
	if err := h.run(t, "", "hives", "tree", "-o", "json"); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Runtime       hivectl.TreeRuntime        `json:"runtime"`
		Subscriptions []hivectl.TreeSubscription `json:"subscriptions"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &doc); err != nil {
		t.Fatalf("json: %v\n%s", err, h.out.String())
	}
	if !doc.Runtime.Live.Available || doc.Runtime.Durable.Attempts != 1 || len(doc.Subscriptions) == 0 {
		t.Fatalf("json runtime = %+v", doc.Runtime)
	}
	for _, s := range doc.Subscriptions {
		if s.ID == set.Profiles[0].Name && (s.Readiness != "connected" || s.Repositories[0].Activity != "in progress" || s.Repositories[0].History != "1 attempt(s) · 1 completed") {
			t.Fatalf("json subscription = %+v", s)
		}
	}
}

func TestHivesTreeDoesNotMigrateLegacy(t *testing.T) {
	h := newHivesHarness(t)
	legacy := "HIVE_HUB=wss://legacy.example/contribute\nHIVE_TOKEN=SECRET\n"
	if err := os.WriteFile(h.store.EnvPath(), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "", "hives", "tree"); err == nil || !strings.Contains(err.Error(), "does not migrate") {
		t.Fatalf("expected explicit non-migration message, got %v", err)
	}
	if _, err := os.Stat(h.store.Path()); !os.IsNotExist(err) {
		t.Fatal("tree wrote profiles")
	}
	data, err := os.ReadFile(h.store.EnvPath())
	if err != nil || string(data) != legacy {
		t.Fatal("tree altered legacy configuration")
	}
	if h.signalCalls != 0 || len(h.reg.calls) != 0 {
		t.Fatal("tree triggered a write side effect")
	}
}

func TestHivesTreeRejectsInvalidInput(t *testing.T) {
	h := newHivesHarness(t)
	h.seed(t, twoHives())
	for _, args := range [][]string{{"hives", "tree", "extra"}, {"hives", "tree", "-o", "bogus"}} {
		if err := h.run(t, "", args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if err := os.WriteFile(h.store.Path(), []byte("profiles: [invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, "", "hives", "tree"); err == nil {
		t.Fatal("invalid profiles treated as empty")
	}
}
