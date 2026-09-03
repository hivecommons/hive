package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/tui/panes"
)

// fakePane exercises the branches the T3 stubs cannot: a pane whose Init and
// Update RETURN commands, and whose Update returns a changed pane. The app's
// plumbing for those paths ships in T3 so the pane tasks slot in without app
// changes — which means it must be tested now, not when its first real user
// lands.
type fakePane struct {
	title   string
	updates int
}

// fakeInitMsg identifies which pane's Init command produced it, so a test can
// assert that all four were collected rather than only that SOMETHING was.
// That distinction started mattering in T12: Init now always returns the poll
// and tick commands, so a non-nil result no longer says anything about the
// panes.
type fakeInitMsg struct{ title string }

func (f *fakePane) Init() tea.Cmd { return func() tea.Msg { return fakeInitMsg{f.title} } }
func (f *fakePane) Update(tea.Msg) (panes.Pane, tea.Cmd) {
	f.updates++
	return f, func() tea.Msg { return nil }
}
func (f *fakePane) View(width, height int) string { return f.title }
func (f *fakePane) Title() string                 { return f.title }

// modelWithFakes swaps every pane for a fake and shortens the poll cadence so
// a test can run a whole tick — fetch, delivery and re-arm — without waiting
// out a real interval. The dashboard URL is pinned for the whole package in
// TestMain, so the fakes' model polls a closed port rather than whatever the
// developer has running on localhost:3001.
func modelWithFakes() (model, []*fakePane) {
	m := newModel()
	m.interval = time.Millisecond
	fakes := make([]*fakePane, paneCount)
	for i := range fakes {
		fakes[i] = &fakePane{title: m.panes[i].Title()}
		m.panes[i] = fakes[i]
	}
	return m, fakes
}

// TestNewExposesTheRootModel pins the exported constructor the golden test in
// pkg/tui/panes enters through.
func TestNewExposesTheRootModel(t *testing.T) {
	if _, ok := New().(model); !ok {
		t.Fatalf("New() = %T, want the root model", New())
	}
}

// TestInitBatchesPaneCommands pins that a pane's Init command is collected
// rather than dropped — the seam T5/T7/T9/T11 use to issue their first fetch —
// alongside the poll loop's own startup commands.
//
// It drains the batch and looks for each pane by name. Asserting merely that
// Init() is non-nil would now pass on the tick command alone, with every
// pane's Init silently discarded.
func TestInitBatchesPaneCommands(t *testing.T) {
	m, _ := modelWithFakes()

	seen := map[string]bool{}
	sawTick := false
	for _, msg := range drain(m.Init()) {
		switch msg := msg.(type) {
		case fakeInitMsg:
			seen[msg.title] = true
		case tickMsg:
			sawTick = true
		}
	}

	for _, title := range []string{"AGENTS", "GOVERNOR", "TOKENS", "EVENTS"} {
		if !seen[title] {
			t.Errorf("Init() dropped the %s pane's initial command", title)
		}
	}
	if !sawTick {
		t.Error("Init() did not arm the poll loop")
	}
}

// TestUnboundKeyRoutesToFocusedPaneOnly pins the key-routing seam: a key that
// is not a global binding reaches exactly the focused pane, and its command
// is propagated.
func TestUnboundKeyRoutesToFocusedPaneOnly(t *testing.T) {
	m, fakes := modelWithFakes()
	m.focus = 2

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if cmd == nil {
		t.Fatal("the focused pane's Update command was dropped")
	}
	for i, f := range fakes {
		want := 0
		if i == 2 {
			want = 1
		}
		if f.updates != want {
			t.Fatalf("pane %d saw %d updates, want %d — keys must reach only the focused pane", i, f.updates, want)
		}
	}
}

// TestNonKeyMessagesBroadcastToAllPanes pins the other half of the routing
// contract: a poll result or SSE event is not addressed to whichever pane
// happens to be focused, so every pane must see it.
func TestNonKeyMessagesBroadcastToAllPanes(t *testing.T) {
	m, fakes := modelWithFakes()

	type dataMsg struct{}
	_, cmd := m.Update(dataMsg{})
	if cmd == nil {
		t.Fatal("the panes' Update commands were dropped")
	}
	for i, f := range fakes {
		if f.updates != 1 {
			t.Fatalf("pane %d saw %d updates, want 1 — non-key messages broadcast to every pane", i, f.updates)
		}
	}
}
