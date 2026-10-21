package tui

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/hivecommons/hive/pkg/tui/panes"
)

// The contribution tree's app-side tests (RFC #13153, TUI surface). They drive
// the real profile store over a temporary directory and inject discovery, so
// every assertion about Apply is about what profiles.yml says afterwards.

const (
	treeRepoA    = "github/github.com/org/a"
	treeRepoB    = "github/github.com/org/b"
	treeHubOne   = "https://one.example"
	treeHubTwo   = "https://two.example"
	treeOrgAlpha = "https://alpha.example"
	treeOrgBeta  = "https://beta.example"
)

func treeFixtureSet() *hivectl.ProfileSet {
	return &hivectl.ProfileSet{Version: hivectl.ProfilesVersion, Active: "alpha", Profiles: []hivectl.Profile{
		{Name: "alpha", Hub: "wss://alpha.example/contribute?token=endpoint-secret", RegistrationToken: "registration-secret", Selection: &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected}},
		{Name: "beta", Hub: "wss://beta.example/contribute", RegistrationToken: "other-secret", Selection: &hivectl.RepositorySelection{Mode: hivectl.SelectionSelected}},
	}, Hubs: []hivectl.HubEntry{
		{Name: "one", URL: treeHubOne, Follow: hivectl.HubFollowCurrent},
		{Name: "two", URL: treeHubTwo},
	}}
}

type treeHarness struct {
	*hivesHarness
	hubErr    atomic.Pointer[error]
	repoErr   atomic.Pointer[error]
	signalErr atomic.Pointer[error]
	applyErr  atomic.Pointer[error]
	hubCalls  atomic.Int64
	repoCalls atomic.Int64
}

func newTreeHarness(t *testing.T, set *hivectl.ProfileSet) *treeHarness {
	t.Helper()
	h := &treeHarness{hivesHarness: newHivesHarness(t, set)}
	h.signal = hivectl.RelaySwitchResult{Running: true, Target: "pid 123"}
	h.model.hivesEnv.signalRelay = func(context.Context) (hivectl.RelaySwitchResult, error) {
		h.signals.Add(1)
		if p := h.signalErr.Load(); p != nil {
			return hivectl.RelaySwitchResult{}, *p
		}
		return h.signal, nil
	}
	h.model.hivesEnv.discoverHubHives = func(_ context.Context, hubURL string) ([]hivectl.HubHive, error) {
		h.hubCalls.Add(1)
		if p := h.hubErr.Load(); p != nil && hubURL == treeHubTwo {
			return nil, *p
		}
		if hubURL == treeHubOne {
			return []hivectl.HubHive{{ID: treeOrgAlpha, HubURL: treeOrgAlpha}, {ID: treeOrgBeta, HubURL: treeOrgBeta}}, nil
		}
		return []hivectl.HubHive{{ID: treeOrgAlpha, HubURL: treeOrgAlpha}}, nil
	}
	h.model.hivesEnv.discoverRepositories = func(_ context.Context, hub string) ([]string, error) {
		h.repoCalls.Add(1)
		if p := h.repoErr.Load(); p != nil && strings.Contains(hub, "beta") {
			return nil, *p
		}
		return []string{treeRepoA, treeRepoB}, nil
	}
	store := h.store
	h.model.hivesEnv.applyStaged = func(ctx context.Context, edits *hivectl.StagedEdits) (hivectl.ApplyReceipt, error) {
		if p := h.applyErr.Load(); p != nil {
			return hivectl.ApplyReceipt{Saved: hivectl.StageNotSaved}, *p
		}
		return edits.Apply(ctx, store)
	}
	return h
}

// openTree opens the overlay, presses t and settles discovery.
func (h *treeHarness) openTree(t *testing.T) {
	t.Helper()
	h.open(t)
	h.run(t, h.send(t, key("t")))
	if h.model.hives == nil || !h.model.hives.InTree() {
		t.Fatalf("t did not open the tree:\n%s", h.view())
	}
}

func (h *treeHarness) press(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case " ":
			msg = tea.KeyMsg{Type: tea.KeySpace}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		case "left":
			msg = tea.KeyMsg{Type: tea.KeyLeft}
		case "right":
			msg = tea.KeyMsg{Type: tea.KeyRight}
		default:
			msg = key(k)
		}
		h.run(t, h.send(t, msg))
	}
}

// moveTo walks the cursor to the row with the given key.
func (h *treeHarness) moveTo(t *testing.T, rowKey string) panes.HiveTreeRow {
	t.Helper()
	h.press(t, "g")
	for i := 0; i < 64; i++ {
		row, ok := h.model.hives.TreeSelected()
		if ok && row.Key == rowKey {
			return row
		}
		h.press(t, "j")
	}
	var keys []string
	for _, r := range h.model.hives.TreeVisible() {
		keys = append(keys, r.Key)
	}
	t.Fatalf("row %q not visible; visible: %v", rowKey, keys)
	return panes.HiveTreeRow{}
}

func treeSavedSelected(set *hivectl.ProfileSet, name, repo string) bool {
	for _, p := range set.Profiles {
		if p.Name == name && p.Selection != nil {
			for _, r := range append(append([]string{}, p.Selection.Repositories...), p.Selection.Include...) {
				if r == repo {
					return true
				}
			}
		}
	}
	return false
}

const (
	treeAlphaKey  = "hub:https://one.example/sub:alpha"
	treeBetaKey   = "hub:https://one.example/sub:beta"
	treeHubOneKey = "hub:https://one.example"
)

func TestTreeOpensWithSharedBranchesAndUnavailableRuntime(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	h.openTree(t)
	view := h.view()
	for _, want := range []string{"CONTRIBUTION TREE", "live: unavailable — no profile store", "durable: unavailable — no profile store", "one", "two", "alpha", "beta", "Shared across 2 hubs", "No staged changes.", "[ ]", "Future Hives: current"} {
		if !strings.Contains(view, want) {
			t.Errorf("tree view lacks %q:\n%s", want, view)
		}
	}
	for _, secret := range []string{"registration-secret", "other-secret", "endpoint-secret"} {
		if strings.Contains(view, secret) {
			t.Fatalf("tree view leaked %q", secret)
		}
	}
	if h.hubCalls.Load() != 2 || h.repoCalls.Load() != 2 {
		t.Fatalf("discovery calls = %d hubs, %d repos", h.hubCalls.Load(), h.repoCalls.Load())
	}
	if !strings.Contains(view, "space select") {
		t.Fatalf("tree footer missing:\n%s", view)
	}
}

func TestTreeStagesWithoutWritingAndDiscards(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	h.openTree(t)
	before := h.profiles(t).Revision()
	h.moveTo(t, treeAlphaKey+"/repo:"+treeRepoA)
	h.press(t, " ")
	if !h.model.hives.TreeStaged() {
		t.Fatalf("space did not stage:\n%s", h.view())
	}
	view := h.view()
	if !strings.Contains(view, "Staged: +1 repositories selected") || !strings.Contains(view, "staged: select "+treeRepoA) || !strings.Contains(view, "[x]") {
		t.Fatalf("staging bar/note missing:\n%s", view)
	}
	if h.profiles(t).Revision() != before {
		t.Fatal("staging wrote profiles.yml")
	}
	if h.signals.Load() != 0 {
		t.Fatal("staging signaled the relay")
	}
	// Changed filter keeps the row and its ancestors.
	// (alpha is shared across both hubs, so both branches change.)
	h.press(t, "3")
	visible := h.model.hives.TreeVisible()
	if len(visible) != 6 {
		t.Fatalf("changed filter visible = %d rows", len(visible))
	}
	for _, row := range visible {
		if !row.Changed || (row.Kind == panes.HiveTreeRepo && row.ID != treeRepoA) {
			t.Fatalf("changed filter kept %+v", row)
		}
	}
	h.press(t, "1", "U")
	if h.model.hives.TreeStaged() || !strings.Contains(h.view(), "staged edits discarded") {
		t.Fatalf("U did not discard:\n%s", h.view())
	}
	// Refresh is allowed once nothing is pending and re-runs discovery.
	h.press(t, "r")
	if h.hubCalls.Load() != 4 {
		t.Fatalf("refresh did not rediscover: %d hub calls", h.hubCalls.Load())
	}
}

func TestTreeBranchActionsAndHubEnable(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	h.openTree(t)
	alpha := h.moveTo(t, treeAlphaKey)
	if alpha.Following || alpha.Check != panes.HiveTreeCheckOff {
		t.Fatalf("alpha row = %+v", alpha)
	}
	h.press(t, " ", "f")
	alpha, _ = h.model.hives.TreeSelected()
	if alpha.Check != panes.HiveTreeCheckAll || !alpha.Following || !alpha.Changed {
		t.Fatalf("after space+f alpha = %+v", alpha)
	}
	// Deselect current while following warns that following may repopulate.
	h.press(t, " ")
	if !strings.Contains(h.view(), "following stays on") {
		t.Fatalf("deselect-current note missing:\n%s", h.view())
	}
	// Exclude a repository at beta; the exclusion is distinct from deselection.
	h.moveTo(t, treeBetaKey+"/repo:"+treeRepoB)
	h.press(t, "x")
	repo, _ := h.model.hives.TreeSelected()
	if !repo.Excluded || !strings.Contains(strings.Join(repo.Badges, " "), "Excluded by beta") {
		t.Fatalf("exclude row = %+v", repo)
	}
	if lines := strings.Join(h.model.hives.TreeSummaryLines(), "\n"); !strings.Contains(lines, "exclude "+treeRepoB+" at beta") {
		t.Fatalf("diff lacks exclusion:\n%s", lines)
	}
	// Disable beta then the whole hub one.
	h.moveTo(t, treeBetaKey)
	h.press(t, "e")
	if row, _ := h.model.hives.TreeSelected(); !row.Disabled || !strings.Contains(strings.Join(row.Badges, " "), "Disabled — scope retained") {
		t.Fatalf("disable row = %+v", row)
	}
	h.moveTo(t, treeHubOneKey)
	h.press(t, "e")
	if !strings.Contains(h.view(), "disable subscriptions under one") {
		t.Fatalf("hub disable note missing:\n%s", h.view())
	}
	// Wrong-row keys are refused with a hint, not silently.
	h.press(t, "x")
	if !strings.Contains(h.view(), "does not apply") {
		t.Fatalf("x on hub not refused:\n%s", h.view())
	}
	h.press(t, "R")
	if !strings.Contains(h.view(), "legacy unrestricted hive") {
		t.Fatalf("R on hub not refused:\n%s", h.view())
	}
}

func TestTreeApplyShowsDiffThenTruthfulReceipt(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	h.openTree(t)
	h.moveTo(t, treeAlphaKey)
	h.press(t, " ")
	h.press(t, "A")
	if !h.model.hives.TreeConfirming() {
		t.Fatalf("A did not open the confirmation:\n%s", h.view())
	}
	view := h.view()
	if !strings.Contains(view, "Apply staged changes?") || !strings.Contains(view, "+2 repositories selected") || !strings.Contains(view, "shared branch alpha changes in all of") || !strings.Contains(view, "enter apply") {
		t.Fatalf("confirmation lacks the diff:\n%s", view)
	}
	h.press(t, "esc")
	if h.model.hives.TreeConfirming() || !h.model.hives.TreeStaged() {
		t.Fatal("esc on the confirmation did not keep the staged edits")
	}
	if h.profiles(t).Revision() == "" {
		t.Fatal("fixture revision empty")
	}
	h.press(t, "A", "enter")
	view = h.view()
	if !strings.Contains(view, "saved: "+hivectl.StageSaved) || !strings.Contains(view, "relay reload: notified") || !strings.Contains(view, "acknowledged: pending") || !strings.Contains(view, "relay adoption pending") {
		t.Fatalf("receipt not truthful:\n%s", view)
	}
	if strings.Contains(view, "live now") {
		t.Fatal("receipt claimed liveness")
	}
	set := h.profiles(t)
	if !treeSavedSelected(set, "alpha", treeRepoA) || !treeSavedSelected(set, "alpha", treeRepoB) {
		t.Fatalf("apply did not save the selection: %+v", set.Profiles[0].Selection)
	}
	if h.signals.Load() != 1 {
		t.Fatalf("relay signals = %d", h.signals.Load())
	}
	if !h.model.hives.InTree() || h.model.hives.TreeStaged() {
		t.Fatalf("session did not reopen clean after apply")
	}
	if !strings.Contains(h.env(t), "org/a") {
		t.Fatalf("projection lacks the selection:\n%s", h.env(t))
	}
	// Apply with nothing staged is a no-op note.
	h.press(t, "A")
	if !strings.Contains(h.view(), "nothing is staged") {
		t.Fatalf("empty apply note missing:\n%s", h.view())
	}
}

func TestTreeApplyReceiptsNoManagedRelayAndReloadFailed(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	h.signal = hivectl.RelaySwitchResult{}
	h.openTree(t)
	h.moveTo(t, treeAlphaKey+"/repo:"+treeRepoA)
	h.press(t, " ", "A", "enter")
	if v := h.view(); !strings.Contains(v, "relay reload: "+hivectl.StageReloadNoManaged) {
		t.Fatalf("no-managed-relay receipt missing:\n%s", v)
	}
	if !treeSavedSelected(h.profiles(t), "alpha", treeRepoA) {
		t.Fatal("first apply did not save")
	}
	failure := errors.New("signal failed")
	h.signalErr.Store(&failure)
	h.moveTo(t, treeAlphaKey+"/repo:"+treeRepoB)
	h.press(t, " ")
	if !h.model.hives.TreeStaged() {
		t.Fatalf("second edit not staged:\n%s", h.view())
	}
	h.press(t, "A", "enter")
	v := h.view()
	if !strings.Contains(v, "relay reload: "+hivectl.StageReloadFailed) || !strings.Contains(v, "governs its next start") {
		t.Fatalf("reload-failed receipt missing:\n%s", v)
	}
	if !treeSavedSelected(h.profiles(t), "alpha", treeRepoB) {
		t.Fatal("reload failure lost the save")
	}
}

func TestTreeStaleApplyReopensWithoutSaving(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	h.openTree(t)
	h.moveTo(t, treeAlphaKey+"/repo:"+treeRepoA)
	h.press(t, " ")
	// Another surface writes in between.
	other, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := other.SetDisabled("beta", true); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Commit(other); err != nil {
		t.Fatal(err)
	}
	h.press(t, "A", "enter")
	v := h.view()
	if !strings.Contains(v, "nothing was saved") || !strings.Contains(v, "another hivectl, TUI or Commons web session") {
		t.Fatalf("stale note missing:\n%s", v)
	}
	set := h.profiles(t)
	if treeSavedSelected(set, "alpha", treeRepoA) {
		t.Fatal("stale review was merged")
	}
	if h.model.hives.TreeStaged() {
		t.Fatal("stale session kept its edits")
	}
	if row := h.moveTo(t, treeBetaKey); !row.Disabled {
		t.Fatalf("reopened tree does not show the concurrent write: %+v", row)
	}
}

func TestTreeProjectionFailureIsNeverShownAsApplied(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	h.openTree(t)
	h.moveTo(t, treeAlphaKey+"/repo:"+treeRepoA)
	h.press(t, " ")
	if err := os.Remove(h.store.EnvPath()); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.store.EnvPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	h.press(t, "A", "enter")
	v := h.view()
	if !strings.Contains(v, "saved: "+hivectl.StageProfilesSaved) || !strings.Contains(v, "press A to retry") || strings.Contains(v, "✓") {
		t.Fatalf("projection failure misreported:\n%s", v)
	}
	if h.signals.Load() != 0 {
		t.Fatal("relay signaled while the projection is stale")
	}
	h.press(t, "x")
	if !strings.Contains(h.view(), "projection failed") {
		t.Fatalf("edit during stale projection not refused:\n%s", h.view())
	}
	if err := os.Remove(h.store.EnvPath()); err != nil {
		t.Fatal(err)
	}
	h.press(t, "A", "enter")
	if v := h.view(); !strings.Contains(v, "saved: "+hivectl.StageSaved) {
		t.Fatalf("retry did not repair:\n%s", v)
	}
	// A generic apply failure stays on the tree with the error.
	h.moveTo(t, treeAlphaKey+"/repo:"+treeRepoB)
	h.press(t, " ")
	boom := errors.New("disk on fire")
	h.applyErr.Store(&boom)
	h.press(t, "A", "enter")
	if v := h.view(); !strings.Contains(v, "disk on fire") || !strings.Contains(v, "saved: "+hivectl.StageNotSaved) {
		t.Fatalf("generic failure:\n%s", v)
	}
}

func TestTreeDiscoveryOutageIsNeverEmptyOrWildcard(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	down := errors.New("hub unavailable: https://beta.example/contribute?token=endpoint-secret")
	h.repoErr.Store(&down)
	hubDown := &hivectl.HubUnavailableError{Hub: treeHubTwo, Reason: "registry 503", Err: errors.New("503")}
	var hubErr error = hubDown
	h.hubErr.Store(&hubErr)
	h.openTree(t)
	view := h.view()
	if strings.Contains(view, "endpoint-secret") {
		t.Fatal("discovery reason leaked the URL")
	}
	beta := h.moveTo(t, treeBetaKey)
	if beta.Check != panes.HiveTreeCheckDisabled || beta.CanSelect || !beta.NeedsReview {
		t.Fatalf("unavailable beta = %+v", beta)
	}
	if joined := strings.Join(beta.Badges, " · "); !strings.Contains(joined, "Discovery unavailable — ") || strings.Contains(joined, "endpoint-secret") {
		t.Fatalf("unavailable badge missing: %+v", beta.Badges)
	}
	h.press(t, " ")
	if !strings.Contains(h.view(), "nothing to check") {
		t.Fatalf("vacuous check not refused:\n%s", h.view())
	}
	h.moveTo(t, treeBetaKey+"/none")
	if row, _ := h.model.hives.TreeSelected(); row.Kind != panes.HiveTreePlaceholder || !strings.Contains(row.Label, "Membership unknown") {
		t.Fatalf("placeholder = %+v", row)
	}
	h.press(t, " ")
	if !strings.Contains(h.view(), "does not apply") {
		t.Fatalf("placeholder edit not refused:\n%s", h.view())
	}
	// Bulk inclusion at the hub is refused while a branch is incomplete.
	h.moveTo(t, treeHubOneKey)
	h.press(t, " ")
	if !strings.Contains(h.view(), "nothing to check") && !strings.Contains(h.view(), "membership incomplete") {
		t.Fatalf("hub bulk include over an incomplete branch not refused:\n%s", h.view())
	}
	// The needs-review filter surfaces the unavailable branch.
	h.press(t, "2")
	seen := map[string]bool{}
	for _, row := range h.model.hives.TreeVisible() {
		seen[row.Key] = true
		if !row.NeedsReview && row.Kind == panes.HiveTreeRepo {
			t.Fatalf("needs-review filter kept a reviewed repository: %+v", row)
		}
	}
	if !seen[treeBetaKey] || !seen[treeBetaKey+"/none"] || !seen["hub:"+treeHubTwo] {
		t.Fatalf("needs-review filter hid the unavailable branches: %v", seen)
	}
	if h.profiles(t).Profiles[1].Selection.Mode != hivectl.SelectionSelected || len(h.profiles(t).Profiles[1].Selection.Repositories) != 0 {
		t.Fatal("outage changed the saved selection")
	}
}

func TestTreeLegacyProfileNeedsReviewAndRestrict(t *testing.T) {
	set := treeFixtureSet()
	set.Profiles[1].Selection = nil
	h := newTreeHarness(t, set)
	h.openTree(t)
	beta := h.moveTo(t, treeBetaKey)
	if !beta.Legacy || beta.Check != panes.HiveTreeCheckDisabled || !strings.Contains(strings.Join(beta.Badges, " "), "Unrestricted — legacy") {
		t.Fatalf("legacy beta = %+v", beta)
	}
	h.press(t, " ")
	if !strings.Contains(h.view(), "nothing to check") {
		t.Fatalf("legacy select not refused:\n%s", h.view())
	}
	h.press(t, "l", "j")
	if row, _ := h.model.hives.TreeSelected(); row.Kind != panes.HiveTreeRepo || !row.Legacy || row.Check != panes.HiveTreeCheckDisabled || row.CanSelect {
		t.Fatalf("legacy repository row = %+v", row)
	}
	h.press(t, " ")
	if !strings.Contains(h.view(), "review and restrict first") {
		t.Fatalf("legacy repository select not refused:\n%s", h.view())
	}
	h.moveTo(t, treeBetaKey)
	h.press(t, "R")
	v := h.view()
	if !strings.Contains(v, "legacy conversion for beta") || !strings.Contains(v, "legacy conversion: beta") {
		t.Fatalf("review-restrict not staged:\n%s", v)
	}
	if h.profiles(t).Profiles[1].Selection != nil {
		t.Fatal("legacy profile migrated before Apply")
	}
	h.press(t, "A", "enter")
	after := h.profiles(t).Profiles[1].Selection
	if after == nil || after.Mode != hivectl.SelectionAllCurrent || len(after.MembershipSnapshot) != 2 || after.FollowNewRepositories {
		t.Fatalf("review-restrict apply = %+v", after)
	}
}

func TestTreeNavigationSearchHelpAndLeave(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	h.openTree(t)
	// Collapse hub one: its descendants disappear; → reopens; → again enters.
	h.moveTo(t, treeHubOneKey)
	h.press(t, "h")
	if visible := h.model.hives.TreeVisible(); len(visible) != 5 {
		t.Fatalf("collapsed visible = %d", len(visible))
	}
	h.press(t, "right", "right")
	if row, _ := h.model.hives.TreeSelected(); row.Key != treeAlphaKey {
		t.Fatalf("→ → landed on %q", row.Key)
	}
	h.press(t, "left")
	if row, _ := h.model.hives.TreeSelected(); row.Key != treeAlphaKey || !strings.Contains(h.view(), "▸") {
		t.Fatalf("← did not collapse alpha: %q", row.Key)
	}
	h.press(t, "left")
	if row, _ := h.model.hives.TreeSelected(); row.Key != treeHubOneKey {
		t.Fatalf("← on a collapsed row did not move to its parent: %q", row.Key)
	}
	h.press(t, "G")
	if row, _ := h.model.hives.TreeSelected(); row.Level != 3 {
		t.Fatalf("G did not reach the last row: %+v", row)
	}
	// Search keeps ancestry; esc clears.
	h.press(t, "/")
	if !h.model.hives.Typing() || !h.model.hives.TreeSearching() {
		t.Fatal("/ did not start searching")
	}
	h.press(t, "o", "r", "g", "/", "b")
	visible := h.model.hives.TreeVisible()
	for _, row := range visible {
		if row.Kind == panes.HiveTreeRepo && row.ID != treeRepoB {
			t.Fatalf("search leaked %q", row.ID)
		}
	}
	if len(visible) == 0 || !strings.Contains(h.view(), "search: org/b") {
		t.Fatalf("search view:\n%s", h.view())
	}
	h.press(t, "backspace", "enter")
	if h.model.hives.TreeSearching() || !strings.Contains(h.view(), "search: org/") {
		t.Fatalf("enter did not keep the query:\n%s", h.view())
	}
	h.press(t, "/", "esc")
	if strings.Contains(h.view(), "search:") {
		t.Fatalf("esc did not clear the query:\n%s", h.view())
	}
	// Letters typed into the search never reach the list bindings.
	h.press(t, "/", "a", "d", "q", "esc")
	if h.model.hives == nil || !h.model.hives.InTree() {
		t.Fatal("search letters leaked to list bindings")
	}
	h.press(t, "?")
	if !strings.Contains(h.view(), "Contribution tree keys") || !strings.Contains(h.view(), "? close help") {
		t.Fatalf("help missing:\n%s", h.view())
	}
	h.press(t, "?")
	// Leaving with staged edits warns first.
	h.moveTo(t, treeAlphaKey)
	h.press(t, "l")
	h.moveTo(t, treeAlphaKey+"/repo:"+treeRepoA)
	h.press(t, " ", "esc")
	if !h.model.hives.InTree() || !strings.Contains(h.view(), "staged edits would be lost") {
		t.Fatalf("first esc did not warn:\n%s", h.view())
	}
	h.press(t, "r")
	if !strings.Contains(h.view(), "before refreshing") {
		t.Fatalf("refresh with staged edits not refused:\n%s", h.view())
	}
	h.press(t, "esc")
	if h.model.hives.InTree() || h.model.hivesStage != nil {
		t.Fatal("second esc did not leave")
	}
	if !strings.Contains(h.view(), "t tree") {
		t.Fatalf("list footer lacks the tree key:\n%s", h.view())
	}
}

func TestTreeOpenFailuresAndStaleMessages(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	h.open(t)
	h.model.hivesEnv.store = nil
	h.run(t, h.send(t, key("t")))
	if !strings.Contains(h.view(), "cannot locate") {
		t.Fatalf("missing store not reported:\n%s", h.view())
	}
	h.press(t, "esc")
	h.model.hivesEnv.store = &stubHivesStore{loadErr: errors.New("corrupt profiles")}
	h.run(t, h.send(t, key("t")))
	if !strings.Contains(h.view(), "corrupt profiles") {
		t.Fatalf("load error not reported:\n%s", h.view())
	}
	// Messages for a superseded overlay are dropped.
	h.press(t, "esc")
	h.model.hivesEnv.store = h.store
	h.run(t, h.send(t, key("t")))
	stale := hivesTreeOpenedMsg{overlayID: h.model.hivesID + 1, err: errors.New("late")}
	h.run(t, h.send(t, stale))
	if strings.Contains(h.view(), "late") {
		t.Fatal("stale open message applied")
	}
	h.run(t, h.send(t, hivesTreeAppliedMsg{overlayID: h.model.hivesID + 1, err: errors.New("late apply")}))
	if strings.Contains(h.view(), "late apply") {
		t.Fatal("stale apply message applied")
	}
	// Keys during a pending apply are ignored.
	h.moveTo(t, treeAlphaKey+"/repo:"+treeRepoA)
	h.press(t, " ", "A")
	next, cmd := h.model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	h.model = next.(model)
	if !h.model.hives.Pending() {
		t.Fatal("apply not pending")
	}
	h.press(t, "esc")
	if !h.model.hives.Pending() {
		t.Fatal("esc during pending apply was accepted")
	}
	h.run(t, cmd)
	if h.model.hives.Pending() {
		t.Fatal("apply never settled")
	}
}

func TestTreeDiscoveryWithoutSourcesAndUnusableHubURL(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	h.model.hivesEnv.discoverHubHives = nil
	h.model.hivesEnv.discoverRepositories = nil
	h.openTree(t)
	var badges []string
	for _, row := range h.model.hives.TreeVisible() {
		badges = append(badges, row.Badges...)
	}
	if joined := strings.Join(badges, " · "); !strings.Contains(joined, "hub discovery not configured") || !strings.Contains(joined, "repository discovery not configured") {
		t.Fatalf("missing-source reasons absent: %+v", badges)
	}
	// A hub whose URL cannot become a Hive origin is omitted by the read model;
	// its discovery reason must still surface as a placeholder, never vanish.
	rows, _, err := hiveTreeRows(h.model.hivesStage, map[string]string{"odd": "not a usable origin"}, nil)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.Key == "orphan:odd" {
			found = true
			if row.Kind != panes.HiveTreePlaceholder || !row.NeedsReview || !strings.Contains(row.Label, "not a usable origin") {
				t.Fatalf("orphan row = %+v", row)
			}
		}
	}
	if !found {
		t.Fatalf("unusable hub vanished from the rows: %+v", rows)
	}
	for _, c := range []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "discovery timed out"},
		{context.Canceled, "discovery cancelled"},
		{errors.New("get https://x.example/?t=1: refused"), "discovery unavailable"},
		{errors.New("HTTP 503; selection unchanged"), "HTTP 503"},
	} {
		if got := hivesTreeDiscoveryReason(c.err); got != c.want {
			t.Errorf("reason(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestTreeReceiptWording(t *testing.T) {
	ok := hivectl.ApplyReceipt{Saved: hivectl.StageSaved, RelayReload: hivectl.StageReloadNotified, Acknowledged: "yes"}
	if got := hivesTreeReceipt(ok, nil); !strings.HasPrefix(got, "✓ applied;") {
		t.Fatalf("acknowledged receipt = %q", got)
	}
	unchanged := hivectl.ApplyReceipt{Saved: hivectl.StageUnchanged, RelayReload: hivectl.StageReloadNotRequested, Acknowledged: "unknown"}
	if got := hivesTreeReceipt(unchanged, nil); !strings.HasPrefix(got, "nothing to apply") {
		t.Fatalf("unchanged receipt = %q", got)
	}
	if got := hiveDash(""); got != "—" {
		t.Fatalf("hiveDash = %q", got)
	}
}

func TestTreeRuntimeOverlayLabelsRowsAndHeader(t *testing.T) {
	h := newTreeHarness(t, treeFixtureSet())
	calls := 0
	h.model.hivesEnv.treeRuntime = func() hivectl.TreeRuntime {
		calls++
		return hivectl.TreeRuntime{
			Live: hivectl.TreeLive{Available: true, PID: 5, AdoptedScopeVersion: 2, Acknowledged: "acknowledged",
				CurrentTask:   &hivectl.RelayCurrentTask{Hive: treeOrgAlpha, Repository: treeRepoA},
				Subscriptions: map[string]hivectl.TreeLiveSubscription{treeOrgAlpha: {Readiness: "connected", Acknowledged: "acknowledged"}}},
			Durable: hivectl.TreeDurable{Reason: "activity store not readable"},
		}
	}
	h.openTree(t)
	if calls == 0 {
		t.Fatal("runtime source was not consulted")
	}
	view := h.view()
	for _, want := range []string{"live: relay pid 5 · scope v2 acknowledged · working on " + treeRepoA, "durable: unavailable — activity store not readable"} {
		if !strings.Contains(view, want) {
			t.Errorf("tree view lacks %q:\n%s", want, view)
		}
	}
	alpha := h.moveTo(t, treeAlphaKey)
	detail := strings.Join(alpha.Detail, "\n")
	for _, want := range []string{"readiness: connected", "activity: working on " + treeRepoA, "history: unavailable"} {
		if !strings.Contains(detail, want) {
			t.Errorf("alpha detail lacks %q: %v", want, alpha.Detail)
		}
	}
	beta := h.moveTo(t, treeBetaKey)
	if !strings.Contains(strings.Join(beta.Detail, "\n"), "readiness: not adopted by relay") {
		t.Errorf("beta detail = %v", beta.Detail)
	}
	h.press(t, "l")
	repo, _ := h.model.hives.TreeSelected()
	if repo.Kind != panes.HiveTreeRepo || !strings.Contains(strings.Join(repo.Detail, "\n"), "activity: idle") {
		t.Errorf("beta repo detail = %+v", repo)
	}
	// A real store wires the default runtime reader; without a relay the
	// overlay is unavailable with a reason, never idle.
	env := defaultHivesEnv()
	if env.store == nil || env.treeRuntime == nil {
		t.Skip("no profile store in this environment")
	}
	t.Setenv("HIVE_RELAY_STATUS_FILE", "")
	rt := env.treeRuntime()
	if rt.Live.Available && rt.Live.PID == 0 {
		t.Fatalf("live = %+v", rt.Live)
	}
	if rt.Durable.Available || rt.Durable.Reason != hivectl.DurableActivityNotShipped {
		t.Fatalf("durable = %+v", rt.Durable)
	}
}
