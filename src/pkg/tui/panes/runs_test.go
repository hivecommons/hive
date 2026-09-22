package panes

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/tui/client"
)

func TestRunsSelectionAndEmptyState(t *testing.T) {
	pane := NewRuns()
	if _, ok := pane.SelectedRun(); ok {
		t.Fatal("SelectedRun before data returned ok")
	}

	updated, _ := pane.Update(RunsMsg{})
	loaded := updated.(Runs)
	if view := loaded.View(40, 6); !strings.Contains(view, "no active runs") || strings.Contains(view, "waiting for data") {
		t.Fatalf("empty runs view not loaded empty state:\n%s", view)
	}
}

func TestRunsSelectionMovesAndClamps(t *testing.T) {
	pane, _ := NewRuns().Update(RunsMsg{Runs: []client.Run{{Key: "one"}, {Key: "two"}}})
	runs := pane.(Runs)

	next, _ := runs.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	runs = next.(Runs)
	selected, ok := runs.SelectedRun()
	if !ok || selected.Key != "two" {
		t.Fatalf("after j selected %+v ok=%v, want two", selected, ok)
	}

	next, _ = runs.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	runs = next.(Runs)
	selected, _ = runs.SelectedRun()
	if selected.Key != "two" {
		t.Fatalf("selection did not clamp at bottom: %+v", selected)
	}

	next, _ = runs.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	runs = next.(Runs)
	selected, _ = runs.SelectedRun()
	if selected.Key != "one" {
		t.Fatalf("after k selected %+v, want one", selected)
	}
}

func TestRunAge(t *testing.T) {
	now := time.Date(2026, time.September, 22, 20, 0, 0, 0, time.UTC)
	if got := runAge(client.Run{WaitingSince: "2026-09-22T19:45:00Z"}, now); got != "15m" {
		t.Fatalf("runAge waiting = %q, want 15m", got)
	}
	if got := runAge(client.Run{StageStartedAt: "2026-09-22T18:00:00Z"}, now); got != "2h" {
		t.Fatalf("runAge stage = %q, want 2h", got)
	}
	if got := runAge(client.Run{}, now); got != "—" {
		t.Fatalf("runAge empty = %q, want dash", got)
	}
}
