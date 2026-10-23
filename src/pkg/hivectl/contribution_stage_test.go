package hivectl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	stageRepoA  = "github/github.com/org/a"
	stageRepoB  = "github/github.com/org/b"
	stageHubOne = "https://one.example"
	stageHubTwo = "https://two.example"
)

// stageFixture is the shared fixture every tree surface's tests also use.
func stageFixture() (*ProfileSet, []TreeHubSnapshot, map[string]TreeRepositorySnapshot) {
	return stageFixtureData()
}

// stageFixtureData mirrors hivectltest.ContributionStageFixture, which cannot be
// imported here without an import cycle. Keep the two in sync: any edit here
// must be applied to internal/hivectltest/contribution_fixture.go too, since the
// surface tests compare against that copy; TestStageFixtureMatchesHivectltest
// fails when they diverge. It returns two scoped subscriptions ("alpha", shared
// across hubs one and two; "beta", on hub one only), both with complete
// discovery of repositories a and b and nothing selected. Each call returns
// fresh values the caller may modify.
func stageFixtureData() (*ProfileSet, []TreeHubSnapshot, map[string]TreeRepositorySnapshot) {
	set := &ProfileSet{Version: ProfilesVersion, Active: "alpha", Profiles: []Profile{
		{Name: "alpha", Hub: "wss://alpha.example/contribute?token=endpoint-secret", RegistrationToken: "registration-secret", Selection: &RepositorySelection{Mode: SelectionSelected}},
		{Name: "beta", Hub: "wss://beta.example/contribute", RegistrationToken: "other-secret", Selection: &RepositorySelection{Mode: SelectionSelected}},
	}, Hubs: []HubEntry{
		{Name: "one", URL: stageHubOne, Follow: HubFollowCurrentAndFutureRepos},
		{Name: "two", URL: stageHubTwo, Follow: HubFollowCurrent},
	}}
	hubs := []TreeHubSnapshot{
		{Name: "one", URL: stageHubOne, Discovery: TreeComplete, HiveOrigins: []string{"https://alpha.example", "https://beta.example"}},
		{Name: "two", URL: stageHubTwo, Discovery: TreeComplete, HiveOrigins: []string{"https://alpha.example"}},
	}
	repos := map[string]TreeRepositorySnapshot{
		"alpha": {Discovery: TreeComplete, Repositories: []string{stageRepoA, stageRepoB}},
		"beta":  {Discovery: TreeComplete, Repositories: []string{stageRepoA, stageRepoB}},
	}
	return set, hubs, repos
}

func mustStage(t *testing.T, set *ProfileSet, hubs []TreeHubSnapshot, repos map[string]TreeRepositorySnapshot) *StagedEdits {
	t.Helper()
	stage, err := NewStagedEdits(set, hubs, repos)
	if err != nil {
		t.Fatal(err)
	}
	return stage
}

func mustStageOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func stageSubscription(t *testing.T, stage *StagedEdits, name string) TreeSubscription {
	t.Helper()
	tree, err := stage.Tree()
	mustStageOK(t, err)
	for _, sub := range tree.Subscriptions {
		if sub.ID == name {
			return sub
		}
	}
	t.Fatalf("subscription %q missing", name)
	return TreeSubscription{}
}

func TestStagedSelectionTransitions(t *testing.T) {
	// Shared fixtures are UI-agnostic: both surfaces consume the same tree/diff.
	for _, tc := range []struct {
		name     string
		selected []string
		state    string
		count    int
	}{
		{"zero", nil, "off", 0},
		{"some", []string{stageRepoA}, "partial", 1},
		{"all", []string{stageRepoA, stageRepoB}, "all-current", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, hubs, repos := stageFixture()
			set.Profiles[0].Selection.Repositories = tc.selected
			stage := mustStage(t, set, hubs, repos)
			sub := stageSubscription(t, stage, "alpha")
			if sub.Selection.State != tc.state || sub.Selection.Selected != tc.count {
				t.Fatalf("selection = %+v", sub.Selection)
			}
			if !stage.Diff().Empty() {
				t.Fatal("opening staged edits changed consent")
			}
			mustStageOK(t, stage.SelectCurrent("alpha", false, true))
			sub = stageSubscription(t, stage, "alpha")
			if sub.Selection.State != "all-current" || sub.Selection.Selected != 2 {
				t.Fatalf("select current = %+v", sub.Selection)
			}
			mustStageOK(t, stage.SelectCurrent("alpha", false, false))
			sub = stageSubscription(t, stage, "alpha")
			if sub.Selection.State != "off" || sub.Selection.Selected != 0 {
				t.Fatalf("deselect current = %+v", sub.Selection)
			}
			if !reflect.DeepEqual(set.Profiles[0].Selection.Repositories, tc.selected) {
				t.Fatal("staging modified original consent")
			}
		})
	}
}

func TestStagedDeselectRetainsFollowAndExclusions(t *testing.T) {
	set, hubs, repos := stageFixture()
	set.Profiles[0].Selection = &RepositorySelection{Mode: SelectionAllCurrent, MembershipSnapshot: []string{stageRepoA, stageRepoB}, Include: []string{stageRepoB}, Exclude: []string{stageRepoB}, FollowNewRepositories: true, Sources: []string{"curated"}}
	stage := mustStage(t, set, hubs, repos)
	mustStageOK(t, stage.SelectCurrent("alpha", false, false))
	sub := stageSubscription(t, stage, "alpha")
	if sub.Selection.State != "off" || sub.Selection.Excluded != 1 || !sub.Follow {
		t.Fatalf("deselect current = %+v", sub)
	}
	if len(stage.Diff().Follow) != 0 {
		t.Fatal("deselect current stopped following")
	}
	p, _ := stage.current.Find("alpha")
	if len(p.Selection.Include) != 0 || len(p.Selection.Repositories) != 0 || len(p.Selection.MembershipSnapshot) != 0 || !reflect.DeepEqual(p.Selection.Sources, []string{"curated"}) {
		t.Fatal("deselect did not preserve just policy/provenance/exclusions")
	}
	mustStageOK(t, stage.SelectCurrent("alpha", false, true))
	sub = stageSubscription(t, stage, "alpha")
	if sub.Selection.State != "partial" || sub.Selection.Selected != 1 || sub.Selection.Excluded != 1 {
		t.Fatalf("parent inclusion erased exclusion: %+v", sub.Selection)
	}
	mustStageOK(t, stage.SetFollow("alpha", false))
	if changes := stage.Diff().Follow; len(changes) != 1 || changes[0].After {
		t.Fatalf("stop following = %+v", changes)
	}
	// The existing select-none operation deliberately has different semantics.
	mustStageOK(t, set.SetSelectionMode("alpha", "none", nil))
	if set.Profiles[0].Selection.FollowNewRepositories {
		t.Fatal("select none must stop following")
	}
}

func TestStagedClearExclusionDoesNotApproveOrRemoveInclusion(t *testing.T) {
	set, hubs, repos := stageFixture()
	set.Profiles[0].Selection.Include = []string{stageRepoA}
	set.Profiles[0].Selection.Exclude = []string{stageRepoA, stageRepoB}
	stage := mustStage(t, set, hubs, repos)
	mustStageOK(t, stage.SetExcluded("alpha", stageRepoA, false))
	mustStageOK(t, stage.SetExcluded("alpha", stageRepoB, false))
	sub := stageSubscription(t, stage, "alpha")
	if sub.Selection.Selected != 1 || sub.Repositories[0].ID != stageRepoA || !sub.Repositories[0].Selected || sub.Repositories[1].Selected {
		t.Fatalf("clearing exclusions changed inclusion: %+v", sub)
	}
	d := stage.Diff()
	if d.ClearedExclusionCount != 2 || d.SelectedCount != 1 {
		t.Fatalf("diff = %+v", d)
	}
	mustStageOK(t, stage.SelectRepository("alpha", stageRepoA, false))
	if stageSubscription(t, stage, "alpha").Selection.Selected != 0 {
		t.Fatal("individual deselect failed")
	}
	if len(stage.current.Profiles[0].Selection.Exclude) != 0 {
		t.Fatal("deselect created exclusion")
	}
}

func TestStagedHubDisablePreservesMembershipAndFollow(t *testing.T) {
	set, hubs, repos := stageFixture()
	for i := range set.Profiles {
		set.Profiles[i].Selection.Repositories = []string{stageRepoA}
		set.Profiles[i].Selection.FollowNewRepositories = true
	}
	set.Profiles[0].Selection.Exclude = []string{stageRepoB}
	stage := mustStage(t, set, hubs, repos)
	mustStageOK(t, stage.SetHubEnabled(stageHubOne, false))
	for _, name := range []string{"alpha", "beta"} {
		sub := stageSubscription(t, stage, name)
		if !sub.Disabled || !sub.Follow || sub.Selection.Selected != 1 {
			t.Fatalf("disabled membership = %+v", sub)
		}
	}
	d := stage.Diff()
	if len(d.Enabled) != 2 || len(d.Follow) != 0 || len(d.Membership) != 0 || len(d.HubFollow) != 1 || d.HubFollow[0].After != HubFollowOff {
		t.Fatalf("disable diff = %+v", d)
	}
	if len(d.SharedBranches) != 1 || !reflect.DeepEqual(d.SharedBranches[0].Hubs, []string{stageHubOne, stageHubTwo}) {
		t.Fatalf("shared effect = %+v", d.SharedBranches)
	}
	if stage.current.Hubs[1].EffectiveFollow() != HubFollowCurrent {
		t.Fatal("disabled unrelated hub enrollment")
	}
	mustStageOK(t, stage.SetHubEnabled(stageHubOne, true))
	if stageSubscription(t, stage, "alpha").Disabled || stage.current.Hubs[0].EffectiveFollow() != HubFollowOff {
		t.Fatal("enable lost membership or resumed future enrollment")
	}
	mustStageOK(t, stage.Discard())
	if !stage.Diff().Empty() || stage.current.Hubs[0].EffectiveFollow() != HubFollowCurrentAndFutureRepos {
		t.Fatal("discard failed")
	}
}

func TestStagedCrossHiveExclusionDisclosure(t *testing.T) {
	set, hubs, repos := stageFixture()
	for i := range set.Profiles {
		set.Profiles[i].Selection.Repositories = []string{stageRepoA}
	}
	// Disabled scoped memberships still participate in the global exclusion union.
	set.Profiles[1].Disabled = true
	set.Profiles = append(set.Profiles,
		Profile{Name: "legacy", Hub: "wss://legacy.example", RegistrationToken: "legacy-secret"},
		Profile{Name: "paused-legacy", Hub: "wss://paused.example", Disabled: true},
	)
	stage := mustStage(t, set, hubs, repos)
	mustStageOK(t, stage.SetExcluded("alpha", stageRepoA, true))
	d := stage.Diff()
	if d.ExcludedCount != 1 || d.DeselectedCount != 2 || len(d.Exclusions) != 1 {
		t.Fatalf("exclusion diff = %+v", d)
	}
	e := d.Exclusions[0]
	if !reflect.DeepEqual(e.ScopedSubscriptions, []string{"alpha", "beta"}) || !reflect.DeepEqual(e.LegacyUnrestricted, []string{"legacy"}) || !reflect.DeepEqual(e.Sources, []string{"alpha"}) {
		t.Fatalf("linked effects = %+v", e)
	}
	mustStageOK(t, stage.SetExcluded("beta", stageRepoA, true))
	mustStageOK(t, stage.SetExcluded("alpha", stageRepoA, false))
	d = stage.Diff()
	if len(d.Exclusions) != 1 || !reflect.DeepEqual(d.Exclusions[0].Sources, []string{"beta"}) || stageSubscription(t, stage, "alpha").Selection.Selected != 0 {
		t.Fatalf("remaining source missing: %+v", d)
	}
}

func TestStagedClearingOneVetoNamesRemainingSource(t *testing.T) {
	set, hubs, repos := stageFixture()
	for i := range set.Profiles {
		set.Profiles[i].Selection.Include = []string{stageRepoA}
		set.Profiles[i].Selection.Exclude = []string{stageRepoA}
	}
	stage := mustStage(t, set, hubs, repos)
	mustStageOK(t, stage.SetExcluded("alpha", stageRepoA, false))
	d := stage.Diff()
	if d.SelectedCount != 0 || d.ClearedExclusionCount != 1 || len(d.Exclusions) != 1 || !reflect.DeepEqual(d.Exclusions[0].Sources, []string{"beta"}) {
		t.Fatalf("remaining veto = %+v", d)
	}
}

func TestStagedLegacyConversionAndAtomicBranchRefusal(t *testing.T) {
	set, hubs, repos := stageFixture()
	set.Profiles[1].Selection = nil
	stage := mustStage(t, set, hubs, repos)
	for _, selected := range []bool{true, false} {
		if err := stage.SelectCurrent(stageHubOne, true, selected); !errors.Is(err, ErrStageLegacy) {
			t.Fatalf("legacy parent operation = %v", err)
		}
		if !stage.Diff().Empty() {
			t.Fatal("rejected parent action partially edited another profile")
		}
	}
	if err := stage.SelectRepository("beta", stageRepoA, true); !errors.Is(err, ErrStageLegacy) {
		t.Fatalf("individual migration = %v", err)
	}
	if err := stage.SetExcluded("beta", stageRepoA, true); !errors.Is(err, ErrStageLegacy) {
		t.Fatalf("exclude migration = %v", err)
	}
	if err := stage.SetFollow("beta", true); !errors.Is(err, ErrStageLegacy) {
		t.Fatalf("follow migration = %v", err)
	}
	mustStageOK(t, stage.SetHiveEnabled("beta", false))
	if stage.current.Profiles[1].Selection != nil {
		t.Fatal("disable migrated legacy scope")
	}
	mustStageOK(t, stage.ReviewAndRestrict("beta"))
	sub := stageSubscription(t, stage, "beta")
	if sub.Selection.Legacy || sub.Selection.Selected != 2 || sub.Follow || !sub.Disabled {
		t.Fatalf("restrict = %+v", sub)
	}
	if !reflect.DeepEqual(stage.Diff().LegacyConversions, []string{"beta"}) {
		t.Fatal("conversion not named")
	}
	mustStageOK(t, stage.SelectCurrent(stageHubOne, true, true))
	if stage.Diff().SelectedCount != 4 {
		t.Fatalf("bulk branch did not select all: %+v", stage.Diff())
	}
	if set.Profiles[1].Selection != nil {
		t.Fatal("review migrated input profile before Apply")
	}
}

func TestStagedIncompleteMembership(t *testing.T) {
	for _, state := range []TreeDiscovery{TreeUnknown, TreeUnavailable, TreeStale} {
		t.Run(string(state), func(t *testing.T) {
			set, hubs, repos := stageFixture()
			set.Profiles[0].Selection.Repositories = []string{stageRepoA}
			repos["alpha"] = TreeRepositorySnapshot{Discovery: state, Repositories: []string{stageRepoA}}
			stage := mustStage(t, set, hubs, repos)
			if err := stage.SelectCurrent(stageHubOne, true, true); !errors.Is(err, ErrStageIncomplete) {
				t.Fatalf("incomplete bulk inclusion = %v", err)
			}
			if !stage.Diff().Empty() {
				t.Fatal("failed bulk action changed state")
			}
			if stageSubscription(t, stage, "alpha").Selection.State == "all-current" {
				t.Fatal("incomplete membership reported all current")
			}
			mustStageOK(t, stage.SelectRepository("alpha", stageRepoA, false))
			mustStageOK(t, stage.SelectCurrent("beta", false, true))
			mustStageOK(t, stage.SelectCurrent(stageHubOne, true, false))
			set.Profiles[0].Selection = nil
			legacy := mustStage(t, set, hubs, repos)
			if err := legacy.ReviewAndRestrict("alpha"); !errors.Is(err, ErrStageIncomplete) {
				t.Fatalf("incomplete restrict = %v", err)
			}
			if !legacy.Diff().Empty() || legacy.current.Profiles[0].Selection != nil {
				t.Fatal("failed discovery migrated legacy")
			}
		})
	}
	// A complete repository list does not make unavailable hub membership complete.
	set, hubs, repos := stageFixture()
	hubs[0].Discovery = TreeUnavailable
	stage := mustStage(t, set, hubs, repos)
	if err := stage.SelectCurrent(stageHubOne, true, true); !errors.Is(err, ErrStageIncomplete) {
		t.Fatalf("hub bulk = %v", err)
	}
	if err := stage.SetHubEnabled(stageHubOne, true); !errors.Is(err, ErrStageIncomplete) {
		t.Fatalf("hub enable = %v", err)
	}
	mustStageOK(t, stage.SetHubEnabled(stageHubOne, false))
}

func TestStagedSnapshotIsolationAndEmptyMembership(t *testing.T) {
	set, hubs, repos := stageFixture()
	stage := mustStage(t, set, hubs, repos)
	set.Profiles[0].RegistrationToken = "changed-secret"
	set.Hubs[0].Follow = HubFollowOff
	hubs[0].HiveOrigins[0] = "https://unrelated.example"
	repos["alpha"].Repositories[0] = stageRepoB
	tree, err := stage.Tree()
	mustStageOK(t, err)
	tree.Subscriptions[0].Repositories[0].ID = "corrupt"
	tree.Hubs[0].Subscriptions[0] = "corrupt"
	mustStageOK(t, stage.SelectCurrent("alpha", false, true))
	sub := stageSubscription(t, stage, "alpha")
	if sub.SharedAcrossHubs != 2 || sub.Selection.Selected != 2 || stage.current.Profiles[0].RegistrationToken != "registration-secret" {
		t.Fatal("caller mutated the staged snapshot")
	}
	diff := stage.Diff()
	diff.Membership[0].After.Repositories[0] = "corrupt"
	if stageSubscription(t, stage, "alpha").Selection.Selected != 2 {
		t.Fatal("diff exposed mutable consent")
	}
	set, hubs, repos = stageFixture()
	repos["alpha"] = TreeRepositorySnapshot{Discovery: TreeComplete}
	empty := mustStage(t, set, hubs, repos)
	mustStageOK(t, empty.SelectCurrent("alpha", false, true))
	if sub := stageSubscription(t, empty, "alpha"); sub.Selection.State != "off" || sub.Selection.Repositories != 0 || sub.Selection.MembershipIncomplete {
		t.Fatalf("empty branch = %+v", sub)
	}
}

func stageRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	mustStageOK(t, err)
	return data
}

func TestStagedEmptyApplyWritesAndSignalsNothing(t *testing.T) {
	set, hubs, repos := stageFixture()
	store := NewProfileStore(filepath.Join(t.TempDir(), "absent"))
	stage := mustStage(t, set, hubs, repos)
	stage.signal = func(context.Context, *ProfileStore) (RelaySwitchResult, error) {
		t.Fatal("empty Apply signaled relay")
		return RelaySwitchResult{}, nil
	}
	// Net no-op policy toggles and exclusion edits must also be empty.
	mustStageOK(t, stage.SetFollow("alpha", true))
	mustStageOK(t, stage.SetFollow("alpha", false))
	mustStageOK(t, stage.SetExcluded("alpha", stageRepoA, true))
	mustStageOK(t, stage.SetExcluded("alpha", stageRepoA, false))
	receipt, err := stage.Apply(context.Background(), store)
	mustStageOK(t, err)
	if receipt.Saved != StageUnchanged || receipt.RelayReload != StageReloadNotRequested || receipt.Acknowledged != "unknown" {
		t.Fatalf("empty receipt = %+v", receipt)
	}
	if _, err := os.Stat(filepath.Dir(store.Path())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty apply created store: %v", err)
	}
}

func TestStagedApplyReceipts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		running   bool
		signalErr error
		reload    string
	}{
		{"notified", true, nil, StageReloadNotified},
		{"no managed relay", false, nil, StageReloadNoManaged},
		{"failed notification", false, errors.New("provider-secret in command output"), StageReloadFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, hubs, repos := stageFixture()
			store := NewProfileStore(t.TempDir())
			mustStageOK(t, store.Commit(set))
			mustStageOK(t, os.WriteFile(store.EnvPath(), append(stageRead(t, store.EnvPath()), []byte("GITHUB_TOKEN=github-secret\n")...), 0o600))
			stage := mustStage(t, set, hubs, repos)
			mustStageOK(t, stage.SelectRepository("alpha", stageRepoA, true))
			signals := 0
			stage.signal = func(context.Context, *ProfileStore) (RelaySwitchResult, error) {
				signals++
				saved, err := store.Load()
				mustStageOK(t, err)
				if saved.SelectionSummary("alpha") != "1 repos" || !bytes.Contains(stageRead(t, store.EnvPath()), []byte(stageRepoA)) {
					t.Fatal("signal preceded persistence")
				}
				return RelaySwitchResult{Running: tc.running, Target: "provider-secret"}, tc.signalErr
			}
			receipt, err := stage.Apply(context.Background(), store)
			if (tc.signalErr != nil) != errors.Is(err, ErrStageReload) {
				t.Fatalf("apply error = %v", err)
			}
			if receipt.Saved != StageSaved || receipt.RelayReload != tc.reload || receipt.Acknowledged != "pending" || receipt.ScopeVersion != 2 || signals != 1 {
				t.Fatalf("receipt = %+v; signals = %d", receipt, signals)
			}
			assertStageNoSecrets(t, receipt)
			if err != nil && strings.Contains(err.Error(), "provider-secret") {
				t.Fatal("returned secret command error")
			}
			before := stageRead(t, store.Path())
			again, err := stage.Apply(context.Background(), store)
			mustStageOK(t, err)
			if again.Saved != StageUnchanged || signals != 1 || !bytes.Equal(before, stageRead(t, store.Path())) {
				t.Fatal("repeated apply repeated persistence or notification")
			}
			if !bytes.Contains(stageRead(t, store.EnvPath()), []byte("GITHUB_TOKEN=github-secret")) {
				t.Fatal("lost unmanaged credential during projection")
			}
		})
	}
}

func TestStagedApplyUsesRealMissingRelayResult(t *testing.T) {
	set, hubs, repos := stageFixture()
	stage := mustStage(t, set, hubs, repos)
	mustStageOK(t, stage.SetHiveEnabled("alpha", false))
	receipt, err := stage.Apply(context.Background(), NewProfileStore(t.TempDir()))
	mustStageOK(t, err)
	if receipt.RelayReload != StageReloadNoManaged || receipt.Acknowledged != "pending" {
		t.Fatalf("missing pid receipt = %+v", receipt)
	}
}

func TestStagedStaleApplyRequiresNewReview(t *testing.T) {
	set, hubs, repos := stageFixture()
	store := NewProfileStore(t.TempDir())
	mustStageOK(t, store.Commit(set))
	stage := mustStage(t, set, hubs, repos)
	mustStageOK(t, stage.SelectRepository("alpha", stageRepoA, true))
	other, err := store.Load()
	mustStageOK(t, err)
	mustStageOK(t, other.SetDisabled("beta", true))
	mustStageOK(t, store.CommitExpecting(other, other.Revision()))
	profiles, env := stageRead(t, store.Path()), stageRead(t, store.EnvPath())
	stage.signal = func(context.Context, *ProfileStore) (RelaySwitchResult, error) {
		t.Fatal("stale apply signaled")
		return RelaySwitchResult{}, nil
	}
	receipt, err := stage.Apply(context.Background(), store)
	if !errors.Is(err, ErrStaleRevision) || receipt.Saved != StageNotSaved || !stage.Diff().Stale {
		t.Fatalf("stale = %+v, %v", receipt, err)
	}
	if !bytes.Equal(profiles, stageRead(t, store.Path())) || !bytes.Equal(env, stageRead(t, store.EnvPath())) {
		t.Fatal("stale Apply overwrote a file")
	}
	mustStageOK(t, stage.Discard())
	if err := stage.SetHiveEnabled("alpha", false); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale review accepted new edit: %v", err)
	}
	if _, err := stage.Apply(context.Background(), store); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("discard bypassed renewed review: %v", err)
	}
	fresh := mustStage(t, other, hubs, repos)
	mustStageOK(t, fresh.SelectRepository("alpha", stageRepoA, true))
	_, err = fresh.Apply(context.Background(), store)
	mustStageOK(t, err)
	saved, err := store.Load()
	mustStageOK(t, err)
	if !saved.Profiles[1].Disabled {
		t.Fatal("renewed review lost concurrent change")
	}
}

func TestStagedProjectionFailureAndGuardedRetry(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		name := "retry"
		if concurrent {
			name = "concurrent edit before retry"
		}
		t.Run(name, func(t *testing.T) {
			set, hubs, repos := stageFixture()
			store := NewProfileStore(t.TempDir())
			mustStageOK(t, store.Save(set))
			// A directory at contributor.env reliably fails projection, including as root.
			mustStageOK(t, os.Mkdir(store.EnvPath(), 0o700))
			stage := mustStage(t, set, hubs, repos)
			mustStageOK(t, stage.SelectRepository("alpha", stageRepoA, true))
			signals := 0
			stage.signal = func(context.Context, *ProfileStore) (RelaySwitchResult, error) {
				signals++
				return RelaySwitchResult{}, nil
			}
			receipt, err := stage.Apply(context.Background(), store)
			if !errors.Is(err, ErrStageProjection) || receipt.Saved != StageProfilesSaved || receipt.RelayReload != StageReloadNotRequested || receipt.Acknowledged != "unknown" || signals != 0 {
				t.Fatalf("partial receipt = %+v, %v", receipt, err)
			}
			saved, err := store.Load()
			mustStageOK(t, err)
			if saved.SelectionSummary("alpha") != "1 repos" || stage.Diff().Empty() {
				t.Fatal("partial save lost profiles or retry diff")
			}
			if err := stage.Discard(); !errors.Is(err, ErrStageProjection) {
				t.Fatal("discard claimed to undo already-saved profiles")
			}
			if err := stage.SetFollow("alpha", true); !errors.Is(err, ErrStageProjection) {
				t.Fatal("edited a partially saved diff")
			}
			mustStageOK(t, os.Remove(store.EnvPath()))
			if concurrent {
				mustStageOK(t, saved.SetDisabled("beta", true))
				mustStageOK(t, store.CommitExpecting(saved, saved.Revision()))
				profiles, env := stageRead(t, store.Path()), stageRead(t, store.EnvPath())
				receipt, err = stage.Apply(context.Background(), store)
				if !errors.Is(err, ErrStaleRevision) || !stage.Diff().Stale || signals != 0 || receipt.Saved != StageProfilesSaved {
					t.Fatalf("retry stale = %+v, %v", receipt, err)
				}
				if !bytes.Equal(profiles, stageRead(t, store.Path())) || !bytes.Equal(env, stageRead(t, store.EnvPath())) {
					t.Fatal("retry overwrote concurrent edit")
				}
			} else {
				receipt, err = stage.Apply(context.Background(), store)
				mustStageOK(t, err)
				if receipt.Saved != StageSaved || receipt.Acknowledged != "pending" || signals != 1 || !stage.Diff().Empty() {
					t.Fatalf("retry = %+v", receipt)
				}
				if !bytes.Contains(stageRead(t, store.EnvPath()), []byte(stageRepoA)) {
					t.Fatal("retry did not repair projection")
				}
			}
		})
	}
}

func TestStagedSaveFailureAndCancellation(t *testing.T) {
	set, hubs, repos := stageFixture()
	stage := mustStage(t, set, hubs, repos)
	mustStageOK(t, stage.SetFollow("alpha", true))
	stage.signal = func(context.Context, *ProfileStore) (RelaySwitchResult, error) {
		t.Fatal("failed save signaled")
		return RelaySwitchResult{}, nil
	}
	path := filepath.Join(t.TempDir(), "file")
	mustStageOK(t, os.WriteFile(path, []byte("file prevents creating store directory"), 0o600))
	receipt, err := stage.Apply(context.Background(), NewProfileStore(path))
	if !errors.Is(err, ErrStageSave) || receipt.Saved != StageNotSaved || receipt.Acknowledged != "unknown" {
		t.Fatalf("failed save = %+v, %v", receipt, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := NewProfileStore(filepath.Join(t.TempDir(), "absent"))
	receipt, err = stage.Apply(ctx, store)
	if !errors.Is(err, context.Canceled) || receipt.Saved != StageNotSaved {
		t.Fatalf("cancel = %+v, %v", receipt, err)
	}
	if _, err := os.Stat(filepath.Dir(store.Path())); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled apply created a file")
	}
	if stage.Diff().Empty() {
		t.Fatal("failed apply discarded edits")
	}
}

func assertStageNoSecrets(t *testing.T, value any) {
	t.Helper()
	j, err := json.Marshal(value)
	mustStageOK(t, err)
	y, err := yaml.Marshal(value)
	mustStageOK(t, err)
	for _, data := range [][]byte{j, y} {
		for _, secret := range []string{"registration-secret", "other-secret", "legacy-secret", "provider-secret", "github-secret", "endpoint-secret"} {
			if bytes.Contains(data, []byte(secret)) {
				t.Fatalf("serialized output contains %q", secret)
			}
		}
	}
}

func TestStagedDiffNeverContainsSecrets(t *testing.T) {
	set, hubs, repos := stageFixture()
	stage := mustStage(t, set, hubs, repos)
	mustStageOK(t, stage.SelectCurrent(stageHubOne, true, true))
	mustStageOK(t, stage.SetExcluded("alpha", stageRepoB, true))
	mustStageOK(t, stage.SetFollow("alpha", true))
	mustStageOK(t, stage.SetHubEnabled(stageHubOne, false))
	assertStageNoSecrets(t, stage.Diff())
	tree, err := stage.Tree()
	mustStageOK(t, err)
	assertStageNoSecrets(t, tree)
	first, err := json.Marshal(stage.Diff())
	mustStageOK(t, err)
	second, err := json.Marshal(stage.Diff())
	mustStageOK(t, err)
	if !bytes.Equal(first, second) {
		t.Fatal("diff order is nondeterministic")
	}
	// Even validation failures must not forward a bad credential's value.
	set.Profiles[0].RegistrationToken = "registration-secret\n"
	if _, err := NewStagedEdits(set, hubs, repos); err == nil || strings.Contains(err.Error(), "registration-secret") {
		t.Fatalf("validation redaction = %v", err)
	}
}

func TestStagedSelectCurrentRetainsSavedTargetsAndRefreshSource(t *testing.T) {
	for _, mode := range []string{SelectionSelected, SelectionAllCurrent} {
		t.Run(mode, func(t *testing.T) {
			set, hubs, repos := stageFixture()
			saved := "github/github.com/org/saved-but-no-longer-advertised"
			selection := set.Profiles[0].Selection
			selection.Mode = mode
			if mode == SelectionSelected {
				selection.Repositories = []string{saved}
			} else {
				selection.MembershipSnapshot = []string{saved}
			}
			selection.FollowNewRepositories = true
			selection.Sources = []string{"curated"}
			stage := mustStage(t, set, hubs, repos)
			mustStageOK(t, stage.SelectCurrent("alpha", false, true))
			p := stage.current.Profiles[0]
			if p.Selection.Mode != mode || !p.Selection.FollowNewRepositories || !reflect.DeepEqual(p.Selection.Sources, []string{"curated"}) {
				t.Fatal("select current changed the approved refresh source")
			}
			if sub := stageSubscription(t, stage, "alpha"); sub.Selection.Selected != 3 || sub.Selection.State != "all-current" {
				t.Fatalf("select current removed saved membership: %+v", sub)
			}
		})
	}
}

func TestStagedSeparateIdentitiesAtOneOrigin(t *testing.T) {
	set, hubs, repos := stageFixture()
	set.Profiles[1].Hub = set.Profiles[0].Hub
	stage := mustStage(t, set, hubs, repos)
	mustStageOK(t, stage.SelectRepository("alpha", stageRepoA, true))
	if stageSubscription(t, stage, "beta").Selection.Selected != 0 {
		t.Fatal("individual edit merged identities at one origin")
	}
	mustStageOK(t, stage.SetHubEnabled(stageHubTwo, false))
	if !stageSubscription(t, stage, "alpha").Disabled || !stageSubscription(t, stage, "beta").Disabled {
		t.Fatal("hub action missed a distinct subscription at the advertised origin")
	}
	if len(stage.Diff().SharedBranches) != 2 {
		t.Fatal("shared identity effects were not named separately")
	}
}

// A stale snapshot beside mode "selected" must not hide repositories that are
// new, and opening a session never stages anything by itself.
func TestStagedFollowStagesWaitingRepositoriesAsAuto(t *testing.T) {
	set, hubs, repos := stageFixture()
	set.Profiles[0].Selection = &RepositorySelection{Mode: SelectionSelected, Repositories: []string{stageRepoA}, MembershipSnapshot: []string{stageRepoA, stageRepoB}}
	stage := mustStage(t, set, hubs, repos)
	if !stage.Diff().Empty() {
		t.Fatal("opening staged edits changed consent")
	}
	mustStageOK(t, stage.SetFollow("alpha", true))
	sub := stageSubscription(t, stage, "alpha")
	if sub.Selection.Selected != 2 || sub.Selection.Auto != 1 {
		t.Fatalf("selection = %+v", sub.Selection)
	}
	for _, repo := range sub.Repositories {
		if want := repo.ID == stageRepoB; repo.Auto != want || repo.Selected != true {
			t.Fatalf("repository %s: selected=%v auto=%v, want auto=%v", repo.ID, repo.Selected, repo.Auto, want)
		}
	}
	d := stage.Diff()
	if !reflect.DeepEqual(d.Auto, []StageTarget{{"alpha", stageRepoB}}) {
		t.Fatalf("Auto = %+v", d.Auto)
	}
	if len(d.Membership) != 1 || !reflect.DeepEqual(d.Membership[0].After.Auto, []string{stageRepoB}) {
		t.Fatalf("the saved selection must carry the marker: %+v", d.Membership)
	}
	if set.Profiles[0].Selection.Auto != nil || set.Profiles[0].Selection.FollowNewRepositories {
		t.Fatal("staging modified the original consent")
	}
}

func TestStagedFollowAddsNothingWhenDiscoveryIsIncomplete(t *testing.T) {
	set, hubs, repos := stageFixture()
	repos["alpha"] = TreeRepositorySnapshot{Discovery: TreeUnavailable}
	stage := mustStage(t, set, hubs, repos)
	mustStageOK(t, stage.SetFollow("alpha", true))
	sel := stage.current.Profiles[0].Selection
	if !sel.FollowNewRepositories || len(sel.Repositories) != 0 || len(sel.Auto) != 0 {
		t.Fatalf("an outage must not read as \"everything is new\": %+v", sel)
	}
}

// Exclusions are a union across subscriptions, so a repository vetoed anywhere
// is not added automatically anywhere.
func TestStagedFollowSkipsRepositoriesExcludedOnAnySubscription(t *testing.T) {
	set, hubs, repos := stageFixture()
	set.Profiles[1].Selection.Exclude = []string{stageRepoA}
	stage := mustStage(t, set, hubs, repos)
	mustStageOK(t, stage.SetFollow("alpha", true))
	if got := stage.current.Profiles[0].Selection.Auto; !reflect.DeepEqual(got, []string{stageRepoB}) {
		t.Fatalf("Auto = %v, want only b", got)
	}
}

// Under a followed Hive, turning a repository off must also exclude it, or the
// next check would see it as new and add it straight back. Without follow it is
// just off.
func TestStagedTurningOffUnderFollowExcludes(t *testing.T) {
	for _, follow := range []bool{true, false} {
		set, hubs, repos := stageFixture()
		set.Profiles[0].Selection = &RepositorySelection{Mode: SelectionSelected, Repositories: []string{stageRepoA, stageRepoB}, Auto: []string{stageRepoB}, FollowNewRepositories: follow}
		stage := mustStage(t, set, hubs, repos)
		mustStageOK(t, stage.SelectRepository("alpha", stageRepoB, false))
		sel := stage.current.Profiles[0].Selection
		if excluded := reflect.DeepEqual(sel.Exclude, []string{stageRepoB}); excluded != follow {
			t.Fatalf("follow=%v: Exclude = %v", follow, sel.Exclude)
		}
		if sel.Auto != nil || !reflect.DeepEqual(sel.Repositories, []string{stageRepoA}) {
			t.Fatalf("follow=%v: selection = %+v", follow, sel)
		}
		if follow {
			if d := stage.Diff(); d.ExcludedCount != 1 || len(d.Exclusions) != 1 || d.Exclusions[0].Repository != stageRepoB {
				t.Fatalf("the exclusion must be named in the diff: %+v", d.Exclusions)
			}
		}
	}
	set, hubs, repos := stageFixture()
	set.Profiles[0].Selection = &RepositorySelection{Mode: SelectionSelected, Repositories: []string{stageRepoA}, FollowNewRepositories: true}
	stage := mustStage(t, set, hubs, repos)
	mustStageOK(t, stage.SelectRepository("alpha", stageRepoB, false))
	if got := stage.current.Profiles[0].Selection.Exclude; len(got) != 0 {
		t.Fatalf("turning off a repository that was not on created an exclusion: %v", got)
	}
}

func TestStagedAcceptFollowedOnlyTouchesFollowedHives(t *testing.T) {
	set, hubs, repos := stageFixture()
	set.Profiles[0].Selection.FollowNewRepositories = true
	stage := mustStage(t, set, hubs, repos)
	n, err := stage.AcceptFollowed()
	if err != nil || n != 2 {
		t.Fatalf("staged %d, %v; want a and b on alpha", n, err)
	}
	if beta := stage.current.Profiles[1].Selection; len(beta.Repositories) != 0 || len(beta.Auto) != 0 {
		t.Fatalf("a Hive that is not followed was changed: %+v", beta)
	}
	if n, err := stage.AcceptFollowed(); err != nil || n != 0 {
		t.Fatalf("second call staged %d, %v; want a no-op", n, err)
	}
}

func TestAutoSurvivesYAMLRoundTripAndIsOmittedWhenEmpty(t *testing.T) {
	selection := &RepositorySelection{Mode: SelectionSelected, Repositories: []string{stageRepoA}, Auto: []string{stageRepoA}}
	data, err := yaml.Marshal(selection)
	if err != nil || !strings.Contains(string(data), "auto:") {
		t.Fatalf("marshal = %q, %v", data, err)
	}
	var back RepositorySelection
	if err := yaml.Unmarshal(data, &back); err != nil || !reflect.DeepEqual(&back, selection) {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	plain, err := yaml.Marshal(&RepositorySelection{Mode: SelectionSelected})
	if err != nil || strings.Contains(string(plain), "auto") {
		t.Fatalf("an empty Auto must not appear on disk: %q, %v", plain, err)
	}
}

// Staged edits stay reversible: follow on then off undoes the automatic members
// staged in this session, while automatic members that were already saved are
// real membership and stay.
func TestStagedFollowOnThenOffIsANetNoOpButKeepsSavedAuto(t *testing.T) {
	set, hubs, repos := stageFixture()
	set.Profiles[0].Selection = &RepositorySelection{Mode: SelectionSelected, Repositories: []string{stageRepoA}, Auto: []string{stageRepoA}, FollowNewRepositories: true}
	stage := mustStage(t, set, hubs, repos)
	mustStageOK(t, stage.SetFollow("alpha", false))
	sel := stage.current.Profiles[0].Selection
	if !reflect.DeepEqual(sel.Repositories, []string{stageRepoA}) || !reflect.DeepEqual(sel.Auto, []string{stageRepoA}) {
		t.Fatalf("turning follow off dropped a saved automatic member: %+v", sel)
	}
	mustStageOK(t, stage.SetFollow("alpha", true))
	if got := stage.current.Profiles[0].Selection.Auto; !reflect.DeepEqual(got, []string{stageRepoA, stageRepoB}) {
		t.Fatalf("follow on did not stage the new repository: Auto = %v", got)
	}
	mustStageOK(t, stage.SetFollow("alpha", false))
	sel = stage.current.Profiles[0].Selection
	if !reflect.DeepEqual(sel.Repositories, []string{stageRepoA}) || !reflect.DeepEqual(sel.Auto, []string{stageRepoA}) {
		t.Fatalf("on-then-off left staged members behind: %+v", sel)
	}
	if d := stage.Diff(); len(d.Auto) != 0 || len(d.Selected) != 0 {
		t.Fatalf("diff should show no automatic additions: %+v", d)
	}
}
