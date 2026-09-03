package panes

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/tui/client"
)

// GovernorMsg delivers the governor's live status and configured evaluation
// cadence to the Governor pane.
//
// The values come from separate client calls: Client.Governor reads live state
// from GET /api/status, while Client.GovernorEvalInterval reads configuration
// from GET /api/config/governor. Keeping both in one delivery gives the pane a
// coherent frame without teaching it how to fetch. The app wires that delivery
// in T13b; T7 only defines and renders the contract.
type GovernorMsg struct {
	Status       client.GovernorStatus
	EvalInterval time.Duration
}

// Governor is the governor-status pane: mode, actionable queue depth, next
// evaluation, evaluation cadence, and ACMM level.
type Governor struct {
	stub

	// status is the most recent successful live-state read. A failed fetch
	// never becomes a GovernorMsg, so the pane keeps the last state it saw.
	status client.GovernorStatus

	// evalInterval is configuration rather than live state. It is delivered
	// alongside status because it is the only reliable source for the relative
	// next-evaluation display; GovernorStatus.NextKick is a pre-formatted
	// server-local string that cannot be parsed back into an instant safely.
	evalInterval time.Duration

	// loaded separates "no message yet" from a successfully fetched zero-value
	// status. The former renders the shared waiting placeholder; the latter
	// renders honest unknown values.
	loaded bool
}

// NewGovernor returns the Governor pane in its pre-data state.
func NewGovernor() Governor { return Governor{stub: stub{title: "GOVERNOR"}} }

// Update implements Pane. A GovernorMsg replaces the displayed snapshot;
// every other message is inert under the shared pane routing contract.
func (p Governor) Update(msg tea.Msg) (Pane, tea.Cmd) {
	if data, ok := msg.(GovernorMsg); ok {
		p.status = data.Status
		p.evalInterval = data.EvalInterval
		p.loaded = true
		return p, nil
	}
	return p.update(msg, p)
}

// View implements Pane.
func (p Governor) View(width, height int) string {
	if !p.loaded {
		return stubView(p.Title(), width, height)
	}
	return contentView(p.Title(), p.body(), width, height)
}

const governorLabelWidth = len("eval interval")

// body renders the design sketch's label/value rows. Text alignment, rather
// than pane-owned borders or colors, supplies the structure: the app owns all
// surrounding chrome under the T3 pane contract.
func (p Governor) body() string {
	mode := "—"
	queue := "—"
	if p.status.Active {
		if p.status.Mode != "" {
			mode = strings.ToUpper(p.status.Mode)
		}
		queue = fmt.Sprintf("%d actionable", p.status.QueueDepth())
	}

	interval := formatGovernorDuration(p.evalInterval)
	nextEval := "—"
	if p.status.Active && p.status.NextKick != "" && p.evalInterval > 0 {
		// buildGovernor creates NextKick as "now + eval interval", but formats
		// it without seconds in the server's local timezone. The interval is
		// therefore the only value from which a trustworthy relative display
		// can be made; NextKick is used only to distinguish scheduled from off.
		nextEval = "in " + interval
	}

	acmm := "—"
	if p.status.ACMMLevelConfigured && p.status.ACMMLevel > 0 {
		acmm = fmt.Sprintf("L%d", p.status.ACMMLevel)
	}

	rows := [][2]string{
		{"mode", mode},
		{"queue depth", queue},
		{"next eval", nextEval},
		{"eval interval", interval},
		{"acmm level", acmm},
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, fmt.Sprintf("%-*s %s", governorLabelWidth, row[0], row[1]))
	}
	return strings.Join(lines, "\n")
}

// formatGovernorDuration presents the second-granularity configuration in a
// compact form. time.Duration.String includes a redundant trailing "0s" for
// exact minutes ("5m0s"), while the pane sketch and operator config use "5m".
func formatGovernorDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	if d < time.Second {
		return "<1s"
	}
	d = d.Round(time.Second)
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	}
	if d%time.Minute == 0 {
		return strings.TrimSuffix(d.String(), "0s")
	}
	return d.String()
}
