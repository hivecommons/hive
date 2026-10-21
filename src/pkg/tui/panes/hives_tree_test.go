package panes_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/tui/panes"
)

// The contribution tree screen's own rules (RFC #13153), tested against the
// pane directly: visibility (collapse, filter, search with ancestry), cursor
// motion, the staging bar, the two-step leave and the Apply confirmation. What
// a key does to the staged session lives in pkg/tui/hives_tree_test.go.

func treeFixtureRows() []panes.HiveTreeRow {
	return []panes.HiveTreeRow{
		{Kind: panes.HiveTreeHub, Key: "hub:one", Level: 1, ID: "one", Label: "one", Check: panes.HiveTreeCheckPartial, Detail: []string{"2 hive(s)"}},
		{Kind: panes.HiveTreeHive, Key: "hub:one/sub:alpha", ParentKey: "hub:one", Level: 2, ID: "alpha", Label: "alpha", Check: panes.HiveTreeCheckAll, CanSelect: true, Badges: []string{"On"}},
		{Kind: panes.HiveTreeRepo, Key: "hub:one/sub:alpha/repo:a", ParentKey: "hub:one/sub:alpha", Level: 3, ID: "github/github.com/org/a", Subscription: "alpha", Label: "github/github.com/org/a", Check: panes.HiveTreeCheckAll, CanSelect: true, Changed: true},
		{Kind: panes.HiveTreeHive, Key: "hub:one/sub:beta", ParentKey: "hub:one", Level: 2, ID: "beta", Label: "beta", Check: panes.HiveTreeCheckDisabled, NeedsReview: true, Legacy: true},
		{Kind: panes.HiveTreePlaceholder, Key: "hub:one/sub:beta/none", ParentKey: "hub:one/sub:beta", Level: 3, Label: "Unrestricted — legacy", NeedsReview: true},
		{Kind: panes.HiveTreeHub, Key: "hub:two", Level: 1, ID: "two", Label: "two", Check: panes.HiveTreeCheckOff},
		{Kind: panes.HiveTreeHive, Key: "hub:two/sub:gamma", ParentKey: "hub:two", Level: 2, ID: "gamma", Label: "gamma", Check: panes.HiveTreeCheckOff, CanSelect: true},
		{Kind: panes.HiveTreeRepo, Key: "hub:two/sub:gamma/repo:b", ParentKey: "hub:two/sub:gamma", Level: 3, ID: "github/github.com/org/b", Subscription: "gamma", Label: "github/github.com/org/b", Check: panes.HiveTreeCheckOff, CanSelect: true, NeedsReview: true, Badges: []string{"Unreviewed"}},
	}
}

func openTree(t *testing.T, summary panes.HiveTreeSummary) panes.HivesOverlay {
	t.Helper()
	o, ok := loadedHives().BeginTree()
	if !ok {
		t.Fatal("BeginTree refused on an idle list")
	}
	if !o.InTree() || !strings.Contains(o.View(100), "Reading hubs") {
		t.Fatalf("tree did not open loading:\n%s", o.View(100))
	}
	if _, ok := o.TreeSelected(); ok {
		t.Fatal("a row was selected while loading")
	}
	return o.SetTree(treeFixtureRows(), summary)
}

func treeKeys(rows []panes.HiveTreeRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Key)
	}
	return out
}

func TestHivesTreeBeginIsRefusedOffTheIdleList(t *testing.T) {
	if _, ok := panes.NewHivesOverlay().BeginTree(); ok {
		t.Error("tree opened over the loading list")
	}
	form, _ := loadedHives().BeginAdd()
	if _, ok := form.BeginTree(); ok {
		t.Error("tree opened over the add form")
	}
	pending, _, _ := loadedHives().Submit()
	if _, ok := pending.BeginTree(); ok {
		t.Error("tree opened over an in-flight action")
	}
	// Tree mutators are no-ops off the tree screen.
	list := loadedHives()
	if list.SetTree(treeFixtureRows(), panes.HiveTreeSummary{}).InTree() || list.SetTreeError(errors.New("x")).TreeError() != "" || list.SetTreeNote("n").InTree() {
		t.Error("tree setters changed the list screen")
	}
	if list.TreeBeginSearch().TreeSearching() || list.TreeStaged() || list.TreeClose().InTree() {
		t.Error("tree state leaked onto the list screen")
	}
	if _, ok := list.TreeLeave(); ok {
		t.Error("TreeLeave left a screen that was not the tree")
	}
	if _, ok := list.BeginTreeConfirm(); ok {
		t.Error("confirmation opened off the tree")
	}
	if !strings.Contains(list.View(100), "t tree") {
		t.Errorf("list footer does not advertise the tree:\n%s", list.View(100))
	}
}

func TestHivesTreeVisibilityCollapseFilterAndSearchKeepAncestry(t *testing.T) {
	o := openTree(t, panes.HiveTreeSummary{Live: "unavailable", Durable: ""})
	if got := len(o.TreeVisible()); got != len(treeFixtureRows()) {
		t.Fatalf("all rows visible = %d", got)
	}
	view := o.View(100)
	for _, want := range []string{"CONTRIBUTION TREE", "filter: all", "live: unavailable", "durable: —", "[-]", "[x]", "[·]", "[ ]", "Unrestricted — legacy", "2 hive(s)", "No staged changes.", "1 of 8"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	// Collapse hub one: its descendants vanish; its own row stays.
	o = o.TreeExpand(false)
	if got := treeKeys(o.TreeVisible()); strings.Join(got, ",") != "hub:one,hub:two,hub:two/sub:gamma,hub:two/sub:gamma/repo:b" {
		t.Fatalf("collapsed = %v", got)
	}
	if !strings.Contains(o.View(100), "▸ ▸ [-]") {
		t.Errorf("collapsed glyph missing:\n%s", o.View(100))
	}
	// Collapsing again on a closed hub (no parent) is a no-op; expanding re-opens.
	o = o.TreeExpand(false).TreeExpand(true)
	if got := len(o.TreeVisible()); got != 8 {
		t.Fatalf("re-expanded = %d", got)
	}
	// Expanding an open branch enters it.
	o = o.TreeExpand(true)
	if row, _ := o.TreeSelected(); row.Key != "hub:one/sub:alpha" {
		t.Fatalf("→ on an open hub landed on %q", row.Key)
	}
	// ← on a leaf moves to its parent.
	o = o.TreeMove(1)
	if row, _ := o.TreeSelected(); row.Kind != panes.HiveTreeRepo {
		t.Fatalf("cursor = %+v", row)
	}
	o = o.TreeExpand(false)
	if row, _ := o.TreeSelected(); row.Key != "hub:one/sub:alpha" {
		t.Fatalf("← on a leaf landed on %q", row.Key)
	}
	// Needs-review filter keeps matches and their ancestors only.
	o = o.TreeJump(true).TreeSetFilter(panes.HiveTreeFilterReview)
	if got := treeKeys(o.TreeVisible()); strings.Join(got, ",") != "hub:one,hub:one/sub:beta,hub:one/sub:beta/none,hub:two,hub:two/sub:gamma,hub:two/sub:gamma/repo:b" {
		t.Fatalf("needs-review = %v", got)
	}
	if row, _ := o.TreeSelected(); row.Key != "hub:two/sub:gamma/repo:b" {
		t.Fatalf("cursor after filter = %q (expected clamp onto the last visible row)", row.Key)
	}
	o = o.TreeSetFilter(panes.HiveTreeFilterChanged)
	if got := treeKeys(o.TreeVisible()); strings.Join(got, ",") != "hub:one,hub:one/sub:alpha,hub:one/sub:alpha/repo:a" {
		t.Fatalf("changed = %v", got)
	}
	if !strings.Contains(o.View(100), "filter: changed") {
		t.Errorf("header lacks the filter name:\n%s", o.View(100))
	}
	// Search matches labels and ids case-insensitively and keeps ancestry.
	o = o.TreeSetFilter(panes.HiveTreeFilterAll).TreeBeginSearch()
	if !o.TreeSearching() {
		t.Fatal("search did not take the keyboard")
	}
	o = o.TreeSearchType("ORG/B")
	if got := treeKeys(o.TreeVisible()); strings.Join(got, ",") != "hub:two,hub:two/sub:gamma,hub:two/sub:gamma/repo:b" {
		t.Fatalf("search = %v", got)
	}
	if v := o.View(100); !strings.Contains(v, "search: ORG/B▏") || !strings.Contains(v, "type to search") {
		t.Errorf("search view:\n%s", v)
	}
	o = o.TreeSearchBackspace().TreeSearchBackspace().TreeSearchBackspace().TreeSearchBackspace().TreeSearchBackspace().TreeSearchBackspace()
	if got := len(o.TreeVisible()); got != 8 {
		t.Fatalf("backspace past empty = %d visible", got)
	}
	o = o.TreeSearchType("nomatch").TreeEndSearch(false)
	if o.TreeSearching() || len(o.TreeVisible()) != 0 {
		t.Fatalf("kept query: searching=%v visible=%d", o.TreeSearching(), len(o.TreeVisible()))
	}
	if _, ok := o.TreeSelected(); ok {
		t.Error("a row was selected with nothing visible")
	}
	v := o.View(100)
	if !strings.Contains(v, `no rows match the all filter and search "nomatch"`) || !strings.Contains(v, "search: nomatch") || strings.Contains(v, "▏") {
		t.Errorf("empty-search view:\n%s", v)
	}
	// Motion on an empty list clamps to zero; the footer drops the position.
	o = o.TreeMove(3).TreeJump(true)
	if !strings.HasPrefix(strings.TrimSpace(lastLine(v)), "j/k move") && !strings.Contains(v, "j/k move") {
		t.Errorf("footer on empty list:\n%s", v)
	}
	o = o.TreeEndSearch(true)
	if len(o.TreeVisible()) != 8 || strings.Contains(o.View(100), "search:") {
		t.Errorf("clear did not drop the query:\n%s", o.View(100))
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

func TestHivesTreeCursorMotionAndReprojectionKeepTheKey(t *testing.T) {
	o := openTree(t, panes.HiveTreeSummary{})
	o = o.TreeMove(-5)
	if row, _ := o.TreeSelected(); row.Key != "hub:one" {
		t.Fatalf("move up past the top = %q", row.Key)
	}
	o = o.TreeMove(100)
	if row, _ := o.TreeSelected(); row.Key != "hub:two/sub:gamma/repo:b" {
		t.Fatalf("move down past the end = %q", row.Key)
	}
	o = o.TreeJump(false).TreeMove(2)
	if row, _ := o.TreeSelected(); row.Key != "hub:one/sub:alpha/repo:a" {
		t.Fatalf("cursor = %q", row.Key)
	}
	// A reprojection with the same key keeps the cursor; a missing key resets.
	rows := treeFixtureRows()
	rows[2].Check = panes.HiveTreeCheckOff
	o = o.SetTree(rows, panes.HiveTreeSummary{Staged: true, Lines: []string{"alpha: - github/github.com/org/a"}})
	if row, _ := o.TreeSelected(); row.Key != "hub:one/sub:alpha/repo:a" || row.Check != panes.HiveTreeCheckOff {
		t.Fatalf("reprojection lost the cursor: %+v", row)
	}
	if !o.TreeStaged() || len(o.TreeSummaryLines()) != 1 {
		t.Fatal("summary not recorded")
	}
	if v := o.View(100); !strings.Contains(v, "Staged: alpha: - github/github.com/org/a  — A apply  U discard") {
		t.Errorf("stage line:\n%s", v)
	}
	o = o.SetTree(rows[:1], panes.HiveTreeSummary{})
	if row, _ := o.TreeSelected(); row.Key != "hub:one" {
		t.Fatalf("missing key did not reset the cursor: %q", row.Key)
	}
	// Collapsed state is remembered by key across reprojections.
	o = o.TreeExpand(false).SetTree(treeFixtureRows(), panes.HiveTreeSummary{})
	if got := len(o.TreeVisible()); got != 4 {
		t.Fatalf("collapse forgotten across reprojection: %d visible", got)
	}
	// An empty projection renders the none marker, not a blank screen.
	o = o.SetTree(nil, panes.HiveTreeSummary{})
	if v := o.View(100); !strings.Contains(v, "no hubs or hives to show") {
		t.Errorf("empty tree view:\n%s", v)
	}
}

func TestHivesTreeWindowsLongTrees(t *testing.T) {
	o, _ := loadedHives().BeginTree()
	var rows []panes.HiveTreeRow
	for i := 0; i < 40; i++ {
		rows = append(rows, panes.HiveTreeRow{Kind: panes.HiveTreeHub, Key: fmt.Sprintf("hub:%02d", i), Level: 1, Label: fmt.Sprintf("hub-%02d", i)})
	}
	o = o.SetTree(rows, panes.HiveTreeSummary{})
	v := o.View(100)
	if !strings.Contains(v, "hub-00") || strings.Contains(v, "hub-20") {
		t.Errorf("window at the top:\n%s", v)
	}
	o = o.TreeMove(20)
	v = o.View(100)
	if !strings.Contains(v, "hub-20") || strings.Contains(v, "hub-00") || !strings.Contains(v, "21 of 40") {
		t.Errorf("window around the cursor:\n%s", v)
	}
	o = o.TreeJump(true)
	v = o.View(100)
	if !strings.Contains(v, "hub-39") || !strings.Contains(v, "hub-28") || strings.Contains(v, "hub-27") {
		t.Errorf("window at the end:\n%s", v)
	}
}

func TestHivesTreeErrorsNotesAndHelp(t *testing.T) {
	o := openTree(t, panes.HiveTreeSummary{})
	if o.SetTreeError(nil).TreeError() != "" {
		t.Error("nil error recorded")
	}
	o = o.SetTreeNote("✓ change saved; saved: x; relay reload: y").SetTreeError(errors.New("nothing to check"))
	if o.TreeError() != "nothing to check" || !strings.Contains(o.View(100), "nothing to check") || strings.Contains(o.View(100), "change saved") {
		t.Errorf("error did not replace the note:\n%s", o.View(100))
	}
	o = o.SetTreeNote("✓ change saved; saved: x; relay reload: y")
	v := o.View(100)
	if o.TreeError() != "" || !strings.Contains(v, "✓ change saved;") || !strings.Contains(v, "\n") || !strings.Contains(v, "saved: x;") || !strings.Contains(v, "relay reload: y") || strings.Contains(v, "x; relay") {
		t.Errorf("note did not replace the error or wrap on ';':\n%s", v)
	}
	o = o.TreeToggleHelp()
	v = o.View(100)
	if !strings.Contains(v, "Contribution tree keys") || !strings.Contains(v, "? close help") || strings.Contains(v, "CONTRIBUTION TREE") {
		t.Errorf("help view:\n%s", v)
	}
	o = o.TreeToggleHelp()
	if strings.Contains(o.View(100), "Contribution tree keys") {
		t.Error("help did not close")
	}
	// Typing() reports the search field only; esc-quits footer wording.
	if o.Typing() {
		t.Error("Typing on the tree without a search")
	}
	if !o.TreeBeginSearch().Typing() {
		t.Error("Typing did not report the search field")
	}
	q := loadedHives().WithEscQuit()
	q, _ = q.BeginTree()
	q = q.SetTree(treeFixtureRows(), panes.HiveTreeSummary{})
	if !strings.Contains(q.View(100), "esc list (esc/q quit)") {
		t.Errorf("esc-quits footer:\n%s", q.View(100))
	}
}

func TestHivesTreeLeaveWarnsOnceWhenStaged(t *testing.T) {
	o := openTree(t, panes.HiveTreeSummary{})
	left, ok := o.TreeLeave()
	if !ok || left.InTree() {
		t.Fatal("clean leave refused")
	}
	if !strings.Contains(left.View(100), "t tree") {
		t.Errorf("did not return to the list:\n%s", left.View(100))
	}
	o = o.SetTree(treeFixtureRows(), panes.HiveTreeSummary{Staged: true, Lines: []string{"x"}})
	o, ok = o.TreeLeave()
	if ok || !o.InTree() || !strings.Contains(o.TreeError(), "staged edits would be lost") {
		t.Fatalf("first leave did not warn: ok=%v err=%q", ok, o.TreeError())
	}
	// Any cursor motion resets the warning.
	o = o.TreeMove(1)
	o, ok = o.TreeLeave()
	if ok {
		t.Fatal("leave after motion skipped the warning")
	}
	o, ok = o.TreeLeave()
	if !ok || o.InTree() {
		t.Fatal("second leave did not close")
	}
	// TreeClose is unconditional.
	o = openTree(t, panes.HiveTreeSummary{Staged: true, Lines: []string{"x"}})
	if o.TreeClose().InTree() {
		t.Fatal("TreeClose kept the tree")
	}
}

func TestHivesTreeApplyConfirmationAndPending(t *testing.T) {
	o := openTree(t, panes.HiveTreeSummary{})
	if _, ok := o.BeginTreeConfirm(); ok {
		t.Fatal("confirmation opened with nothing staged")
	}
	o = o.SetTree(treeFixtureRows(), panes.HiveTreeSummary{Staged: true, Lines: []string{"alpha: + org/a", "beta: legacy conversion"}})
	o, ok := o.BeginTreeConfirm()
	if !ok || !o.TreeConfirming() || !o.InTree() {
		t.Fatal("confirmation did not open")
	}
	if _, ok := o.BeginTreeConfirm(); ok {
		t.Error("confirmation re-entered from the confirmation")
	}
	if o.TreeBeginSearch().TreeSearching() {
		t.Error("search opened over the confirmation")
	}
	v := o.View(100)
	for _, want := range []string{"Apply staged changes?", "• alpha: + org/a", "• beta: legacy conversion", "shared lock", "acknowledgement is reported separately", "enter apply  esc cancel"} {
		if !strings.Contains(v, want) {
			t.Errorf("confirmation lacks %q:\n%s", want, v)
		}
	}
	o = o.SetTreeError(errors.New("apply refused"))
	if !strings.Contains(o.View(100), "apply refused") {
		t.Errorf("confirmation error missing:\n%s", o.View(100))
	}
	cancelled := o.TreeConfirmCancel()
	if cancelled.TreeConfirming() || !cancelled.InTree() {
		t.Fatal("cancel did not return to the tree")
	}
	if cancelled.TreeConfirmCancel().TreeConfirming() {
		t.Error("cancel off the confirmation changed mode")
	}
	o = o.BeginTreeApply()
	if !o.Pending() || !strings.Contains(o.View(100), "working…") {
		t.Fatalf("apply not pending:\n%s", o.View(100))
	}
	o = o.EndTreeApply()
	if o.Pending() || o.TreeConfirming() || !o.InTree() {
		t.Fatalf("EndTreeApply state: pending=%v confirming=%v inTree=%v", o.Pending(), o.TreeConfirming(), o.InTree())
	}
	// A leave with the warning already shown proceeds even from the confirmation path.
	o, _ = o.TreeLeave()
	o, ok = o.TreeLeave()
	if !ok || o.InTree() {
		t.Fatal("leave after the warning did not close")
	}
}

func TestHivesTreeFilterNames(t *testing.T) {
	for f, want := range map[panes.HiveTreeFilter]string{panes.HiveTreeFilterAll: "all", panes.HiveTreeFilterReview: "needs review", panes.HiveTreeFilterChanged: "changed", panes.HiveTreeFilter(9): "all"} {
		if got := f.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", f, got, want)
		}
	}
}
