package panes_test

import (
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hivecommons/hive/pkg/tui"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// Golden frames for the Hives overlay (#8128).
//
// Regenerate after a DELIBERATE change with:
//
//	cd src && go test ./pkg/tui/panes/... -update
//
// and read the regenerated file in the diff. In particular, read it for
// CREDENTIALS: profiles.yml carries a registration token per hive, and a golden
// file is a frame committed to the repository. panes.HiveRow has no token field
// precisely so that cannot happen; these fixtures would show it if it ever did.

// TestHivesOverlayOpensGolden pins the complete FRAME the moment `H` is
// pressed, driven through the real app so the golden covers the binding, the
// modal branch and the centring as well as the box.
//
// The LOADING state is what this one pins, for the same reason the ACMM
// overlay's frame golden does — and one more. A populated frame reached through
// the app would read the REAL ~/.config/hive of whoever ran the test, so the
// golden would depend on which hives that person contributes to, and could
// commit their hub URLs and contributor ids to this repository. The populated
// states below are pinned against the pane directly, from a fixture.
func TestHivesOverlayOpensGolden(t *testing.T) {
	m := tui.New()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("H")})

	view := m.View()
	// The overlay must not grow the frame: one that did would scroll the
	// operator's screen the moment it opened.
	if got := lipgloss.Width(view); got != 100 {
		t.Errorf("frame width = %d, want 100", got)
	}
	if got := lipgloss.Height(view); got != 30 {
		t.Errorf("frame height = %d, want 30", got)
	}
	requireGolden(t, []byte(view), filepath.Join("testdata", "hives.golden"))
}

// hivesGoldenWidth is the box width the pane-level goldens render at. It
// matches what the overlay is given inside a 100-column frame, so these files
// and hives.golden describe the same box.
const hivesGoldenWidth = 100

// hivesGoldenRows is the fixture the populated goldens are drawn from: an
// active hive, a second one with a session label, and a third — with the three
// reachable states represented, since that column is the part of this list the
// CLI's own `--check` is the closest analogue to.
func hivesGoldenRows() []panes.HiveRow {
	yes, no := true, false
	return []panes.HiveRow{
		{Name: "acme", Hub: "wss://acme.hive.hivecommons.dev/contribute", ContributorID: "contrib_a1b2", Active: true, Reachable: &yes},
		{Name: "acme-review", Hub: "wss://acme.hive.hivecommons.dev/contribute", ContributorID: "contrib_a1b2", Session: "review", Reachable: &yes},
		{Name: "lab", Hub: "wss://lab.example.test/contribute", Reachable: &no},
	}
}

func hivesGoldenOverlay() panes.HivesOverlay {
	return panes.NewHivesOverlay().SetHives(hivesGoldenRows(),
		"/home/op/.config/hive/profiles.yml", "/home/op/.config/hive/contributor.env", "ranked")
}

// TestHivesListGolden pins the populated list: every hive with its hub,
// contributor id and session, the active one marked, and the probe column in
// all three of its states.
func TestHivesListGolden(t *testing.T) {
	requireGolden(t, []byte(hivesGoldenOverlay().View(hivesGoldenWidth)),
		filepath.Join("testdata", "hives_list.golden"))
}

// TestHivesRemoveConfirmationGolden pins the state this overlay's riskiest key
// leads to, left PARTIALLY typed — which is the state worth pinning, because it
// is the one in which enter must do nothing.
func TestHivesRemoveConfirmationGolden(t *testing.T) {
	o, ok := hivesGoldenOverlay().BeginRemove()
	if !ok {
		t.Fatal("BeginRemove() refused a selected row")
	}
	o = o.Type("acm")
	requireGolden(t, []byte(o.View(hivesGoldenWidth)),
		filepath.Join("testdata", "hives_remove.golden"))
}

// TestHivesAddFormGolden pins the two-field add form with the cursor on the
// second field, so the golden shows both the field marker and what the form
// says about what `enter` will actually do.
func TestHivesAddFormGolden(t *testing.T) {
	o, ok := hivesGoldenOverlay().BeginAdd()
	if !ok {
		t.Fatal("BeginAdd() refused to open the form")
	}
	o = o.Type("lab2").NextField(1).Type("wss://lab2.example.test/contribute")
	requireGolden(t, []byte(o.View(hivesGoldenWidth)),
		filepath.Join("testdata", "hives_add.golden"))
}
