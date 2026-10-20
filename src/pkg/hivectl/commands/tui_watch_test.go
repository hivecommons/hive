package commands

import (
	"strings"
	"testing"
)

func TestTUIWatchEntryAndMutualExclusion(t *testing.T) {
	oldWatch, oldRun, oldHives := runTUIWatch, runTUI, runTUIHivesOnly
	t.Cleanup(func() { runTUIWatch, runTUI, runTUIHivesOnly = oldWatch, oldRun, oldHives })
	called := 0
	runTUIWatch = func() error { called++; return nil }
	runTUI = func() error { t.Fatal("watch entered dashboard mode"); return nil }
	runTUIHivesOnly = func() error { t.Fatal("watch entered configuration mode"); return nil }
	_, _, root := newTestRoot()
	root.SetArgs([]string{"tui", "--watch"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatal("watch entry not called")
	}
	_, _, root = newTestRoot()
	root.SetArgs([]string{"tui", "--watch", "--hives"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "hives") || !strings.Contains(err.Error(), "watch") {
		t.Fatalf("mutual exclusion error: %v", err)
	}
	if called != 1 {
		t.Fatal("conflicting flags entered watch")
	}
}
