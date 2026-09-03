package panes_test

import (
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hivecommons/hive/pkg/tui"
	"github.com/hivecommons/hive/pkg/tui/client"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// acmmGoldenPacks is the fixture the golden frames are drawn from. It is
// deliberately not L1-L6: the server owns the pack list and its order, and a
// golden pinned against the canonical six would happily freeze code that had
// hardcoded them.
func acmmGoldenPacks() client.ACMMStatus {
	return client.ACMMStatus{
		Level: 4,
		Packs: []client.Pack{
			{
				Level: 2, Name: "Assisted", Description: "Agents propose, humans merge.",
				AgentCount: 4,
				Governor:   client.PackGovernor{Modes: "advisory", MergePolicy: "human"},
			},
			{
				Level: 4, Name: "Supervised", Description: "Agents merge green PRs under review.",
				AgentCount: 9, Current: true,
				Governor: client.PackGovernor{Modes: "supervised", MergePolicy: "lgtm"},
			},
			{
				Level: 5, Name: "Autonomous", Description: "Agents own the merge path.",
				AgentCount: 12,
				Governor:   client.PackGovernor{Modes: "autonomous", MergePolicy: "auto"},
			},
		},
	}
}

// TestACMMOverlayOpensGolden pins the complete FRAME the moment `A` is pressed,
// driven through the real app so the golden covers the binding, the modal
// branch and the centring as well as the box.
//
// The LOADING state is what this one pins, for the same reason
// TestModelPickerGolden does: a populated frame reached through the app would
// need a dashboard to answer, which would make the golden depend on whether
// something happens to be listening on this machine. The populated list, the
// confirmation and the receipt are pinned below against the pane directly,
// which renders them with no renderer, no goroutines and no network.
//
// Regenerate after a DELIBERATE change with:
//
//	cd src && go test ./pkg/tui/panes/... -update
//
// and read the regenerated file in the diff.
func TestACMMOverlayOpensGolden(t *testing.T) {
	m := tui.New()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("A")})

	view := m.View()
	// The overlay must not grow the frame: one that did would scroll the
	// operator's screen the moment it opened.
	if got := lipgloss.Width(view); got != 100 {
		t.Errorf("frame width = %d, want 100", got)
	}
	if got := lipgloss.Height(view); got != 30 {
		t.Errorf("frame height = %d, want 30", got)
	}
	requireGolden(t, []byte(view), filepath.Join("testdata", "acmm.golden"))
}

// acmmGoldenWidth is the box width the pane-level goldens render at. It matches
// what the overlay is given inside a 100-column frame, so these files and
// acmm.golden describe the same box.
const acmmGoldenWidth = 100

// TestACMMPackListGolden pins the populated pack list: every level with its
// name, description, agent count and recommended governor settings, the cursor
// on the level in force, and that level marked.
func TestACMMPackListGolden(t *testing.T) {
	o := panes.NewACMMOverlay().SetStatus(acmmGoldenPacks())
	requireGolden(t, []byte(o.View(acmmGoldenWidth)), filepath.Join("testdata", "acmm_list.golden"))
}

// TestACMMConfirmationGolden pins the typed-confirmation state — the frame this
// whole task exists to put in front of an operator.
//
// The phrase is left PARTIALLY typed, which is the state worth pinning: it
// shows the echo field, and it is the state in which enter must do nothing.
func TestACMMConfirmationGolden(t *testing.T) {
	o := panes.NewACMMOverlay().SetStatus(acmmGoldenPacks())
	// Move down to L5, the level below the L4 in force.
	o = o.Move(1)
	o, ok := o.BeginConfirm()
	if !ok {
		t.Fatal("BeginConfirm() refused a non-current level")
	}
	o = o.Type("APPLY L")
	requireGolden(t, []byte(o.View(acmmGoldenWidth)), filepath.Join("testdata", "acmm_confirm.golden"))
}

// TestACMMReceiptGolden pins the reconciliation receipt, the frame the
// acceptance criteria are most specific about: every category present, empty
// ones reading "none" rather than vanishing.
//
// The result carries a deliberate MIX — populated lists, an empty one, and both
// kinds of governor change — so the golden would show a category being dropped
// in either direction.
func TestACMMReceiptGolden(t *testing.T) {
	o := panes.NewACMMOverlay().SetStatus(acmmGoldenPacks()).Move(1)
	o, ok := o.BeginConfirm()
	if !ok {
		t.Fatal("BeginConfirm() refused a non-current level")
	}
	o = o.Type(panes.ConfirmPhrase(5))
	o, _, ok = o.Apply()
	if !ok {
		t.Fatal("the exact phrase did not apply")
	}
	o = o.SetResult(client.ACMMLevelResult{
		OK:          true,
		Level:       5,
		PackAgents:  []string{"scanner", "reviewer", "feature"},
		PackUpdated: []string{"reviewer"},
		Paused:      nil,
		Resumed:     []string{"feature"},
		GovernorChanges: &client.GovernorChanges{
			EvalIntervalS: &client.GovernorIntervalChange{From: 300, To: 120},
			Cadences: []client.GovernorCadenceChange{
				{Mode: "autonomous", Agent: "scanner", From: "6h", To: "1h"},
			},
		},
	})
	requireGolden(t, []byte(o.View(acmmGoldenWidth)), filepath.Join("testdata", "acmm_receipt.golden"))
}
