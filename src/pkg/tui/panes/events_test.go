package panes

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/tui/client"
)

func testEvent(second, action string) client.Event {
	return client.Event{
		Timestamp: "2026-08-29T12:04:" + second + "Z",
		User:      "operator",
		Action:    action,
	}
}

func updateEvents(t *testing.T, pane Pane, msg tea.Msg) Events {
	t.Helper()
	next, cmd := pane.Update(msg)
	if cmd != nil {
		t.Fatal("Events.Update returned an unexpected command")
	}
	events, ok := next.(Events)
	if !ok {
		t.Fatalf("Events.Update returned %T, want Events", next)
	}
	return events
}

func TestEventsScrollMovementAndBounds(t *testing.T) {
	events := []client.Event{
		testEvent("04", "newest"),
		testEvent("03", "middle"),
		testEvent("02", "oldest"),
	}
	pane := updateEvents(t, NewEvents(), EventsMsg{Events: events})

	if got := pane.View(48, 3); !strings.Contains(got, "newest") || strings.Contains(got, "middle") {
		t.Fatalf("initial view does not start at newest event:\n%s", got)
	}

	pane = updateEvents(t, pane, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if got := pane.View(48, 3); !strings.Contains(got, "middle") || strings.Contains(got, "newest") {
		t.Fatalf("j did not scroll toward older events:\n%s", got)
	}

	pane = updateEvents(t, pane, tea.KeyMsg{Type: tea.KeyDown})
	pane = updateEvents(t, pane, tea.KeyMsg{Type: tea.KeyDown})
	if pane.offset != 2 {
		t.Fatalf("scroll past oldest offset = %d, want 2", pane.offset)
	}

	pane = updateEvents(t, pane, tea.KeyMsg{Type: tea.KeyUp})
	pane = updateEvents(t, pane, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	pane = updateEvents(t, pane, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if pane.offset != 0 {
		t.Fatalf("scroll past newest offset = %d, want 0", pane.offset)
	}
}

func TestEventsReplacementPreservesScrollAnchor(t *testing.T) {
	old := []client.Event{
		testEvent("04", "newest"),
		testEvent("03", "anchored"),
		testEvent("02", "oldest"),
	}
	pane := updateEvents(t, NewEvents(), EventsMsg{Events: old})
	pane = updateEvents(t, pane, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})

	newest := testEvent("05", "new arrival")
	pane = updateEvents(t, pane, EventsMsg{Events: append([]client.Event{newest}, old...)})
	if pane.offset != 2 {
		t.Fatalf("replacement offset = %d, want anchored event's new index 2", pane.offset)
	}
	if got := pane.View(48, 3); !strings.Contains(got, "anchored") || strings.Contains(got, "new arrival") {
		t.Fatalf("replacement yanked scrolled view to newest event:\n%s", got)
	}

	following := updateEvents(t, NewEvents(), EventsMsg{Events: old})
	following = updateEvents(t, following, EventsMsg{Events: append([]client.Event{newest}, old...)})
	if following.offset != 0 || !strings.Contains(following.View(48, 3), "new arrival") {
		t.Fatalf("replacement did not follow newest while at top:\n%s", following.View(48, 3))
	}
}

func TestEventsLoadedEmptyAndInputOwnership(t *testing.T) {
	pane := updateEvents(t, NewEvents(), EventsMsg{})
	if got := pane.View(40, 6); !strings.Contains(got, "no events yet") || strings.Contains(got, placeholder) {
		t.Fatalf("successful empty feed renders incorrectly:\n%s", got)
	}

	input := []client.Event{testEvent("04", "original")}
	pane = updateEvents(t, pane, EventsMsg{Events: input})
	input[0].Action = "mutated"
	if got := pane.View(40, 6); !strings.Contains(got, "original") || strings.Contains(got, "mutated") {
		t.Fatalf("pane retained ownership of caller's slice:\n%s", got)
	}
}

func TestEventsRowsDoNotWrap(t *testing.T) {
	pane := updateEvents(t, NewEvents(), EventsMsg{Events: []client.Event{{
		Timestamp: "not-a-timestamp",
		Action:    "a deliberately long event message that cannot fit",
	}}})
	view := pane.View(24, 5)
	if lines := strings.Count(view, "\n") + 1; lines != 5 {
		t.Fatalf("View rendered %d lines, want 5:\n%s", lines, view)
	}
	if width := visibleWidth(view); width != 24 {
		t.Fatalf("View width = %d, want 24:\n%s", width, view)
	}
	if !strings.Contains(view, "--:--:--") {
		t.Fatalf("invalid timestamp has no stable fallback:\n%s", view)
	}
	if strings.Contains(view, "cannot fit") {
		t.Fatalf("long event wrapped instead of being clipped:\n%s", view)
	}
}
