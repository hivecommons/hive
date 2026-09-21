package panes_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/tui/panes"
)

// The Hives overlay's own rules (#8128), tested against the pane directly: no
// renderer, no goroutines, no filesystem. The app-side half — what a key
// actually does to profiles.yml — is in pkg/tui/hives_test.go.

func hiveFixture() []panes.HiveRow {
	return []panes.HiveRow{
		{Name: "acme", Hub: "wss://acme.example/contribute", ContributorID: "contrib_a1", Active: true},
		{Name: "other", Hub: "wss://other.example/contribute", ContributorID: "contrib_b2", Session: "review"},
		{Name: "third", Hub: "wss://third.example/contribute"},
	}
}

func loadedHives() panes.HivesOverlay {
	return panes.NewHivesOverlay().SetHives(hiveFixture(), "/cfg/profiles.yml", "/cfg/contributor.env")
}

func TestHivesOverlayStartsLoading(t *testing.T) {
	o := panes.NewHivesOverlay()
	if !o.Loading() {
		t.Error("a freshly opened overlay is not loading")
	}
	if _, ok := o.Selected(); ok {
		t.Error("a loading overlay offered a selection")
	}
	if !strings.Contains(o.View(90), "Reading hive profiles") {
		t.Errorf("the loading state is not rendered:\n%s", o.View(90))
	}
}

func TestHivesOverlayMoveClampsAtTheEdges(t *testing.T) {
	o := loadedHives()
	if row, _ := o.Selected(); row.Name != "acme" {
		t.Fatalf("opening selection = %q, want the first row", row.Name)
	}
	o = o.Move(-1)
	if row, _ := o.Selected(); row.Name != "acme" {
		t.Errorf("up from the top moved to %q", row.Name)
	}
	o = o.Move(99)
	if row, _ := o.Selected(); row.Name != "third" {
		t.Errorf("down past the end selected %q, want the last row", row.Name)
	}
}

// The cursor follows the NAME across a reload, not the index. `use` reorders
// the list (active first), so an index-preserving reload would silently move
// the cursor onto whichever hive slid into that slot.
func TestHivesOverlayKeepsTheCursorOnTheSameHiveAcrossAReload(t *testing.T) {
	o := loadedHives().Move(2) // "third"
	reordered := []panes.HiveRow{
		{Name: "third", Hub: "wss://third.example/contribute", Active: true},
		{Name: "acme", Hub: "wss://acme.example/contribute"},
		{Name: "other", Hub: "wss://other.example/contribute"},
	}
	o = o.SetHives(reordered, "/cfg/profiles.yml", "/cfg/contributor.env")
	if row, _ := o.Selected(); row.Name != "third" {
		t.Errorf("cursor landed on %q after a reorder, want third", row.Name)
	}
}

// A hive that vanished from the file (removed in another terminal) cannot leave
// the cursor pointing past the end.
func TestHivesOverlayResetsTheCursorWhenTheSelectedHiveDisappears(t *testing.T) {
	o := loadedHives().Move(2)
	o = o.SetHives(hiveFixture()[:1], "/cfg/profiles.yml", "/cfg/contributor.env")
	row, ok := o.Selected()
	if !ok || row.Name != "acme" {
		t.Errorf("selection = %+v (ok=%v), want the first surviving row", row, ok)
	}
}

func TestHivesOverlayReachableColumnHasThreeStates(t *testing.T) {
	o := loadedHives()
	if !strings.Contains(o.View(100), "checking…") {
		t.Errorf("an unprobed hub does not render as pending:\n%s", o.View(100))
	}
	o = o.SetReachable("wss://acme.example/contribute", true)
	o = o.SetReachable("wss://other.example/contribute", false)
	view := o.View(100)
	if !strings.Contains(view, "yes") || !strings.Contains(view, "no") {
		t.Errorf("probe answers are not rendered:\n%s", view)
	}
	rows := o.Rows()
	if rows[0].Reachable == nil || !*rows[0].Reachable {
		t.Errorf("row 0 reachable = %v, want true", rows[0].Reachable)
	}
	if rows[1].Reachable == nil || *rows[1].Reachable {
		t.Errorf("row 1 reachable = %v, want false", rows[1].Reachable)
	}
	if rows[2].Reachable != nil {
		t.Errorf("row 2 was answered without being probed: %v", rows[2].Reachable)
	}
}

// ── Submit's refusals ───────────────────────────────────────────────────────

func TestHivesSubmitOnTheListEmitsUse(t *testing.T) {
	o := loadedHives().Move(1)
	next, action, ok := o.Submit()
	if !ok {
		t.Fatal("enter on a loaded row did not emit an action")
	}
	if action.Kind != panes.HivesActionUse || action.Name != "other" {
		t.Errorf("action = %+v, want a use of other", action)
	}
	if !next.Pending() {
		t.Error("the overlay is not pending after accepting an action")
	}
}

// The acceptance criterion for `d`: an unconfirmed removal emits nothing.
func TestHivesRemoveRefusesEveryConfirmationButTheName(t *testing.T) {
	base, ok := loadedHives().BeginRemove()
	if !ok {
		t.Fatal("d refused to open the confirmation on a selected row")
	}
	for _, typed := range []string{"", "acm", "other", "acme acme", "y", "yes"} {
		o := base
		if typed != "" {
			o = o.Type(typed)
		}
		if _, action, ok := o.Submit(); ok {
			t.Errorf("confirmation %q removed a hive (%+v)", typed, action)
		}
	}
	// The exact name, and — matching `hivectl hives remove`, whose names are
	// unique case-insensitively — the same name in another case.
	for _, typed := range []string{"acme", "ACME", "  acme  "} {
		o := base.Type(typed)
		_, action, ok := o.Submit()
		if !ok || action.Kind != panes.HivesActionRemove || action.Name != "acme" {
			t.Errorf("confirmation %q did not remove: ok=%v action=%+v", typed, ok, action)
		}
	}
}

func TestHivesRenameRefusesAnEmptyOrUnchangedName(t *testing.T) {
	o, ok := loadedHives().BeginRename()
	if !ok {
		t.Fatal("r refused to open the rename form")
	}
	if _, _, ok := o.Submit(); ok {
		t.Error("the prefilled, unchanged name was accepted as a rename")
	}
	cleared := o
	for range "acme" {
		cleared = cleared.Backspace()
	}
	if _, _, ok := cleared.Submit(); ok {
		t.Error("an empty new name was accepted as a rename")
	}
	_, action, ok := cleared.Type("acme-prod").Submit()
	if !ok || action.Kind != panes.HivesActionRename || action.Name != "acme" || action.NewName != "acme-prod" {
		t.Errorf("rename action = %+v (ok=%v)", action, ok)
	}
}

// Adding is the one action that is meaningful with nothing selected: an empty
// machine is exactly where the first hive comes from.
func TestHivesAddOpensOnAnEmptyList(t *testing.T) {
	o := panes.NewHivesOverlay().SetHives(nil, "/cfg/profiles.yml", "/cfg/contributor.env")
	next, ok := o.BeginAdd()
	if !ok {
		t.Fatal("a refused to open the add form on an empty list")
	}
	if !next.Typing() {
		t.Error("the add form is not composing")
	}
	// The other two forms need a row and refuse without one.
	if _, ok := o.BeginRemove(); ok {
		t.Error("d opened a removal with nothing selected")
	}
	if _, ok := o.BeginRename(); ok {
		t.Error("r opened a rename with nothing selected")
	}
}

func TestHivesAddFormSwitchesFields(t *testing.T) {
	o, _ := loadedHives().BeginAdd()
	o = o.Type("acme2").NextField(1).Type("wss://acme.example/contribute")
	_, action, ok := o.Submit()
	if !ok {
		t.Fatal("a complete add form was refused")
	}
	if action.Kind != panes.HivesActionAdd || action.Name != "acme2" || action.Hub != "wss://acme.example/contribute" {
		t.Errorf("add action = %+v", action)
	}
	// Wrapping back to the name field must not leak the hub into it.
	back := o.NextField(1)
	if _, again, _ := back.Type("X").Submit(); again.Name != "acme2X" {
		t.Errorf("after wrapping, typing landed in the wrong field: %+v", again)
	}
}

// Navigation is refused while a form is composing: the cursor is what the form
// refers to, so letting j/k slide it under a half-typed removal confirmation
// would be exactly the mistake the confirmation exists to prevent.
func TestHivesMoveIsRefusedWhileAFormIsOpen(t *testing.T) {
	o, _ := loadedHives().BeginRemove()
	moved := o.Move(1)
	if row, _ := moved.Selected(); row.Name != "acme" {
		t.Errorf("the cursor moved under an open confirmation, to %q", row.Name)
	}
}

// Nothing is accepted while a write is in flight — not a second submit, not a
// keystroke, not a cancel.
func TestHivesIgnoresInputWhilePending(t *testing.T) {
	o, _, ok := loadedHives().Submit()
	if !ok {
		t.Fatal("the first submit was refused")
	}
	if _, _, ok := o.Submit(); ok {
		t.Error("a second submit was accepted while one was pending")
	}
	if o.Cancel().Pending() != true {
		t.Error("cancel dismissed an in-flight action")
	}
	if typed, _ := o.Type("x"), 0; typed.Typing() {
		t.Error("typing opened a form while an action was pending")
	}
	if _, ok := o.BeginAdd(); ok {
		t.Error("a opened the add form while an action was pending")
	}
}

// A failed write leaves the overlay on the form it was submitted from, holding
// what was typed, so a corrected retry is a couple of keys.
func TestHivesActionErrorKeepsTheForm(t *testing.T) {
	o, _ := loadedHives().BeginAdd()
	o = o.Type("third").NextField(1).Type("wss://third.example/contribute")
	o, _, _ = o.Submit()
	o = o.SetActionError(errors.New("register with the hub failed"))
	if o.Pending() {
		t.Error("the overlay is still pending after a failure")
	}
	if !o.Typing() {
		t.Error("a failed add bounced out of the form")
	}
	view := o.View(100)
	if !strings.Contains(view, "register with the hub failed") {
		t.Errorf("the failure is not rendered:\n%s", view)
	}
	if !strings.Contains(view, "[third]") {
		t.Errorf("the typed name was lost on failure:\n%s", view)
	}
}

func TestHivesActionResultReturnsToTheListWithTheReceipt(t *testing.T) {
	o, _ := loadedHives().BeginRemove()
	o, _, _ = o.Type("acme").Submit()
	o = o.SetActionResult("✓ removed hive \"acme\"")
	if o.Pending() || o.Typing() {
		t.Error("a successful action did not return to the list")
	}
	if !strings.Contains(o.View(100), "removed hive") {
		t.Errorf("the receipt is not held on the list:\n%s", o.View(100))
	}
}

// A read failure is not rendered as an empty list: showing "no hives
// configured" above an add hint would invite an operator to write over a file
// that holds credentials the hub cannot reprint.
func TestHivesListErrorIsNotAnEmptyList(t *testing.T) {
	o := panes.NewHivesOverlay().SetListError(errors.New("profiles.yml is not valid YAML"))
	view := o.View(100)
	if !strings.Contains(view, "not valid YAML") {
		t.Errorf("the read failure is not rendered:\n%s", view)
	}
	if strings.Contains(view, "no hives configured —") {
		t.Errorf("a failed read was rendered as an empty list:\n%s", view)
	}
	if _, ok := o.Selected(); ok {
		t.Error("a failed read still offered a selection")
	}
}

// The list scrolls rather than growing: a contributor with many hives must not
// push the overlay past the terminal.
func TestHivesListWindowsLongLists(t *testing.T) {
	many := make([]panes.HiveRow, 0, 20)
	for i := 0; i < 20; i++ {
		many = append(many, panes.HiveRow{Name: string(rune('a'+i)) + "-hive", Hub: "wss://h.example/contribute"})
	}
	o := panes.NewHivesOverlay().SetHives(many, "/cfg/profiles.yml", "/cfg/contributor.env")
	lines := strings.Count(o.View(100), "\n")
	o = o.Move(19)
	if got := strings.Count(o.View(100), "\n"); got != lines {
		t.Errorf("the box grew from %d to %d lines when the cursor moved", lines, got)
	}
	if !strings.Contains(o.View(100), "t-hive") {
		t.Errorf("the cursor scrolled out of the window:\n%s", o.View(100))
	}
}
