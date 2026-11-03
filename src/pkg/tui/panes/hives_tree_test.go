package panes_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

func treeView() panes.HiveTreeView {
	return panes.HiveTreeView{
		Scope:  "2 repositories",
		Staged: 0,
		Nodes: []panes.HiveTreeNode{
			{Key: "hub", Kind: panes.HiveTreeHub, Short: "acme-hub", Identity: "wss://acme.example", Hub: "wss://acme.example", Selection: "-", Enabled: true, Subscriptions: 2, Repositories: 3, Provenance: "profile", Explanation: "partial", ExclusionSource: "none"},
			{Key: "hub/s1", Parent: "hub", Kind: panes.HiveTreeHive, Depth: 1, Short: "alpha", Identity: "acme/alpha", Hub: "wss://acme.example", Subscription: "alpha", Selection: "x", Enabled: true, Follow: "following", Readiness: "ready", Activity: "active", Badges: []string{"shared", "new"}, Subscriptions: 1, Repositories: 2},
			{Key: "hub/s1/r1", Parent: "hub/s1", Kind: panes.HiveTreeRepo, Depth: 2, Short: "repo-one", Identity: "acme/repo-one", Subscription: "alpha", Repository: "acme/repo-one", Selection: "x"},
			{Key: "hub/s1/r2", Parent: "hub/s1", Kind: panes.HiveTreeRepo, Depth: 2, Short: "repo-two", Identity: "acme/repo-two", Subscription: "alpha", Repository: "acme/repo-two", Selection: " ", Excluded: true},
			{Key: "hub/s2", Parent: "hub", Kind: panes.HiveTreeHive, Depth: 1, Short: "legacy", Identity: "acme/legacy", Subscription: "legacy", Selection: " ", Legacy: true, Follow: "paused", Subscriptions: 1, Repositories: 1},
		},
	}
}

func openTree(t *testing.T, view panes.HiveTreeView) panes.HivesOverlay {
	t.Helper()
	o, ok := loadedHives().OpenTree()
	if !ok {
		t.Fatal("OpenTree refused on a loaded list")
	}
	return o.SetTree(view)
}

func focus(t *testing.T, o panes.HivesOverlay, key string) panes.HivesOverlay {
	t.Helper()
	o = o.TreeMove(-20)
	for i := 0; i < 20; i++ {
		if n, _ := o.TreeCurrent(); n.Key == key {
			return o
		}
		o = o.TreeMove(1)
	}
	t.Fatalf("could not focus %q", key)
	return o
}

func TestHivesTreeOpenAndClose(t *testing.T) {
	o := openTree(t, treeView())
	if !o.InTree() || !o.Typing() {
		t.Error("tree mode must report InTree and Typing")
	}
	if _, ok := o.OpenTree(); ok {
		t.Error("tree opened twice")
	}
	o = o.CloseTree()
	if o.InTree() {
		t.Error("CloseTree stayed in the tree")
	}
	o, _ = o.OpenTree()
	if !o.TreeLoading() {
		t.Error("reopened tree is not loading")
	}
	if _, ok := o.TreeCurrent(); ok {
		t.Error("loading tree offered a node")
	}
	if !strings.Contains(o.View(90), "Reading the contribution tree") {
		t.Errorf("loading text missing:\n%s", o.View(90))
	}
	if !strings.Contains(o.View(90), "esc back") {
		t.Error("loading footer missing")
	}
}

func TestHivesTreeRenderKinds(t *testing.T) {
	o := openTree(t, treeView())
	v := o.View(120)
	for _, want := range []string{"Effective scope: 2 repositories", "Staged: none", "acme-hub", "alpha", "legacy", "NAME", "FOLLOW", "READINESS", "ACTIVITY", "Identity: wss://acme.example", "Provenance: profile", "Selection: partial", "Exclusion source: none", "A apply  D discard", "- acme-hub"} {
		if !strings.Contains(v, want) {
			t.Errorf("hub view missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "repo-one") {
		t.Error("collapsed hive's repos were drawn")
	}
	o = o.TreeMove(1).TreeExpand(true)
	v = o.View(120)
	for _, want := range []string{"Badges: shared, new", "Follow: following", "Readiness: ready", "Activity: active"} {
		if !strings.Contains(v, want) {
			t.Errorf("hive view missing %q:\n%s", want, v)
		}
	}
	o = o.TreeMove(1)
	v = o.View(120)
	for _, want := range []string{"repo-one", "repo-two", "Identity: acme/repo-one"} {
		if !strings.Contains(v, want) {
			t.Errorf("repo view missing %q:\n%s", want, v)
		}
	}
	if !strings.Contains(v, "▸") {
		t.Error("no cursor marker")
	}
	narrow := o.View(60)
	if strings.Contains(narrow, "READINESS") {
		t.Errorf("narrow view drew wide columns:\n%s", narrow)
	}
	if !strings.Contains(narrow, "NAME") {
		t.Error("narrow view lost the header")
	}
}

func TestHivesTreeFooterVariants(t *testing.T) {
	view := treeView()
	view.Staged = 2
	o := openTree(t, view)
	if !strings.Contains(o.View(120), "A apply (2 staged)") {
		t.Errorf("staged count missing:\n%s", o.View(120))
	}
	if !strings.Contains(o.View(120), "Staged: 2 unapplied") {
		t.Error("staged line missing")
	}
	if !strings.Contains(o.TreeBeginSearch().View(120), "type to filter") {
		t.Error("search footer missing")
	}
	q := panes.NewHivesOverlay().SetHives(hiveFixture(), "p", "c", "ranked")
	q = q.WithEscQuit()
	q, _ = q.OpenTree()
	q = q.SetTree(treeView())
	if !strings.Contains(q.View(120), "q quit") {
		t.Errorf("hives-only footer missing q quit:\n%s", q.View(120))
	}
}

func TestHivesTreeMoveAndExpand(t *testing.T) {
	o := openTree(t, treeView())
	if n, _ := o.TreeCurrent(); n.Key != "hub" {
		t.Fatalf("initial focus = %q", n.Key)
	}
	o = o.TreeMove(-5)
	if n, _ := o.TreeCurrent(); n.Key != "hub" {
		t.Errorf("up past top = %q", n.Key)
	}
	o = o.TreeMove(99)
	if n, _ := o.TreeCurrent(); n.Key != "hub/s2" {
		t.Errorf("down past end = %q", n.Key)
	}
	// Collapsing a leaf-like row moves to its parent.
	o = o.TreeExpand(false)
	if n, _ := o.TreeCurrent(); n.Key != "hub" {
		t.Errorf("collapse on childless row = %q, want parent", n.Key)
	}
	o = o.TreeExpand(false)
	if len(visibleKeys(o)) != 1 {
		t.Errorf("collapsed hub still shows children: %v", visibleKeys(o))
	}
	o = o.TreeToggleExpand()
	if len(visibleKeys(o)) != 3 {
		t.Errorf("toggle did not expand: %v", visibleKeys(o))
	}
	o = o.TreeToggleExpand()
	if len(visibleKeys(o)) != 1 {
		t.Errorf("toggle did not collapse: %v", visibleKeys(o))
	}
	// Expanding a childless node is a no-op.
	o = o.TreeToggleExpand().TreeMove(2).TreeExpand(true)
	if n, _ := o.TreeCurrent(); n.Key != "hub/s2" {
		t.Errorf("focus = %q", n.Key)
	}
	// Cursor on a hidden child falls back to its ancestor after SetTree.
	o = focus(t, o, "hub").TreeExpand(true).TreeMove(1).TreeExpand(true).TreeMove(1)
	o = o.TreeMove(-1).TreeExpand(false).TreeMove(-1).TreeExpand(false)
	o = o.SetTree(treeView())
	if n, _ := o.TreeCurrent(); n.Key != "hub" {
		t.Errorf("focus after refresh = %q", n.Key)
	}
	if got := panes.NewHivesOverlay().TreeToggleExpand(); got.InTree() {
		t.Error("toggle outside the tree entered it")
	}
}

func visibleKeys(o panes.HivesOverlay) []string {
	var keys []string
	for _, line := range strings.Split(o.View(120), "\n") {
		if !strings.Contains(line, "[") || !strings.Contains(line, "] ") {
			continue
		}
		for _, k := range []string{"acme-hub", "alpha", "legacy", "repo-one", "repo-two"} {
			if strings.Contains(line, k) && !strings.Contains(line, "Identity:") {
				keys = append(keys, k)
				break
			}
		}
	}
	return keys
}

func TestHivesTreeSearch(t *testing.T) {
	o := openTree(t, treeView())
	o = o.TreeType("x") // not searching yet: ignored
	o = o.TreeBeginSearch()
	if !o.TreeSearching() {
		t.Fatal("search did not open")
	}
	o = o.TreeType("repo-two")
	v := o.View(120)
	if !strings.Contains(v, "Search: [repo-two]") || !strings.Contains(v, "repo-two") || strings.Contains(v, "legacy") {
		t.Errorf("filter view wrong:\n%s", v)
	}
	o = o.TreeBackspace()
	if !strings.Contains(o.View(120), "Search: [repo-tw]") {
		t.Error("backspace did not trim")
	}
	o = o.TreeEndSearch()
	if o.TreeSearching() || !strings.Contains(o.View(120), "Search: [repo-tw]") {
		t.Error("end search lost the filter")
	}
	o = o.TreeEndSearch().TreeBackspace().TreeType("z")
	o = o.TreeBeginSearch().TreeType("zzz")
	if !strings.Contains(o.View(120), "no matches") {
		t.Errorf("no-match state missing:\n%s", o.View(120))
	}
	o = o.TreeCancel()
	if o.TreeSearching() || strings.Contains(o.View(120), "no matches") {
		t.Error("cancel did not clear the filter")
	}
	o = o.TreeBeginSearch()
	for i := 0; i < 3; i++ {
		o = o.TreeBackspace()
	}
}

// Cancelling a filter while focused on a row only the filter revealed moves
// focus to the nearest ancestor that is still drawn.
func TestHivesTreeSearchCancelRefocusesAncestor(t *testing.T) {
	// Movement is only honoured while browsing, so leave the prompt with the
	// filter kept, focus the revealed row, then reopen the prompt and cancel.
	o := openTree(t, treeView()).TreeBeginSearch().TreeType("repo-two").TreeEndSearch()
	o = focus(t, o, "hub/s1/r2")
	o = o.TreeBeginSearch().TreeCancel()
	if n, _ := o.TreeCurrent(); n.Key != "hub/s1" {
		t.Errorf("focus after cancel = %q, want the collapsed hive", n.Key)
	}
}

func TestHivesTreeEmpty(t *testing.T) {
	o := openTree(t, panes.HiveTreeView{Scope: "none"})
	if !strings.Contains(o.View(100), "no subscriptions") {
		t.Errorf("empty state missing:\n%s", o.View(100))
	}
	o = o.TreeMove(1)
	if _, a, ok := o.TreeToggleSelect(); ok || a != (panes.HivesTreeAction{}) {
		t.Error("empty tree produced an action")
	}
	if _, _, ok := o.TreeToggleExclude(); ok {
		t.Error("exclude on empty tree")
	}
	if _, _, ok := o.TreeToggleFollow(); ok {
		t.Error("follow on empty tree")
	}
	if _, _, ok := o.TreeToggleEnabled(); ok {
		t.Error("enable on empty tree")
	}
	if _, _, ok := o.TreeRestrict(); ok {
		t.Error("restrict on empty tree")
	}
}

func TestHivesTreeActions(t *testing.T) {
	tests := []struct {
		name    string
		focus   string
		do      func(panes.HivesOverlay) (panes.HivesOverlay, panes.HivesTreeAction, bool)
		want    panes.HivesTreeAction
		direct  bool
		prompt  string
		noteHas string
	}{
		{"select repo on", "hub/s1/r2", panes.HivesOverlay.TreeToggleSelect, panes.HivesTreeAction{Kind: panes.HivesTreeSelect, Target: "alpha", Repository: "acme/repo-two", Value: true}, true, "", ""},
		{"select repo off", "hub/s1/r1", panes.HivesOverlay.TreeToggleSelect, panes.HivesTreeAction{Kind: panes.HivesTreeSelect, Target: "alpha", Repository: "acme/repo-one", Value: false}, true, "", ""},
		{"select hub asks", "hub", panes.HivesOverlay.TreeToggleSelect, panes.HivesTreeAction{Kind: panes.HivesTreeSelectBranch, Target: "wss://acme.example", Hub: true, Value: true}, false, "Select all current repositories in acme-hub?\nAffects 2 subscriptions and 3 repositories.", ""},
		{"deselect hive asks", "hub/s1", panes.HivesOverlay.TreeToggleSelect, panes.HivesTreeAction{Kind: panes.HivesTreeSelectBranch, Target: "alpha", Value: false}, false, "Deselect all current repositories in alpha?\nAffects 1 subscription and 2 repositories.", ""},
		{"exclude repo", "hub/s1/r1", panes.HivesOverlay.TreeToggleExclude, panes.HivesTreeAction{Kind: panes.HivesTreeExclude, Target: "alpha", Repository: "acme/repo-one", Value: true}, true, "", ""},
		{"clear exclusion", "hub/s1/r2", panes.HivesOverlay.TreeToggleExclude, panes.HivesTreeAction{Kind: panes.HivesTreeExclude, Target: "alpha", Repository: "acme/repo-two", Value: false}, true, "", ""},
		{"exclude hub refused", "hub", panes.HivesOverlay.TreeToggleExclude, panes.HivesTreeAction{}, false, "", "x excludes a repository"},
		{"follow stops", "hub/s1", panes.HivesOverlay.TreeToggleFollow, panes.HivesTreeAction{Kind: panes.HivesTreeFollow, Target: "alpha", Value: false}, true, "", ""},
		{"follow from repo", "hub/s1/r1", panes.HivesOverlay.TreeToggleFollow, panes.HivesTreeAction{Kind: panes.HivesTreeFollow, Target: "alpha", Value: false}, true, "", ""},
		{"follow starts", "hub/s2", panes.HivesOverlay.TreeToggleFollow, panes.HivesTreeAction{Kind: panes.HivesTreeFollow, Target: "legacy", Value: true}, true, "", ""},
		{"follow hub refused", "hub", panes.HivesOverlay.TreeToggleFollow, panes.HivesTreeAction{}, false, "", "f follows new repositories"},
		{"enable sub", "hub/s2", panes.HivesOverlay.TreeToggleEnabled, panes.HivesTreeAction{Kind: panes.HivesTreeEnable, Target: "legacy", Value: true}, true, "", ""},
		{"disable from repo", "hub/s1/r1", panes.HivesOverlay.TreeToggleEnabled, panes.HivesTreeAction{Kind: panes.HivesTreeEnable, Target: "alpha", Value: false}, true, "", ""},
		{"disable hub asks", "hub", panes.HivesOverlay.TreeToggleEnabled, panes.HivesTreeAction{Kind: panes.HivesTreeHubEnable, Target: "wss://acme.example", Hub: true, Value: false}, false, "Disable every subscription under acme-hub?", ""},
		{"restrict legacy asks", "hub/s2", panes.HivesOverlay.TreeRestrict, panes.HivesTreeAction{Kind: panes.HivesTreeRestrict, Target: "legacy"}, false, "Review and restrict legacy?", ""},
		{"restrict non-legacy refused", "hub/s1", panes.HivesOverlay.TreeRestrict, panes.HivesTreeAction{}, false, "", "R reviews and restricts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := openTree(t, treeView())
			o = focus(t, expandKeys(o), tc.focus)
			o, got, ok := tc.do(o)
			if tc.noteHas != "" {
				if ok || !strings.Contains(o.View(120), tc.noteHas) {
					t.Fatalf("want note %q, ok=%v:\n%s", tc.noteHas, ok, o.View(120))
				}
				return
			}
			if tc.direct {
				if !ok || got != tc.want {
					t.Fatalf("got %+v ok=%v, want %+v", got, ok, tc.want)
				}
				return
			}
			if ok || !o.TreeConfirming() {
				t.Fatalf("bulk action did not ask first (ok=%v)", ok)
			}
			v := o.View(120)
			for _, part := range strings.Split(tc.prompt, "\n") {
				if !strings.Contains(v, part) {
					t.Errorf("prompt missing %q:\n%s", part, v)
				}
			}
			if !strings.Contains(v, "Nothing is written until Apply") {
				t.Errorf("confirm hint missing:\n%s", v)
			}
			if !strings.Contains(o.View(120), "enter/y stage") {
				t.Error("confirm footer missing")
			}
			o2, act, ok := o.TreeConfirm()
			if !ok || act != tc.want || o2.TreeConfirming() {
				t.Errorf("confirm gave %+v ok=%v, want %+v", act, ok, tc.want)
			}
			if _, _, ok := o2.TreeConfirm(); ok {
				t.Error("confirm accepted twice")
			}
			if c := o.TreeCancel(); c.TreeConfirming() {
				t.Error("cancel left the prompt open")
			}
		})
	}
}

// expandKeys expands every branch so repository rows are reachable.
func expandKeys(o panes.HivesOverlay) panes.HivesOverlay {
	o = o.TreeMove(1).TreeExpand(true).TreeMove(-1)
	return o
}

func TestHivesTreeActionsRefusedOutsideBrowse(t *testing.T) {
	o := openTree(t, treeView()).TreeBeginSearch()
	if _, _, ok := o.TreeToggleSelect(); ok {
		t.Error("select while searching")
	}
	if _, _, ok := o.TreeToggleEnabled(); ok {
		t.Error("enable while searching")
	}
	if _, _, ok := o.TreeToggleFollow(); ok {
		t.Error("follow while searching")
	}
	if _, _, ok := o.TreeToggleExclude(); ok {
		t.Error("exclude while searching")
	}
	if _, _, ok := o.TreeRestrict(); ok {
		t.Error("restrict while searching")
	}
	a, _ := o.TreeCurrent()
	b, _ := o.TreeMove(1).TreeCurrent()
	if a.Key != b.Key {
		t.Error("moved while searching")
	}
	if _, _, ok := openTree(t, treeView()).TreeConfirm(); ok {
		t.Error("confirm with no prompt")
	}
	if _, ok := panes.NewHivesOverlay().TreeStartApply(); ok {
		t.Error("start apply outside the tree")
	}
}

func TestHivesTreeApplyFlow(t *testing.T) {
	// Nothing staged.
	o := openTree(t, treeView()).TreeBeginApply()
	if o.TreeReviewing() || !strings.Contains(o.View(120), "nothing staged") {
		t.Errorf("empty apply not refused:\n%s", o.View(120))
	}
	// Stale.
	stale := treeView()
	stale.Stale, stale.Staged = true, 1
	o = openTree(t, stale)
	if !o.TreeStale() {
		t.Error("stale flag lost")
	}
	if !strings.Contains(o.View(120), "STALE:") {
		t.Error("stale banner missing")
	}
	o = o.TreeBeginApply()
	if o.TreeReviewing() || !strings.Contains(o.View(120), "profiles changed in another") {
		t.Errorf("stale apply not refused:\n%s", o.View(120))
	}
	// Review → apply → receipt.
	view := treeView()
	view.Staged = 2
	view.Diff = []string{"+ select acme/repo-one in alpha", "- follow legacy"}
	o = openTree(t, view)
	if o.TreeStaged() != 2 {
		t.Errorf("TreeStaged = %d", o.TreeStaged())
	}
	if _, ok := o.TreeStartApply(); ok {
		t.Error("apply started before review")
	}
	o = o.TreeBeginApply()
	if !o.TreeReviewing() {
		t.Fatal("review did not open")
	}
	v := o.View(120)
	for _, want := range []string{"Review staged changes", "+ select acme/repo-one in alpha", "- follow legacy", "Nothing has been written yet", "Enter apply · Esc back"} {
		if !strings.Contains(v, want) {
			t.Errorf("review missing %q:\n%s", want, v)
		}
	}
	if o.TreeCancel().TreeReviewing() {
		t.Error("cancel stayed in review")
	}
	o, ok := o.TreeStartApply()
	if !ok || !o.Pending() {
		t.Fatal("apply did not start")
	}
	if !strings.Contains(o.View(120), "Applying staged edits") {
		t.Error("pending text missing")
	}
	if !o.TreeCancel().Pending() {
		t.Error("cancel while pending changed state")
	}
	o = o.SetTreeReceipt([]string{"saved 2 edits", "profiles.yml written"}, "")
	if !o.TreeReceipt() || o.Pending() {
		t.Fatal("receipt did not show")
	}
	v = o.View(120)
	if !strings.Contains(v, "Apply receipt") || !strings.Contains(v, "saved 2 edits") || !strings.Contains(v, "enter/esc continue") {
		t.Errorf("receipt wrong:\n%s", v)
	}
	o = o.SetTreeReceipt([]string{"partial"}, "disk full")
	if !strings.Contains(o.View(120), "disk full") {
		t.Error("receipt error missing")
	}
	if o.TreeCancel().TreeReceipt() {
		t.Error("cancel stayed on receipt")
	}
}

func TestHivesTreeErrorAndNote(t *testing.T) {
	o := openTree(t, treeView())
	o = o.SetTreeError("hub refused the edit")
	if !strings.Contains(o.View(120), "hub refused the edit") {
		t.Error("error missing")
	}
	o = o.SetTreeNote("saved")
	v := o.View(120)
	if !strings.Contains(v, "saved") || strings.Contains(v, "hub refused") {
		t.Errorf("note did not replace error:\n%s", v)
	}
	loading, _ := loadedHives().OpenTree()
	loading = loading.SetTreeError("read failed")
	if loading.TreeLoading() {
		t.Error("error left the tree loading")
	}
}

func TestHivesTreeLeave(t *testing.T) {
	o := openTree(t, treeView())
	if _, leave := o.TreeBeginLeave(panes.HivesTreeClose); !leave {
		t.Error("clean tree should leave at once")
	}
	if _, leave := o.TreeBeginLeave(panes.HivesTreeStay); leave {
		t.Error("D with nothing staged still asks")
	}
	staged := treeView()
	staged.Staged = 1
	o = openTree(t, staged)
	o, leave := o.TreeBeginLeave(panes.HivesTreeQuit)
	if leave {
		t.Fatal("staged edits left without asking")
	}
	to, ok := o.TreeDiscarding()
	if !ok || to != panes.HivesTreeQuit {
		t.Errorf("discard state = %v %v", to, ok)
	}
	v := o.View(120)
	if !strings.Contains(v, "Discard 1 unapplied edit?") || !strings.Contains(v, "y discard") {
		t.Errorf("discard prompt wrong:\n%s", v)
	}
	staged.Staged = 3
	o, _ = openTree(t, staged).TreeBeginLeave(panes.HivesTreeClose)
	if !strings.Contains(o.View(120), "Discard 3 unapplied edits?") {
		t.Error("plural discard prompt wrong")
	}
	staleV := treeView()
	staleV.Stale = true
	o, leave = openTree(t, staleV).TreeBeginLeave(panes.HivesTreeClose)
	if leave || !strings.Contains(o.View(120), "Discard this stale review and reload?") {
		t.Errorf("stale discard wrong:\n%s", o.View(120))
	}
	o = o.TreeCancel()
	if _, ok := o.TreeDiscarding(); ok {
		t.Error("cancel kept the discard prompt")
	}
	o = o.CloseTree()
	if o.InTree() {
		t.Error("CloseTree did not leave")
	}
}

func TestHivesTreeRowWindow(t *testing.T) {
	view := panes.HiveTreeView{Scope: "many"}
	view.Nodes = append(view.Nodes, panes.HiveTreeNode{Key: "hub", Kind: panes.HiveTreeHub, Short: "hub"})
	for i := 0; i < 30; i++ {
		key := "hub/" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		view.Nodes = append(view.Nodes, panes.HiveTreeNode{Key: key, Parent: "hub", Kind: panes.HiveTreeHive, Depth: 1, Short: "sub-" + key, Subscription: key})
	}
	o := openTree(t, view).TreeMove(15)
	v := o.View(100)
	if !strings.Contains(v, "of 31 rows") {
		t.Errorf("window indicator missing:\n%s", v)
	}
	long := panes.HiveTreeView{Scope: strings.Repeat("s", 300), Nodes: []panes.HiveTreeNode{{Key: "h", Kind: panes.HiveTreeHub, Short: strings.Repeat("n", 100), Identity: strings.Repeat("i", 200), Badges: []string{strings.Repeat("b", 100)}}}}
	for _, w := range []int{20, 40, 70, 120} {
		if openTree(t, long).View(w) == "" {
			t.Errorf("width %d rendered nothing", w)
		}
	}
}

func TestHivesTreeReviewViewport(t *testing.T) {
	v := treeView()
	v.Staged = 60
	for i := 0; i < 60; i++ {
		v.Diff = append(v.Diff, fmt.Sprintf("change-%02d github/github.com/org/repo-%02d", i, i))
	}
	o := openTree(t, v).TreeBeginApply()
	check := func(width, height int) string {
		t.Helper()
		view := o.View(width, height)
		if lipgloss.Width(view) > width || lipgloss.Height(view) > height {
			t.Fatalf("review exceeds %dx%d: %dx%d", width, height, lipgloss.Width(view), lipgloss.Height(view))
		}
		for _, hint := range []string{"Nothing has been written", "Effective scope:", "Enter apply", "Esc back", "PgUp/PgDn", "Lines "} {
			if !strings.Contains(view, hint) {
				t.Fatalf("missing %q: %s", hint, view)
			}
		}
		return view
	}
	first := check(80, 24)
	if !strings.Contains(first, "change-00") || strings.Contains(first, "change-59") {
		t.Fatal("review is not bounded at the first page")
	}
	o = o.TreeReviewScroll("pgdown", 80, 24)
	if check(80, 24) == first {
		t.Fatal("PageDown did not move")
	}
	o = o.TreeReviewScroll("pgup", 80, 24)
	if check(80, 24) != first {
		t.Fatal("PageUp did not return")
	}
	o = o.TreeReviewScroll("end", 80, 24)
	if !strings.Contains(check(80, 24), "change-59") {
		t.Fatal("End did not reveal final change")
	}
	check(40, 12)
	check(120, 60) // clamps an offset beyond the new final page
	o = o.TreeReviewScroll("home", 80, 24)
	if check(80, 24) != first {
		t.Fatal("Home did not return")
	}
	for _, keys := range [][2]string{{"down", "up"}, {"j", "k"}} {
		o = o.TreeReviewScroll(keys[0], 80, 24)
		if check(80, 24) == first {
			t.Fatal("line scroll did not move")
		}
		o = o.TreeReviewScroll(keys[1], 80, 24)
		if check(80, 24) != first {
			t.Fatal("reverse line scroll did not return")
		}
	}
	if o.TreeReviewFits(39, 24) || o.TreeReviewFits(80, 11) || !strings.Contains(o.View(39, 11), "Enter disabled") || !strings.Contains(o.View(80, 11), "resize to 40x12") {
		t.Fatal("small terminal must pause Apply explicitly")
	}
	for _, line := range strings.Split(o.View(30, 8), "\n") {
		if strings.TrimSpace(line) == "." {
			t.Fatal("paused message wrapped a lone period onto its own line")
		}
	}
	if o.TreeCancel().TreeStaged() != 60 {
		t.Fatal("Escape lost staged edits")
	}
}

func TestHivesTreeReviewWrapsFullUnicodeIdentity(t *testing.T) {
	v := treeView()
	v.Staged = 1
	identity := "github/github.com/" + strings.Repeat("界é", 40) + "/target-final"
	v.Diff = []string{identity}
	o := openTree(t, v).TreeBeginApply()
	// A tall, narrow view exposes every wrapped row; joining the rows must
	// recover the entire identity, with no clipping or Unicode corruption.
	view := o.View(40, 80)
	if lipgloss.Width(view) > 40 || !strings.Contains(strings.ReplaceAll(view, "\n", ""), identity) {
		t.Fatalf("identity lost or overflowed: %s", view)
	}
}
