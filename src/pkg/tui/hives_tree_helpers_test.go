package tui

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl"
)

// Pure projection helpers behind the contribution tree (#13291): no model, no
// store, no goroutines.

func treeSel(state string, selected, repos int, mod func(*hivectl.TreeSelection)) hivectl.TreeSelection {
	sel := hivectl.TreeSelection{State: state}
	sel.Selected, sel.Repositories = selected, repos
	if mod != nil {
		mod(&sel)
	}
	return sel
}

func TestHivesTreeMarkAndStateLabel(t *testing.T) {
	tests := []struct {
		name        string
		sel         hivectl.TreeSelection
		mark, label string
	}{
		{"all", treeSel("all-current", 2, 2, nil), "x", "All current"},
		{"partial", treeSel("partial", 1, 2, nil), "-", "Partial"},
		{"off", treeSel("off", 0, 2, nil), " ", "Off"},
		{"legacy", treeSel("off", 0, 2, func(s *hivectl.TreeSelection) { s.Legacy = true }), "-", "Unrestricted — legacy"},
		{"unknown", treeSel("mystery", 0, 0, nil), " ", "mystery"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hivesTreeMark(tc.sel); got != tc.mark {
				t.Errorf("mark = %q, want %q", got, tc.mark)
			}
			if got := hivesTreeStateLabel(tc.sel); got != tc.label {
				t.Errorf("label = %q, want %q", got, tc.label)
			}
		})
	}
}

func TestHivesTreeBadges(t *testing.T) {
	tests := []struct {
		name     string
		sel      hivectl.TreeSelection
		shared   int
		disabled bool
		want     []string
	}{
		{"none", treeSel("off", 0, 0, nil), 1, false, nil},
		{"disabled", treeSel("off", 0, 0, nil), 1, true, []string{"Disabled"}},
		{"everything", treeSel("partial", 1, 3, func(s *hivectl.TreeSelection) {
			s.Excluded, s.Unreviewed, s.Legacy, s.MembershipIncomplete = 1, 1, true, true
		}), 3, true, []string{"Disabled", "Excluded", "Unreviewed", "Shared across 3 hubs", "Unrestricted — legacy", "Membership incomplete"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hivesTreeBadges(tc.sel, tc.shared, tc.disabled); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("badges = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHivesTreeExplainScopeAndExclusions(t *testing.T) {
	one := treeSel("partial", 1, 3, nil)
	if got := hivesTreeExplain(one, 1); got != "Partial: 1 of 3 repositories selected" {
		t.Errorf("explain = %q", got)
	}
	incomplete := treeSel("all-current", 3, 3, func(s *hivectl.TreeSelection) { s.MembershipIncomplete = true })
	got := hivesTreeScope(incomplete, 2)
	for _, want := range []string{"All current: 3 of 3", "across 2 subscriptions", "Membership incomplete"} {
		if !strings.Contains(got, want) {
			t.Errorf("scope %q missing %q", got, want)
		}
	}
	if got := hivesTreeExclusions(one); got != "none" {
		t.Errorf("exclusions = %q", got)
	}
	excl := treeSel("partial", 1, 3, func(s *hivectl.TreeSelection) { s.Excluded = 2 })
	if got := hivesTreeExclusions(excl); !strings.HasPrefix(got, "2 excluded;") {
		t.Errorf("exclusions = %q", got)
	}
}

// One unrestricted Hive must not describe its restricted siblings, and an
// unrestricted Hive must never read as "0 of N selected" (it receives all N).
func TestHivesTreeGroupExplainKeepsRestrictedSiblingsVisible(t *testing.T) {
	restricted := hivectl.TreeSubscription{Selection: treeSel("partial", 2, 12, nil)}
	legacy := hivectl.TreeSubscription{Selection: treeSel("off", 0, 12, func(s *hivectl.TreeSelection) { s.Legacy = true })}
	hub := treeSel("partial", 4, 36, func(s *hivectl.TreeSelection) { s.Legacy = true })
	got := hivesTreeGroupExplain(hub, []hivectl.TreeSubscription{restricted, restricted, legacy})
	for _, want := range []string{"4 of 24 selected in 2 restricted Hive(s)", "1 unrestricted — legacy"} {
		if !strings.Contains(got, want) {
			t.Errorf("group %q missing %q", got, want)
		}
	}
	if got := hivesTreeExplain(legacy.Selection, 1); !strings.HasPrefix(got, "Unrestricted — legacy:") || strings.Contains(got, "0 of") {
		t.Errorf("single unrestricted explain = %q", got)
	}
	if got := hivesTreeGroupExplain(treeSel("off", 0, 4, nil), []hivectl.TreeSubscription{restricted, restricted}); !strings.Contains(got, "across 2 subscriptions") {
		t.Errorf("all-restricted group must keep the plain scope wording, got %q", got)
	}
}

func TestHivesTreeProvenance(t *testing.T) {
	set := &hivectl.ProfileSet{Presets: []hivectl.PresetProvenance{{ID: "core", ReviewedRevision: "r7"}}}
	tests := []struct {
		name    string
		set     *hivectl.ProfileSet
		sources []string
		want    string
	}{
		{"no sources", set, nil, "attribution unavailable"},
		{"nil set", nil, []string{"core"}, "attribution unavailable"},
		{"known", set, []string{"core"}, "preset core revision r7"},
		{"unknown", set, []string{"ghost"}, "ghost (attribution unavailable)"},
		{"mixed", set, []string{"core", "ghost"}, "preset core revision r7; ghost (attribution unavailable)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hivesTreeProvenance(tc.set, tc.sources); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHivesTreeNames(t *testing.T) {
	hubs := []struct {
		hub  hivectl.TreeHub
		want string
	}{
		{hivectl.TreeHub{ID: "https://hub.example", Name: "Named"}, "Named"},
		{hivectl.TreeHub{ID: "https://hub.example"}, "hub.example"},
		{hivectl.TreeHub{ID: "http://plain.example"}, "plain.example"},
	}
	for _, tc := range hubs {
		if got := hivesTreeHubName(tc.hub); got != tc.want {
			t.Errorf("hub name = %q, want %q", got, tc.want)
		}
	}
	repos := map[string]string{
		"github/hub/acme/widget": "acme/widget",
		"acme/widget":            "acme/widget",
		"":                       "",
	}
	for id, want := range repos {
		if got := hivesTreeRepoName(id); got != want {
			t.Errorf("repo name(%q) = %q, want %q", id, got, want)
		}
	}
	if onOff(true) != "on" || onOff(false) != "off" || enabledWord(true) != "enabled" || enabledWord(false) != "disabled" {
		t.Error("on/off wording wrong")
	}
}

func TestHivesTreeErrorText(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"stale", fmt.Errorf("wrap: %w", hivectl.ErrStaleRevision), "profiles changed; discard this stale review and review again before Apply"},
		{"no store", errNoHivesStore, errNoHivesStore.Error()},
		{"tree store", errHivesTreeStore, errHivesTreeStore.Error()},
		{"incomplete", fmt.Errorf("x: %w", hivectl.ErrStageIncomplete), hivectl.ErrStageIncomplete.Error()},
		{"legacy", hivectl.ErrStageLegacy, hivectl.ErrStageLegacy.Error()},
		{"save", hivectl.ErrStageSave, hivectl.ErrStageSave.Error()},
		{"projection", hivectl.ErrStageProjection, hivectl.ErrStageProjection.Error()},
		{"reload", hivectl.ErrStageReload, hivectl.ErrStageReload.Error()},
		{"unknown hides detail", errors.New("open /home/u/.config/hive/secret: denied"), "cannot stage contribution edit; review the target and try again"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := hivesTreeErrorText(tc.err)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "secret") {
				t.Error("error text leaked the underlying detail")
			}
		})
	}
}

func TestHivesTreeReceiptLines(t *testing.T) {
	got := hivesTreeReceiptLines(hivectl.ApplyReceipt{Saved: "yes", RelayReload: "signaled", Acknowledged: "no"})
	if len(got) != 4 || got[0] != "Saved: yes" || got[1] != "Relay reload: signaled" || got[2] != "Acknowledged: no" || !strings.Contains(got[3], "does not prove relay adoption") {
		t.Errorf("receipt = %q", got)
	}
}

func TestHivesTreeDiffLines(t *testing.T) {
	if got := hivesTreeDiffLines(hivectl.ContributionDiff{Stale: true}); len(got) != 1 || !strings.HasPrefix(got[0], "Stale review") {
		t.Errorf("stale = %q", got)
	}
	if got := hivesTreeDiffLines(hivectl.ContributionDiff{}); len(got) != 1 || got[0] != "No staged changes." {
		t.Errorf("empty = %q", got)
	}
	d := hivectl.ContributionDiff{
		Membership:            []hivectl.StageSelectionChange{{Subscription: "alpha"}},
		SelectedCount:         1,
		DeselectedCount:       1,
		ExcludedCount:         2,
		ClearedExclusionCount: 1,
		Selected:              []hivectl.StageTarget{{Subscription: "alpha", Repository: "acme/a"}},
		Deselected:            []hivectl.StageTarget{{Subscription: "alpha", Repository: "acme/b"}},
		Exclusions: []hivectl.StageExclusionEffect{
			{StageTarget: hivectl.StageTarget{Subscription: "alpha", Repository: "acme/c"}, Excluded: true, Sources: []string{"beta"}, ScopedSubscriptions: []string{"gamma"}, LegacyUnrestricted: []string{"old"}},
			{StageTarget: hivectl.StageTarget{Subscription: "alpha", Repository: "acme/d"}},
		},
		Follow:            []hivectl.StageBoolChange{{Target: "alpha", Before: false, After: true}},
		Enabled:           []hivectl.StageBoolChange{{Target: "beta", Before: true, After: false}},
		HubFollow:         []hivectl.StageHubFollowChange{{Hub: "h", Before: "off", After: "on"}},
		LegacyConversions: []string{"old"},
		SharedBranches:    []hivectl.StageSharedBranch{{Subscription: "alpha", Hubs: []string{"z", "a"}}},
	}
	got := strings.Join(hivesTreeDiffLines(d), "\n")
	for _, want := range []string{
		"+1 repositories selected, −1 deselected, 2 exclusions added, 1 cleared; follow changes: 1; enable/disable: 1; legacy conversions: 1.",
		"+ select acme/a on alpha",
		"− deselect acme/b on alpha",
		"exclude acme/c on alpha; still excluded by beta; affects gamma; Unrestricted — legacy still receives it on old",
		"clear exclusion of acme/d on alpha",
		"follow new repositories on alpha: off → on",
		"beta: enabled → disabled",
		"hub h follow: off → on",
		"review and restrict old: Unrestricted — legacy → current membership",
		"shared: alpha also appears under a, z",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diff missing %q in:\n%s", want, got)
		}
	}
}
