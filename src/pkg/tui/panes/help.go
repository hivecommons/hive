package panes

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/hivecommons/hive/pkg/tui/theme"
)

// Binding is one row of the help overlay's key table.
type Binding struct {
	// Keys is the binding as an operator types it ("tab / shift+tab").
	Keys string
	// Action is what the binding does, from the design doc's table.
	Action string
	// Scope is where the binding applies — "global", or the pane that must be
	// focused for it to do anything.
	Scope string
	// Available reports whether pressing Keys does something TODAY.
	//
	// The design doc's §4 table is a ROADMAP: some of its rows belong to tasks
	// that have not landed. app.go's footerText already refuses to advertise
	// those ("showing them now would advertise actions that silently do
	// nothing"), and the same rule matters more here, because help is exactly
	// where an operator goes to find out what they can do. So the overlay shows
	// the whole table — the roadmap is useful — but says plainly which half is
	// live.
	//
	// A task that wires its key flips this flag, which the golden test then
	// forces it to notice.
	Available bool
}

// HelpBindings is the keybinding table from src/docs/design/tui.md §4, in the
// design doc's own order, with each row flagged for whether it works yet.
//
// Transcribed rather than derived: there is no runtime registry of bindings to
// read (app.go's Update switches on key strings directly), so this is a second
// copy of the same facts and the golden test is what keeps it honest.
func HelpBindings() []Binding {
	return []Binding{
		{Keys: "tab / shift+tab", Action: "Cycle pane focus forward / backward", Scope: "global", Available: true},
		{Keys: "?", Action: "Toggle this help overlay", Scope: "global", Available: true},
		{Keys: "q / ctrl+c", Action: "Quit", Scope: "global", Available: true},
		{Keys: "j / k, ↓ / ↑", Action: "Move the selection within the focused pane", Scope: "Agents / Events panes", Available: true},
		{Keys: "p", Action: "Pause or resume the selected agent", Scope: "Agents pane", Available: true},
		{Keys: "m", Action: "Open the model picker for the selected agent", Scope: "Agents pane", Available: true},
		{Keys: "K", Action: "Kick the selected agent now", Scope: "Agents pane", Available: true},
		{Keys: "A", Action: "Open the ACMM level overlay", Scope: "global", Available: true},
		// "(local)" was dropped from the scope when #5644 landed: `a` now
		// reaches remote and containerized hives through the dashboard's
		// terminal proxy, with local tmux kept as the co-located fast path.
		{Keys: "a", Action: "Attach to the selected agent's tmux session", Scope: "Agents pane", Available: true},
	}
}

var (
	// The overlay is drawn with a THICK border for the same reason the focused
	// pane is (see app.go): test and CI terminals render through termenv's
	// Ascii profile with colours stripped, so a colour-only distinction would
	// be invisible in exactly the environment the golden file pins. The border
	// weight survives every profile.
	helpBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.ThickBorder()).
			BorderForeground(theme.BorderFocus).
			Padding(0, 1)
	helpTitleStyle   = lipgloss.NewStyle().Bold(true)
	helpSectionStyle = lipgloss.NewStyle().Bold(true)
	helpFootStyle    = lipgloss.NewStyle().Faint(true)
	helpKeyStyle     = lipgloss.NewStyle().Bold(true)
)

// helpUnavailableHeading introduces the roadmap half of the table. It names the
// reason rather than just the state, so the rows below read as "coming", not as
// "broken".
const helpUnavailableHeading = "Not wired up yet — listed because the design reserves them"

// helpDismiss tells the operator how to get out. A modal that does not say how
// to close it is the same trap as a full-screen program that does not say how
// to quit, which is why app.go's splash names `q`.
const helpDismiss = "press any key to dismiss"

// Help renders the help overlay's box: the keybinding table, bordered and
// sized to its own content.
//
// It returns the BOX only, not a positioned frame. Placing it — centring it
// over the grid — is the app's job, the same division pane.go already draws
// between a pane's content and the chrome around it. That keeps this function
// independent of the terminal size, which in turn is what lets the golden test
// pin it without a running program.
func Help() string {
	bindings := HelpBindings()

	// Column widths are measured from the data rather than hardcoded, so a
	// task adding a binding cannot silently misalign the table. lipgloss.Width
	// is used instead of len: the arrow keys in the table are multi-byte, and
	// byte length would over-pad their column.
	keyW, actionW := 0, 0
	for _, b := range bindings {
		keyW = max(keyW, lipgloss.Width(b.Keys))
		actionW = max(actionW, lipgloss.Width(b.Action))
	}

	row := func(b Binding) string {
		return helpKeyStyle.Width(keyW).Render(b.Keys) + "  " +
			lipgloss.NewStyle().Width(actionW).Render(b.Action) + "  " +
			b.Scope
	}

	var body strings.Builder
	body.WriteString(helpTitleStyle.Render("Keybindings"))
	body.WriteString("\n\n")
	for _, b := range bindings {
		if b.Available {
			body.WriteString(row(b) + "\n")
		}
	}
	// The roadmap section is drawn only when it has rows. T19 wired `A`, the
	// last unavailable binding in the design doc's §4 table, so today it has
	// none — and a heading reading "Not wired up yet" above nothing at all
	// would say the opposite of the truth it was written to tell. The section
	// is kept rather than deleted because the table is a transcription of a
	// design doc that can gain rows again.
	if unavailable := unavailableBindings(bindings); len(unavailable) > 0 {
		body.WriteString("\n")
		body.WriteString(helpSectionStyle.Render(helpUnavailableHeading))
		body.WriteString("\n")
		for _, b := range unavailable {
			body.WriteString(row(b) + "\n")
		}
	}
	body.WriteString("\n")
	body.WriteString(helpFootStyle.Render(helpDismiss))

	return helpBoxStyle.Render(body.String())
}

// unavailableBindings returns the rows whose keys are not wired yet.
func unavailableBindings(bindings []Binding) []Binding {
	var out []Binding
	for _, b := range bindings {
		if !b.Available {
			out = append(out, b)
		}
	}
	return out
}
