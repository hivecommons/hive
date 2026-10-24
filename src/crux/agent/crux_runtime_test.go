package agent

import (
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

func TestAsRuntime_NilManagerIsNilRuntime(t *testing.T) {
	var m *Manager
	if rt := AsRuntime(m); rt != nil {
		t.Fatalf("AsRuntime(nil) = %#v, want a nil interface so `rt != nil` guards keep working", rt)
	}
}

func TestAsRuntime_WrapsManagerUnchanged(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, slog.Default(), ProjectContext{})
	rt := AsRuntime(m)
	if rt == nil {
		t.Fatal("AsRuntime(non-nil) returned nil")
	}
	if got, ok := rt.(*Manager); !ok || got != m {
		t.Fatalf("AsRuntime returned %T, want the same *Manager", rt)
	}
	if _, err := rt.GetStatus("no-such-agent"); err == nil {
		t.Fatal("GetStatus on an unknown agent should fail through the Runtime surface as it does on the manager")
	}
}

// TestSnapshotCopiesPaneCapture pins that the crux.Process a snapshot hands
// out still answers PaneLines from the capture it was taken with, and that
// the copy does not alias the live capture.
func TestSnapshotCopiesPaneCapture(t *testing.T) {
	a := &AgentProcess{Name: "scanner", lastPaneCapture: []string{"hello", "world"}}
	snap := a.snapshot()
	if got := snap.PaneLines(10); len(got) != 2 || got[0] != "hello" || got[1] != "world" {
		t.Fatalf("snapshot PaneLines = %q, want [hello world]", got)
	}
	a.lastPaneCapture[0] = "mutated"
	if got := snap.PaneLines(10); len(got) != 2 || got[0] != "hello" {
		t.Fatalf("snapshot PaneLines after live mutation = %q, want the captured copy", got)
	}
	if got, want := snap.FilteredPaneLines(1), a.FilteredPaneLines(1); len(got) != 1 || len(want) != 1 || got[0] != "world" {
		t.Fatalf("snapshot FilteredPaneLines(1) = %q, live = %q, want [world]", got, want)
	}
}
