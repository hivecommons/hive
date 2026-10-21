package tui

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/hivecommons/hive/pkg/hivectltest"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// The contribution tree's app-side tests (#13291). They start from the shared
// state-transition fixture of #13289 (hivectltest.ContributionStageFixture) and
// compare every diff and receipt with the shared staging layer driven directly,
// which is also what the Web tree's tests compare against — so the two surfaces
// produce identical diffs and receipts for the same edits.

var (
	treeEnter = tea.KeyMsg{Type: tea.KeyEnter}
	treeEsc   = tea.KeyMsg{Type: tea.KeyEsc}
	treeSpace = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	treeCtrlR = tea.KeyMsg{Type: tea.KeyCtrlR}
)

// treeDiscovery returns fresh fixture snapshots on every call.
func treeDiscovery(context.Context, *hivectl.ProfileSet) ([]hivectl.TreeHubSnapshot, map[string]hivectl.TreeRepositorySnapshot) {
	_, hubs, repos := hivectltest.ContributionStageFixture()
	return hubs, repos
}

func treeHarness(t *testing.T, edit func(*hivectl.ProfileSet)) *hivesHarness {
	t.Helper()
	set, _, _ := hivectltest.ContributionStageFixture()
	if edit != nil {
		edit(set)
	}
	h := newHivesHarness(t, set)
	h.model.hivesEnv.discoverTree = treeDiscovery
	return h
}

// openTree opens the overlay, presses t and settles the tree read.
func (h *hivesHarness) openTree(t *testing.T) {
	t.Helper()
	h.open(t)
	h.run(t, h.send(t, key("t")))
	if h.model.hives == nil || !h.model.hives.InTree() || h.model.hivesTree == nil {
		t.Fatalf("t did not open the contribution tree:\n%s", h.view())
	}
}

// reference opens the shared staging layer directly over an identical store,
// as a second surface would.
func treeReference(t *testing.T, edit func(*hivectl.ProfileSet)) (*hivectl.StagedEdits, *hivectl.ProfileStore) {
	t.Helper()
	set, hubs, repos := hivectltest.ContributionStageFixture()
	if edit != nil {
		edit(set)
	}
	store := hivectl.NewProfileStore(t.TempDir())
	if err := store.Save(set); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadForReview()
	if err != nil {
		t.Fatal(err)
	}
	edits, err := hivectl.NewStagedEdits(loaded, hubs, repos)
	if err != nil {
		t.Fatal(err)
	}
	return edits, store
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return data
}

func (h *hivesHarness) treeFocus(t *testing.T) panes.HiveTreeNode {
	t.Helper()
	n, ok := h.model.hives.TreeCurrent()
	if !ok {
		t.Fatal("no focused tree node")
	}
	return n
}

// focusRepoA moves from the first hub to repository a of alpha under it.
func (h *hivesHarness) focusRepoA(t *testing.T) {
	t.Helper()
	h.send(t, key("j"))
	if n := h.treeFocus(t); n.Subscription != "alpha" || n.Kind != panes.HiveTreeHive {
		t.Fatalf("focus = %+v, want subscription alpha", n)
	}
	h.send(t, key("l"))
	h.send(t, key("j"))
	if n := h.treeFocus(t); n.Repository != hivectltest.ContributionFixtureRepoA {
		t.Fatalf("focus = %+v, want repository a", n)
	}
}

func TestHivesTreeRendersSharedTreeWithoutCredentials(t *testing.T) {
	h := treeHarness(t, nil)
	h.openTree(t)
	view := h.view()
	for _, want := range []string{
		"Effective scope: Off: 0 of 4 repositories selected across 2 subscriptions",
		"Staged: none",
		"SEL", "NAME", "BADGES", "FOLLOW", "READINESS", "ACTIVITY",
		"one", "two", "alpha", "beta",
		"Shared across 2 hubs",
		"unavailable",
		"Identity: https://one.example",
		"Provenance: tracked hub one",
		"Exclusion source: none",
		"A apply",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("tree view lacks %q:\n%s", want, view)
		}
	}
	for _, secret := range []string{"endpoint-secret", "registration-secret", "other-secret"} {
		if strings.Contains(view, secret) {
			t.Fatalf("tree rendered credential %q:\n%s", secret, view)
		}
	}
	h.send(t, key("j"))
	view = h.view()
	for _, want := range []string{"Identity: https://alpha.example (subscription alpha)", "Provenance: attribution unavailable", "not following"} {
		if !strings.Contains(view, want) {
			t.Errorf("subscription detail lacks %q:\n%s", want, view)
		}
	}
}

func TestHivesTreeStagesWithoutWritingThenAppliesLikeTheSharedLayer(t *testing.T) {
	h := treeHarness(t, nil)
	h.openTree(t)
	profiles, env := readFile(t, h.store.Path()), readFile(t, h.store.EnvPath())

	h.focusRepoA(t)
	h.send(t, treeSpace)
	if got := h.model.hives.TreeStaged(); got != 1 {
		t.Fatalf("staged = %d, want 1:\n%s", got, h.view())
	}
	if n := h.treeFocus(t); n.Selection != "x" || n.Repository != hivectltest.ContributionFixtureRepoA {
		t.Fatalf("focus after staging = %+v", n)
	}
	if !bytes.Equal(profiles, readFile(t, h.store.Path())) || !bytes.Equal(env, readFile(t, h.store.EnvPath())) {
		t.Fatal("staging wrote to disk before Apply")
	}

	ref, refStore := treeReference(t, nil)
	if err := ref.SelectRepository("alpha", hivectltest.ContributionFixtureRepoA, true); err != nil {
		t.Fatal(err)
	}
	if got, want := h.model.hivesTree.edits.Diff(), ref.Diff(); !reflect.DeepEqual(got, want) {
		t.Fatalf("TUI diff = %+v\nshared diff = %+v", got, want)
	}

	h.send(t, key("A"))
	if !h.model.hives.TreeReviewing() {
		t.Fatalf("A did not open the review:\n%s", h.view())
	}
	view := h.view()
	lines := hivesTreeDiffLines(ref.Diff())
	if !strings.HasPrefix(lines[0], "+1 repositories selected") || !strings.Contains(view, "+1 repositories selected") {
		t.Errorf("review summary = %q:\n%s", lines[0], view)
	}
	for _, line := range lines[1:] {
		if !strings.Contains(view, line) {
			t.Errorf("review lacks %q:\n%s", line, view)
		}
	}
	if !bytes.Equal(profiles, readFile(t, h.store.Path())) {
		t.Fatal("opening the review wrote to disk")
	}

	cmd := h.send(t, treeEnter)
	if cmd == nil {
		t.Fatal("enter on the review did not apply")
	}
	applied, ok := cmd().(hivesTreeAppliedMsg)
	if !ok {
		t.Fatal("apply did not answer with a receipt")
	}
	want, err := ref.Apply(context.Background(), refStore)
	if (err == nil) != (applied.err == nil) || applied.receipt != want {
		t.Fatalf("TUI receipt = %+v (%v), shared receipt = %+v (%v)", applied.receipt, applied.err, want, err)
	}
	if want.Saved != hivectl.StageSaved {
		t.Fatalf("shared receipt = %+v", want)
	}
	h.run(t, h.send(t, applied))
	if !h.model.hives.TreeReceipt() {
		t.Fatalf("no receipt screen:\n%s", h.view())
	}
	view = h.view()
	for _, line := range hivesTreeReceiptLines(want) {
		if !strings.Contains(view, line) {
			t.Errorf("receipt lacks %q:\n%s", line, view)
		}
	}
	if !bytes.Equal(readFile(t, h.store.Path()), readFile(t, refStore.Path())) {
		t.Fatal("TUI Apply saved different profiles from the shared layer")
	}
	if h.model.hivesTree.staged() {
		t.Fatal("applied edits are still staged")
	}
	h.send(t, treeEnter)
	if h.model.hives.TreeReceipt() {
		t.Fatal("enter did not dismiss the receipt")
	}
}

func TestHivesTreeBulkSelectionShowsCountsFirst(t *testing.T) {
	h := treeHarness(t, nil)
	h.openTree(t)
	hub := h.treeFocus(t)
	if hub.Kind != panes.HiveTreeHub || hub.Short != "one" {
		t.Fatalf("first focus = %+v", hub)
	}
	h.send(t, treeSpace)
	if !h.model.hives.TreeConfirming() || h.model.hives.TreeStaged() != 0 {
		t.Fatalf("bulk selection staged before confirming:\n%s", h.view())
	}
	if view := h.view(); !strings.Contains(view, "Affects 2 subscriptions and 4 repositories.") {
		t.Fatalf("bulk prompt lacks affected counts:\n%s", view)
	}
	h.send(t, key("n"))
	if h.model.hives.TreeConfirming() || h.model.hives.TreeStaged() != 0 {
		t.Fatal("n did not cancel the bulk selection")
	}
	h.send(t, treeSpace)
	h.send(t, key("y"))
	ref, _ := treeReference(t, nil)
	if err := ref.SelectCurrent(hub.Hub, true, true); err != nil {
		t.Fatal(err)
	}
	if got, want := h.model.hivesTree.edits.Diff(), ref.Diff(); !reflect.DeepEqual(got, want) {
		t.Fatalf("TUI diff = %+v\nshared diff = %+v", got, want)
	}
}

func TestHivesTreeDiscardRestoresWithoutWriting(t *testing.T) {
	h := treeHarness(t, nil)
	h.openTree(t)
	profiles := readFile(t, h.store.Path())
	h.focusRepoA(t)
	h.send(t, treeSpace)
	h.send(t, key("D"))
	if _, ok := h.model.hives.TreeDiscarding(); !ok {
		t.Fatalf("D did not ask before discarding:\n%s", h.view())
	}
	if !strings.Contains(h.view(), "Discard 1 unapplied edit?") {
		t.Fatalf("discard prompt:\n%s", h.view())
	}
	h.send(t, treeEsc)
	if h.model.hives.TreeStaged() != 1 {
		t.Fatal("cancelling the discard lost the staged edit")
	}
	h.send(t, key("D"))
	h.run(t, h.send(t, key("y")))
	if h.model.hives.TreeStaged() != 0 || h.model.hivesTree == nil || h.model.hivesTree.staged() {
		t.Fatalf("discard left edits staged:\n%s", h.view())
	}
	if !bytes.Equal(profiles, readFile(t, h.store.Path())) {
		t.Fatal("discard wrote to disk")
	}
}

func TestHivesTreeRefreshKeepsFocusAndStagedEdits(t *testing.T) {
	h := treeHarness(t, nil)
	h.openTree(t)
	h.focusRepoA(t)
	h.send(t, treeSpace)
	before := h.treeFocus(t).Key
	session := h.model.hivesTree
	h.run(t, h.send(t, treeCtrlR))
	if h.model.hivesTree != session || h.model.hives.TreeStaged() != 1 {
		t.Fatalf("refresh dropped staged edits:\n%s", h.view())
	}
	if got := h.treeFocus(t).Key; got != before {
		t.Fatalf("refresh moved focus from %q to %q", before, got)
	}
	if h.model.hives.TreeStale() {
		t.Fatal("refresh of unchanged profiles marked the review stale")
	}
}

func TestHivesTreeEditsBlockedWhileRefreshOutstanding(t *testing.T) {
	h := treeHarness(t, nil)
	h.openTree(t)
	h.focusRepoA(t)
	cmd := h.send(t, treeCtrlR)
	if !h.model.hives.TreeLoading() {
		t.Fatal("refresh did not mark the tree loading")
	}
	h.send(t, treeSpace)
	if h.model.hives.TreeStaged() != 0 || (h.model.hivesTree != nil && h.model.hivesTree.staged()) {
		t.Fatal("edit staged while a refresh was outstanding")
	}
	h.run(t, cmd)
	if h.model.hives.TreeLoading() {
		t.Fatal("refresh left the tree loading")
	}
	if n := h.treeFocus(t); n.Repository != hivectltest.ContributionFixtureRepoA {
		t.Fatalf("focus = %+v, want repository a after refresh", n)
	}
	h.send(t, treeSpace)
	if h.model.hives.TreeStaged() != 1 {
		t.Fatal("could not stage after the refresh landed")
	}
}

func TestHivesTreeStaleRevisionForcesRenewedReview(t *testing.T) {
	h := treeHarness(t, nil)
	h.openTree(t)
	h.focusRepoA(t)
	h.send(t, treeSpace)

	other, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := other.SetDisabled("beta", true); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CommitExpecting(other, other.Revision()); err != nil {
		t.Fatal(err)
	}
	concurrent := readFile(t, h.store.Path())

	h.send(t, key("A"))
	h.run(t, h.send(t, treeEnter))
	if !h.model.hives.TreeStale() || !strings.Contains(h.view(), "Saved: "+hivectl.StageNotSaved) {
		t.Fatalf("stale Apply did not refuse:\n%s", h.view())
	}
	if !bytes.Equal(concurrent, readFile(t, h.store.Path())) {
		t.Fatal("stale Apply overwrote a concurrent change")
	}
	h.send(t, treeEsc)
	h.send(t, key("A"))
	if h.model.hives.TreeReviewing() {
		t.Fatal("a stale review reopened for Apply")
	}
	h.send(t, key("D"))
	h.run(t, h.send(t, key("y")))
	if h.model.hives.TreeStale() || h.model.hivesTree == nil || h.model.hivesTree.stale {
		t.Fatalf("discard did not start a renewed review:\n%s", h.view())
	}
	if !bytes.Equal(concurrent, readFile(t, h.store.Path())) {
		t.Fatal("renewed review wrote to disk")
	}
}

func TestHivesTreeLegacyReviewAndRestrict(t *testing.T) {
	legacy := func(set *hivectl.ProfileSet) { set.Profiles[0].Selection = nil }
	h := treeHarness(t, legacy)
	h.openTree(t)
	if !strings.Contains(h.view(), "Unrestricted — legacy") {
		t.Fatalf("legacy badge missing:\n%s", h.view())
	}
	h.send(t, key("j"))
	h.send(t, key("R"))
	if !h.model.hives.TreeConfirming() {
		t.Fatalf("R did not ask first:\n%s", h.view())
	}
	h.send(t, treeEnter)
	ref, _ := treeReference(t, legacy)
	if err := ref.ReviewAndRestrict("alpha"); err != nil {
		t.Fatal(err)
	}
	got := h.model.hivesTree.edits.Diff()
	if !reflect.DeepEqual(got, ref.Diff()) || len(got.LegacyConversions) != 1 {
		t.Fatalf("TUI diff = %+v\nshared diff = %+v", got, ref.Diff())
	}
}

func TestHivesTreeSearchKeepsAncestry(t *testing.T) {
	h := treeHarness(t, nil)
	h.openTree(t)
	h.send(t, key("/"))
	for _, r := range "org/b" {
		h.send(t, key(string(r)))
	}
	h.send(t, treeEnter)
	view := h.view()
	for _, want := range []string{"one", "alpha", "org/b", "Search: [org/b]"} {
		if !strings.Contains(view, want) {
			t.Errorf("filtered tree lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "org/a") {
		t.Fatalf("filter kept a non-matching repository:\n%s", view)
	}
	if h.model.hives.TreeStaged() != 0 {
		t.Fatal("typing a search staged an edit")
	}
}

func TestHivesTreeQuitWithUnappliedEditsAsksFirst(t *testing.T) {
	h := treeHarness(t, nil)
	m := newHivesOnlyModel()
	m.width, m.height = 110, 34
	m.hivesEnv = h.model.hivesEnv
	h.model = m
	h.run(t, h.model.Init())
	h.run(t, h.send(t, key("t")))
	h.focusRepoA(t)
	h.send(t, treeSpace)

	if cmd := h.send(t, key("q")); cmd != nil {
		t.Fatal("q with unapplied edits quit without asking")
	}
	if leave, ok := h.model.hives.TreeDiscarding(); !ok || leave != panes.HivesTreeQuit {
		t.Fatalf("q did not open the in-TUI discard confirmation:\n%s", h.view())
	}
	h.send(t, key("n"))
	if h.model.hives.TreeStaged() != 1 {
		t.Fatal("cancelling the quit lost the staged edit")
	}
	h.send(t, key("q"))
	cmd := h.send(t, key("y"))
	if cmd == nil {
		t.Fatal("accepting the discard did not quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("accepting the discard did not quit")
	}
}

func TestHivesTreeNarrowKeepsIdentityScopeAndApplyHint(t *testing.T) {
	h := treeHarness(t, nil)
	h.model.width = 60
	h.openTree(t)
	h.focusRepoA(t)
	h.send(t, treeSpace)
	view := h.view()
	for _, want := range []string{"Effective scope:", "Staged: 1 unapplied — A apply", "Identity: github/github.com/org/a", "[x]"} {
		if !strings.Contains(view, want) {
			t.Errorf("narrow tree lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "READINESS") {
		t.Errorf("narrow tree kept the wide columns:\n%s", view)
	}
}
