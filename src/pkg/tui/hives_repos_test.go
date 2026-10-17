package tui

import (
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

func TestHivesRepoToggleMatchesSharedCommit(t *testing.T) {
	set := seededHives()
	const repo = "github/github.com/org/repo"
	if err := set.IncludeRepo("acme", repo); err != nil {
		t.Fatal(err)
	}
	h := newHivesHarness(t, set)
	expected := seededHives()
	if err := expected.IncludeRepo("acme", repo); err != nil {
		t.Fatal(err)
	}
	store := hivectl.NewProfileStore(t.TempDir())
	for _, want := range []string{"none", "1 repos"} {
		if err := expected.ToggleRepoExclusion("acme", repo); err != nil {
			t.Fatal(err)
		}
		if err := store.Commit(expected); err != nil {
			t.Fatal(err)
		}
		msg := h.model.runHivesAction(1, panes.HivesAction{Kind: panes.HivesActionToggleRepo, Name: "acme", Repository: "Org/Repo"})().(hivesActionMsg)
		if msg.err != nil {
			t.Fatal(msg.err)
		}
		if !strings.Contains(msg.note, "applies on next start") {
			t.Fatalf("missing reload receipt: %s", msg.note)
		}
		for _, paths := range [][2]string{{h.store.Path(), store.Path()}, {h.store.EnvPath(), store.EnvPath()}} {
			actual, err := os.ReadFile(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			target, err := os.ReadFile(paths[1])
			if err != nil {
				t.Fatal(err)
			}
			if string(actual) != string(target) {
				t.Fatalf("TUI differs from shared commit:\n%s\n%s", actual, target)
			}
		}
		current, err := h.store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if rows := hiveRows(current); rows[0].Scope != want {
			t.Fatalf("scope %q want %q", rows[0].Scope, want)
		}
	}
	if h.signals.Load() != 2 {
		t.Fatal("toggle did not reload exactly once per commit")
	}
	// Bad input and an unknown profile cannot trigger persistence or signaling.
	for _, action := range []panes.HivesAction{
		{Kind: panes.HivesActionToggleRepo, Name: "acme", Repository: "bare"},
		{Kind: panes.HivesActionToggleRepo, Name: "missing", Repository: "org/repo"},
	} {
		if msg := h.model.runHivesAction(1, action)().(hivesActionMsg); msg.err == nil {
			t.Fatal("invalid toggle accepted")
		}
	}
	if h.signals.Load() != 2 {
		t.Fatal("invalid toggle signaled")
	}
}

func TestHivesRepoKeyOpensModal(t *testing.T) {
	h := newHivesHarness(t, seededHives())
	h.open(t)
	h.send(t, key("x"))
	if !h.model.hives.Typing() {
		t.Fatal("x did not open repository form")
	}
	h.send(t, key("org/ard")) // a, r and d must be text, not overlay actions.
	if !strings.Contains(h.model.hives.View(100), "org/ard") {
		t.Fatal("repository text was handled as commands")
	}
	h.send(t, key("x"))
	if h.signals.Load() != 0 {
		t.Fatal("typing executed a mutation")
	}
}
