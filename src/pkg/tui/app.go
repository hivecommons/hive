// Package tui implements Hive's full-screen terminal dashboard — the
// keyboard-driven operator view over the same dashboard API the web UI and
// hivectl already consume (kubestellar/hive#4907).
//
// This package is the TUI's own root. It deliberately holds no Hive logic: the
// TUI is a second CLIENT of the dashboard API, not a second implementation of
// the fleet model, so everything it displays arrives over the documented HTTP
// contract in dashboard/openapi.json.
//
// SCAFFOLD ONLY (T1, #4916). The model here draws one centered line and quits.
// Panes, the layout grid, polling, and every API call are separate tasks that
// build on this frame; see #4907 for the task graph.
package tui

import (
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// splash is the only thing the scaffold frame draws. It names the binding that
// gets an operator back out, because a full-screen alt-screen program that does
// not say how to exit is a trap — especially over SSH, where the reflex of
// closing the terminal also kills whatever else the session was doing.
const splash = "Hive TUI (q to quit)"

// model is the root bubbletea model.
//
// It is a VALUE type with value receivers, which is the bubbletea convention
// rather than an accident: Update returns the next model instead of mutating
// the current one, so a message handler can never leave a half-updated frame
// visible if it returns early.
type model struct {
	// width and height track the terminal size reported by tea.WindowSizeMsg.
	// They are zero until the first message arrives — bubbletea sends one at
	// startup, but View can be called before it lands, so View must tolerate
	// zero rather than assume it has been sized.
	width  int
	height int
}

// newModel returns the root model in its initial state. Unexported because the
// program is entered through Run; the tests use it directly to drive the model
// without a terminal.
func newModel() model {
	return model{}
}

// Init implements tea.Model. The scaffold has nothing to kick off — no polling,
// no first fetch — so it issues no command.
func (m model) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		// KeyMsg.String() normalizes both plain runes ("q") and control
		// combinations ("ctrl+c") into one comparable form, so the quit
		// bindings can be listed together rather than split across a type
		// switch on key type.
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
	}
	return m, nil
}

// View implements tea.Model.
func (m model) View() string {
	if m.width <= 0 || m.height <= 0 {
		// Not sized yet. Return the bare line rather than centering into a
		// zero-sized box, which lipgloss would render as an empty frame — a
		// blank screen for however long it takes the first WindowSizeMsg to
		// arrive.
		return splash
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, splash)
}

// Run starts the TUI on this process's own terminal and blocks until the
// operator quits. It returns whatever error bubbletea reports, including the
// failure to open a terminal when the program is run without a TTY.
func Run() error {
	return run(os.Stdin, os.Stdout)
}

// run is Run with its terminal injected.
//
// The split exists so tests can drive the REAL program — the same
// tea.NewProgram call with the same options — over pipes instead of a TTY.
// That matters beyond coverage: teatest builds its own program internally, so a
// teatest-only suite never executes this constructor and would not notice
// WithAltScreen being dropped. Alt-screen is not cosmetic — it is what restores
// the operator's scrollback on exit, so `hivectl tui` leaves the terminal the
// way it found it.
func run(in io.Reader, out io.Writer) error {
	_, err := tea.NewProgram(
		newModel(),
		tea.WithAltScreen(),
		tea.WithInput(in),
		tea.WithOutput(out),
	).Run()
	return err
}
