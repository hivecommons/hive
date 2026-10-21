package tui

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/internal/hivectltest"
	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// Key-by-key coverage of the tree's browsing keys that the scenario tests in
// hives_tree_test.go do not reach: x / f / e stage through the same shared
// layer as the Web tree, D with nothing staged only notes, q outside
// --hives-only stays put, and esc with nothing staged closes at once.

func TestHivesTreeExcludeFollowEnableMatchSharedLayer(t *testing.T) {
	h := treeHarness(t, nil)
	h.openTree(t)

	if cmd := h.send(t, key("q")); cmd != nil || !h.model.hives.InTree() {
		t.Fatalf("q outside hives-only left the tree:\n%s", h.view())
	}
	h.send(t, key("D"))
	if _, ok := h.model.hives.TreeDiscarding(); ok {
		t.Fatal("D with nothing staged asked to discard")
	}
	if !strings.Contains(h.view(), "nothing staged; nothing to discard") {
		t.Fatalf("D with nothing staged did not explain:\n%s", h.view())
	}
	h.send(t, key("k"))
	h.send(t, tea.KeyMsg{Type: tea.KeyUp})
	if n := h.treeFocus(t); n.Kind != panes.HiveTreeHub {
		t.Fatalf("k above the first row moved focus to %+v", n)
	}

	ref, _ := treeReference(t, nil)
	h.send(t, key("j"))
	h.send(t, key("f"))
	if err := ref.SetFollow("alpha", true); err != nil {
		t.Fatal(err)
	}
	if n := h.treeFocus(t); n.Follow != "following" || !strings.Contains(h.view(), "Staged. Nothing is written until A apply.") {
		t.Fatalf("f did not stage follow: %+v\n%s", n, h.view())
	}

	h.send(t, key("l"))
	h.send(t, key("j"))
	h.send(t, key("x"))
	if err := ref.SetExcluded("alpha", hivectltest.ContributionFixtureRepoA, true); err != nil {
		t.Fatal(err)
	}
	n := h.treeFocus(t)
	if !n.Excluded || n.ExclusionSource != "excluded by alpha" || n.Explanation != "Not selected: an exclusion vetoes it on every subscription" {
		t.Fatalf("x did not render the exclusion: %+v", n)
	}
	if !strings.Contains(h.view(), "Excluded") {
		t.Fatalf("Excluded badge missing:\n%s", h.view())
	}

	h.send(t, key("h"))
	if n := h.treeFocus(t); n.Subscription != "alpha" || n.Kind != panes.HiveTreeHive {
		t.Fatalf("h did not collapse back to alpha: %+v", n)
	}
	h.send(t, key("e"))
	if err := ref.SetHiveEnabled("alpha", false); err != nil {
		t.Fatal(err)
	}
	if n := h.treeFocus(t); n.Enabled || !strings.HasPrefix(n.Explanation, "Disabled; saved scope retained.") {
		t.Fatalf("e did not disable alpha: %+v", n)
	}

	if got, want := h.model.hivesTree.edits.Diff(), ref.Diff(); !reflect.DeepEqual(got, want) {
		t.Fatalf("TUI diff = %+v\nshared diff = %+v", got, want)
	}
	staged := h.model.hives.TreeStaged()
	if staged == 0 {
		t.Fatalf("nothing staged:\n%s", h.view())
	}

	h.send(t, key("A"))
	if !h.model.hives.TreeReviewing() {
		t.Fatalf("A did not open the review:\n%s", h.view())
	}
	h.send(t, treeEsc)
	if h.model.hives.TreeReviewing() || h.model.hives.TreeStaged() != staged {
		t.Fatalf("esc from the review lost staged edits:\n%s", h.view())
	}
}

func TestHivesTreeSearchEditsAndEscCloses(t *testing.T) {
	h := treeHarness(t, nil)
	h.openTree(t)
	h.send(t, key("/"))
	for _, r := range "org/ax" {
		h.send(t, key(string(r)))
	}
	h.send(t, tea.KeyMsg{Type: tea.KeyBackspace})
	h.send(t, treeSpace)
	h.send(t, treeEnter)
	view := h.view()
	if !strings.Contains(view, "org/a") || strings.Contains(view, "org/b") {
		t.Fatalf("backspace and space did not edit the search:\n%s", view)
	}

	cmd := h.send(t, treeEsc)
	if h.model.hives == nil || h.model.hives.InTree() || h.model.hivesTree != nil {
		t.Fatalf("esc with nothing staged did not close the tree:\n%s", h.view())
	}
	if cmd == nil {
		t.Fatal("closing the tree did not reload the hive list")
	}
	h.run(t, cmd)
	if h.model.hives == nil || h.model.hives.InTree() {
		t.Fatalf("hive list did not come back:\n%s", h.view())
	}
}

func TestHivesTreeDirectHubRendersWithoutProvenance(t *testing.T) {
	h := treeHarness(t, func(set *hivectl.ProfileSet) {
		set.Profiles = append(set.Profiles, hivectl.Profile{
			Name: "gamma", Hub: "wss://gamma.example/contribute", RegistrationToken: "gamma-secret",
			Selection: &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected},
		})
	})
	h.openTree(t)
	var direct panes.HiveTreeNode
	for _, n := range hivesTreeView(h.model.hivesTree).Nodes {
		if n.Kind == panes.HiveTreeHub && n.Hub == "direct" {
			direct = n
		}
	}
	if !direct.Direct || direct.Follow != "n/a" || direct.Provenance != "not enrolled through a hub" ||
		!strings.HasPrefix(direct.Identity, "direct — ") || direct.Subscriptions != 1 {
		t.Fatalf("direct hub node = %+v", direct)
	}
}

func TestHivesTreeOpenReportsStoreErrors(t *testing.T) {
	if _, err := hivesProfileStore(hivesEnv{}); !errors.Is(err, errNoHivesStore) {
		t.Fatalf("nil store err = %v", err)
	}
	if _, err := hivesProfileStore(hivesEnv{store: &stubHivesStore{}}); !errors.Is(err, errHivesTreeStore) {
		t.Fatalf("stub store err = %v", err)
	}

	empty := hivectl.NewProfileStore(t.TempDir())
	set, err := hivesReviewSet(empty)
	if err != nil || set == nil || set.Version != hivectl.ProfilesVersion || len(set.Profiles) != 0 {
		t.Fatalf("empty store: set = %+v, err = %v", set, err)
	}

	broken := hivectl.NewProfileStore(t.TempDir())
	if err := os.WriteFile(broken.Path(), []byte("version: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := hivesReviewSet(broken); err == nil || err.Error() != "cannot read contribution profiles" {
		t.Fatalf("broken store err = %v", err)
	}

	h := treeHarness(t, nil)
	h.openTree(t)
	h.send(t, hivesTreeLoadedMsg{overlayID: h.model.hivesID, err: errors.New("review unavailable")})
	if !strings.Contains(h.view(), "review unavailable") {
		t.Fatalf("tree read error not shown:\n%s", h.view())
	}
	h.send(t, hivesTreeLoadedMsg{overlayID: h.model.hivesID + 1, err: errors.New("stale overlay")})
	if strings.Contains(h.view(), "stale overlay") {
		t.Fatalf("a message for another overlay was applied:\n%s", h.view())
	}
}
