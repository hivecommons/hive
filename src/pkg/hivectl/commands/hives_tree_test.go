package commands

import (
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
			for _, want := range []string{"Direct Hives", "github/github.com/org/repo", "unavailable"} {
				if !strings.Contains(output, want) {
					t.Fatalf("missing %q: %s", want, output)
				}
			}
		})
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
